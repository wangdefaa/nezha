package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：①没有服务端登出，前端「退出」只删本地 cookie，被盗 token 在过期前一直可用；
// ②jwt_sessions.ua_hash 只写不校验（项目说明声称校验），被盗 token 换任意 UA 都能用。

const (
	victimUA   = "Mozilla/5.0 victim-browser"
	attackerUA = "curl/8.4.0 attacker"
)

// newSessionTestRouter 用真实 routers() 组装路由，库中预置 admin/correct-horse。
func newSessionTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	origDB, origConf, origLoc := singleton.DB, singleton.Conf, singleton.Localizer
	t.Cleanup(func() { singleton.DB, singleton.Conf, singleton.Localizer = origDB, origConf, origLoc })
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	require.NoError(t, idcodec.Init([]byte(strings.Repeat("k", 64))))
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.WAF{}, &model.JWTSession{}, &model.Oauth2Bind{}, &model.APIToken{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{JWTTimeout: 24, JWTSecretKey: "session-test-secret"}}
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.User{Username: "admin", Password: string(hash)}).Error)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	routers(r, testFrontendDist{})
	return r
}

// sessionReq 发请求；jwt 非空时带 nz-jwt cookie，csrf 非空时按双提交带 cookie + 头。
func sessionReq(r *gin.Engine, method, path, body, ua, jwtToken, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.RemoteAddr = "1.2.3.4:4444"
	if jwtToken != "" {
		req.AddCookie(&http.Cookie{Name: "nz-jwt", Value: jwtToken})
	}
	if csrf != "" {
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
		req.Header.Set(csrfHeaderName, csrf)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// loginAs 以给定 UA 登录，返回 JWT 与服务端下发的 CSRF token。
func loginAs(t *testing.T, r *gin.Engine, ua string) (string, string) {
	t.Helper()
	w := sessionReq(r, http.MethodPost, "/api/v1/login", `{"username":"admin","password":"correct-horse"}`, ua, "", "")
	var resp model.CommonResponse[model.LoginResponse]
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Data.Token)
	for _, ck := range w.Result().Cookies() {
		if ck.Name == csrfCookieName {
			return resp.Data.Token, ck.Value
		}
	}
	t.Fatal("login did not mint csrf cookie")
	return "", ""
}

func profileOK(r *gin.Engine, ua, jwtToken string) bool {
	w := sessionReq(r, http.MethodGet, "/api/v1/profile", "", ua, jwtToken, "")
	return strings.Contains(w.Body.String(), `"username":"admin"`)
}

func TestLogoutRevokesServerSideSession(t *testing.T) {
	r := newSessionTestRouter(t)
	token, csrf := loginAs(t, r, victimUA)
	require.True(t, profileOK(r, victimUA, token))

	w := sessionReq(r, http.MethodPost, "/api/v1/logout", `{}`, victimUA, token, csrf)
	require.Contains(t, w.Body.String(), `"success":true`, w.Body.String())
	require.False(t, profileOK(r, victimUA, token), "登出后旧 token 必须失效")
}

func TestSessionRejectsDifferentUserAgent(t *testing.T) {
	r := newSessionTestRouter(t)
	token, _ := loginAs(t, r, victimUA)
	require.False(t, profileOK(r, attackerUA, token), "UA 与签发时不一致的会话必须拒绝")
	require.True(t, profileOK(r, victimUA, token), "同 UA 正常使用不受影响")
}

// 会话 cookie 必须 HttpOnly（脚本读不到 token）；CSRF cookie 必须保持 JS 可读，
// 前端双提交与访客主题的 `!!document.cookie` 登录态判断都依赖它。
func TestSessionCookieHTTPOnly(t *testing.T) {
	r := newSessionTestRouter(t)
	w := sessionReq(r, http.MethodPost, "/api/v1/login", `{"username":"admin","password":"correct-horse"}`, victimUA, "", "")
	seen := map[string]bool{}
	for _, ck := range w.Result().Cookies() {
		seen[ck.Name] = ck.HttpOnly
	}
	require.Contains(t, seen, "nz-jwt")
	require.True(t, seen["nz-jwt"], "nz-jwt 必须 HttpOnly")
	require.Contains(t, seen, csrfCookieName)
	require.False(t, seen[csrfCookieName], "nz-csrf 必须 JS 可读")
}
