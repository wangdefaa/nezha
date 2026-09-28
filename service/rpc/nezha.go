package rpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	geoipx "github.com/nezhahq/nezha/pkg/geoip"
	"github.com/nezhahq/nezha/pkg/tsdb"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

var _ pb.NezhaServiceServer = (*NezhaHandler)(nil)

var NezhaHandlerSingleton *NezhaHandler

// ErrRequestTaskStreamSuperseded：结果到达时该 stream 已不是此 server 的当前 stream。
var ErrRequestTaskStreamSuperseded = errors.New("request task stream superseded")

// currentRequestTaskServer 以 ServerShared 当前条目校验收到的结果；
// 编辑导致的 *Server 替换会继承同一 stream holder，仍视为有效。
func currentRequestTaskServer(clientID uint64, stream pb.NezhaService_RequestTaskServer) (*model.Server, error) {
	current, ok := singleton.ServerShared.Get(clientID)
	if !ok || current == nil || current.GetTaskStream() != stream {
		return nil, ErrRequestTaskStreamSuperseded
	}
	return current, nil
}

type NezhaHandler struct {
	Auth *authHandler
}

func NewNezhaHandler() *NezhaHandler {
	return &NezhaHandler{
		Auth: &authHandler{},
	}
}

// attachRequestTaskStream resolves the server for clientID and publishes the
// task stream. It mirrors the !ok || server == nil guard the other RPC entry
// points use: the server can be deleted between CheckRequestTask and this
// lookup, in which case Get returns a nil *Server and SetTaskStream would
// panic.
func attachRequestTaskStream(clientID uint64, stream pb.NezhaService_RequestTaskServer) (*model.Server, bool) {
	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return nil, false
	}
	server.SetTaskStream(stream)
	return server, true
}

// clearRequestTaskStream detaches the dropped stream from whichever *Server is
// currently published for clientID. Edit and transfer rotation publish a new
// *Server that adopts the same stream holder, so cleanup must target the live
// map entry; the captured server is only the fallback for a removed entry.
func clearRequestTaskStream(clientID uint64, captured *model.Server, stream pb.NezhaService_RequestTaskServer) {
	if current, ok := singleton.ServerShared.Get(clientID); ok && current != nil {
		current.ClearTaskStreamIfCurrent(stream)
		return
	}
	captured.ClearTaskStreamIfCurrent(stream)
}

func (s *NezhaHandler) RequestTask(stream pb.NezhaService_RequestTaskServer) error {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.CheckRequestTask(stream.Context()); err != nil {
		return err
	}

	server, ok := attachRequestTaskStream(clientID, stream)
	if !ok {
		return nil
	}
	defer clearRequestTaskStream(clientID, server, stream)
	var result *pb.TaskResult
	for {
		result, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> RequestTask error: %v, clientID: %d\n", err, clientID)
			return err
		}
		if _, err = currentRequestTaskServer(clientID, stream); err != nil {
			return err
		}
		// 仅拨测类任务（HTTP/TCP/ICMP）需要服务监控；Upgrade/Keepalive 跳过。
		if model.IsServiceMonitorType(result.GetType()) {
			singleton.ServiceSentinelShared.Dispatch(singleton.ReportData{
				Data:     result,
				Reporter: clientID,
			})
		}
	}
}

func (s *NezhaHandler) ReportSystemState(stream pb.NezhaService_ReportSystemStateServer) error {
	clientID, err := s.Auth.Check(stream.Context())
	if err != nil {
		return err
	}
	var state *pb.State
	for {
		state, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> ReportSystemState error: %v, clientID: %d\n", err, clientID)
			return err
		}
		innerState := model.PB2State(state)

		server, ok := singleton.ServerShared.Get(clientID)
		if !ok || server == nil {
			return errors.New("server not found")
		}

		server.LastActive = time.Now()
		server.State = &innerState

		if singleton.TSDBEnabled() {
			if err := singleton.TSDBShared.WriteServerMetrics(&tsdb.ServerMetrics{
				ServerID:       clientID,
				Timestamp:      time.Now(),
				CPU:            innerState.CPU,
				MemUsed:        innerState.MemUsed,
				SwapUsed:       innerState.SwapUsed,
				DiskUsed:       innerState.DiskUsed,
				NetInSpeed:     innerState.NetInSpeed,
				NetOutSpeed:    innerState.NetOutSpeed,
				NetInTransfer:  innerState.NetInTransfer,
				NetOutTransfer: innerState.NetOutTransfer,
				Load1:          innerState.Load1,
				Load5:          innerState.Load5,
				Load15:         innerState.Load15,
				TCPConnCount:   innerState.TcpConnCount,
				UDPConnCount:   innerState.UdpConnCount,
				ProcessCount:   innerState.ProcessCount,
				Uptime:         innerState.Uptime,
			}); err != nil {
				log.Printf("NEZHA>> Failed to write server metrics to TSDB: %v", err)
			}
		}

		// 应对 dashboard / agent 重启的情况，如果从未记录过，先打点，等到小时时间点时入库
		if server.PrevTransferInSnapshot == 0 || server.PrevTransferOutSnapshot == 0 {
			server.PrevTransferInSnapshot = state.NetInTransfer
			server.PrevTransferOutSnapshot = state.NetOutTransfer
		}

		if err = stream.Send(&pb.Receipt{Proced: true}); err != nil {
			return err
		}
	}
}

func (s *NezhaHandler) onReportSystemInfo(c context.Context, r *pb.Host) error {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.Check(c); err != nil {
		return err
	}
	host := model.PB2Host(r)

	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return errors.New("server not found")
	}

	/**
	 * 这里的 singleton 中的数据都是关机前的旧数据
	 * 当 agent 重启时，bootTime 变大，agent 端会先上报 host 信息，然后上报 state 信息
	 * 这时可以借助上报顺序的空档，立即记录停机前的数据并重置 Prev* 数据，并由接下来的 state 方法重新赋值
	 */
	if !server.LastActive.IsZero() && host.BootTime > server.Host.BootTime {
		singleton.RecordTransferHourlyUsage(server)
		server.PrevTransferInSnapshot = 0
		server.PrevTransferOutSnapshot = 0
	}

	server.Host = &host
	return nil
}

func (s *NezhaHandler) ReportSystemInfo(c context.Context, r *pb.Host) (*pb.Receipt, error) {
	if err := s.onReportSystemInfo(c, r); err != nil {
		return nil, err
	}
	return &pb.Receipt{Proced: true}, nil
}

func (s *NezhaHandler) ReportSystemInfo2(c context.Context, r *pb.Host) (*pb.Uint64Receipt, error) {
	if err := s.onReportSystemInfo(c, r); err != nil {
		return nil, err
	}
	return &pb.Uint64Receipt{Data: singleton.DashboardBootTime}, nil
}

func (s *NezhaHandler) ReportGeoIP(c context.Context, r *pb.GeoIP) (*pb.GeoIP, error) {
	clientID, err := s.Auth.Check(c)
	if err != nil {
		return nil, err
	}

	geoip := model.PB2GeoIP(r)
	sanitizeReportedIP(&geoip.IP)
	if geoip.IP.IPv4Addr == "" && geoip.IP.IPv6Addr == "" {
		geoip.IP.IPv4Addr = agentContextIP(c)
	}

	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return nil, fmt.Errorf("server not found")
	}

	notifyIPChanged(clientID, server, geoip.IP)
	geoip.CountryCode = lookupCountryCode(geoip.IP, r.GetUse6())
	// 将地区码写入到 Host
	server.GeoIP = &geoip

	return &pb.GeoIP{Ip: nil, CountryCode: geoip.CountryCode, DashboardBootTime: singleton.DashboardBootTime}, nil
}

// agentContextIP 上报未带 IP 时回退：已配置真实 IP 头用真实 IP，否则用连接 IP。
func agentContextIP(c context.Context) string {
	ip, _ := c.Value(model.CtxKeyRealIP{}).(string)
	if ip == "" {
		ip, _ = c.Value(model.CtxKeyConnectingIP{}).(string)
	}
	return ip
}

// sanitizeReportedIP agent 上报的 IP 是任意字符串，旧实现原样写入运行态并拼进管理员的 IP 变更通知。
// 只保留合法地址（去掉 zone、按地址族归位），非法值清空后按未上报处理。
func sanitizeReportedIP(ip *model.IP) {
	ip.IPv4Addr = canonicalReportedIP(ip.IPv4Addr, true)
	ip.IPv6Addr = canonicalReportedIP(ip.IPv6Addr, false)
}

func canonicalReportedIP(raw string, want4 bool) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is4() != want4 {
		return ""
	}
	return addr.String()
}

// notifyIPChanged 发送 IP 变更通知，静音标签按服务器去重：旧实现不带标签，持有 agent 密钥者
// （普通成员即可）交替上报就能让管理员的通知组无限发送。正常 agent 默认 30 分钟上报一次，不受影响。
func notifyIPChanged(clientID uint64, server *model.Server, newIP model.IP) {
	if !shouldNotifyIPChange(clientID, server, newIP) {
		return
	}
	singleton.NotificationShared.SendNotification(singleton.Conf.IPChangeNotificationGroupID,
		fmt.Sprintf("[%s] %s, %s => %s", singleton.Localizer.T("IP Changed"), server.Name,
			singleton.IPDesensitize(server.GeoIP.IP.Join()), singleton.IPDesensitize(newIP.Join())),
		singleton.NotificationMuteLabel.IPChanged(clientID))
}

// shouldNotifyIPChange 已开启提醒、服务器在覆盖范围内且新旧 IP 均非空并确有变化。
func shouldNotifyIPChange(clientID uint64, server *model.Server, newIP model.IP) bool {
	conf := singleton.Conf
	if server.GeoIP == nil || !conf.EnableIPChangeNotification {
		return false
	}
	ignored := conf.IgnoredIPNotificationServerIDs[clientID]
	covered := (conf.Cover == model.ConfigCoverAll && !ignored) || (conf.Cover == model.ConfigCoverIgnoreAll && ignored)
	return covered && server.GeoIP.IP.Join() != "" && newIP.Join() != "" && server.GeoIP.IP != newIP
}

// lookupCountryCode 按内置数据库查询地区码：agent 要求或没有 IPv4 时用 IPv6。
func lookupCountryCode(ip model.IP, use6 bool) string {
	addr := ip.IPv4Addr
	if ip.IPv6Addr != "" && (use6 || ip.IPv4Addr == "") {
		addr = ip.IPv6Addr
	}
	location, err := geoipx.Lookup(net.ParseIP(addr))
	if err != nil {
		log.Printf("NEZHA>> geoip.Lookup: %v", err)
	}
	return location
}
