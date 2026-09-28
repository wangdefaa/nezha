package singleton

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
)

const (
	_RuleCheckNoData = iota
	_RuleCheckFail
	_RuleCheckPass
)

type NotificationHistory struct {
	Duration time.Duration
	Until    time.Time
}

// 报警规则
var (
	AlertsLock                    sync.RWMutex
	Alerts                        []*model.AlertRule
	alertsStore                   map[uint64]map[uint64][][]bool       // [alert_id][server_id] -> [timeTick][ruleId] 时间点对应的rule的检查结果
	alertsPrevState               map[uint64]map[uint64]uint8          // [alert_id][server_id] -> 对应报警规则的上一次报警状态
	AlertsCycleTransferStatsStore map[uint64]*model.CycleTransferStats // [alert_id] -> 对应报警规则的周期流量统计
)

// addCycleTransferStatsInfo 向AlertsCycleTransferStatsStore中添加周期流量报警统计信息
func addCycleTransferStatsInfo(alert *model.AlertRule) {
	if alert == nil || !alert.Enabled() || !alert.IsSafeToEvaluate() {
		return
	}
	for _, rule := range alert.Rules {
		if !rule.IsTransferDurationRule() {
			continue
		}
		if AlertsCycleTransferStatsStore[alert.ID] == nil {
			from := rule.GetTransferDurationStart()
			to := rule.GetTransferDurationEnd()
			AlertsCycleTransferStatsStore[alert.ID] = &model.CycleTransferStats{
				Name:       alert.Name,
				From:       from,
				To:         to,
				Max:        uint64(rule.Max),
				Min:        uint64(rule.Min),
				ServerName: make(map[uint64]string),
				Transfer:   make(map[uint64]uint64),
				NextUpdate: make(map[uint64]time.Time),
			}
		}
	}
}

// AlertSentinelStart 报警器启动
func AlertSentinelStart() {
	loadAlertRules()

	time.Sleep(time.Second * 10)
	lastPrint := time.Now()
	var checkCount uint64
	ticker := time.Tick(3 * time.Second) // 3秒钟检查一次
	for startedAt := range ticker {
		checkStatus()
		checkCount++
		if lastPrint.Before(startedAt.Add(-1 * time.Hour)) {
			if Conf.Debug {
				log.Printf("NEZHA>> Checking alert rules %d times each hour %v %v", checkCount, startedAt, time.Now())
			}
			checkCount = 0
			lastPrint = startedAt
		}
	}
}

// loadAlertRules 启动时加载告警规则并初始化采样存储；持久化数据非法的规则跳过求值，避免重启后再次崩溃形成循环。
func loadAlertRules() {
	alertsStore = make(map[uint64]map[uint64][][]bool)
	alertsPrevState = make(map[uint64]map[uint64]uint8)
	AlertsCycleTransferStatsStore = make(map[uint64]*model.CycleTransferStats)
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	if err := DB.Find(&Alerts).Error; err != nil {
		panic(err)
	}
	for _, alert := range Alerts {
		if alert == nil {
			log.Printf("NEZHA>> Skipping invalid nil alert rule loaded from database")
			continue
		}
		alertsStore[alert.ID] = make(map[uint64][][]bool)
		alertsPrevState[alert.ID] = make(map[uint64]uint8)
		if !alert.IsSafeToEvaluate() {
			log.Printf("NEZHA>> Skipping invalid alert rule %d loaded from database", alert.ID)
			continue
		}
		addCycleTransferStatsInfo(alert)
	}
}

func OnRefreshOrAddAlert(alert *model.AlertRule) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	delete(alertsStore, alert.ID)
	delete(alertsPrevState, alert.ID)
	var isEdit bool
	for i := range Alerts {
		if Alerts[i].ID == alert.ID {
			Alerts[i] = alert
			isEdit = true
		}
	}
	if !isEdit {
		Alerts = append(Alerts, alert)
	}
	alertsStore[alert.ID] = make(map[uint64][][]bool)
	alertsPrevState[alert.ID] = make(map[uint64]uint8)
	delete(AlertsCycleTransferStatsStore, alert.ID)
	addCycleTransferStatsInfo(alert)
}

// replaceAlertRules 换上只改了服务器范围或通知组的规则。判断条件没变，所以不像 OnRefreshOrAddAlert
// 那样清空采样与通知状态，免得「单次触发」的规则对还没恢复的故障再发一次通知。
func replaceAlertRules(rules []*model.AlertRule) {
	if len(rules) == 0 {
		return
	}
	byID := make(map[uint64]*model.AlertRule, len(rules))
	for _, r := range rules {
		byID[r.ID] = r
	}
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	for i, a := range Alerts {
		if r, ok := byID[a.ID]; ok {
			Alerts[i] = r
		}
	}
}

func OnDeleteAlert(id []uint64) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	for _, i := range id {
		delete(alertsStore, i)
		delete(alertsPrevState, i)
		currentAlerts := Alerts[:0]
		for _, alert := range Alerts {
			if alert.ID != i {
				currentAlerts = append(currentAlerts, alert)
			}
		}
		Alerts = currentAlerts
		delete(AlertsCycleTransferStatsStore, i)
	}
}

// checkStatus 检查报警规则并发送报警
func checkStatus() {
	AlertsLock.RLock()
	defer AlertsLock.RUnlock()
	m := ServerShared.GetList()

	for _, alert := range Alerts {
		// 跳过未启用或持久化数据非法的规则
		if alert == nil || !alert.Enabled() || !alert.IsSafeToEvaluate() {
			continue
		}
		// 非 admin 的规则只监测 owner 自己的服务器
		ownerIsAdmin := UserRole(alert.UserID).IsAdmin()
		for _, server := range m {
			if alert.UserID != server.GetUserID() && !ownerIsAdmin {
				continue
			}
			checkStatusForServer(alert, server)
		}
	}
}

// checkStatusForServer 隔离单个 alert/server 的求值：任何意外 panic 都不应拖垮进程
// 或阻止同一 tick 内其它服务器的检查。
func checkStatusForServer(alert *model.AlertRule, server *model.Server) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("NEZHA>> Recovered panic evaluating alert rule %d for server %d: %v", alert.ID, server.ID, recovered)
		}
	}()

	alertsStore[alert.ID][server.ID] = append(alertsStore[alert.
		ID][server.ID], alert.Snapshot(AlertsCycleTransferStatsStore[alert.ID], server, DB))
	// 发送通知，分为触发报警和恢复通知
	_, passed := alert.Check(alertsStore[alert.ID][server.ID])
	// 保存当前服务器状态信息
	curServer := model.Server{}
	copier.Copy(&curServer, server)
	if !passed {
		notifyAlertIncident(alert, server, &curServer)
	} else {
		notifyAlertResolved(alert, server, &curServer)
	}
	trimAlertSamples(alert, server.ID)
}

// notifyAlertIncident 本次未通过检查：始终触发模式或上次检查不为失败时触发报警（跳过单次触发+上次失败的情况）。
func notifyAlertIncident(alert *model.AlertRule, server, curServer *model.Server) {
	if alert.TriggerMode != model.ModeAlwaysTrigger && alertsPrevState[alert.ID][server.ID] == _RuleCheckFail {
		return
	}
	alertsPrevState[alert.ID][server.ID] = _RuleCheckFail
	message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Incident"),
		server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
	go NotificationShared.SendNotification(alert.NotificationGroupID, message, NotificationMuteLabel.ServerIncident(server.ID, alert.ID), curServer)
	// 清除恢复通知的静音缓存
	NotificationShared.UnMuteNotification(alert.NotificationGroupID, NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID))
}

// notifyAlertResolved 本次通过检查但上一次的状态为失败，则发送恢复通知。
func notifyAlertResolved(alert *model.AlertRule, server, curServer *model.Server) {
	if alertsPrevState[alert.ID][server.ID] == _RuleCheckFail {
		message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Resolved"),
			server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
		go NotificationShared.SendNotification(alert.NotificationGroupID, message, NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID), curServer)
		// 清除失败通知的静音缓存
		NotificationShared.UnMuteNotification(alert.NotificationGroupID, NotificationMuteLabel.ServerIncident(server.ID, alert.ID))
	}
	alertsPrevState[alert.ID][server.ID] = _RuleCheckPass
}

// trimAlertSamples 清理旧数据：保留窗口由规则定义决定（各规则 Duration 的最大值），而非 Check 的判定结果。
// window==0 表示没有任何有效规则需要回看历史（例如全部 Duration<=0），此时清空采样避免切片无限增长。
func trimAlertSamples(alert *model.AlertRule, serverID uint64) {
	window := alert.RetentionWindow()
	samples := alertsStore[alert.ID][serverID]
	if window <= 0 {
		alertsStore[alert.ID][serverID] = samples[:0]
	} else if window < len(samples) {
		alertsStore[alert.ID][serverID] = samples[len(samples)-window:]
	}
}
