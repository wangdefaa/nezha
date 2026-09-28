package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：列表接口已把 url/header/body 置空（凭据只写不读），但编辑时「测试发送」失败的报错
// 来自 *url.Error，会把库里保存的完整 URL（常含 bot token / access_token）原样回显给调用方。
// 管理员或仅有 nezha:notification:write 的 PAT 只要提交一个非法请求头即可在不改动数据的前提下读出凭据。
func TestUpdateNotificationTestSendDoesNotEchoStoredURL(t *testing.T) {
	defer setupTenancyTest(t)()
	origLoc := singleton.Loc
	singleton.Loc = time.UTC
	t.Cleanup(func() { singleton.Loc = origLoc })

	stored := model.Notification{
		Common: model.Common{UserID: 10}, Name: "telegram",
		URL:           "https://1.1.1.1/bot123456:SECRET-BOT-TOKEN/sendMessage?chat_id=42",
		RequestMethod: model.NotificationRequestMethodPOST, RequestType: model.NotificationRequestTypeJSON,
		RequestBody: `{"text":"#NEZHA#"}`,
	}
	require.NoError(t, singleton.DB.Create(&stored).Error)
	singleton.NotificationShared.InsertForTest(&stored)

	c := ctxAsMemberWithBody(10, map[string]any{
		"name": "telegram", "url": "", "request_method": model.NotificationRequestMethodPOST,
		"request_type": model.NotificationRequestTypeJSON, "request_header": `{"X":"a\nb"}`,
	})
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin})
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	_, err := updateNotification(c)
	t.Logf("测试发送报错: %v", err)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET-BOT-TOKEN", "报错不得回显已保存的通知 URL")
}
