package model

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Scope 命名规范（唯一一套）：nezha:{resource}:{verb}
//
//   - resource: inventory / server / service / alertrule / cron /
//     notification / notification-group / admin
//   - verb: read / write / delete / exec
//
// `*` 通配在 resource 或 verb 位均可：
//   - nezha:server:* 给定资源的所有动作
//   - nezha:* admin-only 全权
//
// inventory 与 server 已拆开：inventory 管“能看到/能删哪些机器”——`GET /api/v1/server`、
// `/server-group`、batch-delete server/group 都用 nezha:inventory:{read,delete}；
// server 管对已知机器的运行态操作（exec、编辑配置、metrics）。
const (
	ScopeNezhaAll = "nezha:*"

	// inventory 资源域：管理后台对“服务器清单”本身的枚举与删除（列出 GET /server、
	// 删除 batch-delete/server、server-group 的列出/删除，以及 MCP server.list）。
	// 刻意与 nezha:server:* 分开：后者是对已知 server 的运行态操作（exec / 文件读写 /
	// 编辑 / metrics），而 inventory 是“能看到/能删哪些机器”的台账权限。拆开后，
	// 一张只跑命令的 PAT 不必同时具备遍历和删除整个清单的能力。
	ScopeInventoryRead   = "nezha:inventory:read"
	ScopeInventoryDelete = "nezha:inventory:delete"

	ScopeServerRead   = "nezha:server:read"
	ScopeServerWrite  = "nezha:server:write"
	ScopeServerDelete = "nezha:server:delete"
	ScopeServerExec   = "nezha:server:exec"

	ScopeServiceRead   = "nezha:service:read"
	ScopeServiceWrite  = "nezha:service:write"
	ScopeServiceDelete = "nezha:service:delete"

	ScopeAlertRuleRead   = "nezha:alertrule:read"
	ScopeAlertRuleWrite  = "nezha:alertrule:write"
	ScopeAlertRuleDelete = "nezha:alertrule:delete"

	ScopeNotificationRead   = "nezha:notification:read"
	ScopeNotificationWrite  = "nezha:notification:write"
	ScopeNotificationDelete = "nezha:notification:delete"

	ScopeNotificationGroupRead   = "nezha:notification-group:read"
	ScopeNotificationGroupWrite  = "nezha:notification-group:write" // #nosec G101 -- scope identifier, not a credential
	ScopeNotificationGroupDelete = "nezha:notification-group:delete"

	ScopeAdminAll = "nezha:admin:*"
)

var AllScopes = []string{
	ScopeInventoryRead, ScopeInventoryDelete,
	ScopeServerRead, ScopeServerWrite, ScopeServerDelete, ScopeServerExec,
	ScopeServiceRead, ScopeServiceWrite, ScopeServiceDelete,
	ScopeAlertRuleRead, ScopeAlertRuleWrite, ScopeAlertRuleDelete,
	ScopeNotificationRead, ScopeNotificationWrite, ScopeNotificationDelete,
	ScopeNotificationGroupRead, ScopeNotificationGroupWrite, ScopeNotificationGroupDelete,

	"nezha:inventory:*",
	"nezha:server:*",
	"nezha:service:*",
	"nezha:alertrule:*",
	"nezha:notification:*",
	"nezha:notification-group:*",
}

var AdminOnlyScopes = []string{ScopeNezhaAll, ScopeAdminAll}

// APITokenPrefix 是明文 token 的人类可识别前缀。`nzp_` = nezha personal access token。
const APITokenPrefix = "nzp_"

// APIToken 是用户用于程序化访问的长期凭证。
// 双层鉴权：闸 1 用 UserID 复用 Server.HasPermission；闸 2 用 Scopes / ServerIDs。
type APIToken struct {
	ID         uint64     `gorm:"primaryKey" json:"id,omitempty"`
	UserID     uint64     `gorm:"index" json:"user_id,omitempty"`
	Name       string     `gorm:"type:varchar(128)" json:"name,omitempty"`
	TokenHash  string     `gorm:"uniqueIndex;type:char(64)" json:"-"`
	ScopesCSV  string     `gorm:"type:text" json:"-"`
	ServersCSV string     `gorm:"type:text" json:"-"`
	ExpiresAt  *time.Time `gorm:"index" json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `gorm:"type:varchar(64)" json:"last_used_ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime" json:"updated_at,omitempty"`
}

func (APIToken) TableName() string {
	return "api_tokens"
}

// HashAPIToken 计算明文 token 的存储哈希。
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Scopes 解码逗号分隔的 scope 列表。
func (t *APIToken) Scopes() []string {
	if t.ScopesCSV == "" {
		return nil
	}
	parts := strings.Split(t.ScopesCSV, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SetScopes 编码 scope 列表为 CSV。
func (t *APIToken) SetScopes(scopes []string) {
	t.ScopesCSV = strings.Join(scopes, ",")
}

// ServerIDs 解码服务器 ID 白名单。空切片 = 不限制（继承用户原有权限）。
func (t *APIToken) ServerIDs() []uint64 {
	if t.ServersCSV == "" {
		return nil
	}
	parts := strings.Split(t.ServersCSV, ",")
	out := make([]uint64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var id uint64
		for _, c := range p {
			if c < '0' || c > '9' {
				id = 0
				break
			}
			id = id*10 + uint64(c-'0')
		}
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// SetServerIDs 编码服务器 ID 白名单。
func (t *APIToken) SetServerIDs(ids []uint64) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, formatUint(id))
	}
	t.ServersCSV = strings.Join(parts, ",")
}

// HasScope 判定 token 是否携带某个 scope。
//
// 匹配规则：
//   - nezha:* 覆盖整个 nezha 命名空间
//   - 资源级通配：nezha:server:* 匹配所有 nezha:server:read/write/delete/exec
//   - 精确匹配
func (t *APIToken) HasScope(scope string) bool {
	for _, s := range t.Scopes() {
		if scopeMatches(s, scope) {
			return true
		}
	}
	return false
}

// scopeMatches 判定 owned scope 是否覆盖 wanted scope。
func scopeMatches(owned, wanted string) bool {
	if owned == wanted {
		return true
	}
	if owned == ScopeNezhaAll {
		return strings.HasPrefix(wanted, "nezha:")
	}
	if strings.HasSuffix(owned, ":*") {
		prefix := strings.TrimSuffix(owned, ":*")
		return strings.HasPrefix(wanted, prefix+":") || wanted == prefix
	}
	return false
}

// CanAccessServer 判定 token 是否被允许操作某 server（白名单层；
// 仍需上层调用 Server.HasPermission 做用户级权限校验）。
func (t *APIToken) CanAccessServer(serverID uint64) bool {
	ids := t.ServerIDs()
	if len(ids) == 0 {
		return true
	}
	return slices.Contains(ids, serverID)
}

// IsExpired 判定 token 是否已过期。ExpiresAt 为 nil 表示永不过期。
func (t *APIToken) IsExpired(now time.Time) bool {
	return t.ExpiresAt != nil && now.After(*t.ExpiresAt)
}

// BeforeCreate 在写入前强校验 TokenHash 必填，避免空哈希撞键。
func (t *APIToken) BeforeCreate(tx *gorm.DB) error {
	if t.TokenHash == "" {
		return gorm.ErrInvalidData
	}
	return nil
}

// formatUint —— 小工具，避免引入 strconv。
func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// APITokenCreateRequest 是创建 PAT 接口的入参。
type APITokenCreateRequest struct {
	Name          string   `json:"name" binding:"required,max=128"`
	Scopes        []string `json:"scopes" binding:"required,min=1,dive,max=64"`
	ServerIDs     []uint64 `json:"server_ids,omitempty"`
	ExpiresInDays int      `json:"expires_in_days,omitempty"` // 0 = 永不过期
}

// APITokenCreateResponse 创建 PAT 接口的出参；明文 token 仅在此刻返回一次。
type APITokenCreateResponse struct {
	ID        uint64     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"`
	Scopes    []string   `json:"scopes"`
	ServerIDs []uint64   `json:"server_ids,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// APITokenView 是 PAT 列表展示用的脱敏视图。
type APITokenView struct {
	ID         uint64     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ServerIDs  []uint64   `json:"server_ids,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ToView 把数据库实体转为列表脱敏视图。
func (t *APIToken) ToView() APITokenView {
	return APITokenView{
		ID:         t.ID,
		Name:       t.Name,
		Scopes:     t.Scopes(),
		ServerIDs:  t.ServerIDs(),
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
		LastUsedIP: t.LastUsedIP,
		CreatedAt:  t.CreatedAt,
	}
}
