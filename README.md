# 哪吒监控 Dashboard(精简版)

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

基于 [哪吒监控](https://github.com/nezhahq/nezha) 改造的轻量后端,聚焦「服务器监控 + 管理后台」核心域。Go 单体二进制,gRPC 数据采集与管理 REST API 复用同一端口(h2c)。

## 特性

- **精简内核** —— 移除 MCP、NAT 穿透、Web 终端、文件管理、DDNS、命令执行等,只保留监控 / 拨测 / 告警 / 管理。
- **多数据库** —— SQLite(纯 Go 驱动,`CGO_ENABLED=0` 免 C 工具链)、MySQL、PostgreSQL。
- **配置入库** —— 站点、通知、OAuth2、安装脚本等动态配置存数据库,后台在线修改;`config.yaml` 只保留端口、数据库、密钥等引导项。
- **主题在线管理** —— 访客前台与管理后台主题均可在后台「主题管理」上传 zip、拖拽或从 GitHub release 拉取,并一键切换,无需重新发版。

## 快速开始

Docker Compose:

```bash
git clone https://github.com/wangdefaa/nezha.git && cd nezha
mkdir -p data && cp config.yaml.example data/config.yaml   # 按需修改
docker compose up -d
```

源码构建:

```bash
./script/fetch-frontends.sh   # 拉取前端产物到 cmd/dashboard/*-dist(//go:embed 依赖)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nezha-dashboard ./cmd/dashboard
```

浏览器访问 `http://<服务器IP>:8008/dashboard`,默认管理员 `admin` / `admin`(登录后请改密)。配置项见 [config.yaml.example](config.yaml.example),均可用 `NZ_` 前缀环境变量覆盖。

## 相关

- 管理前端:[wangdefaa/nezha-admin-dash](https://github.com/wangdefaa/nezha-admin-dash)
- 上游项目:[nezhahq/nezha](https://github.com/nezhahq/nezha)
- 许可证:[Apache-2.0](LICENSE)
