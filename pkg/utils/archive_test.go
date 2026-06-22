package utils

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return zipPath
}

func TestUnzipToDirNormal(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"index.html":    "<html>",
		"assets/app.js": "console.log(1)",
	})
	dest := t.TempDir()
	if err := UnzipToDir(zipPath, dest); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "assets", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "console.log(1)" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestUnzipToDirRejectsZipSlip(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{"../evil.txt": "pwned"})
	dest := t.TempDir()
	err := UnzipToDir(zipPath, dest)
	if err == nil || !strings.Contains(err.Error(), "illegal file path") {
		t.Fatalf("expected zip-slip rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); statErr == nil {
		t.Fatal("zip-slip escaped destination directory")
	}
}
