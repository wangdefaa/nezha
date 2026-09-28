package singleton

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// newRefsHarness 在拨测安全测试的环境上补齐告警规则、流量、用户、设置与令牌表，并接管告警全局状态。
func newRefsHarness(t *testing.T, servers ...*model.Server) *ServiceSentinel {
	t.Helper()
	ss := newServiceMonitorSecurityHarness(t, servers...)
	require.NoError(t, DB.AutoMigrate(model.AlertRule{}, model.ServerGroupServer{}, model.Transfer{},
		model.User{}, model.SettingStore{}, model.APIToken{}))
	for _, s := range servers {
		require.NoError(t, DB.Create(s).Error)
	}
	ServerShared.sortList()
	origAlerts, origStore, origPrev := Alerts, alertsStore, alertsPrevState
	t.Cleanup(func() { Alerts, alertsStore, alertsPrevState = origAlerts, origStore, origPrev })
	Alerts = nil
	alertsStore = map[uint64]map[uint64][][]bool{}
	alertsPrevState = map[uint64]map[uint64]uint8{}
	return ss
}

// addRule 入库并装进内存告警表，模拟启动时的加载。
func addRule(t *testing.T, groupID uint64, ignore map[uint64]bool) *model.AlertRule {
	t.Helper()
	enable := true
	r := &model.AlertRule{Name: "cpu", Enable: &enable, NotificationGroupID: groupID, Rules: []*model.Rule{
		{Type: "cpu", Max: 80, Duration: 3, Cover: model.RuleCoverAll, Ignore: ignore},
	}}
	require.NoError(t, DB.Create(r).Error)
	Alerts = append(Alerts, r)
	return r
}

func newHTTPService(groupID uint64, skip map[uint64]bool) *model.Service {
	return &model.Service{Name: "web", Type: model.TaskTypeHTTPGet, Target: "https://example.com",
		Duration: 30, Cover: model.ServiceCoverIgnoreAll, SkipServers: skip, NotificationGroupID: groupID}
}

func newServers(ids ...uint64) []*model.Server {
	servers := make([]*model.Server, 0, len(ids))
	for _, id := range ids {
		servers = append(servers, &model.Server{Common: model.Common{ID: id}, UUID: fmt.Sprintf("u%d", id)})
	}
	return servers
}

func loadRule(t *testing.T, id uint64) model.AlertRule {
	t.Helper()
	var r model.AlertRule
	require.NoError(t, DB.First(&r, id).Error)
	return r
}

func loadService(t *testing.T, id uint64) model.Service {
	t.Helper()
	var s model.Service
	require.NoError(t, DB.First(&s, id).Error)
	return s
}

func TestDeleteServersPrunesRefs(t *testing.T) {
	ss := newRefsHarness(t, newServers(1, 2, 3)...)
	pruned := addRule(t, 0, map[uint64]bool{1: true, 2: true})
	untouched := addRule(t, 0, map[uint64]bool{3: true})
	alertsPrevState[pruned.ID] = map[uint64]uint8{2: 1}
	svc := newHTTPService(0, map[uint64]bool{1: true, 3: true})
	addServiceMonitorSecurityService(t, ss, svc)
	Conf.IgnoredIPNotification = "1, 3"

	require.NoError(t, DeleteServers([]uint64{1}))

	assert.Equal(t, map[uint64]bool{2: true}, loadRule(t, pruned.ID).Rules[0].Ignore)
	assert.Equal(t, map[uint64]bool{3: true}, loadService(t, svc.ID).SkipServers)
	assert.Equal(t, map[uint64]bool{2: true}, Alerts[0].Rules[0].Ignore)
	assert.Equal(t, map[uint64]bool{1: true, 2: true}, pruned.Rules[0].Ignore, "内存旧对象可能正被告警协程读取，不能原地改")
	assert.Same(t, untouched, Alerts[1], "没引用被删服务器的规则不换对象")
	assert.Equal(t, uint8(1), alertsPrevState[pruned.ID][2], "只改范围不清通知状态")
	m, ok := ss.Get(svc.ID)
	require.True(t, ok)
	assert.Equal(t, map[uint64]bool{3: true}, m.SkipServers)
	assert.Equal(t, "3", Conf.IgnoredIPNotification)
	assert.Equal(t, map[uint64]bool{3: true}, Conf.IgnoredIPNotificationServerIDs)
}

// 白名单为空等于不限制：删服务器时若去掉令牌白名单里的最后一个 ID，令牌反而能访问所有服务器。
func TestDeleteServersKeepsTokenWhitelist(t *testing.T) {
	newRefsHarness(t, newServers(1)...)
	tok := model.APIToken{UserID: 1, Name: "t", TokenHash: "h"}
	tok.SetServerIDs([]uint64{1})
	require.NoError(t, DB.Create(&tok).Error)

	require.NoError(t, DeleteServers([]uint64{1}))

	var got model.APIToken
	require.NoError(t, DB.First(&got, tok.ID).Error)
	assert.Equal(t, []uint64{1}, got.ServerIDs())
}

func TestDeleteNotificationGroupsResetsRefs(t *testing.T) {
	ss := newRefsHarness(t)
	for _, id := range []uint64{7, 8} {
		require.NoError(t, DB.Create(&model.NotificationGroup{Common: model.Common{ID: id}}).Error)
	}
	require.NoError(t, DB.Create(&model.NotificationGroupNotification{NotificationGroupID: 7, NotificationID: 1}).Error)
	gone := addRule(t, 7, nil)
	kept := addRule(t, 8, nil)
	svc := newHTTPService(7, nil)
	addServiceMonitorSecurityService(t, ss, svc)
	Conf.IPChangeNotificationGroupID = 7

	require.NoError(t, DeleteNotificationGroups([]uint64{7}))

	var members int64
	require.NoError(t, DB.Model(&model.NotificationGroupNotification{}).Count(&members).Error)
	assert.Zero(t, members)
	assert.Zero(t, loadRule(t, gone.ID).NotificationGroupID)
	assert.Equal(t, uint64(8), loadRule(t, kept.ID).NotificationGroupID)
	assert.Zero(t, loadService(t, svc.ID).NotificationGroupID)
	assert.Zero(t, Alerts[0].NotificationGroupID)
	assert.Same(t, kept, Alerts[1])
	m, _ := ss.Get(svc.ID)
	assert.Zero(t, m.NotificationGroupID)
	assert.Zero(t, Conf.IPChangeNotificationGroupID)
}

func TestOnUserDeletePrunesRefsOfOwnedServers(t *testing.T) {
	newRefsHarness(t, &model.Server{Common: model.Common{ID: 5, UserID: 2}, UUID: "u5"})
	origUsers, origSecrets := UserInfoMap, AgentSecretToUserId
	t.Cleanup(func() { UserInfoMap, AgentSecretToUserId = origUsers, origSecrets })
	UserInfoMap = map[uint64]model.UserInfo{2: {AgentSecret: "s2"}}
	AgentSecretToUserId = map[string]uint64{"s2": 2}
	require.NoError(t, DB.Create(&model.User{Common: model.Common{ID: 2}, Username: "u2", AgentSecret: "s2"}).Error)
	rule := addRule(t, 0, map[uint64]bool{5: true})

	require.NoError(t, OnUserDelete([]uint64{2}, fmt.Errorf))

	assert.Empty(t, loadRule(t, rule.ID).Rules[0].Ignore)
	assert.Empty(t, Alerts[0].Rules[0].Ignore)
	assert.NotContains(t, AgentSecretToUserId, "s2")
}

func TestDropIDsFromCSV(t *testing.T) {
	cases := []struct {
		in      string
		ids     []uint64
		want    string
		changed bool
	}{
		{"", []uint64{1}, "", false},
		{"1,2", nil, "1,2", false},
		{"1, 2 ,3", []uint64{2}, "1,3", true},
		{"1,x,1", []uint64{1}, "x", true},
		{"4", []uint64{4}, "", true},
	}
	for _, c := range cases {
		got, changed := dropIDsFromCSV(c.in, c.ids)
		assert.Equal(t, c.want, got, c.in)
		assert.Equal(t, c.changed, changed, c.in)
	}
}
