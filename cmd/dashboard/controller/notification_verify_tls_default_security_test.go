package controller

import (
	"fmt"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：verify_tls 缺省为 false（发送时 InsecureSkipVerify），新建通知默认不校验证书；
// 且编辑时不带该字段会把已开启的校验静默关掉。

func storedVerifyTLS(t *testing.T, id uint64) *bool {
	t.Helper()
	var n model.Notification
	require.NoError(t, singleton.DB.First(&n, id).Error)
	return n.VerifyTLS
}

func TestCreateNotificationVerifiesTLSByDefault(t *testing.T) {
	defer setupTenancyTest(t)()
	c := ctxAsMemberWithBody(10, map[string]any{
		"name": "tg", "url": "https://1.1.1.1/botX/sendMessage", "request_method": model.NotificationRequestMethodGET,
		"skip_check": true,
	})
	id, err := createNotification(c)
	require.NoError(t, err)
	v := storedVerifyTLS(t, id)
	t.Logf("未提交 verify_tls 新建后 VerifyTLS=%v", *v)
	require.True(t, v != nil && *v, "新建通知默认必须校验 TLS 证书")
}

func TestUpdateNotificationKeepsVerifyTLSWhenOmitted(t *testing.T) {
	defer setupTenancyTest(t)()
	on := true
	stored := model.Notification{Common: model.Common{UserID: 10}, Name: "tg", URL: "https://1.1.1.1/botX",
		RequestMethod: model.NotificationRequestMethodGET, VerifyTLS: &on}
	require.NoError(t, singleton.DB.Create(&stored).Error)
	singleton.NotificationShared.InsertForTest(&stored)

	c := ctxAsMemberWithBody(10, map[string]any{"name": "renamed", "request_method": model.NotificationRequestMethodGET, "skip_check": true})
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	_, err := updateNotification(c)
	require.NoError(t, err)
	v := storedVerifyTLS(t, stored.ID)
	t.Logf("只改名称（未带 verify_tls）后 VerifyTLS=%v", *v)
	require.True(t, v != nil && *v, "未提交 verify_tls 不得关闭已开启的证书校验")
}
