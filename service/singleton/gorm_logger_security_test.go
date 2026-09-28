package singleton

import (
	"bytes"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// 回归：GORM 默认 logger 会把 ErrRecordNotFound 连同带参 SQL 写日志，未认证登录的用户名因此原样落盘，
// 可用换行伪造日志行、用超长用户名放大写盘。
func TestGormLoggerSkipsRecordNotFound(t *testing.T) {
	var buf bytes.Buffer
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormLogger(&buf)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	var u model.User
	err = db.Where("username = ?", "x\n2026/01/01 NEZHA>> [FORGED] admin login ok").First(&u).Error
	if err == nil {
		t.Fatal("expected record not found")
	}
	if strings.Contains(buf.String(), "[FORGED]") {
		t.Fatalf("record not found 不应把查询参数写进日志: %q", buf.String())
	}
	db.Exec("SELECT * FROM no_such_table")
	if !strings.Contains(buf.String(), "no_such_table") {
		t.Fatal("其它数据库错误仍应记录")
	}
}
