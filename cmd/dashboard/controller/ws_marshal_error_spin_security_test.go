//go:build darwin || linux

package controller

import (
	"math"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：/ws/server 推送循环在序列化失败时直接 continue、不 sleep。运行态 State 一旦出现
// NaN（旧版 agent 上报未过滤），每个 WS 连接（访客即可建立）都会变成一个满核空转的协程。

func processCPUTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &ru))
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// startGuestStream 注入一台 State 含 NaN 的服务器，起 /ws/server 并以访客身份连上。
func startGuestStream(t *testing.T) *websocket.Conn {
	t.Helper()
	origServer, origConf := singleton.ServerShared, singleton.Conf
	t.Cleanup(func() { singleton.ServerShared, singleton.Conf = origServer, origConf })
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{Name: "poisoned", Host: &model.Host{}, State: &model.HostState{CPU: math.NaN()}}
	srv.ID = 1
	sc.InsertForTest(srv)
	singleton.ServerShared = sc
	InitUpgrader()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	done := make(chan struct{})
	r.GET("/api/v1/ws/server", func(c *gin.Context) { defer close(done); commonHandler(serverStream)(c) })
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	// Cleanup 后进先出：先断开客户端，再等推送协程探测到断开退出，最后才还原全局变量。
	t.Cleanup(func() { waitStreamHandlerExit(t, done) })
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/ws/server", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitStreamHandlerExit 客户端断开后推送协程必须自行退出（序列化持续失败时靠 ping 探测）。
func waitStreamHandlerExit(t *testing.T, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(3 * serverStreamInterval):
		t.Error("客户端断开后 /ws/server 推送协程未退出（协程泄漏）")
	}
}

func TestServerStreamDoesNotSpinOnMarshalError(t *testing.T) {
	startGuestStream(t)
	time.Sleep(100 * time.Millisecond)
	before := processCPUTime(t)
	time.Sleep(time.Second)
	used := processCPUTime(t) - before
	t.Logf("单个访客 WS 连接 1 秒内进程 CPU 消耗: %v", used)
	require.Less(t, used, 300*time.Millisecond, "序列化失败时推送循环不得空转")
}
