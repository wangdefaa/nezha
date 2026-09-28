package model

import "testing"

// net_all_speed 必须是入站 + 出站速度之和（曾误写成 NetOutSpeed + NetOutSpeed）。
func TestRuleNetAllSpeedSumsInAndOut(t *testing.T) {
	server := &Server{Common: Common{ID: 1}, Host: &Host{}, State: &HostState{NetInSpeed: 100, NetOutSpeed: 10}}
	rule := &Rule{Type: "net_all_speed", Max: 50}
	if rule.Snapshot(nil, server, nil) {
		t.Fatal("in+out=110 > 50，规则应判定为未通过")
	}
	rule.Max = 200
	if !rule.Snapshot(nil, server, nil) {
		t.Fatal("in+out=110 < 200，规则应判定为通过")
	}
}
