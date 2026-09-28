package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：IdP 用户信息里取不到 user_id_path（字段缺失/为 null/接口报错）时 openID 为空串。
// 旧实现会把空串当合法身份：绑定时写入 OpenID=""，登录时再用 "" 命中该绑定，
// 任意能让自己 user_id 取空的 IdP 账号都能登录成已绑定空身份的用户（账号接管）。

// newOAuth2TestIdP 起一个假 IdP：token 端点正常发 token，userinfo 端点按参数返回。
func newOAuth2TestIdP(t *testing.T, userinfoStatus int, userinfoBody string) {
	t.Helper()
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"attacker-token","token_type":"Bearer"}`))
		case "/userinfo":
			w.WriteHeader(userinfoStatus)
			_, _ = w.Write([]byte(userinfoBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	singleton.Conf.Oauth2["github"] = &model.Oauth2Config{
		ClientID: "client", ClientSecret: "secret",
		Endpoint:    model.Oauth2Endpoint{AuthURL: idp.URL + "/auth", TokenURL: idp.URL + "/token"},
		UserInfoURL: idp.URL + "/userinfo",
		UserIDPath:  "email", // 可为 null 的字段（如 GitHub 隐藏邮箱）
	}
}

// runOAuth2CallbackAs 以给定 action 走一次完整回调；caller 非空时模拟已登录用户（绑定流程）。
func runOAuth2CallbackAs(t *testing.T, action model.Oauth2LoginType, caller *model.User) (*httptest.ResponseRecorder, error) {
	t.Helper()
	stateKey, stateValue := "k-"+fmt.Sprint(action), "s-"+fmt.Sprint(action)
	singleton.Cache.Set(model.CacheKeyOauth2State+stateKey, &model.Oauth2State{
		State: stateValue, Provider: "github", Action: action,
		RedirectURL: "http://panel.example.com/api/v1/oauth2/callback",
	}, cache.DefaultExpiration)
	jwtConfig, err := jwt.New(initParams())
	require.NoError(t, err)
	require.NoError(t, jwtConfig.MiddlewareInit())
	c, w := newOAuth2Ctx(t)
	c.Request = httptest.NewRequest(http.MethodGet, "/oauth2/callback?state="+stateValue+"&code=any-code", nil)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: stateKey})
	c.Set(model.CtxKeyRealIPStr, "1.2.3.4")
	if caller != nil {
		c.Set(model.CtxKeyAuthorizedUser, caller)
	}
	_, err = oauth2callback(jwtConfig)(c)
	return w, err
}

func countJWTSessions(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Count(&n).Error)
	return n
}

func TestOAuth2Callback_LoginWithEmptyOpenIDMustNotHijackEmptyBinding(t *testing.T) {
	defer setupOAuth2Test(t)()
	newOAuth2TestIdP(t, http.StatusOK, `{"login":"attacker","email":null}`)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 1}, Username: "admin", Role: model.RoleAdmin}).Error)
	// 管理员此前在 IdP 未返回 email 时完成过绑定，库中留下 OpenID=""。
	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{UserID: 1, Provider: "github", OpenID: ""}).Error)

	w, err := runOAuth2CallbackAs(t, model.RTypeLogin, nil)
	t.Logf("callback err=%v status=%d set-cookie=%q", err, w.Code, w.Header().Values("Set-Cookie"))
	require.False(t, errors.Is(err, errNoop), "空 OpenID 登录竟然成功（已签发会话并 302 跳转）")
	require.Error(t, err)
	require.Zero(t, countJWTSessions(t), "空 OpenID 不得签发任何会话")
}

func TestOAuth2Callback_BindMustRejectEmptyOpenID(t *testing.T) {
	defer setupOAuth2Test(t)()
	newOAuth2TestIdP(t, http.StatusOK, `{"login":"victim","email":null}`)
	victim := &model.User{Common: model.Common{ID: 2}, Username: "victim"}
	require.NoError(t, singleton.DB.Create(victim).Error)

	_, err := runOAuth2CallbackAs(t, model.RTypeBind, victim)
	var binds int64
	require.NoError(t, singleton.DB.Model(&model.Oauth2Bind{}).Where("open_id = ?", "").Count(&binds).Error)
	t.Logf("bind err=%v empty-openid-binds=%d", err, binds)
	require.Zero(t, binds, "不得写入 OpenID 为空的绑定")
	require.False(t, errors.Is(err, errNoop))
}

func TestOAuth2Callback_RejectsNon2xxUserInfo(t *testing.T) {
	defer setupOAuth2Test(t)()
	newOAuth2TestIdP(t, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 1}, Username: "admin"}).Error)
	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{UserID: 1, Provider: "github", OpenID: ""}).Error)

	_, err := runOAuth2CallbackAs(t, model.RTypeLogin, nil)
	require.False(t, errors.Is(err, errNoop), "userinfo 返回 403 时不得登录成功")
	require.Zero(t, countJWTSessions(t))
}

func TestOAuth2Callback_StateIsSingleUse(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)
	singleton.Cache.Set(model.CacheKeyOauth2State+"k-once", &model.Oauth2State{State: "s-once", Provider: "github"}, cache.DefaultExpiration)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: "k-once"})
	_, err := verifyState(c, "s-once")
	require.NoError(t, err)
	_, err = verifyState(c, "s-once")
	require.Error(t, err, "state 必须一次性使用，防回调重放")
}
