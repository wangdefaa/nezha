package model

import (
	"math"
	"testing"
	"time"
)

// fork 已移除 GPU/温度采集：这些类型及任意未知类型必须被忽略（视为通过），不能落入 src=0 的阈值判断。
func TestRuleSnapshotIgnoresUnsupportedTypes(t *testing.T) {
	for _, ruleType := range []string{"gpu", "gpu_max", "temperature_max", "attacker_controlled", "foo_cycle"} {
		t.Run(ruleType, func(t *testing.T) {
			server := &Server{Common: Common{ID: 1}, State: &HostState{}, Host: &Host{}}
			rule := &Rule{Type: ruleType, Min: 1, Duration: 3, Cover: RuleCoverAll}
			if passed := rule.Snapshot(nil, server, nil); !passed {
				t.Fatal("unsupported rule type must be ignored instead of treated as a failed threshold")
			}
			if (&AlertRule{Rules: []*Rule{rule}}).IsSafeToEvaluate() {
				t.Fatal("unsupported rule type must not be evaluated")
			}
		})
	}
}

func TestAlertRuleCheckRejectsOverflowingPersistedDuration(t *testing.T) {
	rule := &AlertRule{Rules: []*Rule{{
		Type:     "offline",
		Duration: math.MaxUint64,
		Cover:    RuleCoverAll,
	}}}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Check panicked after uint64-to-int overflow: %v", recovered)
		}
	}()
	duration, passed := rule.Check([][]bool{{false}})
	if duration != 0 || !passed {
		t.Fatalf("invalid persisted duration must be ignored safely, got duration=%d passed=%v", duration, passed)
	}
	if window := rule.RetentionWindow(); window != 0 {
		t.Fatalf("invalid persisted duration must not create a retention window, got %d", window)
	}
}

func TestAlertRulePersistedDataSafety(t *testing.T) {
	cycleStart := time.Now().Add(-time.Hour)
	tests := []struct {
		name string
		rule *AlertRule
		want bool
	}{
		{
			name: "normal rule",
			rule: &AlertRule{Rules: []*Rule{{
				Type: "cpu", Duration: 3, Cover: RuleCoverAll,
			}}},
			want: true,
		},
		{
			name: "normal cycle rule",
			rule: &AlertRule{Rules: []*Rule{{
				Type: "transfer_in_cycle", CycleStart: &cycleStart, CycleInterval: 1, Cover: RuleCoverAll,
			}}},
			want: true,
		},
		{
			name: "unknown type",
			rule: &AlertRule{Rules: []*Rule{{
				Type: "attacker_controlled", Duration: 3, Cover: RuleCoverAll,
			}}},
		},
		{
			name: "overflowing duration",
			rule: &AlertRule{Rules: []*Rule{{
				Type: "offline", Duration: math.MaxUint64, Cover: RuleCoverAll,
			}}},
		},
		{
			name: "cycle without start",
			rule: &AlertRule{Rules: []*Rule{{
				Type: "transfer_in_cycle", CycleInterval: 1, Cover: RuleCoverAll,
			}}},
		},
		{
			name: "nil rule",
			rule: &AlertRule{Rules: []*Rule{nil}},
		},
		{
			name: "empty rule list",
			rule: &AlertRule{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.rule.IsSafeToEvaluate(); got != test.want {
				t.Fatalf("IsSafeToEvaluate()=%v, want %v", got, test.want)
			}
		})
	}
}
