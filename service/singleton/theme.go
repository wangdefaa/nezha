package singleton

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	// MaxThemeArchiveSize 主题压缩包（上传 / GitHub 下载）大小上限，超出即报错而非静默截断。
	MaxThemeArchiveSize = 300 << 20

	// AdminTemplatePath 管理端固定使用内置 admin-dist（构建期嵌入、随面板发版），不入库、不可经面板更新。
	AdminTemplatePath = "admin-dist"
)

var (
	// builtinTemplates 由内置 frontend-templates.yaml 解出，作启动期兜底校验与首启 seed 源。
	builtinTemplates []model.FrontendTemplate

	// frontendTemplates 运行期访客主题清单（themes 表投影），整体 swap 保证读者不撕裂。
	frontendTemplates   []model.FrontendTemplate
	frontendTemplatesMu sync.RWMutex

	// themeSourceByPath: path -> source，供静态文件服务按来源分流（builtin 走 embed，余者走磁盘）。
	themeSourceByPath = map[string]model.ThemeSource{}
	themesMu          sync.RWMutex

	// ThemeDir 自定义主题磁盘根目录 = <dataDir>/themes，由 InitDBFromPath 设置。
	ThemeDir string
)

// GetFrontendTemplates 返回运行期主题清单快照（前端下拉 / 校验用）。
func GetFrontendTemplates() []model.FrontendTemplate {
	frontendTemplatesMu.RLock()
	defer frontendTemplatesMu.RUnlock()
	return slices.Clone(frontendTemplates)
}

// ThemeSourceOf 查主题来源；静态服务热路径用，O(1) 读内存。
func ThemeSourceOf(path string) (model.ThemeSource, bool) {
	themesMu.RLock()
	defer themesMu.RUnlock()
	s, ok := themeSourceByPath[path]
	return s, ok
}

// IsValidUserTemplate 校验 path 是已登记的访客主题（themes 表只存访客主题）。
func IsValidUserTemplate(path string) bool {
	frontendTemplatesMu.RLock()
	defer frontendTemplatesMu.RUnlock()
	return slices.ContainsFunc(frontendTemplates, func(t model.FrontendTemplate) bool {
		return t.Path == path
	})
}

// IsBuiltinPath 判断 path 是否为内置主题标识（含管理端；内置不可被上传/删除覆盖）。
func IsBuiltinPath(path string) bool {
	for _, t := range builtinTemplates {
		if t.Path == path {
			return true
		}
	}
	return false
}

// ReloadThemes 从 themes 表重建运行期清单与来源映射，整体 swap。
func ReloadThemes(db *gorm.DB) error {
	var themes []model.Theme
	if err := db.Find(&themes).Error; err != nil {
		return err
	}
	tpls := make([]model.FrontendTemplate, 0, len(themes))
	srcMap := make(map[string]model.ThemeSource, len(themes))
	for i := range themes {
		tpls = append(tpls, themes[i].ToFrontendTemplate())
		srcMap[themes[i].Path] = themes[i].Source
	}
	frontendTemplatesMu.Lock()
	frontendTemplates = tpls
	frontendTemplatesMu.Unlock()
	themesMu.Lock()
	themeSourceByPath = srcMap
	themesMu.Unlock()
	return nil
}

// SeedBuiltinThemes 把内置访客主题登记入库（per-path 幂等，版本升级可补种、不覆盖用户数据）。
// 管理端条目跳过：固定走内置 embed，不进主题管理。
func SeedBuiltinThemes(db *gorm.DB) error {
	for _, t := range builtinUserTemplates() {
		theme := model.Theme{
			Path: t.Path, Name: t.Name, Source: model.ThemeSourceBuiltin,
			Repository: t.Repository, Author: t.Author, VersionTag: t.Version,
			GithubRepo: t.GithubRepo, ReleaseAsset: t.ReleaseAsset,
			IsOfficial: t.IsOfficial,
		}
		// 已存在的内置记录补全 GitHub 来源（支持「内置主题也可更新」）；version_tag 由 syncBuiltinVersion 按是否有磁盘版决定。
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "path"}},
			DoUpdates: clause.AssignmentColumns([]string{"github_repo", "release_asset"}),
		}).Create(&theme).Error; err != nil {
			return err
		}
		if err := syncBuiltinVersion(db, t); err != nil {
			return err
		}
	}
	if err := reconcileBuiltinThemes(db); err != nil {
		return err
	}
	return purgeAdminThemes(db)
}

// syncBuiltinVersion 内置主题无磁盘覆盖版时，version_tag 跟随 yaml（二进制升级内置版本后展示实际在用版本）；
// 有面板拉取的磁盘版时它优先生效，版本号保持拉取时的 tag。
func syncBuiltinVersion(db *gorm.DB, t model.FrontendTemplate) error {
	if hasDiskTheme(t.Path) {
		return nil
	}
	return db.Model(&model.Theme{}).
		Where("path = ? AND source = ? AND (version_tag IS NULL OR version_tag <> ?)",
			t.Path, model.ThemeSourceBuiltin, t.Version).
		Update("version_tag", t.Version).Error
}

// hasDiskTheme 判断 <ThemeDir>/<path> 下是否有可用的磁盘主题（含 index.html，静态服务会优先读它）。
func hasDiskTheme(themePath string) bool {
	if ThemeDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(ThemeDir, themePath, "index.html"))
	return err == nil
}

// builtinUserTemplates 内置清单中的访客主题（剔除管理端条目）。
func builtinUserTemplates() []model.FrontendTemplate {
	out := make([]model.FrontendTemplate, 0, len(builtinTemplates))
	for _, t := range builtinTemplates {
		if !t.IsAdmin {
			out = append(out, t)
		}
	}
	return out
}

// reconcileBuiltinThemes 删除 yaml 已移除的内置主题记录及磁盘残留，防止重新部署（无 embed 兜底）
// 后访客切到这些主题报 404。管理端不在保留名单内，旧版入库的内置 admin-dist 记录及其面板更新版一并清掉。
// 仅作用于 builtin，绝不触碰用户上传/拉取的主题。
func reconcileBuiltinThemes(db *gorm.DB) error {
	keep := make([]string, 0, len(builtinTemplates))
	for _, t := range builtinUserTemplates() {
		keep = append(keep, t.Path)
	}
	if len(keep) == 0 {
		return nil // yaml 异常为空时不动库，避免误删全部内置
	}
	var stale []model.Theme
	if err := db.Where("source = ? AND path NOT IN ?",
		model.ThemeSourceBuiltin, keep).Find(&stale).Error; err != nil {
		return err
	}
	for i := range stale {
		if err := db.Delete(&stale[i]).Error; err != nil {
			return err
		}
		RemoveThemeDir(stale[i].Path)
	}
	return nil
}

// purgeAdminThemes 清理旧版遗留的管理端主题（is_admin=true 的上传/拉取记录及磁盘文件）。
// 模型已去掉 IsAdmin，但 AutoMigrate 不删列：旧库仍有该列时按列清理，新库无此列直接跳过。
func purgeAdminThemes(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&model.Theme{}, "is_admin") {
		return nil
	}
	var paths []string
	if err := db.Model(&model.Theme{}).Where("is_admin = ?", true).Pluck("path", &paths).Error; err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if err := db.Where("is_admin = ?", true).Delete(&model.Theme{}).Error; err != nil {
		return err
	}
	for _, p := range paths {
		RemoveThemeDir(p)
	}
	return nil
}

// ReconcileTemplateSelection 启动兜底：当前选中的访客主题若已不存在则回退默认。
func ReconcileTemplateSelection() error {
	if !IsValidUserTemplate(Conf.UserTemplate) {
		Conf.UserTemplate = model.DefaultUserTemplate
	}
	return nil
}

// SlugifyThemePath 规整主题标识：去 .zip 后缀，仅保留 [A-Za-z0-9._-]，其余转 -。
func SlugifyThemePath(name string) string {
	name = strings.TrimSuffix(name, ".zip")
	var b strings.Builder
	for _, r := range name {
		if isThemePathRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}

// isThemePathRune 主题目录名允许的字符（不含任何路径分隔符）。
func isThemePathRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}

// isValidThemeDirName 只接受 SlugifyThemePath 可能产出的目录名：非空、无分隔符、不以 . 或 - 开头结尾（排除 . 与 ..）。
func isValidThemeDirName(p string) bool {
	if p == "" || strings.Trim(p, ".-") != p {
		return false
	}
	for _, r := range p {
		if !isThemePathRune(r) {
			return false
		}
	}
	return true
}

// RemoveThemeDir 删除自定义主题的磁盘目录（忽略不存在）。path 来自数据库，
// 删除前再校验一次目录名，防止被污染的记录（如 "../x"、"."）删到 ThemeDir 之外或删掉 ThemeDir 本身。
func RemoveThemeDir(themePath string) {
	if ThemeDir == "" || !isValidThemeDirName(themePath) {
		return
	}
	os.RemoveAll(filepath.Join(ThemeDir, themePath))
}

// InstallThemeFromZip 解压本地 zip 到 <ThemeDir>/<themePath>（原子替换）。
func InstallThemeFromZip(srcZipPath, themePath string) error {
	return installThemeArchive(themePath, func(tmp string) error {
		return utils.UnzipToDir(srcZipPath, tmp)
	})
}

// InstallThemeFromGithub 拉取 GitHub latest release 的指定资产并安装，返回版本 tag。
func InstallThemeFromGithub(repo, asset, themePath string) (string, error) {
	owner, name, err := parseGithubRepo(repo)
	if err != nil {
		return "", err
	}
	tag, assetURL, err := fetchLatestReleaseAsset(owner, name, asset)
	if err != nil {
		return "", err
	}
	zipPath, err := downloadToTemp(assetURL)
	if err != nil {
		return "", err
	}
	defer os.Remove(zipPath)
	if err := InstallThemeFromZip(zipPath, themePath); err != nil {
		return "", err
	}
	return tag, nil
}

// installThemeArchive 把内容填充到临时目录后原子替换最终目录，失败回滚。
func installThemeArchive(themePath string, fill func(tmpDir string) error) error {
	if ThemeDir == "" {
		return errors.New("theme dir not initialized")
	}
	if !isValidThemeDirName(themePath) {
		return errors.New("invalid theme path")
	}
	if err := os.MkdirAll(ThemeDir, 0o750); err != nil {
		return err
	}
	rnd, err := utils.GenerateRandomString(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(ThemeDir, ".tmp-"+themePath+"-"+rnd)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return err
	}
	if err := fill(tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	root := themeContentRoot(tmp)
	err = swapDir(root, filepath.Join(ThemeDir, themePath))
	if root != tmp {
		os.RemoveAll(tmp)
	}
	return err
}

// themeContentRoot 处理 zip 单层目录包裹（如 dist/）：dir 缺 index.html 但仅含一个内有 index.html 的子目录时，返回该子目录。
func themeContentRoot(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return dir
	}
	sub := filepath.Join(dir, entries[0].Name())
	if _, err := os.Stat(filepath.Join(sub, "index.html")); err == nil {
		return sub
	}
	return dir
}

// swapDir 用 tmp 原子替换 final：旧目录先挪走→新就位→删旧，兼容 final 已存在与 Windows。
func swapDir(tmp, final string) error {
	old := final + ".old-" + filepath.Base(tmp)
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, old); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Rename(old, final)
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// parseGithubRepo 把 owner/repo 或完整 URL 解析为 owner、repo。
func parseGithubRepo(repo string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "http://github.com/")
	repo = strings.TrimSuffix(strings.Trim(repo, "/"), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("invalid github repository")
	}
	return parts[0], parts[1], nil
}

// fetchLatestReleaseAsset 取 latest release 的 tag 与匹配资产的下载地址。
func fetchLatestReleaseAsset(owner, name, asset string) (string, string, error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, name)
	body, err := githubGet(api)
	if err != nil {
		return "", "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", err
	}
	for _, a := range rel.Assets {
		if a.Name == asset {
			return rel.TagName, a.URL, nil
		}
	}
	return "", "", errors.New("github release asset not found")
}

// hostAllowed GitHub 下载域名白名单（asset 会 302 到 *.githubusercontent.com）。
func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	switch host {
	case "api.github.com", "github.com", "codeload.github.com":
		return true
	}
	return strings.HasSuffix(host, ".githubusercontent.com")
}

// restrictedFetch 受限 GET，手动逐跳跟随重定向（≤5），每跳校验 host 白名单 + SSRF（私网/DNS rebinding）。
func restrictedFetch(rawURL string) (*http.Response, error) {
	for range 6 {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		if !hostAllowed(u.Hostname()) {
			return nil, errors.New("host not allowed: " + u.Hostname())
		}
		client, err := utils.NewRestrictedHTTPClient(rawURL, false)
		if err != nil {
			return nil, err
		}
		req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
		req.Header.Set("User-Agent", "nezha-dashboard")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return nil, errors.New("redirect without location")
		}
		rawURL = loc
	}
	return nil, errors.New("too many redirects")
}

// githubGet 拉取 JSON 文本（限大小）。
func githubGet(rawURL string) ([]byte, error) {
	resp, err := restrictedFetch(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// downloadToTemp 下载资产到临时 zip 文件，返回路径（调用方负责删除）。
func downloadToTemp(rawURL string) (string, error) {
	resp, err := restrictedFetch(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download status %d", resp.StatusCode)
	}
	return SaveLimitedTemp(resp.Body, "nz-theme-*.zip")
}

// ErrThemeArchiveTooLarge 主题压缩包超过 MaxThemeArchiveSize。
var ErrThemeArchiveTooLarge = errors.New("theme archive exceeds size limit")

// SaveLimitedTemp 把 src 写入临时文件，超过 MaxThemeArchiveSize 即删除并报错；成功返回路径（调用方负责删除）。
func SaveLimitedTemp(src io.Reader, pattern string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(src, MaxThemeArchiveSize+1))
	if err == nil && n > MaxThemeArchiveSize {
		err = ErrThemeArchiveTooLarge
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
