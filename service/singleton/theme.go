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

const maxThemeDownloadSize = 300 << 20 // 主题包下载上限

var (
	// builtinTemplates 由内置 frontend-templates.yaml 解出，作启动期兜底校验与首启 seed 源。
	builtinTemplates []model.FrontendTemplate

	// frontendTemplates 运行期权威清单（themes 表投影），整体 swap 保证读者不撕裂。
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

func hasTemplate(path string, wantAdmin bool) bool {
	frontendTemplatesMu.RLock()
	defer frontendTemplatesMu.RUnlock()
	for _, t := range frontendTemplates {
		if t.Path == path && t.IsAdmin == wantAdmin {
			return true
		}
	}
	return false
}

// IsValidUserTemplate 校验 path 是合法的访客主题（存在且非 admin）。
func IsValidUserTemplate(path string) bool { return hasTemplate(path, false) }

// IsBuiltinPath 判断 path 是否为内置主题标识（内置不可被上传/删除覆盖）。
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

// SeedBuiltinThemes 把内置主题登记入库（per-path DoNothing 幂等，版本升级可补种、不覆盖用户数据）。
func SeedBuiltinThemes(db *gorm.DB) error {
	for _, t := range builtinTemplates {
		theme := model.Theme{
			Path: t.Path, Name: t.Name, Source: model.ThemeSourceBuiltin,
			Repository: t.Repository, Author: t.Author, VersionTag: t.Version,
			GithubRepo: t.GithubRepo, ReleaseAsset: t.ReleaseAsset,
			IsAdmin: t.IsAdmin, IsOfficial: t.IsOfficial,
		}
		// 已存在的内置记录补全 GitHub 来源（支持「内置主题也可更新」），不动 version_tag 等用户态。
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "path"}},
			DoUpdates: clause.AssignmentColumns([]string{"github_repo", "release_asset"}),
		}).Create(&theme).Error; err != nil {
			return err
		}
	}
	return reconcileBuiltinThemes(db)
}

// reconcileBuiltinThemes 删除 yaml 已移除的内置主题记录及磁盘残留，防止重新部署（无 embed 兜底）
// 后访客切到这些主题报 404。仅作用于 builtin，绝不触碰用户上传/拉取的主题。
func reconcileBuiltinThemes(db *gorm.DB) error {
	keep := make([]string, 0, len(builtinTemplates))
	for _, t := range builtinTemplates {
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

// ReconcileTemplateSelection 启动兜底：当前选中的主题若已不存在则回退默认。
func ReconcileTemplateSelection() error {
	if !IsValidUserTemplate(Conf.UserTemplate) {
		Conf.UserTemplate = "user-dist"
	}
	if !hasTemplate(Conf.AdminTemplate, true) {
		Conf.AdminTemplate = "admin-dist"
	}
	return nil
}

// SlugifyThemePath 规整主题标识：去 .zip 后缀，仅保留 [A-Za-z0-9._-]，其余转 -。
func SlugifyThemePath(name string) string {
	name = strings.TrimSuffix(name, ".zip")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}

// RemoveThemeDir 删除自定义主题的磁盘目录（忽略不存在）。
func RemoveThemeDir(themePath string) {
	if ThemeDir == "" || themePath == "" {
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
	if err := os.MkdirAll(ThemeDir, 0o755); err != nil {
		return err
	}
	rnd, err := utils.GenerateRandomString(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(ThemeDir, ".tmp-"+themePath+"-"+rnd)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
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
	f, err := os.CreateTemp("", "nz-theme-*.zip")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxThemeDownloadSize)); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
