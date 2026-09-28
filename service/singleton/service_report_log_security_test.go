package singleton

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/nezhahq/nezha/model"
)

// 回归：被拒绝的拨测上报以 %+v 整条写日志，其中 Data 是 agent 任意字符串（单条可达 gRPC 上限 4MiB），
// 可伪造日志行（换行注入）并放大写盘。
func TestIncorrectServiceReportLogIsBounded(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "assigned"},
		&model.Server{Common: model.Common{ID: 2, UserID: 1}, Name: "other"},
	)
	service := &model.Service{
		Common: model.Common{ID: 10, UserID: 1}, Name: "svc", Type: model.TaskTypeHTTPGet,
		Target: "https://example.invalid", Duration: 3600,
		Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r := serviceMonitorResult(2, service.ID, model.TaskTypeHTTPGet, true)
	r.Data.Data = strings.Repeat("A", 1<<20) + "\n2026/01/01 00:00:00 NEZHA>> [FORGED] admin login from 1.2.3.4"
	ss.processReport(r, ServerShared)

	t.Logf("被拒上报写入日志 %d 字节", buf.Len())
	if buf.Len() > 1024 || strings.Contains(buf.String(), "[FORGED]") {
		t.Fatalf("被拒上报的日志不得包含 agent 原始数据, len=%d", buf.Len())
	}
}
