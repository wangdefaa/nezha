package waf

import (
	_ "embed"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

//go:embed waf.html
var errorPageTemplate string

func RealIp(c *gin.Context) {
	switch singleton.Conf.WebRealIPHeader {
	case "":
		// 未配置真实 IP 头：按 config.yaml.example 的约定回退 TCP 对端地址，但只认公网对端（直连部署）。
		// 回环/内网对端多为同机或内网反代，封它等于封全站，保持旧行为（不计入 WAF）。
		if peer := c.RemoteIP(); isPublicPeer(peer) {
			c.Set(model.CtxKeyRealIPStr, peer)
		}
	case model.ConfigUsePeerIP:
		c.Set(model.CtxKeyRealIPStr, c.RemoteIP())
	default:
		ip, err := ipFromConfiguredHeader(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusOK, model.CommonResponse[any]{Success: false, Error: err.Error()})
			return
		}
		c.Set(model.CtxKeyRealIPStr, ip)
	}
	c.Next()
}

// ipFromConfiguredHeader 从运维配置的真实 IP 头取客户端地址（多值取最右，防伪造）。
func ipFromConfiguredHeader(c *gin.Context) (string, error) {
	vals := c.Request.Header.Get(singleton.Conf.WebRealIPHeader)
	if vals == "" {
		return "", errors.New("real ip header not found")
	}
	return utils.GetIPFromHeader(vals)
}

// isPublicPeer 判断对端是否为公网可路由地址（复用 SSRF 防护的保留网段表）。
func isPublicPeer(peer string) bool {
	ip := net.ParseIP(peer)
	return ip != nil && utils.HTTPURLTargetIPAllowed(ip)
}

func Waf(c *gin.Context) {
	if err := model.CheckIP(singleton.DB, c.GetString(model.CtxKeyRealIPStr)); err != nil {
		ShowBlockPage(c, err)
		return
	}
	c.Next()
}

func ShowBlockPage(c *gin.Context, err error) {
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	} else {
		errMsg = "you were blocked by nezha WAF"
	}
	c.Writer.WriteHeader(http.StatusForbidden)
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Writer.WriteString(strings.Replace(errorPageTemplate, "{error}", errMsg, 1))
	c.Abort()
}
