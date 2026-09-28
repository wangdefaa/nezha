package controller

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	apiTokenSecretLength     = 32                         // 明文 token 随机部分长度（base62 字符数）
	apiTokenCtxKey           = model.CtxKeyAPIToken       // 与 model 层 HasPermission 读取的键统一，只写一次
	apiTokenLastUsedCtxKey   = "nz_api_token_used_marker" // #nosec G101 -- gin context key name, not a credential
	apiTokenAuthSchemePrefix = "Bearer "
)

// listAPITokens 列出当前用户的所有 PAT（脱敏，不含 token 明文）。
// @Summary List API tokens
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.APITokenView]
// @Router /api-tokens [get]
func listAPITokens(c *gin.Context) ([]model.APITokenView, error) {
	uid := getUid(c)
	var rows []model.APIToken
	if err := singleton.DB.Where("user_id = ?", uid).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	out := make([]model.APITokenView, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToView())
	}
	return out, nil
}

// createAPIToken 创建一个 PAT。明文 token 仅在响应中返回一次。
// @Summary Create API token
// @Tags auth required
// @Accept json
// @Param body body model.APITokenCreateRequest true "request"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.APITokenCreateResponse]
// @Router /api-tokens [post]
func createAPIToken(c *gin.Context) (*model.APITokenCreateResponse, error) {
	var req model.APITokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return nil, err
	}
	if err := validateAPITokenRequest(&req); err != nil {
		return nil, err
	}
	isAdmin := callerIsAdmin(c)
	scopes, err := cleanAPITokenScopes(req.Scopes, isAdmin)
	if err != nil {
		return nil, err
	}
	serverIDs, err := checkAPITokenServers(c, req.ServerIDs, isAdmin)
	if err != nil {
		return nil, err
	}

	plaintext, tok, err := newAPIToken(getUid(c), req.Name, scopes, serverIDs, req.ExpiresInDays)
	if err != nil {
		return nil, err
	}
	if err := singleton.DB.Create(&tok).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	return &model.APITokenCreateResponse{
		ID:        tok.ID,
		Name:      tok.Name,
		Token:     plaintext,
		Scopes:    tok.Scopes(),
		ServerIDs: tok.ServerIDs(),
		ExpiresAt: tok.ExpiresAt,
	}, nil
}

// validateAPITokenRequest 规整名称并校验各字段上限（binding 标签之外的业务约束）。
func validateAPITokenRequest(req *model.APITokenCreateRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		return errors.New("name required")
	case len(req.Name) > 128:
		return errors.New("name too long (max 128 chars)")
	case req.ExpiresInDays < 0:
		return errors.New("expires_in_days must be >= 0")
	case req.ExpiresInDays > 3650:
		return errors.New("expires_in_days too large (max 3650, i.e. 10 years)")
	case len(req.Scopes) > 32:
		return errors.New("too many scopes (max 32)")
	case len(req.ServerIDs) > 1000:
		return errors.New("too many server_ids (max 1000)")
	}
	return nil
}

// cleanAPITokenScopes 去空白、去重并校验 scope 合法；非 admin 不能签发 admin-only scope。
func cleanAPITokenScopes(scopes []string, isAdmin bool) ([]string, error) {
	allowed := append(append([]string{}, model.AllScopes...), model.AdminOnlyScopes...)
	cleaned := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || slices.Contains(cleaned, s) {
			continue
		}
		if !slices.Contains(allowed, s) {
			return nil, errors.New("unknown scope: " + s)
		}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return nil, errors.New("at least one scope required")
	}
	if !isAdmin {
		for _, s := range cleaned {
			if slices.Contains(model.AdminOnlyScopes, s) {
				return nil, errors.New("only admin can issue scope: " + s)
			}
		}
	}
	return cleaned, nil
}

// checkAPITokenServers 去重 server 白名单，并确认每台 server 存在且调用方有权限（admin 免权限检查）。
func checkAPITokenServers(c *gin.Context, ids []uint64, isAdmin bool) ([]uint64, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	deduped := make([]uint64, 0, len(ids))
	for _, sid := range ids {
		if sid == 0 {
			return nil, errors.New("server_id 0 is invalid")
		}
		if slices.Contains(deduped, sid) {
			continue
		}
		deduped = append(deduped, sid)
		server, _ := singleton.ServerShared.Get(sid)
		if server == nil {
			return nil, errors.New("server not found")
		}
		if !isAdmin && !server.HasPermission(c) {
			return nil, errors.New("permission denied on server")
		}
	}
	return deduped, nil
}

// newAPIToken 生成明文 token 并构造待入库的 PAT（库里只存哈希）；expiresInDays 为 0 表示永不过期。
func newAPIToken(uid uint64, name string, scopes []string, serverIDs []uint64, expiresInDays int) (string, model.APIToken, error) {
	secret, err := utils.GenerateRandomString(apiTokenSecretLength)
	if err != nil {
		return "", model.APIToken{}, err
	}
	plaintext := model.APITokenPrefix + secret
	tok := model.APIToken{UserID: uid, Name: name, TokenHash: model.HashAPIToken(plaintext)}
	tok.SetScopes(scopes)
	if len(serverIDs) > 0 {
		tok.SetServerIDs(serverIDs)
	}
	if expiresInDays > 0 {
		exp := time.Now().Add(time.Duration(expiresInDays) * 24 * time.Hour)
		tok.ExpiresAt = &exp
	}
	return plaintext, tok, nil
}

// deleteAPIToken 吊销一个 PAT。
// @Summary Revoke API token
// @Tags auth required
// @Param id path uint true "token id"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /api-tokens/{id} [delete]
func deleteAPIToken(c *gin.Context) (any, error) {
	id, err := paramID(c)
	if err != nil {
		return nil, err
	}
	q := singleton.DB.Where("id = ?", id)
	if !callerIsAdmin(c) {
		q = q.Where("user_id = ?", getUid(c))
	}
	res := q.Delete(&model.APIToken{})
	if res.Error != nil {
		return nil, newGormError("%v", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, errors.New("not found")
	}
	// 同步断开持有该 PAT 的长连接（目前只有 ws/server），否则已删 PAT 会一直推流到连接自然断开。
	patConnectionRegistryShared.revokeToken(id)
	return nil, nil
}

// apiTokenAuthMiddleware 解析 `Authorization: Bearer nzp_xxx`，
// 命中后把 *model.User 挂到 ctx 上，使下游一切 Server.HasPermission/getUid 复用 JWT 路径。
//
// 不命中（无 Authorization 头或前缀不是 nzp_）：放行下一个中间件（例如 JWT）。
// 命中但 token 无效：直接 401 并 abort，不再走到 JWT。
func apiTokenAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader("Authorization"))
		if raw == "" {
			return
		}
		if !strings.HasPrefix(raw, apiTokenAuthSchemePrefix) {
			return
		}
		plaintext := strings.TrimSpace(strings.TrimPrefix(raw, apiTokenAuthSchemePrefix))
		if !strings.HasPrefix(plaintext, model.APITokenPrefix) {
			// 既然有 Bearer 但不是 PAT 前缀，交给后续 JWT 中间件处理
			return
		}

		realIP := c.GetString(model.CtxKeyRealIPStr)

		var tok model.APIToken
		err := singleton.DB.Where("token_hash = ?", model.HashAPIToken(plaintext)).First(&tok).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				blockBruteForceToken(c)
				abortAPITokenUnauthorized(c, "invalid api token")
				return
			}
			abortAPITokenUnauthorized(c, "api token lookup failed")
			return
		}
		now := time.Now()
		if tok.IsExpired(now) {
			blockBruteForceToken(c)
			abortAPITokenUnauthorized(c, "api token expired")
			return
		}

		var user model.User
		if err := singleton.DB.First(&user, tok.UserID).Error; err != nil {
			blockBruteForceToken(c)
			abortAPITokenUnauthorized(c, "owner of api token not found")
			return
		}

		model.UnblockIP(singleton.DB, realIP, model.BlockIDToken)

		c.Set(model.CtxKeyAuthorizedUser, &user)
		c.Set(apiTokenCtxKey, &tok)

		// last_used 同步更新：开销极低（一行 UPDATE），异步路径在
		// 多连接 sqlite 测试场景下会和测试 teardown 形成竞态，并把
		// `last_used_*` 写丢到不可见的 :memory: 实例。生产路径上等价。
		if v, ok := c.Get(apiTokenLastUsedCtxKey); !ok || v != true {
			c.Set(apiTokenLastUsedCtxKey, true)
			_ = singleton.DB.Model(&model.APIToken{}).
				Where("id = ?", tok.ID).
				Updates(map[string]any{
					"last_used_at": now,
					"last_used_ip": realIP,
				}).Error
		}
	}
}

func abortAPITokenUnauthorized(c *gin.Context, reason string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, model.CommonResponse[any]{
		Success: false,
		Error:   "ApiErrorUnauthorized: " + reason,
	})
}

// APITokenFromContext 取当前请求关联的 PAT，未命中（JWT/匿名）返回 nil。
// restScopeMiddleware（闸 2）、csrfMiddleware、PAT 禁用端点等据此区分 PAT 请求。
func APITokenFromContext(c *gin.Context) *model.APIToken {
	v, ok := c.Get(apiTokenCtxKey)
	if !ok {
		return nil
	}
	t, _ := v.(*model.APIToken)
	return t
}
