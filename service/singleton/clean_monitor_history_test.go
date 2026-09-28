package singleton

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

func setupCleanMonitorHistoryTestDB(t *testing.T) {
	t.Helper()

	previousDB := DB
	var err error
	DB, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "dashboard.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		DB = previousDB
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close transfer cleanup test database: %v", err)
		}
	})

	require.NoError(t, DB.AutoMigrate(&model.Server{}, &model.Transfer{}, &model.AlertRule{}))
	require.NoError(t, DB.Exec("INSERT INTO servers (id, name, uuid) VALUES (1, 'server', 'clean-monitor-history-test')").Error)
}

func transferCount(t *testing.T) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&model.Transfer{}).Count(&count).Error)
	return count
}

func TestCleanMonitorHistoryWithoutRulesDeletesAllTransfers(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.Create(&model.Transfer{ServerID: 1, In: 1}).Error)

	CleanMonitorHistory()

	require.Zero(t, transferCount(t))
}

func TestCleanMonitorHistoryPreservesTransfersWhenAlertRulesCannotBeLoaded(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.Create(&model.Transfer{ServerID: 1, In: 1}).Error)
	// fork 的 alert_rules 没有 fail/recover_trigger_tasks_raw 列
	require.NoError(t, DB.Exec("INSERT INTO alert_rules (id, name, rules_raw) VALUES (1, 'broken', '{')").Error)

	var alerts []model.AlertRule
	require.Error(t, DB.Find(&alerts).Error, "precondition: malformed rules_raw must fail AlertRule.AfterFind")

	CleanMonitorHistory()

	require.EqualValues(t, 1, transferCount(t))
}

// 19daf4ad 只在告警哨兵侧拦截毒化规则；CleanMonitorHistory 在启动时同步执行，
// 同样必须跳过 CycleStart 为空 / CycleInterval 溢出的历史行，否则启动即 panic（除零）或死循环。
func TestCleanMonitorHistorySkipsPoisonedPersistedCycleRules(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.Create(&model.Transfer{ServerID: 1, In: 1}).Error)
	start := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	poisoned := `[{"type":"transfer_in_cycle","cycle_interval":1152921504606846976,"cycle_start":"` + start + `","cover":0},` +
		`{"type":"transfer_in_cycle","cycle_interval":18446744073709551615,"cycle_unit":"day","cycle_start":"` + start + `","cover":0},` +
		`{"type":"transfer_out_cycle","cycle_interval":1,"cover":0},null]`
	require.NoError(t, DB.Exec("INSERT INTO alert_rules (id, name, rules_raw) VALUES (1, 'poisoned', ?)", poisoned).Error)

	done := make(chan struct{})
	go func() { defer close(done); CleanMonitorHistory() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CleanMonitorHistory hung on a poisoned persisted cycle rule")
	}
}

// sqlite 以文本存时间：负时区的周期内数据不能因字符串比较被误删。
func TestCleanMonitorHistoryKeepsInCycleRowsAcrossTimezones(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	enabled := true
	require.NoError(t, DB.Create(&model.AlertRule{Name: "cycle", Enable: &enabled, Rules: []*model.Rule{{
		Type: "transfer_in_cycle", CycleStart: &cycleStart, CycleInterval: 24, Cover: model.RuleCoverAll, Max: 1,
	}}}).Error)
	west := time.FixedZone("UTC-5", -5*3600)
	inCycle := time.Now().Add(-time.Minute).In(west)
	require.NoError(t, DB.Create(&model.Transfer{Common: model.Common{CreatedAt: inCycle}, ServerID: 1, In: 1}).Error)

	CleanMonitorHistory()

	require.EqualValues(t, 1, transferCount(t))
}
