package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：web_real_ip_header 留空（默认，config.yaml.example 称"直连可留空,使用 TCP 对端地址"）时，
// 旧实现根本不写入真实 IP，CheckIP/BlockIP 对空 IP 直接放行 —— 登录爆破、token 爆破的 WAF 全部失效。

// newLoginWAFRouter 组装与生产（ServeWeb）一致的 RealIp→Waf→limitRequestBody→/login 链路。
func newLoginWAFRouter(t *testing.T, realIPHeader string) *gin.Engine {
	t.Helper()
	origDB, origConf, origLoc := singleton.DB, singleton.Conf, singleton.Localizer
	t.Cleanup(func() { singleton.DB, singleton.Conf, singleton.Localizer = origDB, origConf, origLoc })
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	require.NoError(t, idcodec.Init([]byte(strings.Repeat("k", 64))))
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.WAF{}, &model.JWTSession{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{JWTTimeout: 1, JWTSecretKey: "waf-test-secret"}}
	singleton.Conf.WebRealIPHeader = realIPHeader
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.User{Username: "admin", Password: string(hash)}).Error)
	mw, err := jwt.New(initParams())
	require.NoError(t, err)
	require.NoError(t, mw.MiddlewareInit())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(waf.RealIp, waf.Waf, limitRequestBody)
	r.POST("/api/v1/login", mw.LoginHandler)
	return r
}

func postLogin(r *gin.Engine, remoteAddr, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login",
		strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func wafRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, singleton.DB.Model(&model.WAF{}).Count(&n).Error)
	return n
}

func TestWAF_EmptyRealIPHeaderStillThrottlesPublicPeer(t *testing.T) {
	r := newLoginWAFRouter(t, "")
	for i := 0; i < 20; i++ {
		postLogin(r, "1.2.3.4:5555", "wrong-password")
	}
	w := postLogin(r, "1.2.3.4:5555", "correct-horse")
	t.Logf("20 次错误密码后：WAF 记录=%d，第 21 次(正确密码)状态=%d body=%.60q", wafRows(t), w.Code, w.Body.String())
	require.NotZero(t, wafRows(t), "登录失败必须计入 WAF")
	require.Equal(t, http.StatusForbidden, w.Code, "连续失败后同一公网来源应被 WAF 拦截")
}

func TestWAF_EmptyRealIPHeaderKeepsLoopbackPeerUnblocked(t *testing.T) {
	// 同机反代（对端为回环）且未配真实 IP 头时保持旧行为，避免把反代地址封掉导致全站被锁。
	r := newLoginWAFRouter(t, "")
	postLogin(r, "127.0.0.1:5555", "wrong-password")
	w := postLogin(r, "127.0.0.1:5555", "correct-horse")
	require.Equal(t, http.StatusOK, w.Code)
	require.Zero(t, wafRows(t))
}
