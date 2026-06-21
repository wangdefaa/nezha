package controller

// Regression tests for the implicit-cover PAT bypass class on service monitors.
//
// Background: ServerShared.CheckPermission iterates an idList and returns true
// for an empty list — it can only veto explicit IDs. createService pipes
// ss.SkipServers through that helper. But under cover=ServiceCoverAll the
// service's SkipServers map is a *deny set* (empty → fan out to every server
// owned by the user). A PAT scoped to server_ids=[1] can therefore craft a
// "cover all, deny none" config and force dashboard to dispatch service probes
// to servers outside the PAT whitelist.
//
// These tests are deliberately end-to-end through commonHandler so a future
// refactor that moves the guard to a different layer still has to satisfy the
// "PAT can't escape its whitelist via cover semantics" invariant.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

// setupCoverPATFixture builds a member-owned, two-server universe.
// alice (uid=100) owns server 1 and server 2. The caller PAT below will be
// scoped to server_ids=[1] only, so cover-all configs that fan out to
// server 2 must be rejected at the create/update boundary.
func setupCoverPATFixture(t *testing.T) {
	t.Helper()

	originalDB := singleton.DB
	originalCache := singleton.Cache
	originalLoc := singleton.Loc
	originalLocalizer := singleton.Localizer
	originalServer := singleton.ServerShared
	originalCron := singleton.CronShared
	originalUserInfo := singleton.UserInfoMap

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Server{}, &model.User{}, &model.Service{}, &model.NotificationGroup{}, &model.ServiceHistory{}))

	singleton.DB = db
	singleton.Loc = time.UTC
	singleton.Cache = cache.New(time.Minute, time.Minute)
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	// NewServiceSentinel 在构造时会调 CronShared.AddFunc 注册维护/拨测任务，
	// 必须先装配可用的调度器单例（纯调度器封装）。
	singleton.CronShared = singleton.NewCronClass()

	originalSentinel := singleton.ServiceSentinelShared
	sentinel, err := singleton.NewServiceSentinel(make(chan *model.Service, 4))
	require.NoError(t, err)
	singleton.ServiceSentinelShared = sentinel
	t.Cleanup(func() {
		sentinel.Close()
		singleton.ServiceSentinelShared = originalSentinel
	})

	sc := singleton.NewEmptyServerClassForTest()
	for _, id := range []uint64{1, 2} {
		s := &model.Server{}
		s.ID = id
		s.SetUserID(100)
		sc.InsertForTest(s)
	}
	singleton.ServerShared = sc

	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()

	t.Cleanup(func() {
		singleton.DB = originalDB
		singleton.Cache = originalCache
		singleton.Loc = originalLoc
		singleton.Localizer = originalLocalizer
		singleton.ServerShared = originalServer
		singleton.CronShared = originalCron
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})
}

func coverPATRouter(t *testing.T, tok *model.APIToken, handler func(*gin.Context)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, 100, model.RoleMember)
		if tok != nil {
			c.Set(model.CtxKeyAPIToken, tok)
			c.Set(apiTokenCtxKey, tok)
		}
		c.Next()
	})
	r.POST("/api/v1/service", handler)
	return r
}

func TestCreateService_AllowsCoverIgnoreAllEmptySkipForLimitedPAT(t *testing.T) {
	// ServiceCoverIgnoreAll + empty SkipServers is the degenerate "matches
	// nothing" case: DispatchTask iterates only entries marked true in
	// SkipServers, so an empty map causes zero fan-out. Pin the no-op
	// classification so a future refactor that broadens IgnoreAll's
	// semantics has to update this test (and the dispatch-side guard) in
	// lock-step with the writer-side guard.
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 21, UserID: 100}
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "no-op monitor",
		Target:      "example.invalid:80",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: nil,
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.True(t, success, "CoverIgnoreAll with no SkipServers is a no-op; not a bypass: error=%s", errMsg)
}

func TestCreateService_RejectsCoverAllForServerLimitedPAT(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 19, UserID: 100}
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "evil cover-all monitor",
		Target:      "example.invalid:443",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverAll,
		SkipServers: nil,
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT scoped to server_ids=[1] must NOT be able to create a ServiceCoverAll monitor with no SkipServers — DispatchTask fans out to server 2 outside the whitelist")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Service
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no service row must be persisted when the create call is rejected")
}

// Service-monitor deny-list bypass: ServiceCoverAll + SkipServers={1:true}
// passes the writer-side guard (skipCount>0), then DispatchTask probes server
// 2. The write-time guard is the only enforcement point.
func TestCreateService_RejectsCoverAllWithSkipListCoveringOnlyWhitelistedServers(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 32, UserID: 100}
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "cover-all skip-only-whitelisted monitor",
		Target:      "example.invalid:8443",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverAll,
		SkipServers: map[uint64]bool{1: true},
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT [1] must NOT create a ServiceCoverAll whose SkipServers only marks whitelisted servers; DispatchTask would probe server 2")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Service
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no service row must be persisted when the create call is rejected")
}
