package controller

import (
	"io"
	"mime/multipart"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List themes
// @Summary List themes
// @Security BearerAuth
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Theme]
// @Router /theme [get]
func listTheme(c *gin.Context) ([]model.Theme, error) {
	var themes []model.Theme
	if err := singleton.DB.Order("id").Find(&themes).Error; err != nil {
		return nil, err
	}
	return themes, nil
}

// Upload theme zip
// @Summary Upload theme zip (multipart field "file")
// @Security BearerAuth
// @Tags admin required
// @Accept multipart/form-data
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /theme/upload [post]
func uploadTheme(c *gin.Context) (uint64, error) {
	file, err := c.FormFile("file")
	if err != nil {
		return 0, err
	}
	path := singleton.SlugifyThemePath(file.Filename)
	if path == "" || singleton.IsBuiltinPath(path) {
		return 0, singleton.Localizer.ErrorT("invalid theme name")
	}
	tmp, err := saveUploadToTemp(file)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	if err := singleton.InstallThemeFromZip(tmp, path); err != nil {
		return 0, err
	}
	return upsertTheme(c, model.Theme{
		Path:       path,
		Name:       strings.TrimSuffix(file.Filename, ".zip"),
		Source:     model.ThemeSourceUpload,
		VersionTag: c.PostForm("version"),
		IsAdmin:    c.PostForm("is_admin") == "true",
	})
}

// Create theme from GitHub release
// @Summary Create theme from GitHub release
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @Param body body model.ThemeGithubForm true "ThemeGithubForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /theme/github [post]
func createGithubTheme(c *gin.Context) (uint64, error) {
	var f model.ThemeGithubForm
	if err := c.ShouldBindJSON(&f); err != nil {
		return 0, err
	}
	name := f.Name
	if name == "" {
		name = githubRepoName(f.GithubRepo)
	}
	path := singleton.SlugifyThemePath(name)
	if path == "" || singleton.IsBuiltinPath(path) {
		return 0, singleton.Localizer.ErrorT("invalid theme name")
	}
	tag, err := singleton.InstallThemeFromGithub(f.GithubRepo, f.ReleaseAsset, path)
	if err != nil {
		return 0, err
	}
	return upsertTheme(c, model.Theme{
		Path: path, Name: name, Source: model.ThemeSourceGithub,
		GithubRepo: f.GithubRepo, ReleaseAsset: f.ReleaseAsset, VersionTag: tag,
		IsAdmin: f.IsAdmin, Repository: githubRepoURL(f.GithubRepo),
	})
}

// Refresh GitHub theme to latest release
// @Summary Refresh GitHub theme to latest release
// @Security BearerAuth
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /theme/{id}/refresh [post]
func refreshTheme(c *gin.Context) (any, error) {
	t, err := themeByParam(c)
	if err != nil {
		return nil, err
	}
	if t.GithubRepo == "" || t.ReleaseAsset == "" {
		return nil, singleton.Localizer.ErrorT("only github themes can be refreshed")
	}
	tag, err := singleton.InstallThemeFromGithub(t.GithubRepo, t.ReleaseAsset, t.Path)
	if err != nil {
		return nil, err
	}
	t.VersionTag = tag
	if err := singleton.DB.Save(&t).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	return nil, singleton.ReloadThemes(singleton.DB)
}

// Apply theme as current user (guest) template
// @Summary Set theme as current guest template
// @Security BearerAuth
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /theme/{id}/apply [post]
func applyTheme(c *gin.Context) (any, error) {
	t, err := themeByParam(c)
	if err != nil {
		return nil, err
	}
	if t.IsAdmin {
		singleton.Conf.AdminTemplate = t.Path
	} else {
		singleton.Conf.UserTemplate = t.Path
	}
	if err := singleton.Conf.SaveDynamicToDB(singleton.DB); err != nil {
		return nil, newGormError("%v", err)
	}
	return nil, nil
}

// Batch delete themes
// @Summary Batch delete themes (builtin/in-use rejected)
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/theme [post]
func batchDeleteTheme(c *gin.Context) (any, error) {
	var ids []uint64
	if err := c.ShouldBindJSON(&ids); err != nil {
		return nil, err
	}
	var themes []model.Theme
	if err := singleton.DB.Where("id in (?)", ids).Find(&themes).Error; err != nil {
		return nil, err
	}
	for _, t := range themes {
		if t.Source == model.ThemeSourceBuiltin {
			return nil, singleton.Localizer.ErrorT("builtin theme cannot be deleted")
		}
		if t.Path == singleton.Conf.UserTemplate || t.Path == singleton.Conf.AdminTemplate {
			return nil, singleton.Localizer.ErrorT("theme in use")
		}
	}
	if err := singleton.DB.Unscoped().Delete(&model.Theme{}, "id in (?)", ids).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	for _, t := range themes {
		singleton.RemoveThemeDir(t.Path)
	}
	return nil, singleton.ReloadThemes(singleton.DB)
}

// themeByParam 按路径参数 id 取主题。
func themeByParam(c *gin.Context) (model.Theme, error) {
	var t model.Theme
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		return t, err
	}
	if err := singleton.DB.First(&t, id).Error; err != nil {
		return t, err
	}
	return t, nil
}

// upsertTheme 按 path 落库（存在则更新）并刷新运行期清单。
func upsertTheme(c *gin.Context, t model.Theme) (uint64, error) {
	t.UserID = getUid(c)
	var existing model.Theme
	if err := singleton.DB.Where("path = ?", t.Path).First(&existing).Error; err == nil {
		t.ID = existing.ID
		t.CreatedAt = existing.CreatedAt
	}
	if err := singleton.DB.Save(&t).Error; err != nil {
		return 0, newGormError("%v", err)
	}
	if err := singleton.ReloadThemes(singleton.DB); err != nil {
		return 0, newGormError("%v", err)
	}
	return t.ID, nil
}

// saveUploadToTemp 把上传的 multipart 文件落到临时 zip。
func saveUploadToTemp(file *multipart.FileHeader) (string, error) {
	dst, err := os.CreateTemp("", "nz-upload-*.zip")
	if err != nil {
		return "", err
	}
	defer dst.Close()
	src, err := file.Open()
	if err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	defer src.Close()
	if _, err := io.Copy(dst, src); err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	return dst.Name(), nil
}

func githubRepoName(repo string) string {
	repo = strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func githubRepoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") {
		return repo
	}
	return "https://github.com/" + strings.Trim(repo, "/")
}
