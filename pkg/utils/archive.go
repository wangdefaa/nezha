package utils

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxArchiveTotalSize  = 300 << 20 // 解压后总大小上限，防解压炸弹
	maxArchiveFileCount  = 8000      // 文件数上限
	maxArchiveSingleSize = 100 << 20 // 单文件大小上限
)

var ErrArchiveTooLarge = errors.New("archive exceeds size or file-count limit")

// UnzipToDir 安全解压 zipPath 到 destDir（须已存在）。
// 防护：zip-slip 路径穿越、解压炸弹（总量/文件数/单文件上限）、拒绝软链接。
func UnzipToDir(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	cleanDest := filepath.Clean(destDir)
	var total uint64
	for i, f := range zr.File {
		if i+1 > maxArchiveFileCount {
			return ErrArchiveTooLarge
		}
		total += f.UncompressedSize64
		if total > maxArchiveTotalSize {
			return ErrArchiveTooLarge
		}
		if err := extractZipEntry(cleanDest, f); err != nil {
			return err
		}
	}
	return nil
}

// extractZipEntry 解出单个条目，写入限制在 cleanDest 内。
func extractZipEntry(cleanDest string, f *zip.File) error {
	target := filepath.Join(cleanDest, f.Name)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return errors.New("illegal file path in archive: " + f.Name)
	}
	if f.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive contains a symlink: " + f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return writeZipFile(target, f)
}

// writeZipFile 把单个 zip 文件落盘，单文件超限即报错。
func writeZipFile(target string, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()

	n, err := io.Copy(dst, io.LimitReader(rc, maxArchiveSingleSize+1))
	if err != nil {
		return err
	}
	if n > maxArchiveSingleSize {
		return ErrArchiveTooLarge
	}
	return nil
}
