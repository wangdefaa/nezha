package singleton

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归（NZ-AUDIT-005）：RemoveThemeDir 直接 RemoveAll(ThemeDir + 库内 path)，
// 被污染的记录（如迁移脚本写入 "../x"）会删到主题目录之外。
func TestRemoveThemeDirStaysInsideThemeDir(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	orig := ThemeDir
	ThemeDir = filepath.Join(base, "themes")
	t.Cleanup(func() { ThemeDir = orig })
	if err := os.MkdirAll(filepath.Join(ThemeDir, "ok-theme"), 0o750); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"../victim", "..", ".", "a/../../victim", `..\victim`} {
		RemoveThemeDir(p)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("主题目录之外的内容被删除: %v", err)
	}
	if _, err := os.Stat(ThemeDir); err != nil {
		t.Fatalf("ThemeDir 本身不得被删除: %v", err)
	}
	RemoveThemeDir("ok-theme")
	if _, err := os.Stat(filepath.Join(ThemeDir, "ok-theme")); !os.IsNotExist(err) {
		t.Fatalf("合法主题目录应被删除, err=%v", err)
	}
}

func TestInstallThemeArchiveRejectsUnsafePath(t *testing.T) {
	orig := ThemeDir
	ThemeDir = filepath.Join(t.TempDir(), "themes")
	t.Cleanup(func() { ThemeDir = orig })
	called := false
	err := installThemeArchive("../escape", func(string) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("非法主题目录名必须在落盘前拒绝: err=%v called=%v", err, called)
	}
}
