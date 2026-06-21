// Package controller — scope reference table for REST.
//
// Each REST endpoint under /api/v1/* requires a specific scope when
// authenticated via PAT (`Authorization: Bearer nzp_*`).
// JWT-authenticated requests skip scope enforcement.
//
// This file is the authoritative human + LLM-readable index. The actual
// enforcement lives in controller.go (REST). When you change an endpoint's
// scope requirement, update this table.
//
// # Scope naming
//
//	nezha:{resource}:{verb}
//	  resource: inventory | server | service | alertrule |
//	            notification | notification-group | admin
//	  verb:     read | write | delete | exec
//
//	inventory vs server：inventory 管“能看到/能删哪些机器”（列出 server /
//	server-group、删除 server / server-group）；server 管对已知机器的运行态
//	操作（exec / 编辑配置 / metrics）。
//
//	nezha:*               Admin-only superuser
//	nezha:admin:*         Admin-only user/waf/setting/online-user management
//	nezha:<res>:*         All actions on a resource
//
// # REST endpoints (PAT required scope)
//
//	GET    /api/v1/server                            nezha:inventory:read
//	PATCH  /api/v1/server/{id}                       nezha:server:write
//	POST   /api/v1/batch-delete/server               nezha:inventory:delete
//	POST   /api/v1/force-update/server               nezha:server:write
//	POST   /api/v1/server-group                      nezha:server:write
//	PATCH  /api/v1/server-group/{id}                 nezha:server:write
//	POST   /api/v1/batch-delete/server-group         nezha:inventory:delete
//	GET    /api/v1/ws/server                         nezha:inventory:read
//	GET    /api/v1/server-group                      nezha:inventory:read
//	GET    /api/v1/service                           nezha:service:read
//	GET    /api/v1/service/server                    nezha:service:read
//	GET    /api/v1/service/{id}/history              nezha:service:read
//	GET    /api/v1/server/{id}/service               nezha:service:read
//	GET    /api/v1/server/{id}/metrics               nezha:server:read
//
//	GET    /api/v1/service/list                      nezha:service:read
//	POST   /api/v1/service                           nezha:service:write
//	PATCH  /api/v1/service/{id}                      nezha:service:write
//	POST   /api/v1/batch-delete/service              nezha:service:delete
//
//	GET    /api/v1/alert-rule                        nezha:alertrule:read
//	POST   /api/v1/alert-rule                        nezha:alertrule:write
//	PATCH  /api/v1/alert-rule/{id}                   nezha:alertrule:write
//	POST   /api/v1/batch-delete/alert-rule           nezha:alertrule:delete
//
//	GET    /api/v1/notification                      nezha:notification:read
//	POST   /api/v1/notification                      nezha:notification:write
//	PATCH  /api/v1/notification/{id}                 nezha:notification:write
//	POST   /api/v1/batch-delete/notification         nezha:notification:delete
//
//	GET    /api/v1/notification-group                nezha:notification-group:read
//	POST   /api/v1/notification-group                nezha:notification-group:write
//	PATCH  /api/v1/notification-group/{id}           nezha:notification-group:write
//	POST   /api/v1/batch-delete/notification-group   nezha:notification-group:delete
//
//	GET    /api/v1/user                              nezha:admin:*
//	POST   /api/v1/user                              nezha:admin:*
//	POST   /api/v1/batch-delete/user                 nezha:admin:*
//	GET    /api/v1/waf                               nezha:admin:*
//	POST   /api/v1/batch-delete/waf                  nezha:admin:*
//	GET    /api/v1/online-user                       nezha:admin:*
//	POST   /api/v1/online-user/batch-block           nezha:admin:*
//	PATCH  /api/v1/setting                           nezha:admin:*
//	POST   /api/v1/maintenance                       nezha:admin:*
//
// # Endpoints permanently forbidden to PAT
//
// These are personal-account-management endpoints; a PAT must never call them
// (would allow self-elevation chains: PAT → mint stronger PAT → ...).
// `restPATForbiddenMiddleware` returns 403 to PAT-authenticated requests.
//
//	POST   /api/v1/refresh-token
//	GET    /api/v1/profile
//	POST   /api/v1/profile
//	POST   /api/v1/oauth2/{provider}/unbind
//	GET    /api/v1/api-tokens
//	POST   /api/v1/api-tokens
//	DELETE /api/v1/api-tokens/{id}
package controller
