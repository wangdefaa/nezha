package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/goccy/go-json"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	jwtClaimUserID = "uid"
	jwtClaimKeyID  = "keyId"
	jwtKeyIDBytes  = 32
)

func uaHash(c *gin.Context) string {
	sum := sha256.Sum256([]byte(c.Request.UserAgent()))
	return hex.EncodeToString(sum[:])
}

// clientIP 取请求来源 IP:优先 RealIp 中间件解析出的真实 IP,
// 未配置 web_real_ip_header 时回退到对端地址。签发会话与校验会话
// 必须共用本函数,两端 IP 取值一致才不会误判 IP mismatch。
func clientIP(c *gin.Context) string {
	if ip := c.GetString(model.CtxKeyRealIPStr); ip != "" {
		return ip
	}
	return c.RemoteIP()
}

func issueJWTSession(c *gin.Context, user *model.User, jwtTimeoutHours int) (map[string]interface{}, error) {
	keyID, err := utils.GenerateRandomString(jwtKeyIDBytes)
	if err != nil {
		return nil, err
	}
	// encodedUID is reversible Sqids obfuscation keyed by JWTSecretKey, NOT
	// a one-way hash. It exists to defeat enumeration on the wire, not to
	// keep the uid confidential — see L2 note in idcodec docs.
	encodedUID, err := idcodec.Encode(user.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ip := clientIP(c)
	sess := model.JWTSession{
		KeyID:        keyID,
		UserID:       user.ID,
		IP:           ip,
		UAHash:       uaHash(c),
		TokenVersion: user.TokenVersion,
		ExpiresAt:    now.Add(time.Hour * time.Duration(jwtTimeoutHours)),
		CreatedAt:    now,
		LastUsedAt:   now,
	}
	if err := singleton.DB.Create(&sess).Error; err != nil {
		return nil, err
	}
	return map[string]interface{}{
		jwtClaimUserID: encodedUID,
		jwtClaimKeyID:  keyID,
	}, nil
}

func initParams() *jwt.GinJWTMiddleware {
	return &jwt.GinJWTMiddleware{
		Realm:      singleton.Conf.SiteName,
		Key:        []byte(singleton.Conf.JWTSecretKey),
		CookieName: "nz-jwt",
		SendCookie: true,
		// Pin the signing algorithm so a future library default change (or an
		// `alg: none` confusion attempt) cannot weaken token validation.
		SigningAlgorithm: "HS256",
		// Lax keeps OAuth callback redirects (top-level GET navigations from
		// the provider domain) working while blocking cross-site POST CSRF.
		// HttpOnly：脚本读不到会话 token；前端登出走 POST /logout，访客主题的
		// `!!document.cookie` 登录态判断靠同时下发、JS 可读的 nz-csrf 维持。
		// Secure 仍保持默认：不少部署在上游反代终止 TLS。
		CookieSameSite: http.SameSiteLaxMode,
		CookieHTTPOnly: true,
		Timeout:        time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		MaxRefresh:     time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		IdentityKey:    model.CtxKeyAuthorizedUser,
		PayloadFunc:    payloadFunc(),

		IdentityHandler: identityHandler(),
		Authenticator:   authenticator(),
		Authorizator:    authorizator(),
		Unauthorized:    unauthorized(),
		// query: token still accepted because the WebSocket browser API
		// cannot set Authorization headers; removing it would break the
		// /ws/* routes until the frontend migrates to cookie auth.
		TokenLookup:   "header: Authorization, query: token, cookie: nz-jwt",
		TokenHeadName: "Bearer",
		TimeFunc:      time.Now,

		LoginResponse: func(c *gin.Context, _ int, token string, expire time.Time) {
			writeLoginResponse(c, token, expire)
		},
		RefreshResponse: refreshResponse,
	}
}

// writeLoginResponse 种 CSRF cookie 并返回 token；登录与刷新 token 共用。
func writeLoginResponse(c *gin.Context, token string, expire time.Time) {
	setCSRFCookie(c)
	c.JSON(http.StatusOK, model.CommonResponse[model.LoginResponse]{
		Success: true,
		Data:    model.LoginResponse{Token: token, Expire: expire.Format(time.RFC3339)},
	})
}

func payloadFunc() func(data any) jwt.MapClaims {
	return func(data any) jwt.MapClaims {
		if v, ok := data.(map[string]interface{}); ok {
			return v
		}
		return jwt.MapClaims{}
	}
}

func identityHandler() func(c *gin.Context) any {
	return func(c *gin.Context) any {
		sess, ok := sessionFromClaims(c, jwt.ExtractClaims(c))
		if !ok {
			return nil
		}
		user, ok := sessionUser(c, sess)
		if !ok {
			return nil
		}
		_ = singleton.DB.Model(&model.JWTSession{}).
			Where("key_id = ?", sess.KeyID).
			Update("last_used_at", time.Now()).Error

		c.Set(jwtClaimKeyID, sess.KeyID)
		return user
	}
}

// sessionFromClaims 按 claims 取回有效会话：未吊销、未过期，且 claim 中的 uid 与会话一致
// （uid 解不开或不一致说明 token 被篡改，计入 WAF）。
func sessionFromClaims(c *gin.Context, claims jwt.MapClaims) (*model.JWTSession, bool) {
	keyID, _ := claims[jwtClaimKeyID].(string)
	encodedUID, _ := claims[jwtClaimUserID].(string)
	if keyID == "" || encodedUID == "" {
		return nil, false
	}
	claimUID, err := idcodec.Decode(encodedUID)
	if err != nil {
		blockBruteForceToken(c)
		return nil, false
	}
	var sess model.JWTSession
	if err := singleton.DB.First(&sess, "key_id = ?", keyID).Error; err != nil {
		return nil, false
	}
	if sess.RevokedAt != nil || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	if claimUID != sess.UserID {
		blockBruteForceToken(c)
		return nil, false
	}
	return &sess, true
}

// sessionUser 校验会话绑定的客户端环境（IP、UA）并确认用户 token_version 未变。
func sessionUser(c *gin.Context, sess *model.JWTSession) (*model.User, bool) {
	if sess.IP != clientIP(c) {
		c.Set(model.CtxKeyIsIPMismatch, true)
		return nil, false
	}
	// 签发时写入的 UA 哈希必须比对：旧实现只写不查，被盗 token 换任意客户端都能用。
	if sess.UAHash != "" && sess.UAHash != uaHash(c) {
		return nil, false
	}
	var user model.User
	if err := singleton.DB.First(&user, sess.UserID).Error; err != nil {
		return nil, false
	}
	if user.TokenVersion != sess.TokenVersion {
		return nil, false
	}
	return &user, true
}

// blockBruteForceToken 把伪造/篡改 token 的来源计入 WAF。
func blockBruteForceToken(c *gin.Context) {
	realIP := c.GetString(model.CtxKeyRealIPStr)
	model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
}

// Logout
// @Summary Logout (revoke current JWT session)
// @Security BearerAuth
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /logout [post]
func logout(c *gin.Context) (any, error) {
	keyID := c.GetString(jwtClaimKeyID)
	if keyID == "" {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	// 服务端吊销：仅删本地 cookie 时会话仍有效，被盗 token 在过期前（且可无限 refresh）一直可用。
	if err := singleton.RevokeJWTSession(keyID); err != nil {
		return nil, newGormError("%v", err)
	}
	clearSessionCookies(c)
	return nil, nil
}

// clearSessionCookies 让浏览器删除 nz-jwt 与 nz-csrf（属性与签发时一致）。
func clearSessionCookies(c *gin.Context) {
	secure := c.Request.URL.Scheme == "https" || c.Request.TLS != nil
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("nz-jwt", "", -1, "/", "", secure, true)
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(csrfCookieName, "", -1, "/", "", secure, false)
}

// User Login
// @Summary user login
// @Schemes
// @Description user login
// @Accept json
// @param loginRequest body model.LoginRequest true "Login Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.LoginResponse]
// @Router /login [post]
func authenticator() func(c *gin.Context) (any, error) {
	return func(c *gin.Context) (any, error) {
		loginVals, ok := bindLoginRequest(c)
		if !ok {
			return "", jwt.ErrMissingLoginValues
		}
		realip := c.GetString(model.CtxKeyRealIPStr)
		user, err := checkLoginPassword(realip, loginVals)
		if err != nil {
			return nil, err
		}
		model.UnblockIP(singleton.DB, realip, model.BlockIDUnknownUser)
		model.UnblockIP(singleton.DB, realip, int64(user.ID))
		return issueJWTSession(c, user, singleton.Conf.JWTTimeout)
	}
}

// bindLoginRequest 只收 JSON：表单 / text/plain 属跨站「简单请求」，第三方页面可借受害者浏览器提交
// （登录 CSRF：把受害者登进攻击者账号；或反复提交错误口令借 WAF 封掉受害者 IP）。
func bindLoginRequest(c *gin.Context) (model.LoginRequest, bool) {
	var req model.LoginRequest
	if c.ContentType() != binding.MIMEJSON || c.ShouldBindJSON(&req) != nil {
		return req, false
	}
	return req, true
}

// checkLoginPassword 校验用户名与口令（禁用口令登录的用户一律拒绝），失败按原因计入 WAF。
func checkLoginPassword(realip string, req model.LoginRequest) (*model.User, error) {
	var user model.User
	if err := singleton.DB.Select("id", "password", "reject_password", "token_version").Where("username = ?", req.Username).First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeLoginFail, model.BlockIDUnknownUser)
		}
		return nil, jwt.ErrFailedAuthentication
	}
	if user.RejectPassword || bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)) != nil {
		model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeLoginFail, int64(user.ID))
		return nil, jwt.ErrFailedAuthentication
	}
	return &user, nil
}

func authorizator() func(data any, c *gin.Context) bool {
	return func(data any, c *gin.Context) bool {
		_, ok := data.(*model.User)
		return ok
	}
}

func unauthorized() func(c *gin.Context, code int, message string) {
	return func(c *gin.Context, code int, message string) {
		c.JSON(http.StatusOK, model.CommonResponse[any]{
			Success: false,
			Error:   "ApiErrorUnauthorized",
		})
	}
}

// Refresh token
// @Summary Refresh token
// @Security BearerAuth
// @Schemes
// @Description Refresh token
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[model.LoginResponse]
// @Router /refresh-token [post]
func refreshResponse(c *gin.Context, _ int, token string, expire time.Time) {
	if keyID := c.GetString(jwtClaimKeyID); keyID != "" {
		_ = singleton.DB.Model(&model.JWTSession{}).
			Where("key_id = ?", keyID).
			Updates(map[string]interface{}{
				"expires_at":   expire,
				"last_used_at": time.Now(),
			}).Error
	}
	writeLoginResponse(c, token, expire)
}

func fallbackAuthMiddleware(mw *jwt.GinJWTMiddleware) func(c *gin.Context) {
	return func(c *gin.Context) {
		claims, err := mw.GetClaimsFromJWT(c)
		if err != nil {
			return
		}

		switch v := claims["exp"].(type) {
		case nil:
			return
		case float64:
			if int64(v) < mw.TimeFunc().Unix() {
				return
			}
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return
			}
			if n < mw.TimeFunc().Unix() {
				return
			}
		default:
			return
		}

		realIP := c.GetString(model.CtxKeyRealIPStr)

		c.Set("JWT_PAYLOAD", claims)
		identity := mw.IdentityHandler(c)

		// optional 路由语义：有效 JWT 挂 user，失效或无 JWT 一律匿名继续。
		// 失效但合法签发的 token（改密码后 revoked、TokenVersion 变更、会话过期/被清）
		// 不再当作 token 爆破封禁；真正被篡改的 token 已由 identityHandler 精确拦截封禁。
		if identity != nil {
			model.UnblockIP(singleton.DB, realIP, model.BlockIDToken)
			c.Set(mw.IdentityKey, identity)
		}

		c.Next()
	}
}
