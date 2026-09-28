package singleton

import (
	"os"
	"strings"
	"testing"

	"github.com/nezhahq/nezha/model"
)

func TestInitConfigFromPathRotatesJWTSecretKey(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "nezha-config-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := file.WriteString("jwt_secret_key: leaked-secret\nagent_secret_key: agent-secret\njwt_secret_key_last_rotated_version: v2.0.12\n"); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}

	originalConf := Conf
	originalVersion := Version
	originalTemplates := builtinTemplates
	Version = "v2.0.13"
	builtinTemplates = nil
	t.Cleanup(func() {
		Conf = originalConf
		Version = originalVersion
		builtinTemplates = originalTemplates
	})

	if err := InitConfigFromPath(file.Name()); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if Conf.JWTSecretKey == "leaked-secret" {
		t.Fatal("jwt_secret_key was not rotated")
	}
	if Conf.JWTSecretKeyLastRotatedVersion != model.JWTSecretKeyRotationBaselineVersion {
		t.Fatalf("jwt secret key marker = %q, want %q", Conf.JWTSecretKeyLastRotatedVersion, model.JWTSecretKeyRotationBaselineVersion)
	}

	saved, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if strings.Contains(string(saved), "leaked-secret") {
		t.Fatalf("saved config still contains leaked jwt_secret_key: %s", saved)
	}
	if !strings.Contains(string(saved), "jwt_secret_key_last_rotated_version: v2.0.13") {
		t.Fatalf("saved config did not persist jwt secret key marker: %s", saved)
	}
}

// 回归：清空忽略列表后旧集合必须失效；逗号后带空格的 ID 不能被丢弃。
func TestUpdateIgnoredIPNotificationID(t *testing.T) {
	c := &ConfigClass{Config: &model.Config{}}
	c.IgnoredIPNotification = "1, 2,3"
	c.updateIgnoredIPNotificationID()
	for _, id := range []uint64{1, 2, 3} {
		if !c.IgnoredIPNotificationServerIDs[id] {
			t.Fatalf("id %d missing from %v", id, c.IgnoredIPNotificationServerIDs)
		}
	}
	c.IgnoredIPNotification = ""
	c.updateIgnoredIPNotificationID()
	if len(c.IgnoredIPNotificationServerIDs) != 0 {
		t.Fatalf("ids not cleared: %v", c.IgnoredIPNotificationServerIDs)
	}
}
