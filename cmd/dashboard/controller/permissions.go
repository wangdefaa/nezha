package controller

import (
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func callerIsAdmin(c *gin.Context) bool {
	auth, _ := c.Get(model.CtxKeyAuthorizedUser)
	user, ok := auth.(*model.User)
	return ok && user != nil && user.Role.IsAdmin()
}

// patAllowsServer reports whether the caller's PAT (if any) is allowed to
// touch serverID. JWT callers (no PAT in context) always pass. Used as an
// extra guard before the admin / owner short-circuits so a PAT scoped to
// a server_ids whitelist cannot widen reach via the caller's admin role.
func patAllowsServer(c *gin.Context, serverID uint64) bool {
	tok := model.PATFromContext(c)
	return tok == nil || tok.CanAccessServer(serverID)
}

// patHasServerWhitelist 判断调用方是否为带非空 server_ids 白名单的 PAT。
// ServiceCoverAll 会 fan-out 到 owner 的全部 server，受限 PAT 若能写入此类配置
// 即可越出自身白名单；运行时 DispatchTask 不再看 PAT 上下文，所以只能在写入时拦截。
// JWT 与不限 server 的 PAT 没有白名单可越，直接放行。
func patHasServerWhitelist(c *gin.Context) bool {
	v, _ := c.Get(model.CtxKeyAPIToken)
	wl, ok := v.(model.APITokenWhitelistView)
	return ok && len(wl.ServerIDs()) > 0
}

// checkServiceSkipServerPermission 校验服务监控 SkipServers 的写入权限。
// ServiceCoverAll → SkipServers is a deny-set, only ownership required.
// ServiceCoverIgnoreAll → SkipServers is an allow-set, full Server.HasPermission.
//
// Runtime DispatchTask + model.EnabledIDs only consult entries whose
// bool value is true; false entries are no-ops. Filtering to true-only
// here keeps the write-side permission check aligned with the runtime
// fan-out (a member touching `{2: false}` for a foreign-owned server 2
// has no dispatch effect, so rejecting the request is over-restrictive
// and inconsistent with what listing / runtime see).
func checkServiceSkipServerPermission(c *gin.Context, cover uint8, skip map[uint64]bool, ownerUID uint64) error {
	ids := model.EnabledIDs(skip)
	if cover == model.ServiceCoverAll {
		if !denyListOwnedByCaller(ownerUID, ids) {
			return singleton.Localizer.ErrorT("permission denied")
		}
		return nil
	}
	if !singleton.ServerShared.CheckPermission(c, slices.Values(ids)) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}

// denyListOwnedByCaller verifies every id in denyList refers to a server
// owned by ownerUID. Under *CoverAll the deny-list expresses exclusion, not
// access, so it must not point at someone else's servers.
//
// Admin owners are special: runtime DispatchTask fans out
// across the WHOLE system via userIsAdmin(owner), so a safe deny-list for
// an admin-owned resource must be allowed to include foreign-owned servers
// — that's the only way a limited PAT can contain the fan-out. We still
// require each id to refer to a real server, just not to be owned by the
// admin specifically.
func denyListOwnedByCaller(ownerUID uint64, denyList []uint64) bool {
	ownerIsAdmin := model.OwnerIsAdminLookup != nil && model.OwnerIsAdminLookup(ownerUID)
	for _, id := range denyList {
		s, found := singleton.ServerShared.Get(id)
		if !found || s == nil {
			return false
		}
		if ownerIsAdmin {
			continue
		}
		if s.GetUserID() != ownerUID {
			return false
		}
	}
	return true
}

// denyListCoversAllOwnerServersOutsidePATWhitelist reports whether every
// server visible to the service owner that is NOT in the caller PAT's
// server_ids whitelist also appears in denyList. Under *CoverAll semantics
// the runtime dispatch (DispatchTask) fans out to ServerShared
// minus denyList; the only way a server-limited PAT can stay inside its
// whitelist is if denyList already covers every owner-visible server outside
// that whitelist. Returning true means the configuration is safe.
func denyListCoversAllOwnerServersOutsidePATWhitelist(c *gin.Context, ownerUID uint64, denyIDs []uint64) bool {
	tok := model.PATFromContext(c)
	return tok == nil || model.DenyListSafeForLimitedPAT(tok, ownerUID, denyIDs)
}

// coverMode 抽象「cover 字段在 dispatch 时如何解读 servers 字段」。
//
// 写侧 rejectImplicit* 与运行时 batch-delete 入口共用同一条 PAT 收口
// 路径（assertPATCoverFanoutWithinWhitelist），靠它把两边的规则对齐。
// 目前只有服务监控（Service）使用；定时任务已随 cron 模块移除。
type coverMode uint8

const (
	// coverModePinnedByCaller: dispatch 阶段不按 servers 字段做 fan-out，
	// 真实目标由外部信号钉死（原 CronCoverAlertTrigger，现无资源映射到此档，
	// 仅保留给测试与将来的资源）。PAT 在这里不做额外收口。
	coverModePinnedByCaller coverMode = iota

	// coverModeAllMinusDeny: dispatch 时取 owner 全量 server 集合，再减去
	// servers（deny-list）。代表 ServiceCoverAll。受限 PAT
	// 必须确保 deny-list 已覆盖白名单外的全部 owner servers，否则 fan-out
	// 会跑到 PAT 白名单之外。
	coverModeAllMinusDeny

	// coverModeAllowList: dispatch 时只在 servers（allow-list）内 fan-out。
	// 代表 ServiceCoverIgnoreAll。受限 PAT 必须能访
	// 问 allow-list 中的每一个 server。空 allow-list 是「matches nothing」
	// 的退化形态，安全。
	coverModeAllowList
)

// assertPATCoverFanoutWithinWhitelist 是 cover-all / cover-ignore-all 两类
// 「按 owner 全量 fan-out」资源的 PAT 收口。
//
// 任何会按「owner servers 减 denyList」或「allowList 自身」展开的资源都必须
// 在 dispatch 入口（manual 触发 / batch-delete / mutation）调用它；写侧
// rejectImplicit* 也走同一条路径，从根上保证两边不漂移。
//
// JWT 请求或不带 server 白名单的 PAT 直接放行——它们没有「白名单」可越过。
//
// 失败时统一返回 i18n "permission denied"，与既有写侧 guard 行为一致。
func assertPATCoverFanoutWithinWhitelist(c *gin.Context, ownerUID uint64, mode coverMode, servers []uint64) error {
	if !patHasServerWhitelist(c) {
		return nil
	}
	switch mode {
	case coverModePinnedByCaller:
		return nil
	case coverModeAllMinusDeny:
		if !denyListCoversAllOwnerServersOutsidePATWhitelist(c, ownerUID, servers) {
			return singleton.Localizer.ErrorT("permission denied")
		}
		return nil
	case coverModeAllowList:
		tok := model.PATFromContext(c)
		if tok == nil {
			return nil
		}
		for _, id := range servers {
			if !tok.CanAccessServer(id) {
				return singleton.Localizer.ErrorT("permission denied")
			}
		}
		return nil
	default:
		// 未识别 cover 模式按拒绝处理；新增 coverMode 必须显式 wire 到
		// 资源专用入口里，不允许沉默放行。
		return singleton.Localizer.ErrorT("permission denied")
	}
}

// coverModeUnknown 表示 Service 持久化里出现了当前代码不认识的 cover
// 常量。这一档专门让 assertPATCoverFanoutWithinWhitelist 走 default 分支
// fail-closed，保证「未知 cover 必须显式 wire，否则拒绝」的不变量。
const coverModeUnknown coverMode = 255

// patGroupMembershipAccessAllowed returns false when the caller's PAT
// carries a server_ids whitelist that does not cover every current member
// of groupID. JWT requests and unscoped PATs always pass. Used by
// updateServerGroup before the transactional DELETE+INSERT — otherwise a
// PAT scoped to [X] could indirectly remove server Y from a shared group.
func patGroupMembershipAccessAllowed(c *gin.Context, groupID uint64) bool {
	tok := model.PATFromContext(c)
	if tok == nil || !patHasServerWhitelist(c) {
		return true
	}
	var members []model.ServerGroupServer
	if err := singleton.DB.Where("server_group_id = ?", groupID).Find(&members).Error; err != nil {
		return false
	}
	for _, m := range members {
		if !tok.CanAccessServer(m.ServerId) {
			return false
		}
	}
	return true
}

// isValidServiceCover 校验服务监控 cover 枚举：DispatchTask 只认 ServiceCoverAll /
// ServiceCoverIgnoreAll，其它值一旦落库会绕开 PAT cover-fanout 收口，必须在写入时拒绝。
func isValidServiceCover(cover uint8) bool {
	switch cover {
	case model.ServiceCoverAll, model.ServiceCoverIgnoreAll:
		return true
	}
	return false
}

// serviceCoverMode 把 model.ServiceCover* 翻译成共享底座认识的 coverMode。
// Service 没有 alert-trigger 这一档，只有 All 与 IgnoreAll。
func serviceCoverMode(cover uint8) coverMode {
	switch cover {
	case model.ServiceCoverAll:
		return coverModeAllMinusDeny
	case model.ServiceCoverIgnoreAll:
		return coverModeAllowList
	default:
		// 未识别 cover 不允许借 pinned 旁路 PAT 收口。
		return coverModeUnknown
	}
}

// rejectImplicitServiceCoverForLimitedPAT enforces the cover-all PAT guard.
// ServiceCoverAll treats SkipServers as a deny-set: DispatchTask iterates
// ServerShared.Range and probes every server owned by the service owner that
// is NOT marked true in SkipServers. A server-limited PAT must therefore mark
// every owner-visible server outside its whitelist as skipped.
//
// 靠 assertPATCoverFanoutWithinWhitelist 落地，与 service 运行时入口
// （batchDeleteService）共用一条裁决路径。
func rejectImplicitServiceCoverForLimitedPAT(c *gin.Context, cover uint8, skipServers map[uint64]bool, ownerUID uint64) error {
	if cover != model.ServiceCoverAll {
		return nil
	}
	return assertPATCoverFanoutWithinWhitelist(c, ownerUID, coverModeAllMinusDeny, model.EnabledIDs(skipServers))
}

// enforcePATServiceDispatchScope 是 service monitor 运行时入口
// （batchDeleteService 等）的 PAT 收口。SkipServers 是 map[uint64]bool，
// 这里展开成 deny-list 切片喂给共享底座；语义与
// rejectImplicitServiceCoverForLimitedPAT 严格对齐。
func enforcePATServiceDispatchScope(c *gin.Context, svc *model.Service) error {
	if svc == nil {
		return nil
	}
	return assertPATCoverFanoutWithinWhitelist(c, svc.GetUserID(), serviceCoverMode(svc.Cover), model.EnabledIDs(svc.SkipServers))
}

func userCanViewServer(c *gin.Context, server *model.Server) bool {
	if server == nil {
		return false
	}
	// PAT 白名单优先于 admin/owner 早返回：admin 自己签发的 server_ids 受限 PAT
	// 必须只能看见白名单里的 server，否则给自己设的硬边界形同虚设。
	if !patAllowsServer(c, server.GetID()) {
		return false
	}
	if callerIsAdmin(c) {
		return true
	}
	if _, isMember := c.Get(model.CtxKeyAuthorizedUser); isMember {
		if server.HasPermission(c) {
			return true
		}
		return !server.HideForGuest
	}
	return !server.HideForGuest
}

func userCanViewService(c *gin.Context, service *model.Service) bool {
	if service == nil {
		return false
	}
	// HideForGuest 默认公开，置 true 才对 guest 隐藏，语义与 Server.HideForGuest 对齐。
	if service.HideForGuest {
		if _, isMember := c.Get(model.CtxKeyAuthorizedUser); !isMember {
			return false
		}
		// 必须先让 Service.HasPermission 跑 PAT 白名单收口，再让 admin 在无 PAT 请求上
		// 短路放行，否则 admin 自签的受限 PAT 会被早返回绕过 list/history 的 PAT 边界。
		return service.HasPermission(c)
	}
	return true
}

func assertOwnsNotificationGroup(c *gin.Context, groupID uint64) error {
	if groupID == 0 {
		return nil
	}

	var ng model.NotificationGroup
	if err := singleton.DB.First(&ng, groupID).Error; err != nil {
		return singleton.Localizer.ErrorT("notification group id %d does not exist", groupID)
	}
	if !ng.HasPermission(c) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}
