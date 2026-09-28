package main

import (
	"testing"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归（NZ-AUDIT-001）：首次启动不得再以固定口令 admin/admin 创建管理员。

func initialAdminHash(t *testing.T) string {
	t.Helper()
	orig := singleton.DB
	t.Cleanup(func() { singleton.DB = orig })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	singleton.DB = db
	if err := ensureInitialAdmin(); err != nil {
		t.Fatal(err)
	}
	var u model.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.Password
}

func TestInitialAdminIsNotAdminAdmin(t *testing.T) {
	t.Setenv(initialAdminPasswordEnv, "")
	if bcrypt.CompareHashAndPassword([]byte(initialAdminHash(t)), []byte("admin")) == nil {
		t.Fatal("初始管理员口令不得为固定的 admin")
	}
}

func TestInitialAdminUsesEnvPassword(t *testing.T) {
	t.Setenv(initialAdminPasswordEnv, "Env-Provided-Pass-123")
	if bcrypt.CompareHashAndPassword([]byte(initialAdminHash(t)), []byte("Env-Provided-Pass-123")) != nil {
		t.Fatal("设置了环境变量时应使用其值作为初始口令")
	}
}
