package singleton

import (
	"net"
	"net/netip"
	"strings"
)

// IsReservedDashboardHost 判断 domain 是否是 dashboard 自身对外访问的主机名
// （InstallHost / DashboardHost / ListenHost）。OAuth2 回调用它决定是否需要把
// redirect host 重写为运维声明的 DashboardHost。
func IsReservedDashboardHost(domain string) bool {
	if Conf == nil {
		return false
	}

	target := splitDashboardHostname(domain)
	if target == "" {
		return false
	}

	hosts := []string{Conf.InstallHost, Conf.DashboardHost, Conf.ListenHost}
	for _, host := range hosts {
		if reserved := splitDashboardHostname(host); reserved != "" && reserved == target {
			return true
		}
	}
	return false
}

// splitDashboardHostname 归一化主机名：去端口、去括号、去尾点，IP 走 netip 规范化。
func splitDashboardHostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	} else {
		host = strings.Trim(host, "[]")
	}
	host = strings.TrimSuffix(host, ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String()
	}
	return host
}
