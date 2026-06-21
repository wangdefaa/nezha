package singleton

import (
	"github.com/robfig/cron/v3"
)

// CronClass 现在只是 robfig/cron 调度器的纯封装。原先内嵌的 model.Cron 业务
// （定时任务 / 命令下发 / 触发任务）已移除，但调度器单例 CronShared 仍是全系
// 统底座：服务拨测调度、流量清理/打点、Keepalive、JWT 会话 GC、系统维护都通
// 过它的 AddFunc/Remove 注册周期任务。删除 Cron 业务时务必保留可用的调度器。
type CronClass struct {
	*cron.Cron
}

// NewCronClass 构造并启动调度器单例。不再从数据库加载任何业务 cron。
func NewCronClass() *CronClass {
	cronx := cron.New(cron.WithSeconds(), cron.WithLocation(Loc))
	cronx.Start()
	return &CronClass{Cron: cronx}
}

// userIsAdmin 供拨测等非 cron 业务复用（servicesentinel.go / server.go）。
// 0 号用户是历史 global-secret 伪 owner，按 admin 处理。
func userIsAdmin(userID uint64) bool {
	if userID == 0 {
		return true
	}

	UserLock.RLock()
	defer UserLock.RUnlock()

	userInfo, ok := UserInfoMap[userID]
	return ok && userInfo.Role.IsAdmin()
}
