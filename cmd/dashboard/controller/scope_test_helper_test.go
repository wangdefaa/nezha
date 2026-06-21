package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

// setupMCPTest 是 PAT / scope 体系相关测试共用的初始化 helper。
//
// 历史上它定义在已删除的 mcp_test.go 里（MCP 功能已移除）。由于 REST scope
// 中间件、PAT 白名单、WAF 计数、自我管理禁 PAT 等测试仍然有效且需要保留，
// 这里重建一个不依赖任何 MCP 符号的等价版本。函数名保持不变以避免改动 20+
// 处调用点。
func setupMCPTest(t *testing.T) (func(), uint64) {
	t.Helper()
	originalDB := singleton.DB
	originalServer := singleton.ServerShared
	originalConf := singleton.Conf
	originalLocalizer := singleton.Localizer
	originalPATRegistry := patConnectionRegistryShared
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	// Fresh per test: the DB resets token IDs to 1 each run, so a stale
	// revoke tombstone from a prior test would otherwise cancel a reused id.
	patConnectionRegistryShared = newPATConnectionRegistry()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.APIToken{}, &model.Server{}, &model.WAF{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{JWTTimeout: 1}}

	user := model.User{Common: model.Common{ID: 100}, Username: "alice", Role: model.RoleMember}
	require.NoError(t, db.Create(&user).Error)

	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = 7
	srv.Name = "alpha"
	srv.SetUserID(100)
	sc.InsertForTest(srv)
	singleton.ServerShared = sc

	cleanup := func() {
		singleton.DB = originalDB
		singleton.ServerShared = originalServer
		singleton.Conf = originalConf
		singleton.Localizer = originalLocalizer
		patConnectionRegistryShared = originalPATRegistry
	}
	return cleanup, user.ID
}

// mkToken 在测试库里创建一张 PAT 并返回其明文，供 scope / 白名单测试使用。
func mkToken(t *testing.T, uid uint64, scopes []string, serverIDs []uint64) (*model.APIToken, string) {
	t.Helper()
	plain := "nzp_" + strings.Repeat("a", 32) + "_" + ctoa(uid)
	tok := model.APIToken{UserID: uid, Name: "t", TokenHash: model.HashAPIToken(plain)}
	tok.SetScopes(scopes)
	if len(serverIDs) > 0 {
		tok.SetServerIDs(serverIDs)
	}
	require.NoError(t, singleton.DB.Create(&tok).Error)
	return &tok, plain
}

func ctoa(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
