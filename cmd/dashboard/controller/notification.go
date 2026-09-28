package controller

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

const notificationBatchDeleteMaxBodyBytes = 1 << 20

// List notification
// @Summary List notification
// @Security BearerAuth
// @Schemes
// @Description List notification
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Notification]
// @Router /notification [get]
func listNotification(c *gin.Context) ([]*model.Notification, error) {
	slist := singleton.NotificationShared.GetSortedList()

	var notifications []*model.Notification
	if err := copier.Copy(&notifications, &slist); err != nil {
		return nil, err
	}

	// 列表端点不回显写入态凭据：notifications 是 copier 复制出的副本，置零安全，
	// 不影响 singleton 内原始数据。
	for _, n := range notifications {
		n.URL = ""
		n.RequestHeader = ""
		n.RequestBody = ""
	}
	return notifications, nil
}

// Add notification
// @Summary Add notification
// @Security BearerAuth
// @Schemes
// @Description Add notification
// @Tags auth required
// @Accept json
// @param request body model.NotificationForm true "NotificationForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification [post]
func createNotification(c *gin.Context) (uint64, error) {
	var nf model.NotificationForm
	if err := c.ShouldBindJSON(&nf); err != nil {
		return 0, err
	}

	var n model.Notification
	n.UserID = getUid(c)
	// 新建时 n 为零值，合并表单等同逐字段赋值；verify_tls 再按新建规则缺省为 true。
	mergeNotificationForm(&n, &nf)
	n.VerifyTLS = defaultVerifyTLS(nf.VerifyTLS)

	if err := sendTestNotification(&n, nf.SkipCheck); err != nil {
		return 0, err
	}
	if err := singleton.DB.Create(&n).Error; err != nil {
		return 0, newGormError("%v", err)
	}
	singleton.NotificationShared.Update(&n)
	return n.ID, nil
}

// sendTestNotification 未勾选跳过检查时先试发一条，失败则不保存。
func sendTestNotification(n *model.Notification, skipCheck bool) error {
	if skipCheck {
		return nil
	}
	ns := model.NotificationServerBundle{Notification: n, Server: nil, Loc: singleton.Loc}
	return ns.Send(singleton.Localizer.T("a test message"))
}

// defaultVerifyTLS 新建通知未指定 verify_tls 时默认校验证书：旧实现缺省为 false，
// 即 InsecureSkipVerify，携带 bot token 等凭据的 webhook 可被中间人截获。
func defaultVerifyTLS(v *bool) *bool {
	verify := v == nil || *v
	return &verify
}

// Edit notification
// @Summary Edit notification
// @Security BearerAuth
// @Schemes
// @Description Edit notification
// @Tags auth required
// @Accept json
// @Param id path uint true "Notification ID"
// @Param body body model.NotificationForm true "NotificationForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification/{id} [patch]
func updateNotification(c *gin.Context) (any, error) {
	id, err := paramID(c)
	if err != nil {
		return nil, err
	}
	var nf model.NotificationForm
	if err := c.ShouldBindJSON(&nf); err != nil {
		return nil, err
	}

	var n model.Notification
	if err := singleton.DB.First(&n, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("notification id %d does not exist", id)
	}
	if !n.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	mergeNotificationForm(&n, &nf)
	if err := sendTestNotification(&n, nf.SkipCheck); err != nil {
		return nil, err
	}
	if err := singleton.DB.Save(&n).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	singleton.NotificationShared.Update(&n)
	return nil, nil
}

// mergeNotificationForm 把编辑表单合入已存通知。凭据在列表接口已脱敏，前端无法回填：
// URL/请求头/请求体空值视为"不修改"，保留旧值避免误清空；verify_tls 未提交时同样保留原值
// （旧实现把缺省当 false，任何不带该字段的 PATCH 都会静默关闭证书校验）。
func mergeNotificationForm(n *model.Notification, nf *model.NotificationForm) {
	n.Name = nf.Name
	n.RequestMethod = nf.RequestMethod
	n.RequestType = nf.RequestType
	if nf.VerifyTLS != nil {
		n.VerifyTLS = nf.VerifyTLS
	}
	formatMetricUnits := nf.FormatMetricUnits
	n.FormatMetricUnits = &formatMetricUnits
	if nf.URL != "" {
		n.URL = nf.URL
	}
	if nf.RequestHeader != "" {
		n.RequestHeader = nf.RequestHeader
	}
	if nf.RequestBody != "" {
		n.RequestBody = nf.RequestBody
	}
}

// Batch delete notifications
// @Summary Batch delete notifications
// @Security BearerAuth
// @Schemes
// @Description Batch delete notifications
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/notification [post]
func batchDeleteNotification(c *gin.Context) (any, error) {
	var n []uint64
	if c.Request != nil && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, notificationBatchDeleteMaxBodyBytes)
	}
	if err := c.ShouldBindJSON(&n); err != nil {
		return nil, err
	}

	if !singleton.NotificationShared.CheckPermission(c, slices.Values(n)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	if err := deleteWithMembers(&model.Notification{}, &model.NotificationGroupNotification{}, "notification_id in (?)", n); err != nil {
		return nil, err
	}

	singleton.NotificationShared.Delete(n)
	return nil, nil
}
