package rpc

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：ReportGeoIP 的 IP 字段是 agent 发来的任意字符串，且 IP 变更通知不带静音标签——
// 持有 agent 密钥者（普通成员即可）交替上报两个 IP，每次都向管理员配置的全局通知组发送，
// 上报内容还会原样拼进通知正文（钓鱼/伪造告警）。

const geoipSecurityUUID = "abababab-abab-abab-abab-abababababab"

// setupGeoIPNotifyFixture 成员 200 拥有服务器 7；IP 变更通知组 1 内的通知 URL 端口非法，发送立即失败不走网络。
func setupGeoIPNotifyFixture(t *testing.T) *bytes.Buffer {
	t.Helper()
	setupRequestTaskSecurityFixture(t, []*model.Server{requestTaskSecurityServer(7, 200, geoipSecurityUUID)},
		map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"member-secret": 200})
	origNotif, origCache, origLoc := singleton.NotificationShared, singleton.Cache, singleton.Localizer
	t.Cleanup(func() {
		singleton.NotificationShared, singleton.Cache, singleton.Localizer = origNotif, origCache, origLoc
	})
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	singleton.Cache = cache.New(time.Minute, time.Minute)
	db := singleton.DB
	if err := db.AutoMigrate(&model.Notification{}, &model.NotificationGroup{}, &model.NotificationGroupNotification{}); err != nil {
		t.Fatal(err)
	}
	db.Create(&model.NotificationGroup{Common: model.Common{ID: 1}, Name: "ops"})
	db.Create(&model.Notification{Common: model.Common{ID: 1}, Name: "tg", URL: "http://127.0.0.1:x/?m=#NEZHA#",
		RequestMethod: model.NotificationRequestMethodGET})
	db.Create(&model.NotificationGroupNotification{NotificationGroupID: 1, NotificationID: 1})
	singleton.NotificationShared = singleton.NewNotificationClass()
	singleton.Conf.EnableIPChangeNotification = true
	singleton.Conf.Cover = model.ConfigCoverAll
	singleton.Conf.IPChangeNotificationGroupID = 1
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func reportGeoIPAs(t *testing.T, ipv4 string) {
	t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("client_secret", "member-secret", "client_uuid", geoipSecurityUUID))
	if _, err := NewNezhaHandler().ReportGeoIP(ctx, &pb.GeoIP{Ip: &pb.IP{Ipv4: ipv4}}); err != nil {
		t.Fatal(err)
	}
}

func TestReportGeoIPFlappingIsRateLimited(t *testing.T) {
	logs := setupGeoIPNotifyFixture(t)
	for i := 0; i < 50; i++ {
		reportGeoIPAs(t, []string{"1.1.1.1", "8.8.8.8"}[i%2])
	}
	sent := strings.Count(logs.String(), "Try to notify")
	t.Logf("成员 agent 交替上报 50 次 → 管理员 IP 变更通知组被触发 %d 次", sent)
	if sent > 1 {
		t.Fatalf("同一服务器短时间内反复变更 IP 只应通知一次, got %d", sent)
	}
}

func TestReportGeoIPRejectsNonIPStrings(t *testing.T) {
	logs := setupGeoIPNotifyFixture(t)
	reportGeoIPAs(t, "1.1.1.1")
	reportGeoIPAs(t, "8.8.8.8\n[Critical] login anomaly, re-verify at https://evil.example/login")
	server, _ := singleton.ServerShared.Get(7)
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "Sending notification") {
			t.Logf("通知发送日志: %.260s", line)
		}
	}
	t.Logf("上报后运行态 IPv4=%q", server.GeoIP.IP.IPv4Addr)
	if strings.Contains(server.GeoIP.IP.IPv4Addr, "evil") {
		t.Fatal("非法 IP 字符串不得写入运行态/拼进通知")
	}
}

func TestCanonicalReportedIP(t *testing.T) {
	cases := []struct {
		raw   string
		want4 bool
		want  string
	}{
		{" 1.2.3.4 ", true, "1.2.3.4"},
		{"2001:db8::1", false, "2001:db8::1"},
		{"fe80::1%eth0 <b>x</b>", false, "fe80::1"}, // netip 允许任意 zone 内容，必须剥掉
		{"fe80::1%eth0", false, "fe80::1"},
		{"2001:db8::1", true, ""},
		{"1.2.3.4", false, ""},
		{"::ffff:1.2.3.4", false, ""},
		{"not-an-ip", true, ""},
	}
	for _, c := range cases {
		if got := canonicalReportedIP(c.raw, c.want4); got != c.want {
			t.Errorf("canonicalReportedIP(%q, %v) = %q, want %q", c.raw, c.want4, got, c.want)
		}
	}
}
