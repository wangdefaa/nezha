# 哪吒监控 Dashboard(精简改造版)

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

本项目基于 [哪吒监控 nezhahq/nezha](https://github.com/nezhahq/nezha) 改造,聚焦「服务器监控 + 管理后台」这一核心,做了三件事:

1. **大幅精简**——移除 MCP、服务器转移、NAT 穿透、Web 终端、文件管理、DDNS、定时任务、命令执行、GPU/温度监控等非核心功能,只保留监控与告警必需的管理域。
2. **多数据库支持**——在 SQLite 之外适配 MySQL 与 PostgreSQL;并改用纯 Go 的 SQLite 驱动([glebarez](https://github.com/glebarez/sqlite)),全程 `CGO_ENABLED=0` 交叉编译,免 C 工具链。
3. **配置入库**——站点、Agent 连接、通知、OAuth2、安装脚本地址等动态配置迁移进数据库,可在管理后台「系统设置」在线修改;`config.yaml` 只保留端口、数据库、密钥等引导项。

搭配全新管理前端 [nezha-admin-frontend-v2](https://github.com/wangdefaa/nezha-admin-frontend-v2) 使用。

## 保留的功能

- **服务器监控**:系统状态、负载、网络、磁盘
- **服务拨测**:HTTP(含 SSL 证书到期提醒)、TCP、Ping
- **告警通知**:告警规则、通知方式、通知组
- **管理域**:用户、WAF 防火墙、在线用户、API Token(PAT)、OAuth2 登录
- **多前端主题**:用户前台 + 管理后台,模板可在 `service/singleton/frontend-templates.yaml` 配置

## 技术栈

Go · Gin · GORM · gRPC(与 HTTP 复用端口,h2c)· koanf 配置

## 部署

### Docker Compose(推荐)

```bash
git clone https://github.com/wangdefaa/nezha.git && cd nezha
mkdir -p data && cp config.yaml.example data/config.yaml   # 按需修改
docker compose up -d
```

浏览器访问 `http://<服务器IP>:8008/dashboard`,首个管理员账号见首次启动日志。

### 源码构建

```bash
# 1. 拉取前端产物(写入 cmd/dashboard/*-dist,由 //go:embed 打包)
./script/fetch-frontends.sh
# 2. 交叉编译(示例 linux/amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nezha-dashboard ./cmd/dashboard
./nezha-dashboard
```

完整配置项见 [config.yaml.example](config.yaml.example);所有字段均可用环境变量覆盖(前缀 `NZ_`)。

## 数据库

| 类型 | 说明 |
|---|---|
| `sqlite` | 默认,纯 Go 实现、免依赖,适合单机 |
| `mysql` | `database.type: mysql` + 标准 DSN |
| `postgres` | `database.type: postgres` + 标准 DSN |

## 上游与许可

- 基础项目:[哪吒监控 nezhahq/nezha](https://github.com/nezhahq/nezha)
- 用户前台主题:[hamster1963/nezha-dash](https://github.com/hamster1963/nezha-dash)
- 许可证:[Apache-2.0](LICENSE)
