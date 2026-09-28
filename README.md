<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/logo-dark.svg">
    <img src=".github/assets/logo.svg" width="96" alt="哪吒监控 Logo">
  </picture>
</p>

<h1 align="center">哪吒监控 Dashboard(精简版)</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License: Apache 2.0"></a>
</p>

基于 [哪吒监控](https://github.com/nezhahq/nezha) 改造的轻量后端,聚焦「服务器监控 + 管理后台」核心域。Go 单体二进制,gRPC 数据采集与管理 REST API 复用同一端口(h2c)。

## 特性

- **精简内核** —— 移除 MCP、NAT 穿透、Web 终端、文件管理、DDNS、命令执行等,只保留监控 / 拨测 / 告警 / 管理。
- **多数据库** —— SQLite(纯 Go 驱动,`CGO_ENABLED=0` 免 C 工具链)、MySQL、PostgreSQL。
- **配置入库** —— 站点、通知、OAuth2、安装脚本等动态配置存数据库,后台在线修改;`config.yaml` 只保留端口、数据库、密钥等引导项。
- **主题在线管理** —— 访客前台主题可在后台「主题管理」上传 zip、拖拽或从 GitHub release 拉取,并一键切换,无需重新发版;管理后台固定使用内置版本,随面板发版升级。

## 快速开始

Docker(推荐):

```bash
mkdir -p nezha && cd nezha
curl -fsSLO https://raw.githubusercontent.com/wangdefaa/nezha/master/docker-compose.yaml
docker compose up -d
docker compose logs dashboard | grep 随机密码   # 首次启动生成的 admin 密码
```

镜像 `ghcr.io/wangdefaa/nezha` 提供 `linux/amd64` 与 `linux/arm64`,`latest` 跟随最新正式版;配置、数据库与时序数据都在 `./data`。想自己指定初始密码,在 compose 里设置 `NZ_INIT_ADMIN_PASSWORD`;改用 PostgreSQL 见 compose 文件里的注释。

二进制:从 [Releases](https://github.com/wangdefaa/nezha/releases/latest) 下载对应架构的 `dashboard-linux-{amd64,arm64}.zip`(可用 `checksums.txt` 校验),解压后运行:

```bash
mkdir -p data && cp config.yaml.example data/config.yaml   # 按需修改
./dashboard-linux-amd64 -c data/config.yaml -db data/sqlite.db
```

长期运行建议交给 systemd 托管。

源码构建:

```bash
./script/fetch-frontends.sh   # 拉取前端产物到 cmd/dashboard/*-dist(//go:embed 依赖)
go install github.com/swaggo/swag/cmd/swag@v1.16.6
swag init --pd -d cmd/dashboard -g main.go -o cmd/dashboard/docs
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -tags go_json -ldflags="-s -w" -o nezha-dashboard ./cmd/dashboard
```

源码构建时 `pkg/geoip/geoip.db` 只是占位文件,服务器不显示国家/地区;Release 产物内置了 IPInfo 数据库。

浏览器访问 `http://<服务器IP>:8008/dashboard`。首次启动会创建管理员 `admin`,密码取环境变量 `NZ_INIT_ADMIN_PASSWORD`,未设置时随机生成并打印在启动日志中(仅显示这一次,登录后请修改)。配置项见 [config.yaml.example](config.yaml.example),均可用 `NZ_` 前缀环境变量覆盖。

## 相关

- 管理前端:[wangdefaa/nezha-admin-dash](https://github.com/wangdefaa/nezha-admin-dash)
- 上游项目:[nezhahq/nezha](https://github.com/nezhahq/nezha)
- 许可证:[Apache-2.0](LICENSE)
