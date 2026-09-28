package model

import (
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	RuleCoverAll = iota
	RuleCoverIgnoreAll
)

var ruleNow = time.Now

// MaxAlertRuleDuration 保证 uint64 持久化值转 int 时在所有架构都不回绕（取 MaxInt32）。
const MaxAlertRuleDuration uint64 = 1<<31 - 1

// MaxAlertRuleCycleInterval 为日历换算（周=7*interval）在 32 位平台留出乘法余量。
const MaxAlertRuleCycleInterval uint64 = (1<<31 - 1) / 7

// ruleStateMetrics 即时指标：规则类型 → 从服务器状态取值（fork 已移除 gpu/gpu_max/temperature_max）。
// 周期流量类型（*_cycle）另由 cycleTransferValue 求值，见 IsTransferDurationRule。
var ruleStateMetrics = map[string]func(*Server) float64{
	"cpu":            func(s *Server) float64 { return s.State.CPU },
	"memory":         func(s *Server) float64 { return percentage(s.State.MemUsed, s.Host.MemTotal) },
	"swap":           func(s *Server) float64 { return percentage(s.State.SwapUsed, s.Host.SwapTotal) },
	"disk":           func(s *Server) float64 { return percentage(s.State.DiskUsed, s.Host.DiskTotal) },
	"net_in_speed":   func(s *Server) float64 { return float64(s.State.NetInSpeed) },
	"net_out_speed":  func(s *Server) float64 { return float64(s.State.NetOutSpeed) },
	"net_all_speed":  func(s *Server) float64 { return float64(s.State.NetInSpeed + s.State.NetOutSpeed) },
	"transfer_in":    func(s *Server) float64 { return float64(s.State.NetInTransfer) },
	"transfer_out":   func(s *Server) float64 { return float64(s.State.NetOutTransfer) },
	"transfer_all":   func(s *Server) float64 { return float64(s.State.NetOutTransfer + s.State.NetInTransfer) },
	"offline":        lastActiveUnix,
	"load1":          func(s *Server) float64 { return s.State.Load1 },
	"load5":          func(s *Server) float64 { return s.State.Load5 },
	"load15":         func(s *Server) float64 { return s.State.Load15 },
	"tcp_conn_count": func(s *Server) float64 { return float64(s.State.TcpConnCount) },
	"udp_conn_count": func(s *Server) float64 { return float64(s.State.UdpConnCount) },
	"process_count":  func(s *Server) float64 { return float64(s.State.ProcessCount) },
}

// lastActiveUnix 离线规则的取值：最后活跃时间（秒），从未上报为 0。
func lastActiveUnix(s *Server) float64 {
	if s.LastActive.IsZero() {
		return 0
	}
	return float64(s.LastActive.Unix())
}

type NResult struct {
	N uint64
}

type Rule struct {
	// 指标类型，取值见 ruleStateMetrics（cpu/memory/offline 等即时指标）与 IsTransferDurationRule（*_cycle 周期流量）
	Type          string          `json:"type"`
	Min           float64         `json:"min,omitempty" validate:"optional"`                                                        // 最小阈值 (百分比、字节 kb ÷ 1024)
	Max           float64         `json:"max,omitempty" validate:"optional"`                                                        // 最大阈值 (百分比、字节 kb ÷ 1024)
	CycleStart    *time.Time      `json:"cycle_start,omitempty" validate:"optional"`                                                // 流量统计的开始时间
	CycleInterval uint64          `json:"cycle_interval,omitempty" validate:"optional"`                                             // 流量统计周期
	CycleUnit     string          `json:"cycle_unit,omitempty" enums:"hour,day,week,month,year" validate:"optional" default:"hour"` // 流量统计周期单位，默认hour,可选(hour, day, week, month, year)
	Duration      uint64          `json:"duration,omitempty" validate:"optional"`                                                   // 连续采样次数（告警每 3 秒采样一次，实际时长 = duration × 3 秒），至少 3
	Cover         uint64          `json:"cover"`                                                                                    // 覆盖范围 RuleCoverAll/IgnoreAll
	Ignore        map[uint64]bool `json:"ignore,omitempty" validate:"optional"`                                                     // 覆盖范围的排除

	// 只作为缓存使用，记录下次该检测的时间
	NextTransferAt  map[uint64]time.Time `json:"-"`
	LastCycleStatus map[uint64]bool      `json:"-"`
}

// quoteIdent 按方言引用列名（in/out 为 SQL 保留字，postgres 需双引号，mysql/sqlite 用反引号）。
func quoteIdent(db *gorm.DB, col string) string {
	if db.Dialector.Name() == "postgres" {
		return `"` + col + `"`
	}
	return "`" + col + "`"
}

// transferTimeCond 跨库时间条件；sqlite 用 datetime() 规范化时区，pg/mysql 直接比较 timestamp。
func transferTimeCond(db *gorm.DB) string {
	if db.Dialector.Name() == "sqlite" {
		return "datetime(created_at) >= datetime(?) AND server_id = ?"
	}
	return "created_at >= ? AND server_id = ?"
}

func percentage(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

// IsSupportedType 规则类型是否有求值器；未知字符串不得进入持久化告警流水线。
func (u *Rule) IsSupportedType() bool {
	if u == nil {
		return false
	}
	_, ok := ruleStateMetrics[u.Type]
	return ok || u.IsTransferDurationRule()
}

// DurationInt 无截断地转换持久化的 duration；ok=false 视为非法规则。
func (u *Rule) DurationInt() (duration int, ok bool) {
	if u == nil || u.Duration > MaxAlertRuleDuration {
		return 0, false
	}
	return int(u.Duration), true
}

// HasSafeCycleConfiguration 校验周期求值器随后会转换/解引用的全部字段（空 unit 为合法的小时写法）。
func (u *Rule) HasSafeCycleConfiguration() bool {
	return u != nil && u.IsTransferDurationRule() && u.CycleStart != nil &&
		u.CycleInterval > 0 && u.CycleInterval <= MaxAlertRuleCycleInterval
}

// Snapshot 未通过规则返回 false, 通过返回 true
func (u *Rule) Snapshot(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) bool {
	if !u.appliesTo(server, cycleTransferStats) {
		return true
	}
	// 循环区间流量检测 · 短期无需重复检测
	if u.IsTransferDurationRule() && u.NextTransferAt[server.ID].After(time.Now()) {
		return u.LastCycleStatus[server.ID]
	}

	src, ok := u.metricValue(server, db)
	if !ok {
		return true
	}
	if u.IsTransferDurationRule() {
		u.recordCycleTransfer(cycleTransferStats, server, src)
	}

	if u.Type == "offline" && float64(time.Now().Unix())-src > 6 {
		return false
	}
	return !u.outOfRange(src)
}

// appliesTo 判断规则是否需要对该服务器求值；不需要时 Snapshot 直接视为通过。
func (u *Rule) appliesTo(server *Server, cycleTransferStats *CycleTransferStats) bool {
	if u == nil || server == nil || !u.IsSupportedType() {
		return false
	}
	if u.IsTransferDurationRule() && (!u.HasSafeCycleConfiguration() || cycleTransferStats == nil) {
		return false
	}
	switch u.Cover {
	case RuleCoverAll: // 监控全部但是排除了此服务器
		return !u.Ignore[server.ID]
	case RuleCoverIgnoreAll: // 忽略全部但是指定监控了此服务器
		return u.Ignore[server.ID]
	default:
		return true
	}
}

// outOfRange 指标是否越过 Max（>）或低于 Min（<）；阈值为 0 表示不限。
func (u *Rule) outOfRange(src float64) bool {
	return (u.Max > 0 && src > u.Max) || (u.Min > 0 && src < u.Min)
}

// metricValue 取规则关注的指标当前值；未知类型返回 false。
func (u *Rule) metricValue(server *Server, db *gorm.DB) (float64, bool) {
	if u.IsTransferDurationRule() {
		return u.cycleTransferValue(server, db), true
	}
	get, ok := ruleStateMetrics[u.Type]
	if !ok {
		return 0, false
	}
	return get(server), true
}

// cycleTransferValue 周期流量 = 本次上报相对上次快照的增量 + 周期内已落库的 Transfer 汇总。
func (u *Rule) cycleTransferValue(server *Server, db *gorm.DB) float64 {
	in := utils.SubUintChecked(server.State.NetInTransfer, server.PrevTransferInSnapshot)
	out := utils.SubUintChecked(server.State.NetOutTransfer, server.PrevTransferOutSnapshot)
	var delta uint64
	var columns []string
	switch u.Type {
	case "transfer_in_cycle":
		delta, columns = in, []string{"in"}
	case "transfer_out_cycle":
		delta, columns = out, []string{"out"}
	default: // transfer_all_cycle
		delta, columns = out+in, []string{"in", "out"}
	}
	src := float64(delta)
	if u.CycleInterval != 0 {
		src += float64(sumTransferSince(db, columns, u.GetTransferDurationStart(), server.ID))
	}
	return src
}

// sumTransferSince 汇总 serverID 自 since 起落库的 Transfer 指定列之和。
func sumTransferSince(db *gorm.DB, columns []string, since time.Time, serverID uint64) uint64 {
	quoted := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = quoteIdent(db, col)
	}
	var res NResult
	db.Model(&Transfer{}).Select("SUM("+strings.Join(quoted, "+")+") AS n").
		Where(transferTimeCond(db), since.UTC(), serverID).Scan(&res)
	return res.N
}

// recordCycleTransfer 更新下次检测时间与本周期状态，并刷新前端展示用的周期流量统计。
func (u *Rule) recordCycleTransfer(stats *CycleTransferStats, server *Server, src float64) {
	seconds := max(1800*((u.Max-src)/u.Max), 180)
	if u.NextTransferAt == nil {
		u.NextTransferAt = make(map[uint64]time.Time)
	}
	if u.LastCycleStatus == nil {
		u.LastCycleStatus = make(map[uint64]bool)
	}
	u.NextTransferAt[server.ID] = time.Now().Add(time.Second * time.Duration(seconds))
	u.LastCycleStatus[server.ID] = !u.outOfRange(src)
	if stats.ServerName[server.ID] != server.Name {
		stats.ServerName[server.ID] = server.Name
	}
	stats.Transfer[server.ID] = uint64(src)
	stats.NextUpdate[server.ID] = u.NextTransferAt[server.ID]
	// 自动更新周期流量展示起止时间
	stats.From = u.GetTransferDurationStart()
	stats.To = u.GetTransferDurationEnd()
}

// IsTransferDurationRule 判断该规则是否属于周期流量规则 属于则返回true
func (u *Rule) IsTransferDurationRule() bool {
	if u == nil {
		return false
	}
	switch u.Type {
	case "transfer_in_cycle", "transfer_out_cycle", "transfer_all_cycle":
		return true
	default:
		return false
	}
}

func (u *Rule) IsOfflineRule() bool {
	return u != nil && u.Type == "offline"
}

// GetTransferDurationStart 获取周期流量的起始时间
func (u *Rule) GetTransferDurationStart() time.Time {
	startTime, _ := u.getTransferDurationBounds(ruleNow())
	return startTime
}

// GetTransferDurationEnd 获取周期流量结束时间
func (u *Rule) GetTransferDurationEnd() time.Time {
	_, nextTime := u.getTransferDurationBounds(ruleNow())
	return nextTime
}

func (u *Rule) getTransferDurationBounds(now time.Time) (time.Time, time.Time) {
	unit := strings.ToLower(u.CycleUnit)
	startTime := *u.CycleStart
	var nextTime time.Time
	switch unit {
	case "year":
		startTime, nextTime = calendarCycleBounds(startTime, int(u.CycleInterval), 0, now)
	case "month":
		startTime, nextTime = calendarCycleBounds(startTime, 0, int(u.CycleInterval), now)
	case "week":
		nextTime = startTime.AddDate(0, 0, 7*int(u.CycleInterval))
		for now.After(nextTime) {
			startTime = nextTime
			nextTime = nextTime.AddDate(0, 0, 7*int(u.CycleInterval))
		}
	case "day":
		nextTime = startTime.AddDate(0, 0, int(u.CycleInterval))
		for now.After(nextTime) {
			startTime = nextTime
			nextTime = nextTime.AddDate(0, 0, int(u.CycleInterval))
		}
	default:
		// For hour unit or not set.
		interval := 3600 * int64(u.CycleInterval)
		startTime = time.Unix(u.CycleStart.Unix()+(now.Unix()-u.CycleStart.Unix())/interval*interval, 0)
		nextTime = time.Unix(startTime.Unix()+interval, 0)
	}

	return startTime, nextTime
}

// calendarCycleBounds 按日历周期(年/月)推进到包含 now 的当前区间,保持锚定日。
func calendarCycleBounds(anchor time.Time, yearsPerCycle, monthsPerCycle int, now time.Time) (time.Time, time.Time) {
	cycles := 0
	startTime := addCalendarCycle(anchor, yearsPerCycle, monthsPerCycle, cycles)
	nextTime := addCalendarCycle(anchor, yearsPerCycle, monthsPerCycle, cycles+1)
	for now.After(nextTime) {
		cycles++
		startTime = addCalendarCycle(anchor, yearsPerCycle, monthsPerCycle, cycles)
		nextTime = addCalendarCycle(anchor, yearsPerCycle, monthsPerCycle, cycles+1)
	}
	return startTime, nextTime
}

// addCalendarCycle 在 anchor 上叠加 cycles 个周期,锚定日超出目标月天数时取该月最后一天(如 1/29→2/28)。
func addCalendarCycle(anchor time.Time, yearsPerCycle, monthsPerCycle, cycles int) time.Time {
	years, months := yearsPerCycle*cycles, monthsPerCycle*cycles
	h, m, s, ns, loc := anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), anchor.Location()
	first := time.Date(anchor.Year()+years, anchor.Month()+time.Month(months), 1, h, m, s, ns, loc)
	lastDay := time.Date(first.Year(), first.Month()+1, 0, h, m, s, ns, loc).Day()
	return time.Date(first.Year(), first.Month(), min(anchor.Day(), lastDay), h, m, s, ns, loc)
}
