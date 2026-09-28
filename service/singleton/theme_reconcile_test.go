package singleton

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/glebarez/sqlite"
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

// swapBuiltins 临时把 builtinTemplates 设为给定清单，返回还原函数。
func swapBuiltins(tpls ...model.FrontendTemplate) func() {
	orig := builtinTemplates
	builtinTemplates = tpls
	return func() { builtinTemplates = orig }
}

// swapThemeDir 临时把 ThemeDir 指向测试目录，返回还原函数。
func swapThemeDir(dir string) func() {
	orig := ThemeDir
	ThemeDir = dir
	return func() { ThemeDir = orig }
}

// themePaths 按 path 排序列出 themes 表全部记录。
func themePaths(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var paths []string
	if err := db.Model(&model.Theme{}).Order("path").Pluck("path", &paths).Error; err != nil {
		t.Fatalf("pluck: %v", err)
	}
	return paths
}

var testBuiltins = []model.FrontendTemplate{{Path: "admin-dist", IsAdmin: true}, {Path: "user-dist"}}

// TestReconcileBuiltinThemes 验证 yaml 已移除的内置主题与旧版入库的内置管理端被清理，用户主题与在册内置访客主题保留。
func TestReconcileBuiltinThemes(t *testing.T) {
	db := newThemeReconcileDB(t)
	preset := []model.Theme{
		{Path: "user-dist", Source: model.ThemeSourceBuiltin},
		{Path: "admin-dist", Source: model.ThemeSourceBuiltin},  // 管理端不再入库 → 应删
		{Path: "nazhua-dist", Source: model.ThemeSourceBuiltin}, // yaml 已移除 → 应删
		{Path: "my-upload", Source: model.ThemeSourceUpload},    // 用户主题 → 保留
	}
	if err := db.Create(&preset).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer swapBuiltins(testBuiltins...)()
	defer swapThemeDir(t.TempDir())()

	if err := reconcileBuiltinThemes(db); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, want := themePaths(t, db), []string{"my-upload", "user-dist"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestSeedBuiltinThemesSkipsAdmin 首启 seed 只登记内置访客主题，管理端不入库。
func TestSeedBuiltinThemesSkipsAdmin(t *testing.T) {
	db := newThemeReconcileDB(t)
	defer swapBuiltins(testBuiltins...)()

	if err := SeedBuiltinThemes(db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got, want := themePaths(t, db), []string{"user-dist"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestSeedBuiltinThemesSyncsVersion 内置版本随二进制升级：无磁盘版时 version_tag 跟随 yaml，有面板拉取的磁盘版时保持原 tag。
func TestSeedBuiltinThemesSyncsVersion(t *testing.T) {
	db := newThemeReconcileDB(t)
	preset := []model.Theme{
		{Path: "user-dist", Source: model.ThemeSourceBuiltin, VersionTag: "v2.3.1"},
		{Path: "pulled-dist", Source: model.ThemeSourceBuiltin, VersionTag: "v9.9.9"},
	}
	if err := db.Create(&preset).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	defer swapThemeDir(dir)()
	writeIndex(t, filepath.Join(dir, "pulled-dist"))
	defer swapBuiltins(
		model.FrontendTemplate{Path: "user-dist", Version: "v2.4.3"},
		model.FrontendTemplate{Path: "pulled-dist", Version: "v2.4.3"},
	)()

	if err := SeedBuiltinThemes(db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for path, want := range map[string]string{"user-dist": "v2.4.3", "pulled-dist": "v9.9.9"} {
		var got model.Theme
		db.Where("path = ?", path).First(&got)
		if got.VersionTag != want {
			t.Fatalf("%s version_tag = %q, want %q", path, got.VersionTag, want)
		}
	}
}

// writeIndex 在 dir 下写一个 index.html，模拟面板拉取落盘的主题。
func writeIndex(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// legacyTheme 模拟旧版 themes 表结构（含已移除的 is_admin 列）。
type legacyTheme struct {
	model.Theme
	IsAdmin bool
}

func (legacyTheme) TableName() string { return "themes" }

// TestPurgeAdminThemes 旧库遗留的管理端主题（上传/拉取/内置）连同磁盘目录被清理，访客主题不受影响。
func TestPurgeAdminThemes(t *testing.T) {
	db := newThemeReconcileDB(t)
	if err := db.AutoMigrate(&legacyTheme{}); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	preset := []legacyTheme{
		{Theme: model.Theme{Path: "user-dist", Source: model.ThemeSourceBuiltin}},
		{Theme: model.Theme{Path: "admin-dist", Source: model.ThemeSourceBuiltin}, IsAdmin: true},
		{Theme: model.Theme{Path: "my-admin", Source: model.ThemeSourceUpload}, IsAdmin: true},
		{Theme: model.Theme{Path: "my-user", Source: model.ThemeSourceUpload}},
	}
	if err := db.Create(&preset).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	defer swapThemeDir(dir)()
	for _, p := range []string{"admin-dist", "my-admin", "my-user"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	if err := purgeAdminThemes(db); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got, want := themePaths(t, db), []string{"my-user", "user-dist"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	assertThemeDirs(t, dir, map[string]bool{"admin-dist": false, "my-admin": false, "my-user": true})
}

// assertThemeDirs 校验各主题目录的存在性符合预期。
func assertThemeDirs(t *testing.T, dir string, want map[string]bool) {
	t.Helper()
	for p, exist := range want {
		_, err := os.Stat(filepath.Join(dir, p))
		if got := err == nil; got != exist {
			t.Fatalf("dir %s exist=%v, want %v", p, got, exist)
		}
	}
}

// TestPurgeAdminThemesNoLegacyColumn 新库无 is_admin 列时直接跳过、不报错。
func TestPurgeAdminThemesNoLegacyColumn(t *testing.T) {
	db := newThemeReconcileDB(t)
	db.Create(&model.Theme{Path: "user-dist", Source: model.ThemeSourceBuiltin})

	if err := purgeAdminThemes(db); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got, want := themePaths(t, db), []string{"user-dist"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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
