package model

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	pb "github.com/nezhahq/nezha/proto"
)

// task type 数值是下发到 agent 的协议约定。删除中间功能（命令执行/终端/NAT/
// 文件管理/配置下发/服务器转移/MCP）后保留原数值、不复用空缺号段，避免破坏与
// agent 的兼容。
const (
	TaskTypeHTTPGet   = 1
	TaskTypeICMPPing  = 2
	TaskTypeTCPPing   = 3
	TaskTypeUpgrade   = 6
	TaskTypeKeepalive = 7
)

// IsServiceMonitorType 仅放行三种被动拨测类型；Service.Type 与 agent 特权任务共用 Task.Type 命名空间。
func IsServiceMonitorType(t uint64) bool {
	switch t {
	case TaskTypeHTTPGet, TaskTypeICMPPing, TaskTypeTCPPing:
		return true
	default:
		return false
	}
}

// ValidateServiceMonitorType 在 API / 持久化 / 调度边界统一拒绝非拨测类型。
func ValidateServiceMonitorType(t uint64) error {
	if !IsServiceMonitorType(t) {
		return fmt.Errorf("invalid service monitor type %d: allowed types are 1 (HTTP GET), 2 (ICMP ping), and 3 (TCP ping)", t)
	}
	return nil
}

const (
	ServiceCoverAll = iota
	ServiceCoverIgnoreAll
)

type Service struct {
	Common
	Name                string `json:"name"`
	Type                uint8  `json:"type"`
	Target              string `json:"target"`
	SkipServersRaw      string `json:"-"`
	Duration            uint64 `json:"duration"`
	DisplayIndex        int    `json:"display_index"` // 展示排序，越大越靠前
	Notify              bool   `json:"notify,omitempty"`
	NotificationGroupID uint64 `json:"notification_group_id"` // 当前服务监控所属的通知组 ID
	Cover               uint8  `json:"cover"`

	HideForGuest bool `json:"hide_for_guest,omitempty"` // 对游客隐藏

	MinLatency    float32 `json:"min_latency"`
	MaxLatency    float32 `json:"max_latency"`
	LatencyNotify bool    `json:"latency_notify,omitempty"`

	SkipServers map[uint64]bool `gorm:"-" json:"skip_servers"`
	CronJobID   cron.EntryID    `gorm:"-" json:"-"`
}

// CoversServer 判断监控是否作用于 serverID：CoverAll 时 SkipServers 是排除集，
// CoverIgnoreAll 时是包含集；未知 cover 一律不覆盖（与 DispatchTask 的 fail-closed 一致）。
func (m *Service) CoversServer(serverID uint64) bool {
	switch m.Cover {
	case ServiceCoverAll:
		return !m.SkipServers[serverID]
	case ServiceCoverIgnoreAll:
		return m.SkipServers[serverID]
	default:
		return false
	}
}

func (m *Service) PB() *pb.Task {
	if m == nil || !IsServiceMonitorType(uint64(m.Type)) {
		return nil
	}
	return &pb.Task{
		Id:   m.ID,
		Type: uint64(m.Type),
		Data: m.Target,
	}
}

// HasPermission 扩展默认的 owner/admin 检查，让 PAT 的 server_ids 白名单
// 同样能收窄 service monitor 的列出/删除/更新路径：
//   - ServiceCoverAll：SkipServers 是 deny-set。DispatchTask 会探测 owner 在
//     deny-set 之外的所有 server，所以受限 PAT 必须保证 deny-set 已经覆盖
//     白名单外的全部 owner servers。判定与 controller 的
//     enforcePATServiceDispatchScope / rejectImplicitServiceCoverForLimitedPAT
//     共用 DenyListSafeForLimitedPAT。
//   - ServiceCoverIgnoreAll：SkipServers 是 allow-set，要求每个被覆盖的
//     server 都在 PAT 白名单内。
//   - 其它情况保留旧的“PAT 按 owner 关系判定”行为。
func (m *Service) HasPermission(ctx *gin.Context) bool {
	if !m.Common.HasPermission(ctx) {
		return false
	}
	tok := PATFromContext(ctx)
	if tok == nil {
		return true
	}
	switch m.Cover {
	case ServiceCoverAll:
		return DenyListSafeForLimitedPAT(tok, m.GetUserID(), EnabledIDs(m.SkipServers))
	case ServiceCoverIgnoreAll:
		for _, id := range EnabledIDs(m.SkipServers) {
			if !tok.CanAccessServer(id) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// EnabledIDs 取出 map 中值为 true 的 id（SkipServers / Rule.Ignore 的 false 项无运行时效果）。
// 结果无序；空 map 返回 nil。
func EnabledIDs(m map[uint64]bool) []uint64 {
	if len(m) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(m))
	for id, enabled := range m {
		if enabled {
			out = append(out, id)
		}
	}
	return out
}

// CronSpec 返回服务监控请求间隔对应的 cron 表达式
func (m *Service) CronSpec() string {
	if m.Duration == 0 {
		// 默认间隔 30 秒
		m.Duration = 30
	}
	return fmt.Sprintf("@every %ds", m.Duration)
}

func (m *Service) BeforeSave(tx *gorm.DB) error {
	if err := ValidateServiceMonitorType(uint64(m.Type)); err != nil {
		return err
	}
	if data, err := json.Marshal(m.SkipServers); err != nil {
		return err
	} else {
		m.SkipServersRaw = string(data)
	}
	return nil
}

func (m *Service) AfterFind(tx *gorm.DB) error {
	m.SkipServers = make(map[uint64]bool)
	if err := json.Unmarshal([]byte(m.SkipServersRaw), &m.SkipServers); err != nil {
		log.Println("NEZHA>> Service.AfterFind:", err)
		return nil
	}

	return nil
}
