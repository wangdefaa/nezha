package model

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	kmaps "github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/pkg/utils"
)

// JWTSecretEnvKey is the canonical environment variable that injects the JWT
// signing key. When set, the dashboard never writes the key to disk and the
// version-driven rotation in RotateJWTSecretKeyIfNeeded is skipped so that
// rotation is fully controlled by the operator / KMS.
const JWTSecretEnvKey = "NZ_JWTSECRETKEY" // #nosec G101 -- environment variable name, not a hardcoded secret value.

const (
	ConfigUsePeerIP                     = "NZ::Use-Peer-IP"
	JWTSecretKeyRotationBaselineVersion = "v2.0.13"
)

const (
	ConfigCoverAll = iota + 1
	ConfigCoverIgnoreAll
)

type ConfigForGuests struct {
	Language   string `koanf:"language" json:"language"` // 系统语言，默认 zh_CN
	SiteName   string `koanf:"site_name" json:"site_name"`
	CustomCode string `koanf:"custom_code" json:"custom_code,omitempty"`
}

type ConfigDashboard struct {
	InstallHost string `koanf:"install_host" json:"install_host,omitempty"`
	AgentTLS    bool   `koanf:"tls" json:"tls,omitempty"` // 用于前端判断生成的安装命令是否启用 TLS

	// DashboardHost 是 dashboard 对外访问的主机名，专用于 OAuth2 回调地址。
	// 它与 InstallHost（agent 连接用主机名）解耦：两者可以是不同域名。
	// 为空时，OAuth2 回调放行请求 Host（信任请求头），不做强制重写。
	DashboardHost string `koanf:"dashboard_host" json:"dashboard_host,omitempty"`

	WebRealIPHeader   string `koanf:"web_real_ip_header" json:"web_real_ip_header,omitempty"`     // 前端真实IP
	AgentRealIPHeader string `koanf:"agent_real_ip_header" json:"agent_real_ip_header,omitempty"` // Agent真实IP
	UserTemplate      string `koanf:"user_template" json:"user_template,omitempty"`               // 管理端固定内置 admin-dist，不可配置

	// Agent 安装脚本地址（留空使用内置默认脚本）
	InstallScriptLinux   string `koanf:"install_script_linux" json:"install_script_linux,omitempty"`     // Linux/macOS 安装脚本地址
	InstallScriptWindows string `koanf:"install_script_windows" json:"install_script_windows,omitempty"` // Windows 安装脚本地址

	EnablePlainIPInNotification bool `koanf:"enable_plain_ip_in_notification" json:"enable_plain_ip_in_notification,omitempty"` // 通知信息IP不打码

	// IP变更提醒
	EnableIPChangeNotification  bool   `koanf:"enable_ip_change_notification" json:"enable_ip_change_notification,omitempty"`
	IPChangeNotificationGroupID uint64 `koanf:"ip_change_notification_group_id" json:"ip_change_notification_group_id"`
	Cover                       uint8  `koanf:"cover" json:"cover"`                                               // 覆盖范围（0:提醒未被 IgnoredIPNotification 包含的所有服务器; 1:仅提醒被 IgnoredIPNotification 包含的服务器;）
	IgnoredIPNotification       string `koanf:"ignored_ip_notification" json:"ignored_ip_notification,omitempty"` // 特定服务器IP（多个服务器用逗号分隔）
}

type Config struct {
	ConfigForGuests
	ConfigDashboard

	AvgPingCount int `koanf:"avg_ping_count" json:"avg_ping_count,omitempty"`

	Debug          bool   `koanf:"debug" json:"debug,omitempty"`           // debug模式开关
	Location       string `koanf:"location" json:"location,omitempty"`     // 时区，默认为 Asia/Shanghai
	ForceAuth      bool   `koanf:"force_auth" json:"force_auth,omitempty"` // 强制要求认证
	AgentSecretKey string `koanf:"agent_secret_key" json:"agent_secret_key,omitempty"`
	JWTTimeout     int    `koanf:"jwt_timeout" json:"jwt_timeout,omitempty"` // JWT token过期时间（小时）

	// json:"-" 防止经 API 泄露；落盘只走 patchYAMLField（整表序列化会丢掉它）
	JWTSecretKey                   string `koanf:"jwt_secret_key" json:"-"`
	JWTSecretKeyLastRotatedVersion string `koanf:"jwt_secret_key_last_rotated_version" json:"jwt_secret_key_last_rotated_version,omitempty"`
	ListenPort                     uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	ListenHost                     string `koanf:"listen_host" json:"listen_host,omitempty"`

	jwtSecretFromEnv bool // 由 NZ_JWTSECRETKEY 注入：不落盘、不做版本轮换

	// oauth2 配置
	Oauth2 map[string]*Oauth2Config `koanf:"oauth2" json:"oauth2,omitempty"`

	// HTTPS 配置
	HTTPS HTTPSConf `koanf:"https" json:"https"`

	// TSDB 配置
	TSDB TSDBConf `koanf:"tsdb" json:"tsdb"`

	// 内存配置
	Memory MemoryConf `koanf:"memory" json:"memory"`

	// 数据库配置（type: sqlite/mysql/postgres）
	Database DatabaseConf `koanf:"database" json:"database"`

	k        *koanf.Koanf `json:"-"`
	filePath string       `json:"-"`
}

type HTTPSConf struct {
	ListenPort  uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	TLSCertPath string `koanf:"tls_cert_path" json:"tls_cert_path,omitempty"`
	TLSKeyPath  string `koanf:"tls_key_path" json:"tls_key_path,omitempty"`
}

// TSDBConf TSDB 配置
type TSDBConf struct {
	DataPath                 string  `koanf:"data_path" json:"data_path,omitempty"`
	RetentionDays            uint16  `koanf:"retention_days" json:"retention_days,omitempty"`
	MinFreeDiskSpaceGB       float64 `koanf:"min_free_disk_space_gb" json:"min_free_disk_space_gb,omitempty"`
	MaxMemoryMB              int64   `koanf:"max_memory_mb" json:"max_memory_mb,omitempty"`
	WriteBufferSize          int     `koanf:"write_buffer_size" json:"write_buffer_size,omitempty"`
	WriteBufferFlushInterval int     `koanf:"write_buffer_flush_interval" json:"write_buffer_flush_interval,omitempty"`
}

// MemoryConf 内存配置
type MemoryConf struct {
	// GoMemLimitMB Go 运行时内存限制(MB)，0 表示不限制
	GoMemLimitMB int64 `koanf:"go_mem_limit_mb" json:"go_mem_limit_mb,omitempty"`
}

// DatabaseConf 数据库配置
type DatabaseConf struct {
	// Type 数据库类型：sqlite(默认) / mysql / postgres
	Type string `koanf:"type" json:"type,omitempty"`
	// DSN 连接串。mysql/postgres 必填；sqlite 留空时回退到 -db 文件路径
	DSN string `koanf:"dsn" json:"dsn,omitempty"`
}

// Read 读取配置（env 与 yaml），补齐缺省值；缺失的密钥首启生成并写回 yaml。
func (c *Config) Read(path string, frontendTemplates []FrontendTemplate) error {
	c.k = koanf.New(".")
	c.filePath = path
	if err := c.load(); err != nil {
		return err
	}
	c.applyDefaults(frontendTemplates)
	if err := c.ensureJWTSecret(); err != nil {
		return err
	}
	return c.ensureAgentSecret()
}

// load 先载入 NZ_ 前缀环境变量，再合并 yaml 文件（文件不存在则跳过）。
func (c *Config) load() error {
	err := c.k.Load(env.Provider("NZ_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "NZ_")), "_", ".")
	}), nil)
	if err != nil {
		return err
	}
	if _, err := os.Stat(c.filePath); err == nil {
		err = c.k.Load(file.Provider(c.filePath), new(utils.KubeYAML), koanf.WithMergeFunc(mergeDedup))
		if err != nil {
			return err
		}
	}
	return c.k.UnmarshalWithConf("", c, koanfConf(c))
}

// applyDefaults 补齐缺省值；user_template 只能指向已登记的访客主题。
func (c *Config) applyDefaults(frontendTemplates []FrontendTemplate) {
	if c.ListenPort == 0 {
		c.ListenPort = 8008
	}
	if c.Language == "" {
		c.Language = "en_US"
	}
	if c.Location == "" {
		c.Location = "Asia/Shanghai"
	}
	if !slices.ContainsFunc(frontendTemplates, func(v FrontendTemplate) bool {
		return v.Path == c.UserTemplate && !v.IsAdmin
	}) {
		c.UserTemplate = DefaultUserTemplate
	}
	if c.AvgPingCount == 0 {
		c.AvgPingCount = 2
	}
	if c.Cover == 0 {
		c.Cover = 1
	}
	if c.JWTTimeout == 0 {
		c.JWTTimeout = 1
	}
}

// ensureJWTSecret 确定 JWT 签名密钥：env 注入优先且不落盘；否则用 yaml 里的值，缺失时生成并写回。
func (c *Config) ensureJWTSecret() error {
	if envSecret := os.Getenv(JWTSecretEnvKey); envSecret != "" {
		c.JWTSecretKey = envSecret
		c.jwtSecretFromEnv = true
		return nil
	}
	if c.JWTSecretKey != "" {
		log.Printf("NEZHA>> jwt_secret_key loaded from config.yaml; recommend injecting via env %s to keep it off disk", JWTSecretEnvKey)
		return nil
	}
	generated, err := utils.GenerateRandomString(1024)
	if err != nil {
		return err
	}
	c.JWTSecretKey = generated
	log.Printf("NEZHA>> generated new jwt_secret_key; wrote to config.yaml. For production, inject via env %s and remove the field from config.yaml.", JWTSecretEnvKey)
	return c.patchYAMLField("jwt_secret_key", generated)
}

// ensureAgentSecret 缺失时生成 agent 通信密钥并只补写这一个键。
func (c *Config) ensureAgentSecret() error {
	if c.AgentSecretKey != "" {
		return nil
	}
	secret, err := utils.GenerateRandomString(32)
	if err != nil {
		return err
	}
	c.AgentSecretKey = secret
	return c.patchYAMLField("agent_secret_key", secret)
}

// RotateJWTSecretKeyIfNeeded 升级跨过基线版本时轮换一次签名密钥，并记录已处理到的版本。
// env 注入的密钥与 debug 构建（版本号不可比较）跳过。
func (c *Config) RotateJWTSecretKeyIfNeeded(currentVersion string) (bool, error) {
	currentVersion = strings.TrimSpace(currentVersion)
	if c.jwtSecretFromEnv || compareVersion(currentVersion, JWTSecretKeyRotationBaselineVersion) < 0 {
		return false, nil
	}
	marker := c.JWTSecretKeyLastRotatedVersion
	shouldRotate := marker == "" || compareVersion(marker, JWTSecretKeyRotationBaselineVersion) < 0
	if !shouldRotate && marker == currentVersion {
		return false, nil
	}
	if shouldRotate {
		if err := c.rotateJWTSecret(); err != nil {
			return false, err
		}
	}
	c.JWTSecretKeyLastRotatedVersion = currentVersion
	if err := c.patchYAMLField("jwt_secret_key_last_rotated_version", currentVersion); err != nil {
		return false, err
	}
	return shouldRotate, nil
}

func (c *Config) rotateJWTSecret() error {
	secret, err := utils.GenerateRandomString(1024)
	if err != nil {
		return err
	}
	c.JWTSecretKey = secret
	return c.patchYAMLField("jwt_secret_key", secret)
}

// patchYAMLField 只改写 yaml 中的单个键，保留其余内容，文件权限 0600。
func (c *Config) patchYAMLField(key string, value any) error {
	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	raw := map[string]any{}
	if data, err := os.ReadFile(c.filePath); err == nil {
		if len(data) > 0 {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw[key] = value

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(c.filePath, out, 0600)
}

func compareVersion(left, right string) int {
	leftParts, leftOK := parseVersion(left)
	rightParts, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return -1
	}
	for i := range leftParts {
		if leftParts[i] < rightParts[i] {
			return -1
		}
		if leftParts[i] > rightParts[i] {
			return 1
		}
	}
	return 0
}

func parseVersion(version string) ([3]int, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var parsed [3]int
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}, false
		}
		parsed[i] = value
	}
	return parsed, true
}

func koanfConf(c any) koanf.UnmarshalConf {
	return koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				utils.TextUnmarshalerHookFunc()),
			Metadata:         nil,
			Result:           c,
			WeaklyTypedInput: true,
			MatchName: func(mapKey, fieldName string) bool {
				return strings.EqualFold(mapKey, fieldName) ||
					strings.EqualFold(mapKey, strings.ReplaceAll(fieldName, "_", ""))
			},
			Squash: true,
		},
	}
}

func mergeDedup(src, dst map[string]any) error {
	for key := range src {
		if strings.IndexByte(key, '_') == -1 {
			continue
		}

		oldKey := strings.ReplaceAll(key, "_", "")
		if _, ok := dst[oldKey]; ok {
			src[oldKey] = src[key]
			delete(src, key)
		}
	}

	kmaps.Merge(src, dst)
	return nil
}
