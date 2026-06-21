package singleton

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/robfig/cron/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
)

func replaceUserInfoMapForSecurityTest(t *testing.T, users map[uint64]model.UserInfo) {
	t.Helper()

	UserLock.Lock()
	original := UserInfoMap
	UserInfoMap = users
	UserLock.Unlock()

	t.Cleanup(func() {
		UserLock.Lock()
		UserInfoMap = original
		UserLock.Unlock()
	})
}

func TestClassCheckPermission(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})
	sharedClass := &ServerClass{
		class: class[uint64, *model.Server]{
			list: map[uint64]*model.Server{
				1: {Common: model.Common{ID: 1, UserID: 200}},
				2: {Common: model.Common{ID: 2, UserID: 1}},
			},
		},
		uuidToID: map[string]uint64{},
	}

	memberCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	memberCtx.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common: model.Common{ID: 200},
		Role:   model.RoleMember,
	})
	adminCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	adminCtx.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common: model.Common{ID: 1},
		Role:   model.RoleAdmin,
	})

	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{1})) {
		t.Fatal("expected member to access own resource")
	}
	if sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{2})) {
		t.Fatal("expected member to be denied foreign resource")
	}
	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{})) {
		t.Fatal("expected empty iterator to be allowed")
	}
	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{999})) {
		t.Fatal("expected unknown id to be ignored (vacuous true)")
	}
	if !sharedClass.CheckPermission(adminCtx, slices.Values([]uint64{1, 2})) {
		t.Fatal("expected admin to access any resource")
	}
}

func TestServiceMonitorResultSkipsReporterOutsideServiceCover(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "covered-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 100}, Name: "uncovered-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "selected-only-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultSkipsCoveredReporterOwnedByAnotherUser(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "foreign-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "owner-only-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true, 2: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultSkipsMismatchedTaskType(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "http-service",
		Type:        model.TaskTypeHTTPGet,
		Target:      "https://example.invalid",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, false))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeHTTPGet, true))

	waitForTodayStats(t, ss, 10, 1, 0)
}

func TestServiceMonitorResultSkipsUnknownReporter(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "known-reporter-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(999, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 999)
}

func TestServiceMonitorResultAllowsCoveredReporterOwnedByServiceOwner(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "owner-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
}

func TestServiceMonitorResultAllowsCoveredReporterForAdminOwnedService(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "member-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "admin-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{2: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 2)
}

func newServiceMonitorSecurityHarness(t *testing.T, servers ...*model.Server) *ServiceSentinel {
	t.Helper()

	originalDB := DB
	originalConf := Conf
	originalCache := Cache
	originalCronShared := CronShared
	originalServerShared := ServerShared
	originalServiceSentinelShared := ServiceSentinelShared
	originalNotificationShared := NotificationShared
	originalTSDBShared := TSDBShared
	originalLoc := Loc
	var sqlDBClose func() error

	t.Cleanup(func() {
		DB = originalDB
		Conf = originalConf
		Cache = originalCache
		CronShared = originalCronShared
		ServerShared = originalServerShared
		ServiceSentinelShared = originalServiceSentinelShared
		NotificationShared = originalNotificationShared
		TSDBShared = originalTSDBShared
		Loc = originalLoc
		if sqlDBClose != nil {
			_ = sqlDBClose()
		}
	})

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDBClose = sqlDB.Close
	DB = db
	if err := DB.AutoMigrate(
		model.Server{},
		model.Service{},
		model.ServiceHistory{},
		model.Notification{},
		model.NotificationGroup{},
		model.NotificationGroupNotification{},
	); err != nil {
		t.Fatal(err)
	}

	Conf = &ConfigClass{Config: &model.Config{AvgPingCount: 1}}
	Cache = cache.New(time.Minute, time.Minute)
	// ServiceSentinel 构造时会调 CronShared.AddFunc 注册维护/拨测任务，
	// 因此必须先装配一个可用的调度器单例（纯调度器封装，无业务）。
	CronShared = &CronClass{Cron: cron.New(cron.WithSeconds())}
	CronShared.Start()
	NotificationShared = &NotificationClass{
		class:         class[uint64, *model.Notification]{list: map[uint64]*model.Notification{}},
		groupToIDList: map[uint64]map[uint64]*model.Notification{},
		idToGroupList: map[uint64]map[uint64]struct{}{},
		groupList:     map[uint64]string{},
	}
	TSDBShared = nil
	Loc = time.UTC

	serverClass := &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: make(map[string]uint64),
	}
	for _, server := range servers {
		serverClass.list[server.ID] = server
	}
	ServerShared = serverClass

	bus := make(chan *model.Service, 1)
	ss, err := NewServiceSentinel(bus)
	if err != nil {
		t.Fatal(err)
	}
	ServiceSentinelShared = ss
	// LIFO Cleanup ordering: this Close() runs BEFORE the earlier t.Cleanup that
	// restores Conf/Cache/CronShared/NotificationShared/TSDBShared, so the
	// worker has fully exited before we swap those globals out. Skipping this
	// step causes `go test -race` to flag the write-vs-read between the
	// teardown and the still-running worker.
	t.Cleanup(func() { ss.Close() })
	return ss
}

func addServiceMonitorSecurityService(t *testing.T, ss *ServiceSentinel, service *model.Service) {
	t.Helper()

	if err := DB.Create(service).Error; err != nil {
		t.Fatal(err)
	}
	if err := ss.Update(service); err != nil {
		t.Fatal(err)
	}
}

func serviceMonitorResult(reporter, serviceID uint64, taskType uint8, successful bool) ReportData {
	return ReportData{
		Reporter: reporter,
		Data: &pb.TaskResult{
			Id:         serviceID,
			Type:       uint64(taskType),
			Delay:      12,
			Data:       "service monitor result",
			Successful: successful,
		},
	}
}

func waitForServiceHistory(t *testing.T, serviceID, serverID uint64) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		var count int64
		if err := DB.Model(&model.ServiceHistory{}).
			Where("service_id = ? AND server_id = ?", serviceID, serverID).
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("expected service history for service %d from server %d", serviceID, serverID)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func assertNoServiceHistory(t *testing.T, serviceID, serverID uint64) {
	t.Helper()

	var count int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", serviceID, serverID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no service history for service %d from server %d, got %d", serviceID, serverID, count)
	}
}

func waitForTodayStats(t *testing.T, ss *ServiceSentinel, serviceID uint64, wantUp, wantDown uint64) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		ss.serviceResponseDataStoreLock.RLock()
		stats := ss.serviceStatusToday[serviceID]
		var up, down uint64
		if stats != nil {
			up = stats.Up
			down = stats.Down
		}
		ss.serviceResponseDataStoreLock.RUnlock()

		if up == wantUp && down == wantDown {
			return
		}
		if down > wantDown {
			t.Fatalf("expected service %d down count %d, got %d", serviceID, wantDown, down)
		}

		select {
		case <-deadline:
			t.Fatalf("expected service %d stats up=%d down=%d", serviceID, wantUp, wantDown)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
