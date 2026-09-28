package model

import (
	"os"
	"strings"
	"testing"
)

func TestReadConfig(t *testing.T) {
	t.Run("ReadEmptyConfig", func(t *testing.T) {
		file := newTempConfig(t, "")
		c := &Config{}

		if err := c.Read(file, nil); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey != ""},
			{"user_template", c.UserTemplate, c.UserTemplate == "user-dist"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey != ""},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadFile", func(t *testing.T) {
		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick"

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
		}
		file := newTempConfig(t, testCfg)
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test"},
			{"user_template", c.UserTemplate, c.UserTemplate == "um"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none"},
			{"site_name", c.SiteName, c.SiteName == "lowkick"},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	// 管理端固定内置：旧配置残留的 admin_template 被忽略，user_template 也不能指向管理端条目。
	t.Run("UserTemplateRejectsAdminEntry", func(t *testing.T) {
		file := newTempConfig(t, "user_template: am\nadmin_template: am")
		defer os.Remove(file)
		c := &Config{}
		if err := c.Read(file, []FrontendTemplate{{Path: "um"}, {Path: "am", IsAdmin: true}}); err != nil {
			t.Fatalf("read config failed: %v", err)
		}
		if c.UserTemplate != "user-dist" {
			t.Fatalf("user_template = %q, want fallback user-dist", c.UserTemplate)
		}
	})

	// 回归：首启同时生成两个密钥时，补写 agent_secret_key 不能抹掉刚写入的 jwt_secret_key，
	// 否则第二次启动（debug 构建不做版本轮换）会重新生成，已签发会话全部失效。
	t.Run("GeneratedSecretsSurviveRestart", func(t *testing.T) {
		file := newTempConfig(t, "")
		defer os.Remove(file)
		first := &Config{}
		if err := first.Read(file, nil); err != nil {
			t.Fatalf("first read failed: %v", err)
		}
		second := &Config{}
		if err := second.Read(file, nil); err != nil {
			t.Fatalf("second read failed: %v", err)
		}
		if second.JWTSecretKey != first.JWTSecretKey {
			t.Fatal("jwt_secret_key regenerated on second start")
		}
		if second.AgentSecretKey != first.AgentSecretKey {
			t.Fatal("agent_secret_key regenerated on second start")
		}
	})

	// env 注入的 JWT 密钥永不落盘。
	t.Run("EnvJWTSecretNotPersisted", func(t *testing.T) {
		t.Setenv(JWTSecretEnvKey, "from-env")
		file := newTempConfig(t, "")
		defer os.Remove(file)
		c := &Config{}
		if err := c.Read(file, nil); err != nil {
			t.Fatalf("read failed: %v", err)
		}
		saved, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read saved config: %v", err)
		}
		if strings.Contains(string(saved), "jwt_secret_key") || c.AgentSecretKey == "" {
			t.Fatalf("unexpected saved config: %s", saved)
		}
	})

	t.Run("ReadEnv", func(t *testing.T) {
		t.Setenv("NZ_JWTSECRETKEY", "test")
		t.Setenv("NZ_USERTEMPLATE", "um")
		t.Setenv("NZ_AGENTSECRETKEY", "none")
		t.Setenv("NZ_HTTPS_LISTENPORT", "9876")

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
		}
		file := newTempConfig(t, "")
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test"},
			{"user_template", c.UserTemplate, c.UserTemplate == "um"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none"},
			{"https.listenport", c.HTTPS.ListenPort, c.HTTPS.ListenPort == 9876},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadEnvFile", func(t *testing.T) {
		t.Setenv("NZ_JWTSECRETKEY", "test1")
		t.Setenv("NZ_USERTEMPLATE", "um1")
		t.Setenv("NZ_AGENTSECRETKEY", "none1")
		t.Setenv("NZ_SITENAME", "lowkick1")

		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick"

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
			{Path: "um1"},
			{Path: "am1", IsAdmin: true},
		}
		file := newTempConfig(t, testCfg)
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test1"},
			{"jwt_secret_from_env", c.jwtSecretFromEnv, c.jwtSecretFromEnv},
			{"user_template", c.UserTemplate, c.UserTemplate == "um1" || c.UserTemplate == "um"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none" || c.AgentSecretKey == "none1"},
			{"site_name", c.SiteName, c.SiteName == "lowkick" || c.SiteName == "lowkick1"},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})
}

func TestRotateJWTSecretKeyIfNeeded(t *testing.T) {
	tests := []struct {
		name               string
		initialMarker      string
		currentVersion     string
		wantRotated        bool
		wantStoredVersion  string
		wantSecretChanged  bool
		wantSavedConfigKey bool
	}{
		{
			name:               "empty marker rotates leaked secret",
			currentVersion:     "v2.0.13",
			wantRotated:        true,
			wantStoredVersion:  "v2.0.13",
			wantSecretChanged:  true,
			wantSavedConfigKey: true,
		},
		{
			name:               "old marker rotates leaked secret",
			initialMarker:      "v2.0.12",
			currentVersion:     "v2.0.14",
			wantRotated:        true,
			wantStoredVersion:  "v2.0.14",
			wantSecretChanged:  true,
			wantSavedConfigKey: true,
		},
		{
			name:               "threshold marker keeps secret and advances marker",
			initialMarker:      "v2.0.13",
			currentVersion:     "v2.0.14",
			wantStoredVersion:  "v2.0.14",
			wantSavedConfigKey: true,
		},
		{
			name:              "current marker keeps secret",
			initialMarker:     "v2.0.14",
			currentVersion:    "v2.0.14",
			wantStoredVersion: "v2.0.14",
		},
		{
			name:           "debug version skips rotation and marker update",
			currentVersion: "debug",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := newTempConfig(t, "")
			t.Cleanup(func() { os.Remove(file) })

			c := &Config{
				JWTSecretKey:                   "leaked-secret",
				JWTSecretKeyLastRotatedVersion: tt.initialMarker,
				filePath:                       file,
			}

			rotated, err := c.RotateJWTSecretKeyIfNeeded(tt.currentVersion)
			if err != nil {
				t.Fatalf("rotate jwt secret key failed: %v", err)
			}
			if rotated != tt.wantRotated {
				t.Fatalf("rotated = %v, want %v", rotated, tt.wantRotated)
			}
			if c.JWTSecretKeyLastRotatedVersion != tt.wantStoredVersion {
				t.Fatalf("jwt secret key marker = %q, want %q", c.JWTSecretKeyLastRotatedVersion, tt.wantStoredVersion)
			}
			secretChanged := c.JWTSecretKey != "leaked-secret"
			if secretChanged != tt.wantSecretChanged {
				t.Fatalf("secret changed = %v, want %v", secretChanged, tt.wantSecretChanged)
			}

			saved, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read saved config: %v", err)
			}
			hasMarker := strings.Contains(string(saved), "jwt_secret_key_last_rotated_version")
			if hasMarker != tt.wantSavedConfigKey {
				t.Fatalf("saved marker present = %v, want %v, config = %s", hasMarker, tt.wantSavedConfigKey, saved)
			}
		})
	}
}

// Mirrors the upstream single-block declaration so iota lines up exactly:
// ConfigUsePeerIP occupies iota=0 (as a typed string), ConfigCoverAll=1,
// ConfigCoverIgnoreAll=2. Pins persisted `cover` semantics.
const (
	originalConfigUsePeerIP = "NZ::Use-Peer-IP"
	originalConfigCoverAll  = iota
	originalConfigCoverIgnoreAll
)

func TestConfigCoverConstantValues(t *testing.T) {
	if ConfigUsePeerIP != originalConfigUsePeerIP {
		t.Fatalf("ConfigUsePeerIP = %q, want %q", ConfigUsePeerIP, originalConfigUsePeerIP)
	}
	if ConfigCoverAll != originalConfigCoverAll {
		t.Fatalf("ConfigCoverAll = %d, want original value %d", ConfigCoverAll, originalConfigCoverAll)
	}
	if ConfigCoverIgnoreAll != originalConfigCoverIgnoreAll {
		t.Fatalf("ConfigCoverIgnoreAll = %d, want original value %d", ConfigCoverIgnoreAll, originalConfigCoverIgnoreAll)
	}
}

func newTempConfig(t *testing.T, cfg string) string {
	t.Helper()

	file, err := os.CreateTemp(os.TempDir(), "nezha-test-config-*.yml")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	defer file.Close()

	_, err = file.ReadFrom(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("write to temp file failed: %v", err)
	}

	return file.Name()
}
