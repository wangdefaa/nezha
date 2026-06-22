package model

// ThemeSource 主题来源类型。用裸 string 作 GORM 列，避免自定义枚举类型的跨库麻烦。
type ThemeSource = string

const (
	ThemeSourceBuiltin ThemeSource = "builtin" // 内置（嵌入二进制，出厂兜底）
	ThemeSourceUpload  ThemeSource = "upload"  // 后台上传 zip
	ThemeSourceGithub  ThemeSource = "github"  // 从 GitHub release 拉取
)

// Theme 主题清单（仅元信息）。文件内容存磁盘 <ThemeDir>/<Path>/，内置主题则由二进制 embed 提供。
// 取代写死的 frontend-templates.yaml 成为运行期权威清单，从而支持后台动态增删主题、无需重新发版。
type Theme struct {
	Common
	Path         string `gorm:"uniqueIndex;size:191" json:"path"` // 磁盘目录名 / 唯一标识
	Name         string `json:"name"`
	Source       string `gorm:"size:20" json:"source"` // builtin/upload/github
	GithubRepo   string `json:"github_repo,omitempty"` // owner/repo
	ReleaseAsset string `json:"release_asset,omitempty"`
	VersionTag   string `json:"version_tag,omitempty"`
	Author       string `json:"author,omitempty"`
	Repository   string `json:"repository,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsOfficial   bool   `json:"is_official"`
}

// ToFrontendTemplate 投影到对外契约 FrontendTemplate（前端 settings 主题下拉数据源，保持不变）。
func (t *Theme) ToFrontendTemplate() FrontendTemplate {
	return FrontendTemplate{
		Path:       t.Path,
		Name:       t.Name,
		Repository: t.Repository,
		Author:     t.Author,
		Version:    t.VersionTag,
		IsAdmin:    t.IsAdmin,
		IsOfficial: t.IsOfficial,
	}
}

// ThemeGithubForm 新建/刷新 GitHub release 主题的表单。
type ThemeGithubForm struct {
	GithubRepo   string `json:"github_repo" binding:"required"`   // owner/repo 或完整 URL
	ReleaseAsset string `json:"release_asset" binding:"required"` // release 资产文件名（.zip）
	Name         string `json:"name,omitempty"`                   // 留空用 repo 名
	IsAdmin      bool   `json:"is_admin,omitempty"`
}
