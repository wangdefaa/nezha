# AGENTS.md

哪吒监控后端（Dashboard）的精简 fork：Go 单体二进制，gRPC 采集、管理 REST API 与内嵌前端共用一个端口（默认 8008）。已删除上游的 MCP、服务器转移、NAT、Web 终端、文件管理、DDNS、定时任务、命令执行、GPU/温度，只保留监控、拨测、告警和管理后台。

## 命令

```bash
./script/fetch-frontends.sh        # 拉前端产物到 cmd/dashboard/*-dist；//go:embed 依赖它，只编译时可建空目录占位
go vet ./... && gofmt -l . && go test -race -shuffle=on -count=1 ./...   # 与 CI 一致，gofmt 有输出即失败
CGO_ENABLED=0 go build ./cmd/dashboard                                   # 必须 CGO=0（纯 Go sqlite 驱动）
go run ./cmd/dashboard -c data/config.yaml -db data/sqlite.db
swag init --pd -d cmd/dashboard -g main.go -o cmd/dashboard/docs         # 可选，只在 debug 模式挂 /swagger
```

- 本地构建的 `pkg/geoip/geoip.db` 是占位文件，版本号为 `debug`；真实 GeoIP 库只在发版时下载。
- 改 proto：`protoc --go-grpc_out="require_unimplemented_servers=false:." --go_out="." proto/*.proto`，并同步 nezha-agent 的 `proto/`。

## CI 与发版

- `test.yml`：push、PR 和发版都会跑。依次是 tidy、gofmt、vet、race 测试和 CGO=0 构建，另外跑 gosec 与 govulncheck（误报用 `#nosec 规则 -- 理由` 标注）。
- `release.yml`：推 `v*` tag → 先过 test → 嵌入前端与 IPInfo 库（secret `IPINFO_TOKEN`）→ 发布 `dashboard-linux-{amd64,arm64}.zip` 与 checksums。同名 release 已存在就失败，不覆盖资产。同一批二进制再打成多架构镜像推到 `ghcr.io/wangdefaa/nezha`；预发布版本不更新 latest。手动触发只构建二进制并试构建镜像，不发版也不推送，发版前可以先用它验证。
- Go 版本由 `go.mod` 的 `toolchain` 决定。

## 架构要点

请求流：gin 路由 → controller → `service/singleton`（运行态单例）+ `model`（GORM）。

- 路由以 `controller.go::routers()` 为准；改路由要同步 `scope_doc.go` 和 `scope_doc_consistency_test.go::canonicalRoutes()`。
- 新增前端页面时，`frontendPageUrlRegistry`（controller.go）和前端 `main.tsx` 要同时加，否则直接刷新会 404。`/dashboard/*` 固定使用内置的 admin-dist；访客主题存 `themes` 表，文件落盘在 `<dataDir>/themes/`。
- 配置分两层：
  - bootstrap：端口、数据库、密钥、tsdb、https 等，只来自 yaml 或 env（`NZ_` 前缀，下划线表示层级）。
  - 动态配置：站点、OAuth2、安装脚本、真实 IP 头、IP 变更提醒等，存 `setting_stores` 表的单行，运行时从 `singleton.Conf` 读。
  - 不要把整个 Config 序列化写回 yaml：`JWTSecretKey` 带 `json:"-"`，会被丢掉。写回用 `patchYAMLField`。
- 数据库支持 sqlite（默认）、mysql、postgres。新模型要加进 `singleton.go::autoMigrate()`；优先用 GORM API，方言专属操作按 `DB.Dialector.Name()` 分支。
- 删除服务器或通知组要走 `singleton.DeleteServers` / `DeleteNotificationGroups`：它们在同一个事务里清掉告警规则、拨测和 IP 变更提醒里的引用（见 `refs.go`）。新增引用服务器或通知组 ID 的字段时，要一起处理。API 令牌的白名单故意不清，因为空白名单表示不限制。

## 鉴权与安全

- **JWT**：cookie `nz-jwt` 是 HttpOnly，会话存 `jwt_sessions`。校验 UA 哈希、token_version、撤销状态和 IP 一致性；签发和校验共用 `jwt.go::clientIP()`。登录只接受 JSON，登出必须调 `POST /logout`。
- **PAT**：`Bearer nzp_…`。先过用户级权限，再过 scope（`nezha:{resource}:{verb}`）和可选的服务器白名单。每个路由都用 `restScopeMiddleware` 收口，scope 为空直接返回 403；`force_auth=false` 时允许匿名访问的路由，带了 PAT 仍按 scope 限制。`/profile`、`/api-tokens`、`/logout` 等自我管理端点禁止用 PAT。
- **CSRF**：cookie `nz-csrf` 必须和 `X-CSRF-Token` 头一致，只对 cookie 会话的非安全方法生效。
- **请求体**：默认上限 1 MiB（`body_limit.go`），主题上传单独放行。
- **WAF**：依赖真实 IP。`web_real_ip_header` 留空时只封禁公网对端；部署在 CDN 或反代后面时必须配置。
- **不可信输入**：
  - agent 上报的浮点值要经 `model.FiniteOrZero`，IP 要经 `netip` 校验；日志不要整条打印上报数据。
  - 访问用户可控的 URL（webhook、主题下载）必须用 `pkg/utils/http.go` 的 `NewRestrictedHTTPClient`，防 SSRF 和 DNS rebinding。
  - 通知发送失败时，错误要经 `redactURLError` 去掉 URL，因为 URL 里常带 bot token。
- **OAuth2**：取不到用户 ID 一律报错，回调地址优先用 `DashboardHost`。
- **初始管理员**：首次启动时库里没有用户，就创建 `admin`。密码取 `NZ_INIT_ADMIN_PASSWORD`，没设置就随机生成，只在日志里打印一次。

## 约定

- 注释和回答用中文；函数不超过 30 行；commit message 用中文，不超过 50 字。
- 上游 docs 是精简前的快照，以代码和本文件为准。
