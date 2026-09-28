package controller

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	docs "github.com/nezhahq/nezha/cmd/dashboard/docs"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

func ServeWeb(frontendDist fs.FS) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.MaxMultipartMemory = 64 << 20 // multipart 内存缓冲阈值（超出落临时盘）；请求体总上限见 limitRequestBody

	if singleton.Conf.Debug {
		gin.SetMode(gin.DebugMode)
		pprof.Register(r)
		log.Printf("NEZHA>> Swagger(%s) UI available at http://localhost:%d/swagger/index.html", docs.SwaggerInfo.Version, singleton.Conf.ListenPort)
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))
	}

	r.Use(waf.RealIp)
	r.Use(waf.Waf)
	r.Use(limitRequestBody)

	routers(r, frontendDist)

	return r
}

func routers(r *gin.Engine, frontendDist fs.FS) {
	authMiddleware, err := jwt.New(initParams())
	if err != nil {
		log.Fatal("JWT Error:" + err.Error())
	}
	if err := authMiddleware.MiddlewareInit(); err != nil {
		log.Fatal("authMiddleware.MiddlewareInit Error:" + err.Error())
	}

	api := r.Group("api/v1")
	api.POST("/login", authMiddleware.LoginHandler)
	api.GET("/oauth2/:provider", commonHandler(oauth2redirect))

	fallbackAuthMw := fallbackAuthMiddleware(authMiddleware)
	fallbackAuth := api.Group("", fallbackAuthMw)
	fallbackAuth.GET("/setting", commonHandler(listConfig))
	fallbackAuth.GET("/oauth2/callback", commonHandler(oauth2callback(authMiddleware)))

	jwtMw := authMiddleware.MiddlewareFunc()
	patMw := apiTokenAuthMiddleware()
	authMw := jwtOrPATAuthMiddleware(patMw, jwtMw)
	// optional 路由：ForceAuth=true 走严格 PAT-or-JWT；ForceAuth=false 走
	// PAT-or-FallbackJWT，保证两种模式下 PAT 都会被解析，restScopeMiddleware
	// 才能按 scope 真实收口（否则匿名 PAT 请求会被当 guest，scope 失效）。
	optionalAuthMw := utils.IfOr(singleton.Conf.ForceAuth, authMw, patOrFallbackAuthMiddleware(patMw, fallbackAuthMw))

	optionalAuth := api.Group("", optionalAuthMw)
	optionalAuth.GET("/ws/server", restScopeMiddleware(model.ScopeInventoryRead), commonHandler(serverStream))
	optionalAuth.GET("/server-group", restScopeMiddleware(model.ScopeInventoryRead), commonHandler(listServerGroup))

	optionalAuth.GET("/service", restScopeMiddleware(model.ScopeServiceRead), commonHandler(showService))
	optionalAuth.GET("/service/server", restScopeMiddleware(model.ScopeServiceRead), commonHandler(listServerWithServices))
	optionalAuth.GET("/service/:id/history", restScopeMiddleware(model.ScopeServiceRead), commonHandler(getServiceHistory))
	optionalAuth.GET("/server/:id/service", restScopeMiddleware(model.ScopeServiceRead), commonHandler(listServerServices))
	optionalAuth.GET("/server/:id/metrics", restScopeMiddleware(model.ScopeServerRead), commonHandler(getServerMetrics))

	// CSRF middleware applies group-wide. Safe methods short-circuit and
	// PAT bearer requests bypass — so the only callers gated are
	// cookie-JWT POST/PATCH/PUT/DELETE, which is exactly the H6 surface.
	auth := api.Group("", authMw, csrfMiddleware())

	// 「自我管理」类端点 — 显式禁止 PAT 访问（避免 PAT 自我提权链）。
	patForbidden := restPATForbiddenMiddleware()
	auth.POST("/refresh-token", patForbidden, authMiddleware.RefreshHandler)
	auth.POST("/logout", patForbidden, commonHandler(logout))
	auth.GET("/profile", patForbidden, commonHandler(getProfile))
	auth.POST("/profile", patForbidden, commonHandler(updateProfile))
	auth.POST("/oauth2/:provider/unbind", patForbidden, commonHandler(unbindOauth2))
	auth.GET("/api-tokens", patForbidden, commonHandler(listAPITokens))
	auth.POST("/api-tokens", patForbidden, commonHandler(createAPIToken))
	auth.DELETE("/api-tokens/:id", patForbidden, commonHandler(deleteAPIToken))

	// 资源族划分：
	//   - nezha:inventory:* —— 对“服务器台账”的枚举与删除（列出 server / server-group、
	//     删除 server / server-group）。这是管理后台清单管理动作。
	//   - nezha:server:*    —— 对已知 server 的运行态操作（编辑配置、
	//     force-update、batch-move）。
	auth.GET("/server", restScopeMiddleware(model.ScopeInventoryRead), listHandler(listServer))
	auth.PATCH("/server/:id", restScopeMiddleware(model.ScopeServerWrite), commonHandler(updateServer))
	auth.POST("/batch-delete/server", restScopeMiddleware(model.ScopeInventoryDelete), commonHandler(batchDeleteServer))
	auth.POST("/force-update/server", restScopeMiddleware(model.ScopeServerWrite), commonHandler(forceUpdateServer))
	auth.POST("/server-group", restScopeMiddleware(model.ScopeServerWrite), commonHandler(createServerGroup))
	auth.PATCH("/server-group/:id", restScopeMiddleware(model.ScopeServerWrite), commonHandler(updateServerGroup))
	auth.POST("/batch-delete/server-group", restScopeMiddleware(model.ScopeInventoryDelete), commonHandler(batchDeleteServerGroup))

	// service monitor
	auth.GET("/service/list", restScopeMiddleware(model.ScopeServiceRead), listHandler(listService))
	auth.POST("/service", restScopeMiddleware(model.ScopeServiceWrite), commonHandler(createService))
	auth.PATCH("/service/:id", restScopeMiddleware(model.ScopeServiceWrite), commonHandler(updateService))
	auth.POST("/batch-delete/service", restScopeMiddleware(model.ScopeServiceDelete), commonHandler(batchDeleteService))

	auth.GET("/notification-group", restScopeMiddleware(model.ScopeNotificationGroupRead), commonHandler(listNotificationGroup))
	auth.POST("/notification-group", restScopeMiddleware(model.ScopeNotificationGroupWrite), commonHandler(createNotificationGroup))
	auth.PATCH("/notification-group/:id", restScopeMiddleware(model.ScopeNotificationGroupWrite), commonHandler(updateNotificationGroup))
	auth.POST("/batch-delete/notification-group", restScopeMiddleware(model.ScopeNotificationGroupDelete), commonHandler(batchDeleteNotificationGroup))

	auth.GET("/notification", restScopeMiddleware(model.ScopeNotificationRead), listHandler(listNotification))
	auth.POST("/notification", restScopeMiddleware(model.ScopeNotificationWrite), commonHandler(createNotification))
	auth.PATCH("/notification/:id", restScopeMiddleware(model.ScopeNotificationWrite), commonHandler(updateNotification))
	auth.POST("/batch-delete/notification", restScopeMiddleware(model.ScopeNotificationDelete), commonHandler(batchDeleteNotification))

	auth.GET("/alert-rule", restScopeMiddleware(model.ScopeAlertRuleRead), listHandler(listAlertRule))
	auth.POST("/alert-rule", restScopeMiddleware(model.ScopeAlertRuleWrite), commonHandler(createAlertRule))
	auth.PATCH("/alert-rule/:id", restScopeMiddleware(model.ScopeAlertRuleWrite), commonHandler(updateAlertRule))
	auth.POST("/batch-delete/alert-rule", restScopeMiddleware(model.ScopeAlertRuleDelete), commonHandler(batchDeleteAlertRule))

	// 管理员资源 — 仅 nezha:* / nezha:admin:* 持有者可调（adminHandler 进一步校验 user.Role）。
	auth.GET("/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(listUser))
	auth.POST("/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(createUser))
	auth.POST("/batch-delete/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchDeleteUser))
	auth.GET("/waf", restScopeMiddleware(model.ScopeAdminAll), pAdminHandler(listBlockedAddress))
	auth.POST("/batch-delete/waf", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchDeleteBlockedAddress))
	auth.GET("/online-user", restScopeMiddleware(model.ScopeAdminAll), pAdminHandler(listOnlineUser))
	auth.POST("/online-user/batch-block", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchBlockOnlineUser))
	auth.PATCH("/setting", restScopeMiddleware(model.ScopeAdminAll), adminHandler(updateConfig))
	auth.POST("/maintenance", restScopeMiddleware(model.ScopeAdminAll), adminHandler(runMaintenance))

	// 主题管理（访客展示页主题入库解耦）。upload 为 multipart，github 为 JSON。
	auth.GET("/theme", restScopeMiddleware(model.ScopeAdminAll), adminHandler(listTheme))
	auth.POST("/theme/upload", restScopeMiddleware(model.ScopeAdminAll), adminHandler(uploadTheme))
	auth.POST("/theme/github", restScopeMiddleware(model.ScopeAdminAll), adminHandler(createGithubTheme))
	auth.POST("/theme/:id/refresh", restScopeMiddleware(model.ScopeAdminAll), adminHandler(refreshTheme))
	auth.POST("/theme/:id/apply", restScopeMiddleware(model.ScopeAdminAll), adminHandler(applyTheme))
	auth.POST("/batch-delete/theme", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchDeleteTheme))

	r.NoRoute(fallbackToFrontend(frontendDist))
}

func newErrorResponse(err error) model.CommonResponse[any] {
	return model.CommonResponse[any]{
		Success: false,
		Error:   err.Error(),
	}
}

type handlerFunc[T any] func(c *gin.Context) (T, error)
type pHandlerFunc[S ~[]E, E any] func(c *gin.Context) (*model.Value[S], error)

// There are many error types in gorm, so create a custom type to represent all
// gorm errors here instead
type gormError struct {
	msg string
	a   []any
}

func newGormError(format string, args ...any) error {
	return &gormError{
		msg: format,
		a:   args,
	}
}

func (ge *gormError) Error() string {
	return fmt.Sprintf(ge.msg, ge.a...)
}

type wsError struct {
	msg string
	a   []any
}

func newWsError(format string, args ...any) error {
	return &wsError{
		msg: format,
		a:   args,
	}
}

func (we *wsError) Error() string {
	return fmt.Sprintf(we.msg, we.a...)
}

var errNoop = errors.New("wrote")

func commonHandler[T any](handler handlerFunc[T]) func(*gin.Context) {
	return func(c *gin.Context) {
		handle(c, handler)
	}
}

func adminHandler[T any](handler handlerFunc[T]) func(*gin.Context) {
	return func(c *gin.Context) {
		if requireAdmin(c) {
			handle(c, handler)
		}
	}
}

// requireAdmin 校验当前登录用户为管理员；不满足时写入错误响应并返回 false。
func requireAdmin(c *gin.Context) bool {
	auth, _ := c.Get(model.CtxKeyAuthorizedUser)
	user, ok := auth.(*model.User)
	if !ok || user == nil {
		c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("unauthorized")))
		return false
	}
	if !user.Role.IsAdmin() {
		c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("permission denied")))
		return false
	}
	return true
}

func handle[T any](c *gin.Context, handler handlerFunc[T]) {
	data, err := handler(c)
	if err == nil {
		c.JSON(http.StatusOK, model.CommonResponse[T]{Success: true, Data: data})
		return
	}
	switch err.(type) {
	case *gormError:
		log.Printf("NEZHA>> gorm error: %v", err)
		c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("database error")))
		return
	case *wsError:
		// Connection is upgraded to WebSocket, so c.Writer is no longer usable
		if msg := err.Error(); msg != "" {
			log.Printf("NEZHA>> websocket error: %v", err)
		}
		return
	default:
		if !errors.Is(err, errNoop) {
			c.JSON(http.StatusOK, newErrorResponse(err))
		}
		return
	}
}

func listHandler[S ~[]E, E model.CommonInterface](handler handlerFunc[S]) func(*gin.Context) {
	return func(c *gin.Context) {
		data, err := handler(c)
		if err != nil {
			c.JSON(http.StatusOK, newErrorResponse(err))
			return
		}

		filtered := filter(c, data)
		c.JSON(http.StatusOK, model.CommonResponse[S]{Success: true, Data: model.SearchByIDCtx(c, filtered)})
	}
}

func pAdminHandler[S ~[]E, E any](handler pHandlerFunc[S, E]) func(*gin.Context) {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		data, err := handler(c)
		if err != nil {
			c.JSON(http.StatusOK, newErrorResponse(err))
			return
		}

		c.JSON(http.StatusOK, model.PaginatedResponse[S, E]{Success: true, Data: data})
	}
}

func filter[S ~[]E, E model.CommonInterface](ctx *gin.Context, s S) S {
	return slices.DeleteFunc(s, func(e E) bool {
		return !e.HasPermission(ctx)
	})
}

func getUid(c *gin.Context) uint64 {
	user, _ := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	return user.ID
}

// paramID 解析路径参数 :id；非法时原样返回 strconv 的错误。
func paramID(c *gin.Context) (uint64, error) {
	return strconv.ParseUint(c.Param("id"), 10, 64)
}

// parsePagination 读取 limit/offset 查询参数：limit 非法或 <1 取 25，offset 非法或 <0 取 0。
func parsePagination(c *gin.Context) (limit, offset int) {
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = 25
	}
	offset, err = strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	return limit, offset
}

// uniqueIDs 原地排序并去重。slices.Compact 只合并相邻重复，[1,2,1] 这类输入必须先排序，
// 否则后续按 len(ids) 比对存在性会误报“有非法 id”。
func uniqueIDs(ids []uint64) []uint64 {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// ensureIDsExist 确认 ids 在 m 对应的表里全部存在（ids 需已去重），否则返回 invalid。
func ensureIDsExist(m any, ids []uint64, invalid error) error {
	var count int64
	if err := singleton.DB.Model(m).Where("id in (?)", ids).Count(&count).Error; err != nil {
		return newGormError("%v", err)
	}
	if count != int64(len(ids)) {
		return invalid
	}
	return nil
}

// deleteWithMembers 在同一事务里物理删除主表记录（id in ids）与关联表中 memberCond 命中的成员行。
func deleteWithMembers(owner, member any, memberCond string, ids []uint64) error {
	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(owner, "id in (?)", ids).Error; err != nil {
			return err
		}
		return tx.Unscoped().Delete(member, memberCond, ids).Error
	})
	if err != nil {
		return newGormError("%v", err)
	}
	return nil
}

// setFrontendCacheHeader 控制前端静态资源缓存：带内容哈希的构建产物（assets/）长缓存且 immutable；
// 其余（index.html、SPA fallback、logo 等）一律 no-cache，确保主题换 hash 后刷新即取最新引用，避免白屏。
func setFrontendCacheHeader(c *gin.Context, name string) {
	if strings.HasPrefix(name, "assets/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	c.Header("Cache-Control", "no-cache")
}

// frontendPageUrlRegistry 决定哪些 URL 走 index.html fallback 并返回 200；漏一条会让
// 直接刷新该页面变成 404（body 仍是 index.html，浏览器内看起来正常，但监控 / 链接预览会以为站点挂了）。
// 新增前端路由时必须与 nezha-admin-dash/src/main.tsx 同步。
var frontendPageUrlRegistry = []*regexp.Regexp{
	// official user frontend
	regexp.MustCompile(`^/$`),
	regexp.MustCompile(`^/server/\d*$`),
	// backend frontend
	regexp.MustCompile(`^/dashboard/$`),
	regexp.MustCompile(`^/dashboard/login$`),
	regexp.MustCompile(`^/dashboard/service$`),
	regexp.MustCompile(`^/dashboard/notification$`),
	regexp.MustCompile(`^/dashboard/alert-rule$`),
	regexp.MustCompile(`^/dashboard/server-group$`),
	regexp.MustCompile(`^/dashboard/notification-group$`),
	regexp.MustCompile(`^/dashboard/profile$`),
	regexp.MustCompile(`^/dashboard/settings$`),
	regexp.MustCompile(`^/dashboard/settings/user$`),
	regexp.MustCompile(`^/dashboard/settings/online-user$`),
	regexp.MustCompile(`^/dashboard/settings/waf$`),
	regexp.MustCompile(`^/dashboard/settings/api-tokens$`),
	regexp.MustCompile(`^/dashboard/settings/theme$`),
}

func getFallbackStatusCode(path string) int {
	for _, reg := range frontendPageUrlRegistry {
		if reg.MatchString(path) {
			return http.StatusOK
		}
	}
	return http.StatusNotFound
}

func fallbackToFrontend(frontendDist fs.FS) func(*gin.Context) {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		switch {
		case strings.HasPrefix(path, "/api"):
			c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
		case path == "/dashboard":
			c.Redirect(http.StatusMovedPermanently, "/dashboard/")
		// Only /dashboard/ belongs to the admin frontend; /dashboard.. must not be trimmed into ../.
		case strings.HasPrefix(path, "/dashboard/"):
			serveAdminFrontend(c, frontendDist, path)
		default:
			serveUserFrontend(c, frontendDist, path)
		}
	}
}

// serveAdminFrontend 管理端固定内置 admin-dist：只走 serveBuiltin，不读 <ThemeDir> 磁盘，面板无法替换/更新。
func serveAdminFrontend(c *gin.Context, frontendDist fs.FS, path string) {
	stripPath := strings.TrimPrefix(path, "/dashboard/")
	if serveBuiltin(c, frontendDist, singleton.AdminTemplatePath, stripPath, http.StatusOK) ||
		serveBuiltin(c, frontendDist, singleton.AdminTemplatePath, "index.html", getFallbackStatusCode(path)) {
		return
	}
	c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
}

// serveUserFrontend 访客端：当前主题的静态文件 → 当前主题 index.html → 内置 user-dist 的 index.html（避免整站 404）。
func serveUserFrontend(c *gin.Context, frontendDist fs.FS, path string) {
	template := singleton.Conf.UserTemplate
	fallbackStatusCode := getFallbackStatusCode(path)
	if checkLocalFileOrFs(c, frontendDist, template, strings.TrimPrefix(path, "/"), http.StatusOK) ||
		checkLocalFileOrFs(c, frontendDist, template, "index.html", fallbackStatusCode) {
		return
	}
	if template != model.DefaultUserTemplate &&
		checkLocalFileOrFs(c, frontendDist, model.DefaultUserTemplate, "index.html", fallbackStatusCode) {
		return
	}
	c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
}

// checkLocalFileOrFs 访客主题查找次序：磁盘 <ThemeDir>/<path>（自定义/更新版）→ 内置（serveBuiltin）；
// 自定义主题没有 embed 兜底。
func checkLocalFileOrFs(c *gin.Context, frontendFS fs.FS, templateRoot, filePath string, customStatusCode int) bool {
	if filePath != "" && singleton.ThemeDir != "" &&
		tryDiskRoot(c, filepath.Join(singleton.ThemeDir, templateRoot), filePath, customStatusCode) {
		return true
	}
	if src, known := singleton.ThemeSourceOf(templateRoot); known && src != model.ThemeSourceBuiltin {
		return false
	}
	return serveBuiltin(c, frontendFS, templateRoot, filePath, customStatusCode)
}

// serveBuiltin 内置主题：cwd 相对目录（兼容上游目录布局 + 单测 fixture）→ embed（出厂兜底）。
func serveBuiltin(c *gin.Context, frontendFS fs.FS, templateRoot, filePath string, customStatusCode int) bool {
	if filePath != "" && tryDiskRoot(c, templateRoot, filePath, customStatusCode) {
		return true
	}
	if !fs.ValidPath(filePath) {
		return false
	}
	templateFS, err := fs.Sub(frontendFS, templateRoot)
	if err != nil {
		return false
	}
	file, err := templateFS.Open(filePath)
	if err != nil {
		return false
	}
	return serveFrontendFile(c, filePath, file, customStatusCode)
}

// tryDiskRoot 在受限目录 dirRoot 内查找并返回 filePath（os.Root 把路径限制在 root 内，防 URL 穿越）。
func tryDiskRoot(c *gin.Context, dirRoot, filePath string, code int) bool {
	root, err := os.OpenRoot(dirRoot)
	if err != nil {
		return false
	}
	defer root.Close()
	file, err := root.Open(filePath)
	if err != nil {
		return false
	}
	return serveFrontendFile(c, filePath, file, code)
}

// serveFrontendFile 以 customStatusCode 输出普通文件（目录或不可 Seek 的文件返回 false），并关闭 file。
func serveFrontendFile(c *gin.Context, name string, file fs.File, customStatusCode int) bool {
	defer file.Close()
	fileStat, err := file.Stat()
	if err != nil || fileStat.IsDir() {
		return false
	}
	readSeeker, ok := file.(io.ReadSeeker)
	if !ok {
		return false
	}
	setFrontendCacheHeader(c, name)
	http.ServeContent(utils.NewGinCustomWriter(c, customStatusCode), c.Request, name, fileStat.ModTime(), readSeeker)
	return true
}
