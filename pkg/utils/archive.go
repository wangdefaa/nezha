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
// 防护：os.Root 把写入内核级限制在 destDir 内（zip-slip）、解压炸弹（总量/文件数/单文件上限）、拒绝软链接。
func UnzipToDir(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	root, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer root.Close()

	var total uint64
	for i, f := range zr.File {
		if i+1 > maxArchiveFileCount {
			return ErrArchiveTooLarge
		}
		total += f.UncompressedSize64
		if total > maxArchiveTotalSize {
			return ErrArchiveTooLarge
		}
		if err := extractZipEntry(root, f); err != nil {
			return err
		}
	}
	return nil
}

// extractZipEntry 解出单个条目：先显式拒绝软链接与 .. 穿越，再经 os.Root 限制在根内写入。
func extractZipEntry(root *os.Root, f *zip.File) error {
	if f.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive contains a symlink: " + f.Name)
	}
	name := filepath.Clean(f.Name)
	if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
		return errors.New("illegal file path in archive: " + f.Name)
	}
	if f.FileInfo().IsDir() {
		return root.MkdirAll(name, 0o750)
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return writeZipFile(root, name, f)
}

// writeZipFile 把单个 zip 文件经 os.Root 落盘，单文件超限即报错。
func writeZipFile(root *os.Root, name string, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dst, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
