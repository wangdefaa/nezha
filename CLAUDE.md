# CLAUDE.md

哪吒监控面板**后端**（Dashboard）的精简 fork。Go 单体二进制，对外提供监控数据采集（gRPC）+ 管理 REST API + 前端静态托管。已移除上游的 MCP、服务器转移、NAT 穿透、Web 终端、文件管理、DDNS、定时任务(cron task)、命令执行、GPU/温度监控等，只保留「监控 + 拨测 + 告警 + 管理后台」核心域。配套前端见 `service/singleton/frontend-templates.yaml`（产物在构建期下载嵌入，不在本仓库）。

## 常用命令

```bash
# 构建（必须 CGO_ENABLED=0：用纯 Go sqlite 驱动 glebarez，免 C 工具链）
CGO_ENABLED=0 go build ./cmd/dashboard
# 交叉编译示例
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nezha-dashboard ./cmd/dashboard

go test ./...        # 全量测试（测试极多，覆盖鉴权/scope/租户隔离等，改动鉴权务必跑）
go vet ./...
go run ./cmd/dashboard -c data/config.yaml -db data/sqlite.db   # 本地运行

# 拉取前端产物到 cmd/dashboard/{admin,user,...}-dist（//go:embed 依赖其存在）
./script/fetch-frontends.sh   # 需要 curl/unzip/yq
# 仅想编译、无需真实前端：手动建空目录或 touch cmd/dashboard/{admin,user}-dist/a 占位即可

# 生成 swagger 文档（仅 Debug 模式挂载 /swagger；产物 cmd/dashboard/docs 已 gitignore）
swag init --pd -d cmd/dashboard -g main.go -o cmd/dashboard/docs
```

注意：`go build ./cmd/dashboard` 会因 `//go:embed *-dist`（main.go）失败，若对应目录不存在——先跑 fetch-frontends 或建占位目录。proto 已生成（`proto/nezha.pb.go`、`nezha_grpc.pb.go`），常规开发无需重新生成。

## 技术栈与关键依赖

Go 1.26 · Gin（HTTP）· GORM（sqlite/mysql/postgres）· gRPC + HTTP **复用同一端口**（h2c，见 `main.go` 的 `newHTTPandGRPCMux`）· koanf（配置）· appleboy/gin-jwt（JWT）· VictoriaMetrics 内嵌作时序库（`pkg/tsdb`）· robfig/cron（计划任务）· golang.org/x/oauth2 · leonelquinteros/gotext（i18n）· sqids（ID 混淆 `pkg/idcodec`）。

## 目录结构与架构

请求流：`gin 路由 → controller → service/singleton(运行态单例) + model(GORM) → DB`。

- `cmd/dashboard/main.go` — 入口。启动用 `utils.FirstError(...)` 链式短路：加载前端模板 → 读配置(yaml/env) → idcodec → 时区/Cache → 内存上限 → DB(含 AutoMigrate + 动态配置入库) → TSDB → `initSystem`(建默认 admin / LoadSingleton / cron / JWT 会话 GC)。然后 `net.Listen` 单端口，gRPC 与 HTTP 经 mux 复用；优雅退出用 ory/graceful。
- `cmd/dashboard/controller/controller.go` — **路由权威表 `routers()`**。当前保留的 API 域（均在 `/api/v1`）：`server` / `server-group`、`service`(拨测) / `service/.../history`、`notification` / `notification-group`、`alert-rule`、`user`、`waf`、`online-user`、`api-tokens`(PAT)、`oauth2`、`setting`(动态配置读写)、`maintenance`、`ws/server`(WebSocket 推送)、`server/:id/metrics`。改路由必同步看本文件顶部的 scope 注释。
- `cmd/dashboard/controller/waf/` — RealIp + Waf 中间件（全局挂载，先于路由），`waf.html` 封禁页。
- `cmd/dashboard/rpc/` + `service/rpc/` — Agent 用的 gRPC 服务端（上报、心跳、任务下发）。
- `model/` — GORM 模型 + DTO + 大量 `*_test.go`。`config.go` 配置体系；`dynamic_config.go` 动态配置入库；`api_token.go` PAT 与 scope 常量；`waf.go`、`jwt_session.go` 等。
- `service/singleton/` — 运行态单例与共享状态：`singleton.go`(全局变量 + `autoMigrate` + 多驱动 `dbDialector`) ；`config.go`(`Conf` 包装) ；`servicesentinel.go`(拨测) ；`alertsentinel.go`(告警) ；`i18n.go` ；`tsdb.go` ；`theme.go`(主题清单缓存/安装/GitHub 拉取) ；`frontend-templates.yaml`(内嵌, 仅作**内置主题首启 seed 源**)。
- `pkg/` — `tsdb`(时序库)、`geoip`(内嵌 mmdb)、`idcodec`(uid 可逆混淆)、`i18n`、`utils`(含 `http.go` SSRF 防护)、`websocketx`。
- 前端托管：`main.go` 的 `//go:embed *-dist` 把**内置**前端打进二进制；`fallbackToFrontend`(controller.go) 做 SPA fallback：`/dashboard/*` → AdminTemplate，其余 → UserTemplate。
- **访客主题已解耦入库**（`model/theme.go` + `service/singleton/theme.go`）：`themes` 表是运行期权威清单（取代 yaml 直接驱动 `FrontendTemplates`），首启从 `frontend-templates.yaml` seed 内置主题。后台可上传 zip 或从 GitHub release 拉取自定义主题，**文件落盘 `<dataDir>/themes/<path>`，库只存元信息**；`checkLocalFileOrFs` 按主题来源分流：内置走 embed、自定义走磁盘（仍用 `os.Root.Open` 防穿越）。API 见 `controller/theme.go`：`/theme` 列表/`upload`/`github`/`:id/refresh`/`batch-delete`，`/theme/:id/apply` 切换当前访客主题（不走 `PATCH /setting` 以免整表覆盖）。新增主题页路由记得同步 `frontendPageUrlRegistry`。

### 配置：bootstrap vs 动态

- **Bootstrap（仅 yaml/env，不入库）**：端口/监听地址、`database`、`agent_secret_key`、`jwt_secret_key`、时区、`tsdb`、`https`、`memory`、`force_auth`、`debug`。文件默认 `data/config.yaml`（`-c` 覆盖）。所有字段可用环境变量覆盖：**前缀 `NZ_`、下划线分层**（如 `NZ_LISTENPORT`、`NZ_DATABASE_TYPE`、`NZ_JWTSECRETKEY`）。空缺的 `jwt_secret_key`/`agent_secret_key` 首启自动生成并写回 yaml。
- **动态（入库 `setting_stores` 单行 id=1）**：站点名、语言、自定义代码、OAuth2、Agent 安装脚本地址、真实 IP 头、IP 变更提醒等（见 `model/dynamic_config.go::DynamicConfig`）。首启把 yaml 现值迁移入库，之后经后台 `PATCH /setting` 在线改。运行时统一从 `singleton.Conf` 读。

## 数据库

- 多驱动：`database.type` = `sqlite`(默认, glebarez 纯 Go) / `mysql` / `postgres`，驱动选择见 `service/singleton/singleton.go::dbDialector`；sqlite 的 DSN 为空时回退到 `-db` 路径。
- `autoMigrate()`（同文件）显式列出所有表，新增模型必须加进去。
- 跨库兼容注意：维护操作里 `VACUUM` 等只对 sqlite 跑（`PerformMaintenance` 用 `DB.Dialector.Name()` 判别）；写 SQL 时避免 sqlite/mysql/pg 方言差异，优先用 GORM API。

## 鉴权与安全模型

入口在 `controller/controller.go::routers()`，鉴权代码主要在 `jwt.go` / `api_token.go` / `api_token_scope.go` / `csrf.go` / `oauth2.go`。

- **JWT（浏览器/UI）**：cookie `nz-jwt`，HS256 锁定算法防 `alg:none`；会话落库 `jwt_sessions`（`key_id` 索引），校验 UA 哈希、`token_version`、过期、撤销，以及 **IP 一致性**——签发与校验都走 `jwt.go::clientIP()`（优先真实 IP 头，否则对端地址），两端取值必须一致否则误判 mismatch。
- **PAT（程序化）**：`Authorization: Bearer nzp_<secret>`，模型 `model/api_token.go`，哈希存储。双层闸：①复用用户级 `HasPermission`/`Server.HasPermission`；②`Scopes`(`nezha:{resource}:{verb}`，支持 `nezha:server:*`、`nezha:*` 通配) + 可选 `ServerIDs` 白名单。每路由用 `restScopeMiddleware(scope)` 收口，**空 scope = fail-closed 直接 403**（新增路由忘填 scope 不会静默放行）。`nezha:*` / `nezha:admin:*` 为 admin-only。
- **自我管理端点禁 PAT**：`/profile`、`/api-tokens`、`/refresh-token`、oauth2 解绑挂 `restPATForbiddenMiddleware`，杜绝 PAT 自提权链。
- **CSRF 双提交**（`csrf.go`）：仅作用于 cookie-JWT 的非安全方法。token 为 `nonce.HMAC-SHA256(JWTSecretKey)`（签名防 sibling 子域 cookie 注入），cookie `nz-csrf`(JS 可读, SameSite=Strict) 必须等于 `X-CSRF-Token` 头。**PAT 请求与安全方法自动豁免**。login/refresh/oauth2 回调负责种 cookie。
- **WAF**（全局中间件 + `model/waf.go`）：登录失败、坏 token、OAuth2 爆破等触发 `BlockIP`；命中 IP 返回封禁页。依赖真实 IP 头配置正确。
- **SSRF 防护**（`pkg/utils/http.go`）：对**用户可控 URL**（通知 webhook 等）必须用 `NewRestrictedHTTPClient` / `ResolveAllowedHTTPURL`——它解析并拒绝内网/保留网段、并把出站连接 pin 到已校验 IP 防 DNS rebinding。**不要**用 `utils.HttpClient` / `HttpClientSkipTlsVerify` 发用户 URL。

## 重要约定与坑

- **前端路由白名单 `frontendPageUrlRegistry`**（controller.go 内 `fallbackToFrontend`）：决定哪些 URL 直接刷新返回 200 + index.html。新增前端页面路由必须**同时**在此处和前端 `main.tsx` 加一条，否则刷新该页 HTTP 层 404（SPA 内看似正常，但站点监控/链接预览会判站点挂了）。
- **JWT/真实 IP 一致性**：见上，签发与校验共用 `clientIP()`。反代部署务必正确配 `web_real_ip_header`（动态配置），否则在线用户、WAF、IP mismatch 全失真。
- `force_auth=false` 时部分 optional 路由可匿名访问，但 PAT 仍会被解析并按 scope 收口（`patOrFallbackAuthMiddleware`，避免 PAT 被当 guest 使 scope 失效）。
- OAuth2 回调地址优先用动态配置 `DashboardHost`（防 Host 头伪造改写回调，GHSA-9rc6-8cjv-rcvx），与 Agent 连接用的 `InstallHost` 解耦。
- 默认管理员：库中无用户时首启创建 `admin` / `admin`（`main.go::initSystem`），登录后应改密。
- gRPC 与 HTTP 同端口（默认 8008），Agent 也连此端口；判别靠 `Content-Type: application/grpc` + path 前缀。
- 上游 docs/01-05 是精简**前**快照（本仓库已无 docs 目录），勿当现状参考；以代码与本文件为准。
- 全局私有指令：回答用中文，函数尽量 ≤30 行，commit message 用中文 ≤50 字。
