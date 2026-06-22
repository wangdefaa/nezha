package singleton

import (
	"slices"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// newThemeReconcileDB 建共享缓存内存库并迁移 themes 表。
func newThemeReconcileDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Theme{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// swapBuiltins 临时把 builtinTemplates 设为给定 path 集合，返回还原函数。
func swapBuiltins(paths ...string) func() {
	orig := builtinTemplates
	tpls := make([]model.FrontendTemplate, len(paths))
	for i, p := range paths {
		tpls[i] = model.FrontendTemplate{Path: p}
	}
	builtinTemplates = tpls
	return func() { builtinTemplates = orig }
}

// TestReconcileBuiltinThemes 验证 yaml 已移除的内置主题被清理，用户主题与在册内置主题保留。
func TestReconcileBuiltinThemes(t *testing.T) {
	db := newThemeReconcileDB(t)
	preset := []model.Theme{
		{Path: "user-dist", Source: model.ThemeSourceBuiltin},
		{Path: "admin-dist", Source: model.ThemeSourceBuiltin, IsAdmin: true},
		{Path: "nazhua-dist", Source: model.ThemeSourceBuiltin}, // yaml 已移除 → 应删
		{Path: "my-upload", Source: model.ThemeSourceUpload},    // 用户主题 → 保留
	}
	if err := db.Create(&preset).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer swapBuiltins("admin-dist", "user-dist")()

	if err := reconcileBuiltinThemes(db); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var paths []string
	db.Model(&model.Theme{}).Order("path").Pluck("path", &paths)
	if want := []string{"admin-dist", "my-upload", "user-dist"}; !slices.Equal(paths, want) {
		t.Fatalf("got %v, want %v", paths, want)
	}
}

// TestReconcileBuiltinThemesEmptyKeepNoop 兜底：builtinTemplates 异常为空时不得删除任何记录。
func TestReconcileBuiltinThemesEmptyKeepNoop(t *testing.T) {
	db := newThemeReconcileDB(t)
	db.Create(&model.Theme{Path: "nazhua-dist", Source: model.ThemeSourceBuiltin})
	defer swapBuiltins()()

	if err := reconcileBuiltinThemes(db); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var n int64
	db.Model(&model.Theme{}).Count(&n)
	if n != 1 {
		t.Fatalf("empty keep should be no-op, got count=%d", n)
	}
}
