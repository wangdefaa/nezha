package singleton

import (
	"math"
	"testing"

	"github.com/goccy/go-json"

	"github.com/nezhahq/nezha/model"
)

// 回归：拨测结果 Delay 是 protobuf float，agent 可上报 NaN/Inf。旧实现直接计入当日平均延迟，
// 之后 CopyStats 的 30 天 Delay 数组含 NaN，公开的 /api/v1/service 序列化失败（访客首页拨测区空白）。
func TestServiceReportRejectsNonFiniteDelay(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common: model.Common{ID: 10, UserID: 1}, Name: "public-http", Type: model.TaskTypeHTTPGet,
		Target: "https://example.invalid", Duration: 3600,
		Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		r := serviceMonitorResult(1, service.ID, model.TaskTypeHTTPGet, true)
		r.Data.Delay = float32(bad)
		ss.processReport(r, ServerShared)
	}
	_, err := json.Marshal(ss.CopyStats())
	t.Logf("上报 NaN/Inf 延迟后 CopyStats 序列化 err=%v", err)
	if err != nil {
		t.Fatalf("非有限延迟不得进入拨测统计: %v", err)
	}
}
