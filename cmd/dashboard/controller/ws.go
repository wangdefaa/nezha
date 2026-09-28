package controller

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-uuid"
	"golang.org/x/sync/singleflight"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

var upgrader *websocket.Upgrader

func InitUpgrader() {
	var checkOrigin func(r *http.Request) bool

	// Allow CORS from loopback addresses in debug mode
	if singleton.Conf.Debug {
		checkOrigin = func(r *http.Request) bool {
			if checkSameOrigin(r) {
				return true
			}
			hostAddr := r.Host
			host, _, err := net.SplitHostPort(hostAddr)
			if err != nil {
				return false
			}
			if ip := net.ParseIP(host); ip != nil {
				if ip.IsLoopback() {
					return true
				}
			} else {
				// Handle domains like "localhost"
				ip, err := net.LookupHost(host) // #nosec G704 -- 仅 debug 模式做回环判定，只解析不建连
				if err != nil || len(ip) == 0 {
					return false
				}
				if netIP := net.ParseIP(ip[0]); netIP != nil && netIP.IsLoopback() {
					return true
				}
			}
			return false
		}
	}

	upgrader = &websocket.Upgrader{
		ReadBufferSize:  32768,
		WriteBufferSize: 32768,
		CheckOrigin:     checkOrigin,
	}
}

func equalASCIIFold(s, t string) bool {
	for s != "" && t != "" {
		sr, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		tr, size := utf8.DecodeRuneInString(t)
		t = t[size:]
		if sr == tr {
			continue
		}
		if 'A' <= sr && sr <= 'Z' {
			sr = sr + 'a' - 'A'
		}
		if 'A' <= tr && tr <= 'Z' {
			tr = tr + 'a' - 'A'
		}
		if sr != tr {
			return false
		}
	}
	return s == t
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	return equalASCIIFold(u.Host, r.Host)
}

// Websocket server stream
// @Summary Websocket server stream
// @tags common
// @Schemes
// @Description Websocket server stream
// @security BearerAuth
// @Produce json
// @Success 200 {object} model.StreamServerData
// @Router /ws/server [get]
func serverStream(c *gin.Context) (any, error) {
	connId, err := uuid.GenerateUUID()
	if err != nil {
		return nil, newWsError("%v", err)
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return nil, newWsError("%v", err)
	}
	defer conn.Close()

	deregisterPAT := registerPATConnection(c, func() { _ = conn.Close() })
	defer deregisterPAT()

	viewer := newStreamViewer(c)
	singleton.AddOnlineUser(connId, &model.OnlineUser{
		UserID:      viewer.userID,
		IP:          viewer.ip,
		ConnectedAt: time.Now(),
		Conn:        conn,
	})
	defer singleton.RemoveOnlineUser(connId)

	pushServerStats(conn, viewer)
	return nil, newWsError("")
}

// serverStreamInterval /ws/server 的推送间隔。
const serverStreamInterval = 2 * time.Second

// streamViewer 一条 WS 连接的观看者身份，决定推送内容的投影与缓存键。
type streamViewer struct {
	userID      uint64
	isAdmin     bool
	ip          string
	pat         model.APITokenAccessor
	patCacheKey string
}

func newStreamViewer(c *gin.Context) streamViewer {
	v := streamViewer{ip: clientIP(c)}
	if u, ok := c.Get(model.CtxKeyAuthorizedUser); ok {
		user := u.(*model.User)
		v.userID, v.isAdmin = user.ID, user.Role.IsAdmin()
	}
	v.pat, v.patCacheKey = patStreamContext(c)
	return v
}

// pushServerStats 按固定间隔推送。序列化失败只跳过本帧并同样等待——旧实现直接 continue，
// 运行态出现坏数据（如 NaN）时每个连接（访客即可建立）都会空转占满一个 CPU 核。
// 跳过数据帧时改发 ping 探测对端，断开即退出，避免协程与在线用户记录泄漏。
func pushServerStats(conn *websocket.Conn, v streamViewer) {
	sent := 0
	for {
		stat, err := getServerStat(sent == 0, v.userID, v.isAdmin, v.pat, v.patCacheKey)
		switch {
		case err != nil:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(serverStreamInterval)) != nil {
				return
			}
		case !writeServerStatFrame(conn, stat, sent):
			return
		default:
			sent++
		}
		time.Sleep(serverStreamInterval)
	}
}

// writeServerStatFrame 写一帧数据，每 4 帧追加一次 ping；任一写失败返回 false 结束推送。
func writeServerStatFrame(conn *websocket.Conn, stat []byte, sent int) bool {
	if err := conn.WriteMessage(websocket.TextMessage, stat); err != nil {
		return false
	}
	if (sent+1)%4 == 0 {
		return conn.WriteMessage(websocket.PingMessage, []byte{}) == nil
	}
	return true
}

var requestGroup singleflight.Group

// getServerStat 返回当前观看者可见的推流帧。singleflight 键必须含观看者身份
// （GHSA-hvv7-hfrh-7gxj：曾只按 isMember 缓存，向所有登录用户泄露 HideForGuest
// 服务器与完整 Host）；patCacheKey 区分白名单不同的 PAT，避免共用投影。
func getServerStat(withPublicNote bool, viewerUserID uint64, viewerIsAdmin bool, pat model.APITokenAccessor, patCacheKey string) ([]byte, error) {
	cacheKey := fmt.Sprintf("serverStats::%t::%t::%d::%s", withPublicNote, viewerIsAdmin, viewerUserID, patCacheKey)
	v, err, _ := requestGroup.Do(cacheKey, func() (any, error) {
		servers := filterServersForViewer(
			singleton.ServerShared.GetSortedList(),
			viewerUserID, viewerIsAdmin, withPublicNote, pat,
		)
		return json.Marshal(model.StreamServerData{
			Now:     time.Now().Unix() * 1000,
			Online:  singleton.GetOnlineUserCount(),
			Servers: servers,
		})
	})

	return v.([]byte), err
}

// patStreamContext extracts the PAT accessor + a deterministic cache key
// fragment for the singleflight projection. Returns (nil, "jwt") for JWT
// requests so two callers from the same user collapse onto one frame.
func patStreamContext(c *gin.Context) (model.APITokenAccessor, string) {
	tok := APITokenFromContext(c)
	if tok == nil {
		return nil, "jwt"
	}
	return tok, fmt.Sprintf("pat:%d:%s", tok.ID, sortedServerIDsKey(tok))
}

// sortedServerIDsKey 把 PAT 的 server 白名单排序后拼成稳定字符串，用作缓存键片段。
func sortedServerIDsKey(tok *model.APIToken) string {
	ids := tok.ServerIDs()
	slices.Sort(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return strings.Join(parts, ",")
}

// filterServersForViewer projects the global server list down to what a single
// viewer is allowed to see. The rules are:
//   - HideForGuest servers are visible only to their owner and to admins.
//   - Non-owner / non-admin viewers (including authenticated members) get
//     Host.Filter() output, which drops PlatformVersion and agent Version.
//   - Admins are unconstrained.
//   - A non-nil pat whitelist narrows visibility further; servers outside its
//     allow-list are dropped even from admins/owners (a PAT scoped to a
//     subset must never widen via its caller's role).
//
// viewerUserID == 0 represents an unauthenticated guest.
func filterServersForViewer(servers []*model.Server, viewerUserID uint64, viewerIsAdmin bool, withPublicNote bool, pat model.APITokenAccessor) []model.StreamServer {
	out := make([]model.StreamServer, 0, len(servers))
	for _, server := range servers {
		if pat != nil && !pat.CanAccessServer(server.ID) {
			continue
		}
		isOwnerOrAdmin := viewerIsAdmin || (viewerUserID != 0 && server.GetUserID() == viewerUserID)
		if server.HideForGuest && !isOwnerOrAdmin {
			continue
		}
		var countryCode string
		if server.GeoIP != nil {
			countryCode = server.GeoIP.CountryCode
		}
		out = append(out, model.StreamServer{
			ID:           server.ID,
			Name:         server.Name,
			PublicNote:   utils.IfOr(withPublicNote, server.PublicNote, ""),
			DisplayIndex: server.DisplayIndex,
			Host:         utils.IfOr(isOwnerOrAdmin, server.Host, server.Host.Filter()),
			State:        server.State,
			CountryCode:  countryCode,
			LastActive:   server.LastActive,
		})
	}
	return out
}
