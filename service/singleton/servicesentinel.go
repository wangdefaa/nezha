package singleton

import (
	"cmp"
	"fmt"
	"iter"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

const (
	_CurrentStatusSize = 30 // 统计 15 分钟内的数据为当前状态
)

type serviceResponseItem struct {
	model.ServiceResponseItem

	service *model.Service
}

type ReportData struct {
	Data     *pb.TaskResult
	Reporter uint64
}

// _TodayStatsOfService 今日监控记录
type _TodayStatsOfService struct {
	Up    uint64  // 今日在线计数
	Down  uint64  // 今日离线计数
	Delay float64 // 今日平均延迟
}

type serviceResponseData = _TodayStatsOfService

type serviceTaskStatus struct {
	lastStatus uint8
	t          time.Time
	result     []*pb.TaskResult
}

type pingStore struct {
	count        int
	ping         float64
	successCount int
}

/*
使用缓存 channel，处理上报的 Service 请求结果，然后判断是否需要报警
需要记录上一次的状态信息

加锁顺序：serviceResponseDataStoreLock > monthlyStatusLock > servicesLock
*/
type ServiceSentinel struct {
	// 服务监控任务上报通道
	serviceReportChannel chan ReportData // 服务状态汇报管道
	// 服务监控任务调度通道
	dispatchBus chan<- *model.Service

	serviceResponseDataStoreLock sync.RWMutex
	serviceStatusToday           map[uint64]*_TodayStatsOfService // [service_id] -> _TodayStatsOfService
	serviceCurrentStatusData     map[uint64]*serviceTaskStatus    // 当前任务结果缓存
	serviceResponseDataStore     map[uint64]serviceResponseData   // 当前数据

	serviceResponsePing                   map[uint64]map[uint64]*pingStore // guarded by serviceResponseDataStoreLock; [service_id] -> ClientID -> delay
	tlsCertCache                          map[uint64]string                // guarded by serviceResponseDataStoreLock
	serviceReportValidatedHook            func(uint64)
	loadStatsResponseLockedHook           func()
	serviceReportBeforeTLSSideEffectsHook func(uint64)

	servicesLock    sync.RWMutex
	serviceListLock sync.RWMutex
	services        map[uint64]*model.Service
	serviceList     []*model.Service

	// 30天数据缓存
	monthlyStatusLock sync.Mutex
	monthlyStatus     map[uint64]*serviceResponseItem

	// closeOnce + workerWG together let Close() wait for the worker goroutine
	// to fully exit. Without this, a test that swaps ServiceSentinelShared back
	// to its original value in t.Cleanup races against the still-running
	// worker, which keeps reading globals like Conf/CronShared/NotificationShared.
	// Production never calls Close() — the process exits while the worker is
	// still running and that is fine — but tests must drain the worker before
	// restoring globals.
	closeOnce sync.Once
	workerWG  sync.WaitGroup
}

// NewServiceSentinel 创建服务监控器
func NewServiceSentinel(serviceSentinelDispatchBus chan<- *model.Service) (*ServiceSentinel, error) {
	ss := &ServiceSentinel{
		serviceReportChannel:     make(chan ReportData, 200),
		serviceStatusToday:       make(map[uint64]*_TodayStatsOfService),
		serviceCurrentStatusData: make(map[uint64]*serviceTaskStatus),
		serviceResponseDataStore: make(map[uint64]serviceResponseData),
		serviceResponsePing:      make(map[uint64]map[uint64]*pingStore),
		services:                 make(map[uint64]*model.Service),
		tlsCertCache:             make(map[uint64]string),
		// 30天数据缓存
		monthlyStatus: make(map[uint64]*serviceResponseItem),
		dispatchBus:   serviceSentinelDispatchBus,
	}

	// 加载历史记录
	err := ss.loadServiceHistory()
	if err != nil {
		return nil, err
	}

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)
	ss.loadTodayStats(today)

	// 启动服务监控器
	ss.workerWG.Add(1)
	go func() {
		defer ss.workerWG.Done()
		ss.worker()
	}()

	// 每日将游标往后推一天
	_, err = CronShared.AddFunc("0 0 0 * * *", ss.refreshMonthlyServiceStatus)
	if err != nil {
		return nil, err
	}

	// 每周日凌晨 4:00 执行系统存储维护
	_, err = CronShared.AddFunc("0 0 4 * * 0", PerformMaintenance)
	if err != nil {
		log.Printf("NEZHA>> Warning: failed to schedule maintenance task: %v", err)
	}

	return ss, nil
}

func (ss *ServiceSentinel) refreshMonthlyServiceStatus() {
	// 刷新数据防止无人访问
	ss.LoadStats()
	// 将数据往前刷一天
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	for k, v := range ss.monthlyStatus {
		for i := range len(v.Up) - 1 {
			if i == 0 {
				// 30 天在线率，减去已经出30天之外的数据
				v.TotalDown -= v.Down[i]
				v.TotalUp -= v.Up[i]
			}
			v.Up[i], v.Down[i], v.Delay[i] = v.Up[i+1], v.Down[i+1], v.Delay[i+1]
		}
		v.Up[29] = 0
		v.Down[29] = 0
		v.Delay[29] = 0
		// 清理前一天数据
		ss.serviceResponseDataStore[k] = serviceResponseData{}
		ss.serviceStatusToday[k].Delay = 0
		ss.serviceStatusToday[k].Up = 0
		ss.serviceStatusToday[k].Down = 0
	}
}

// Dispatch 将传入的 ReportData 传给 服务状态汇报管道
func (ss *ServiceSentinel) Dispatch(r ReportData) {
	ss.serviceReportChannel <- r
}

// sortServices 按 DisplayIndex 降序、ID 升序排列服务列表
func sortServices(services []*model.Service) {
	slices.SortFunc(services, func(a, b *model.Service) int {
		if a.DisplayIndex != b.DisplayIndex {
			return cmp.Compare(b.DisplayIndex, a.DisplayIndex)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func (ss *ServiceSentinel) UpdateServiceList() {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	ss.serviceListLock.Lock()
	defer ss.serviceListLock.Unlock()

	ss.serviceList = utils.MapValuesToSlice(ss.services)
	sortServices(ss.serviceList)
}

// loadServiceHistory 加载服务监控器的历史状态信息
func (ss *ServiceSentinel) loadServiceHistory() error {
	var services []*model.Service
	if err := DB.Find(&services).Error; err != nil {
		return err
	}
	services, err := ss.registerLoadedServices(services)
	if err != nil {
		return err
	}
	ss.serviceList = services
	sortServices(ss.serviceList)

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)
	ss.initMonthlyStatus(services)
	if TSDBEnabled() {
		ss.loadMonthlyStatusFromTSDB(services, today)
	} else {
		ss.loadMonthlyStatusFromDB(today)
	}
	return nil
}

// registerLoadedServices 为库中拨测服务注册 cron 与内存状态，返回有效服务。旧库可能存有 Service.Type
// 受限前写入的非拨测类型：只隔离（保留库记录供运维排查），绝不注册可能下发特权 Agent 任务的 cron。
func (ss *ServiceSentinel) registerLoadedServices(services []*model.Service) ([]*model.Service, error) {
	valid := services[:0]
	for _, service := range services {
		if err := model.ValidateServiceMonitorType(uint64(service.Type)); err != nil {
			log.Printf("NEZHA>> quarantining service %d: %v", service.ID, err)
			continue
		}
		task := service
		// 通过cron定时将服务监控任务传递给任务调度管道
		cronID, err := CronShared.AddFunc(task.CronSpec(), func() {
			ss.dispatchBus <- task
		})
		if err != nil {
			return nil, err
		}
		service.CronJobID = cronID
		ss.services[service.ID] = service
		ss.serviceCurrentStatusData[service.ID] = &serviceTaskStatus{result: make([]*pb.TaskResult, 0, _CurrentStatusSize)}
		ss.serviceStatusToday[service.ID] = &_TodayStatsOfService{}
		valid = append(valid, service)
	}
	return valid, nil
}

// initMonthlyStatus 为每个服务初始化 30 天统计容器。
func (ss *ServiceSentinel) initMonthlyStatus(services []*model.Service) {
	for _, service := range services {
		ss.monthlyStatus[service.ID] = newServiceResponseItem(service)
	}
}

// loadMonthlyStatusFromTSDB 回填 30 天统计的前 29 格（昨天在下标 28），下标 29 是今天，由实时上报累计。
func (ss *ServiceSentinel) loadMonthlyStatusFromTSDB(services []*model.Service, today time.Time) {
	for _, service := range services {
		dailyStats, err := TSDBShared.QueryServiceDailyStats(service.ID, today, 29)
		if err != nil {
			log.Printf("NEZHA>> Failed to load TSDB history for service %d: %v", service.ID, err)
			continue
		}
		ms := ss.monthlyStatus[service.ID]
		for i := 0; i < 29; i++ {
			ms.Up[i] = dailyStats[i].Up
			ms.TotalUp += dailyStats[i].Up
			ms.Down[i] = dailyStats[i].Down
			ms.TotalDown += dailyStats[i].Down
			ms.Delay[i] = dailyStats[i].Delay
		}
	}
}

func (ss *ServiceSentinel) loadMonthlyStatusFromDB(today time.Time) {
	var mhs []model.ServiceHistory
	DB.Where("created_at > ? AND created_at < ? AND server_id = 0", today.AddDate(0, 0, -29), today).Find(&mhs)
	delayCount := make(map[uint64]map[int]int)
	for _, mh := range mhs {
		dayIndex := 28 - int(today.Sub(mh.CreatedAt).Hours())/24
		if dayIndex < 0 {
			continue
		}
		ms := ss.monthlyStatus[mh.ServiceID]
		if ms == nil {
			continue
		}
		if delayCount[mh.ServiceID] == nil {
			delayCount[mh.ServiceID] = make(map[int]int)
		}
		ms.Delay[dayIndex] = (ms.Delay[dayIndex]*float64(delayCount[mh.ServiceID][dayIndex]) + mh.AvgDelay) / float64(delayCount[mh.ServiceID][dayIndex]+1)
		delayCount[mh.ServiceID][dayIndex]++
		ms.Up[dayIndex] += mh.Up
		ms.TotalUp += mh.Up
		ms.Down[dayIndex] += mh.Down
		ms.TotalDown += mh.Down
	}
}

func (ss *ServiceSentinel) loadTodayStats(today time.Time) {
	if TSDBEnabled() {
		ss.loadTodayStatsFromTSDB()
		return
	}
	ss.loadTodayStatsFromDB(today)
}

// loadTodayStatsFromTSDB 从 TSDB 汇总各服务最近 1 天的拨测统计。
func (ss *ServiceSentinel) loadTodayStatsFromTSDB() {
	for serviceID, ms := range ss.monthlyStatus {
		result, err := TSDBShared.QueryServiceHistory(serviceID, tsdb.Period1Day)
		if err != nil {
			log.Printf("NEZHA>> Failed to load TSDB today stats for service %d: %v", serviceID, err)
			continue
		}
		var totalUp, totalDown uint64
		var totalDelay float64
		var delayCount int
		for _, serverStats := range result.Servers {
			totalUp += serverStats.Stats.TotalUp
			totalDown += serverStats.Stats.TotalDown
			if serverStats.Stats.AvgDelay > 0 {
				totalDelay += serverStats.Stats.AvgDelay
				delayCount++
			}
		}
		ss.serviceStatusToday[serviceID].Up = totalUp
		ss.serviceStatusToday[serviceID].Down = totalDown
		if delayCount > 0 {
			ss.serviceStatusToday[serviceID].Delay = totalDelay / float64(delayCount)
		}
		ms.TotalUp += totalUp
		ms.TotalDown += totalDown
	}
}

// loadTodayStatsFromDB 从 service_histories 汇总今日统计。删服务不清历史，当天删除后重启会残留
// 已删服务的记录：必须跳过，否则空指针让 NewServiceSentinel 失败、dashboard 起不来。
func (ss *ServiceSentinel) loadTodayStatsFromDB(today time.Time) {
	var mhs []model.ServiceHistory
	DB.Where("created_at >= ? AND server_id = 0", today).Find(&mhs)
	totalDelay := make(map[uint64]float64)
	totalDelayCount := make(map[uint64]int)
	for _, mh := range mhs {
		st, ms := ss.serviceStatusToday[mh.ServiceID], ss.monthlyStatus[mh.ServiceID]
		if st == nil || ms == nil {
			continue
		}
		st.Up += mh.Up
		ms.TotalUp += mh.Up
		st.Down += mh.Down
		ms.TotalDown += mh.Down
		totalDelay[mh.ServiceID] += mh.AvgDelay
		totalDelayCount[mh.ServiceID]++
	}
	for id, delay := range totalDelay {
		ss.serviceStatusToday[id].Delay = delay / float64(totalDelayCount[id])
	}
}

func (ss *ServiceSentinel) Update(m *model.Service) error {
	if m == nil {
		return fmt.Errorf("service is nil")
	}
	if err := model.ValidateServiceMonitorType(uint64(m.Type)); err != nil {
		return err
	}

	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.Lock()
	defer ss.servicesLock.Unlock()
	return ss.scheduleLocked(m)
}

// scheduleLocked 须持 Update 的三把锁：注册新 cron，停掉旧 cron（新服务则初始化状态）后替换服务。
func (ss *ServiceSentinel) scheduleLocked(m *model.Service) error {
	var err error
	// 写入新任务
	m.CronJobID, err = CronShared.AddFunc(m.CronSpec(), func() {
		ss.dispatchBus <- m
	})
	if err != nil {
		return err
	}
	if ss.services[m.ID] != nil {
		// 停掉旧任务
		CronShared.Remove(ss.services[m.ID].CronJobID)
	} else {
		// 新任务初始化数据
		ss.initNewServiceState(m)
	}
	// 更新这个任务
	ss.services[m.ID] = m
	return nil
}

// initNewServiceState 新增服务时初始化其 30 天统计、最近结果窗口与当日统计。
func (ss *ServiceSentinel) initNewServiceState(m *model.Service) {
	ss.monthlyStatus[m.ID] = newServiceResponseItem(m)
	if ss.serviceCurrentStatusData[m.ID] == nil {
		ss.serviceCurrentStatusData[m.ID] = new(serviceTaskStatus)
	}
	ss.serviceCurrentStatusData[m.ID].result = make([]*pb.TaskResult, 0, _CurrentStatusSize)
	ss.serviceStatusToday[m.ID] = &_TodayStatsOfService{}
}

// newServiceResponseItem 构造服务的空 30 天统计容器。
func newServiceResponseItem(m *model.Service) *serviceResponseItem {
	return &serviceResponseItem{
		service: m,
		ServiceResponseItem: model.ServiceResponseItem{
			Delay: &[30]float64{},
			Up:    &[30]uint64{},
			Down:  &[30]uint64{},
		},
	}
}

// replaceServices 换上只改了服务器范围或通知组的拨测：走 Update 只替换定时任务，不清统计。
func (ss *ServiceSentinel) replaceServices(services []*model.Service) {
	for _, m := range services {
		if err := ss.Update(m); err != nil {
			log.Printf("NEZHA>> 更新拨测 %d 的引用失败：%v", m.ID, err)
		}
	}
	ss.UpdateServiceList()
}

func (ss *ServiceSentinel) Delete(ids []uint64) {
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.Lock()
	defer ss.servicesLock.Unlock()

	for _, id := range ids {
		delete(ss.serviceCurrentStatusData, id)
		delete(ss.serviceResponseDataStore, id)
		delete(ss.serviceResponsePing, id)
		delete(ss.tlsCertCache, id)
		delete(ss.serviceStatusToday, id)

		// 停掉定时任务。GHSA-jx78-55p5-rwv5：未知 id 过得了权限校验，不判空会 panic 中断循环，
		// 让其余已删库的服务残留内存成僵尸。
		if svc := ss.services[id]; svc != nil {
			CronShared.Remove(svc.CronJobID)
		}
		delete(ss.services, id)

		delete(ss.monthlyStatus, id)
	}
}

func (ss *ServiceSentinel) LoadStats() map[uint64]*serviceResponseItem {
	ss.serviceResponseDataStoreLock.RLock()
	defer ss.serviceResponseDataStoreLock.RUnlock()
	if ss.loadStatsResponseLockedHook != nil {
		ss.loadStatsResponseLockedHook()
	}
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	// 刷新最新一天的数据
	for k := range ss.services {
		ss.refreshMonthlyToday(k)
	}
	// 最后 5 分钟的状态 与 service 对象填充
	for k, v := range ss.serviceResponseDataStore {
		ss.monthlyStatus[k].CurrentDown = v.Down
		ss.monthlyStatus[k].CurrentUp = v.Up
	}
	return ss.monthlyStatus
}

// refreshMonthlyToday 须持 LoadStats 的锁：用当日统计覆盖 30 天窗口的最后一天。
func (ss *ServiceSentinel) refreshMonthlyToday(k uint64) {
	ms, v := ss.monthlyStatus[k], ss.serviceStatusToday[k]
	ms.service = ss.services[k]
	// 30 天在线率：先减去上次加的旧当天数据（防止重复计数），再加上当日数据
	ms.TotalUp -= ms.Up[29]
	ms.TotalDown -= ms.Down[29]
	ms.TotalUp += v.Up
	ms.TotalDown += v.Down
	ms.Up[29] = v.Up
	ms.Down[29] = v.Down
	ms.Delay[29] = v.Delay
}

func (ss *ServiceSentinel) CopyStats() map[uint64]model.ServiceResponseItem {
	var stats map[uint64]*serviceResponseItem
	copier.Copy(&stats, ss.LoadStats())

	sri := make(map[uint64]model.ServiceResponseItem)
	for k, service := range stats {
		service.ServiceName = service.service.Name
		sri[k] = service.ServiceResponseItem
	}

	return sri
}

func (ss *ServiceSentinel) Get(id uint64) (s *model.Service, ok bool) {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	s, ok = ss.services[id]
	return
}

func (ss *ServiceSentinel) GetList() map[uint64]*model.Service {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	return maps.Clone(ss.services)
}

func (ss *ServiceSentinel) GetSortedList() []*model.Service {
	ss.serviceListLock.RLock()
	defer ss.serviceListLock.RUnlock()

	return slices.Clone(ss.serviceList)
}

func (ss *ServiceSentinel) CheckPermission(c *gin.Context, idList iter.Seq[uint64]) bool {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	for id := range idList {
		if s, ok := ss.services[id]; ok {
			if !s.HasPermission(c) {
				return false
			}
		}
	}
	return true
}

func canReportServiceResult(service *model.Service, reporter *model.Server, taskType uint64) bool {
	if service == nil || reporter == nil || uint64(service.Type) != taskType {
		return false
	}
	if !service.CoversServer(reporter.ID) {
		return false
	}
	return service.UserID == reporter.GetUserID() || userIsAdmin(service.UserID)
}

// Close shuts down the ServiceSentinel worker goroutine and waits for it to
// exit. It is idempotent and safe to call more than once.
//
// Why this exists: the worker reads multiple package-level globals during
// each report (Conf, CronShared via notifyCheck, NotificationShared via
// UnMuteNotification, ServerShared, TSDBShared). A test fixture that swaps
// those globals out in t.Cleanup MUST first call Close() — otherwise the
// cleanup write races the still-running worker's read and `go test -race`
// fires (see security_regression_test.go newServiceMonitorSecurityHarness).
// Production never calls Close because the process exits with the worker
// still running, which is fine.
func (ss *ServiceSentinel) Close() {
	ss.closeOnce.Do(func() {
		close(ss.serviceReportChannel)
		ss.workerWG.Wait()
	})
}

// worker 服务监控的实际工作流程
//
// IMPORTANT: this loop reads several package-level globals (Conf, CronShared,
// NotificationShared, ServerShared, TSDBShared). Any test that replaces those
// globals via t.Cleanup must first call ServiceSentinel.Close() so the worker
// drains and exits before the swap, otherwise the race detector trips. See
// the Close() comment above for the full rationale.
func (ss *ServiceSentinel) worker() {
	// 从服务状态汇报管道获取汇报的服务数据
	for r := range ss.serviceReportChannel {
		serverShared := ServerShared
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("NEZHA>> Service monitor report processing panicked: %v", recovered)
				}
			}()
			ss.processReport(r, serverShared)
		}()
	}
}

// processReport 处理单条拨测上报：全程持 lifecycle 读锁；预校验通过后在 serviceResponseDataStoreLock 内
// 完成全部副作用，与服务 Delete/Update 串行化，避免并发删改时空指针与状态撕裂。
func (ss *ServiceSentinel) processReport(r ReportData, serverShared *ServerClass) {
	serverShared.lockLifecycleRead()
	defer serverShared.unlockLifecycleRead()

	reporter, ok := ss.acceptReport(r, serverShared)
	if !ok {
		return
	}
	m := serverShared.GetList()
	// Serialize Delete and Update before this accepted report causes any side effect.
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	st, ok := ss.lockedReportTarget(r.Data, reporter)
	if !ok {
		return
	}
	ss.recordReportMetrics(r)
	stateCode := ss.updateReportStatus(r.Data, st)
	notifyReportStatus(&r, m, st, stateCode)
	ss.checkReportTLS(r.Data, st.service)
}

// acceptReport 锁外预校验：入站结果必须匹配出站任务派发边界，避免 agent 伪造其他服务 ID 写入监控状态。
func (ss *ServiceSentinel) acceptReport(r ReportData, serverShared *ServerClass) (*model.Server, bool) {
	// 延迟是 agent 上报的 float：NaN/Inf 会污染当日均值与 30 天统计，让公开的 /api/v1/service 序列化失败。
	if !model.IsFinite(float64(r.Data.GetDelay())) {
		log.Printf("NEZHA>> Rejected non-finite service monitor delay from server %d", r.Reporter)
		return nil, false
	}
	cs, _ := ss.Get(r.Data.GetId())
	reporter, _ := serverShared.Get(r.Reporter)
	if !canReportServiceResult(cs, reporter, r.Data.GetType()) {
		// 只记录定位字段：Data 是 agent 任意字符串，整条 %+v 会注入伪造日志行并放大写盘。
		log.Printf("NEZHA>> Incorrect service monitor report: service=%d type=%d reporter=%d",
			r.Data.GetId(), r.Data.GetType(), r.Reporter)
		return nil, false
	}
	if ss.serviceReportValidatedHook != nil {
		ss.serviceReportValidatedHook(r.Data.GetId())
	}
	return reporter, true
}

// reportTarget 锁内重新读取的上报目标（预校验后 Delete/Update 可能已替换或移除服务）。
type reportTarget struct {
	service *model.Service
	today   *_TodayStatsOfService
	current *serviceTaskStatus
}

// lockedReportTarget 须持 serviceResponseDataStoreLock：重新读取服务状态并复核上报边界，任一缺失即丢弃本条上报。
func (ss *ServiceSentinel) lockedReportTarget(mh *pb.TaskResult, reporter *model.Server) (reportTarget, bool) {
	st := reportTarget{
		today:   ss.serviceStatusToday[mh.GetId()],
		current: ss.serviceCurrentStatusData[mh.GetId()],
	}
	service, exists := ss.Get(mh.GetId())
	if st.today == nil || st.current == nil || !exists ||
		!canReportServiceResult(service, reporter, mh.GetType()) {
		return st, false
	}
	st.service = service
	return st, true
}

// recordReportMetrics 写入拨测历史：TCP/ICMP Ping 按 AvgPingCount 聚合平均后写入，其余类型直接写 TSDB。
func (ss *ServiceSentinel) recordReportMetrics(r ReportData) {
	mh := r.Data
	if mh.Type != model.TaskTypeTCPPing && mh.Type != model.TaskTypeICMPPing {
		if TSDBEnabled() {
			writeServiceTSDB(mh.GetId(), r.Reporter, float64(mh.Delay), mh.Successful)
		}
		return
	}
	ts := ss.pingStoreOf(mh.GetId(), r.Reporter)
	ts.count++
	ts.ping = (ts.ping*float64(ts.count-1) + float64(mh.Delay)) / float64(ts.count)
	if mh.Successful {
		ts.successCount++
	}
	if ts.count == Conf.AvgPingCount {
		flushPingStore(r, ts)
		*ts = pingStore{}
	}
}

// pingStoreOf 取（或新建）某服务在某上报端的 ping 聚合桶。
func (ss *ServiceSentinel) pingStoreOf(serviceID, reporter uint64) *pingStore {
	byReporter, ok := ss.serviceResponsePing[serviceID]
	if !ok {
		byReporter = make(map[uint64]*pingStore)
		ss.serviceResponsePing[serviceID] = byReporter
	}
	ts, ok := byReporter[reporter]
	if !ok {
		ts = &pingStore{}
		byReporter[reporter] = ts
	}
	return ts
}

// flushPingStore 聚合满 AvgPingCount 次后写入：启用 TSDB 写时序库，否则落 service_histories。
func flushPingStore(r ReportData, ts *pingStore) {
	mh := r.Data
	if TSDBEnabled() {
		writeServiceTSDB(mh.GetId(), r.Reporter, ts.ping, ts.successCount*2 >= ts.count)
		return
	}
	if err := DB.Create(&model.ServiceHistory{
		ServiceID: mh.GetId(),
		AvgDelay:  ts.ping,
		Data:      mh.Data,
		ServerID:  r.Reporter,
	}).Error; err != nil {
		log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
	}
}

// writeServiceTSDB 写一条拨测时序样本。
func writeServiceTSDB(serviceID, serverID uint64, delay float64, successful bool) {
	if err := TSDBShared.WriteServiceMetrics(&tsdb.ServiceMetrics{
		ServiceID:  serviceID,
		ServerID:   serverID,
		Timestamp:  time.Now(),
		Delay:      delay,
		Successful: successful,
	}); err != nil {
		log.Printf("NEZHA>> Failed to save service monitor metrics to TSDB: %v", err)
	}
}

// updateReportStatus 更新当日统计与最近 _CurrentStatusSize 条结果窗口，返回按窗口在线率计算的状态码。
func (ss *ServiceSentinel) updateReportStatus(mh *pb.TaskResult, st reportTarget) uint8 {
	if mh.Successful {
		st.today.Delay = (st.today.Delay*float64(st.today.Up) + float64(mh.Delay)) / float64(st.today.Up+1)
		st.today.Up++
	} else {
		st.today.Down++
	}
	currentTime := time.Now()
	if st.current.t.IsZero() {
		st.current.t = currentTime
	}
	// 写入当前数据
	if st.current.t.Before(currentTime) {
		st.current.t = currentTime.Add(30 * time.Second)
		st.current.result = append(st.current.result, mh)
	}
	rd := ss.refreshResponseData(mh.GetId(), st.current.result)
	stateCode := statusOf(rd)
	if len(st.current.result) == _CurrentStatusSize {
		st.current.t = currentTime
		saveServiceWindow(mh, rd)
		st.current.result = st.current.result[:0]
	}
	return stateCode
}

// refreshResponseData 用结果窗口重算当前状态（永远是最新的 30 个数据）并写回 serviceResponseDataStore。
func (ss *ServiceSentinel) refreshResponseData(serviceID uint64, results []*pb.TaskResult) serviceResponseData {
	var rd serviceResponseData
	for _, res := range results {
		if res.GetId() == 0 {
			continue
		}
		if res.Successful {
			rd.Up++
			rd.Delay = (rd.Delay*float64(rd.Up-1) + float64(res.Delay)) / float64(rd.Up)
		} else {
			rd.Down++
		}
	}
	ss.serviceResponseDataStore[serviceID] = rd
	return rd
}

// upPercentOf 计算在线率百分比，无样本时为 0。
func upPercentOf(rd serviceResponseData) uint64 {
	if rd.Down+rd.Up == 0 {
		return 0
	}
	return rd.Up * 100 / (rd.Down + rd.Up)
}

// saveServiceWindow 结果窗口写满时落一条汇总历史（仅未启用 TSDB 时）。
func saveServiceWindow(mh *pb.TaskResult, rd serviceResponseData) {
	if TSDBEnabled() {
		return
	}
	if err := DB.Create(&model.ServiceHistory{
		ServiceID: mh.GetId(),
		AvgDelay:  rd.Delay,
		Data:      mh.Data,
		Up:        rd.Up,
		Down:      rd.Down,
	}).Error; err != nil {
		log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
	}
}

// notifyReportStatus 延迟报警与状态变更报警。
func notifyReportStatus(r *ReportData, m map[uint64]*model.Server, st reportTarget, stateCode uint8) {
	mh := r.Data
	if mh.Delay > 0 {
		delayCheck(r, m, st.service, mh)
	}
	if stateCode == StatusDown || stateCode != st.current.lastStatus {
		lastStatus := st.current.lastStatus
		// 存储新的状态值
		st.current.lastStatus = stateCode
		notifyCheck(r, m, st.service, mh, lastStatus, stateCode)
	}
}

const tlsCertTimeLayout = "2006-01-02 15:04:05 -0700 MST"

// checkReportTLS TLS 证书报警：抓取失败告警；成功则清除网络错误静音并检查证书过期/变更。
func (ss *ServiceSentinel) checkReportTLS(mh *pb.TaskResult, cs *model.Service) {
	if ss.serviceReportBeforeTLSSideEffectsHook != nil {
		ss.serviceReportBeforeTLSSideEffectsHook(mh.GetId())
	}
	if strings.HasPrefix(mh.Data, "SSL证书错误：") {
		notifyTLSFetchError(mh, cs)
		return
	}
	// 清除网络错误静音缓存
	NotificationShared.UnMuteNotification(cs.NotificationGroupID, NotificationMuteLabel.ServiceTLS(mh.GetId(), "network"))
	if newCert := strings.Split(mh.Data, "|"); len(newCert) > 1 {
		ss.checkTLSCert(mh, cs, newCert)
	}
}

// notifyTLSFetchError 证书抓取失败告警，忽略 i/o timeout、connection timeout、EOF 等网络抖动。
func notifyTLSFetchError(mh *pb.TaskResult, cs *model.Service) {
	if strings.HasSuffix(mh.Data, "timeout") ||
		strings.HasSuffix(mh.Data, "EOF") ||
		strings.HasSuffix(mh.Data, "timed out") {
		return
	}
	if cs.Notify {
		muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), "network")
		go NotificationShared.SendNotification(cs.NotificationGroupID, Localizer.Tf("[TLS] Fetch cert info failed, Reporter: %s, Error: %s", cs.Name, mh.Data), muteLabel)
	}
}

// checkTLSCert 首次获取时缓存证书；签发者与过期时间均变化视为证书变更并更新缓存；开启通知时发即将过期/变更提醒。
func (ss *ServiceSentinel) checkTLSCert(mh *pb.TaskResult, cs *model.Service, newCert []string) {
	if ss.tlsCertCache[mh.GetId()] == "" {
		ss.tlsCertCache[mh.GetId()] = mh.Data
	}
	oldCert := strings.Split(ss.tlsCertCache[mh.GetId()], "|")
	expiresOld, _ := time.Parse(tlsCertTimeLayout, oldCert[1])
	expiresNew, _ := time.Parse(tlsCertTimeLayout, newCert[1])
	isCertChanged := oldCert[0] != newCert[0] && !expiresNew.Equal(expiresOld)
	if isCertChanged {
		ss.tlsCertCache[mh.GetId()] = mh.Data
	}
	if !cs.Notify {
		return
	}
	notifyTLSExpiring(mh, cs, expiresNew)
	if isCertChanged {
		notifyTLSChanged(cs, oldCert[0], expiresOld, newCert[0], expiresNew)
	}
}

// notifyTLSExpiring 证书 7 天内过期提醒；静音标签含过期时间，避免多个监测点对相同证书同时报警。
func notifyTLSExpiring(mh *pb.TaskResult, cs *model.Service, expiresNew time.Time) {
	if !expiresNew.Before(time.Now().AddDate(0, 0, 7)) {
		return
	}
	expiresTimeStr := expiresNew.Format("2006-01-02 15:04:05")
	errMsg := Localizer.Tf(
		"The TLS certificate will expire within seven days. Expiration time: %s",
		expiresTimeStr,
	)
	muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), fmt.Sprintf("expire_%s", expiresTimeStr))
	go NotificationShared.SendNotification(cs.NotificationGroupID, fmt.Sprintf("[TLS] %s %s", cs.Name, errMsg), muteLabel)
}

// notifyTLSChanged 证书变更提醒；变更后缓存已自动更新，所以不需要静音。
func notifyTLSChanged(cs *model.Service, oldIssuer string, expiresOld time.Time, newIssuer string, expiresNew time.Time) {
	errMsg := Localizer.Tf(
		"TLS certificate changed, old: issuer %s, expires at %s; new: issuer %s, expires at %s",
		oldIssuer, expiresOld.Format("2006-01-02 15:04:05"), newIssuer, expiresNew.Format("2006-01-02 15:04:05"))
	go NotificationShared.SendNotification(cs.NotificationGroupID, fmt.Sprintf("[TLS] %s %s", cs.Name, errMsg), "")
}

func delayCheck(r *ReportData, m map[uint64]*model.Server, ss *model.Service, mh *pb.TaskResult) {
	if !ss.LatencyNotify {
		return
	}

	// GHSA-jx78-55p5-rwv5：m 是锁外取的 server 快照，上报端可能已被并发删除，先判空。
	reporterServer := m[r.Reporter]
	if reporterServer == nil {
		return
	}

	notificationGroupID := ss.NotificationGroupID
	minMuteLabel := NotificationMuteLabel.ServiceLatencyMin(mh.GetId())
	maxMuteLabel := NotificationMuteLabel.ServiceLatencyMax(mh.GetId())
	if mh.Delay > ss.MaxLatency {
		// 延迟超过最大值
		msg := Localizer.Tf("[Latency] %s %2f > %2f, Reporter: %s", ss.Name, mh.Delay, ss.MaxLatency, reporterServer.Name)
		go NotificationShared.SendNotification(notificationGroupID, msg, maxMuteLabel)
	} else if mh.Delay < ss.MinLatency {
		// 延迟低于最小值
		msg := Localizer.Tf("[Latency] %s %2f < %2f, Reporter: %s", ss.Name, mh.Delay, ss.MinLatency, reporterServer.Name)
		go NotificationShared.SendNotification(notificationGroupID, msg, minMuteLabel)
	} else {
		// 正常延迟， 清除静音缓存
		NotificationShared.UnMuteNotification(notificationGroupID, minMuteLabel)
		NotificationShared.UnMuteNotification(notificationGroupID, maxMuteLabel)
	}
}

func notifyCheck(r *ReportData, m map[uint64]*model.Server,
	ss *model.Service, mh *pb.TaskResult, lastStatus, stateCode uint8) {
	// GHSA-jx78-55p5-rwv5：同 delayCheck，上报端 server 可能已被并发删除。
	reporterServer := m[r.Reporter]

	// 判断是否需要发送通知
	isNeedSendNotification := ss.Notify && (lastStatus != 0 || stateCode == StatusDown)
	if isNeedSendNotification && reporterServer != nil {
		notificationGroupID := ss.NotificationGroupID
		notificationMsg := Localizer.Tf("[%s] %s Reporter: %s, Error: %s", StatusCodeToString(stateCode), ss.Name, reporterServer.Name, mh.Data)
		muteLabel := NotificationMuteLabel.ServiceStateChanged(mh.GetId())

		// 状态变更时，清除静音缓存
		if stateCode != lastStatus {
			NotificationShared.UnMuteNotification(notificationGroupID, muteLabel)
		}

		go NotificationShared.SendNotification(notificationGroupID, notificationMsg, muteLabel)
	}
}

const (
	_ = iota
	StatusNoData
	StatusGood
	StatusLowAvailability
	StatusDown
)

// statusOf 按结果窗口给出状态码：没有样本才是「无数据」。以前把可用率 0% 也当成无数据，
// 服务彻底不通时通知写成 [No Data]，面板重启时恰好不通的服务更是一条告警都不发。
func statusOf(rd serviceResponseData) uint8 {
	if rd.Up+rd.Down == 0 {
		return StatusNoData
	}
	return GetStatusCode(upPercentOf(rd))
}

// GetStatusCode 按可用率百分比给出状态码（有样本时调用，0% 即故障）。
func GetStatusCode(percent uint64) uint8 {
	if percent > 95 {
		return StatusGood
	}
	if percent > 80 {
		return StatusLowAvailability
	}
	return StatusDown
}

func StatusCodeToString(statusCode uint8) string {
	switch statusCode {
	case StatusNoData:
		return Localizer.T("No Data")
	case StatusGood:
		return Localizer.T("Good")
	case StatusLowAvailability:
		return Localizer.T("Low Availability")
	case StatusDown:
		return Localizer.T("Down")
	default:
		return ""
	}
}
