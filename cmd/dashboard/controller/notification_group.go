package controller

import (
	"slices"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List notification group
// @Summary List notification group
// @Schemes
// @Description List notification group
// @Security BearerAuth
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.NotificationGroupResponseItem]
// @Router /notification-group [get]
func listNotificationGroup(c *gin.Context) ([]*model.NotificationGroupResponseItem, error) {
	var ng []model.NotificationGroup
	if err := singleton.DB.Find(&ng).Error; err != nil {
		return nil, err
	}

	var ngn []model.NotificationGroupNotification
	if err := singleton.DB.Find(&ngn).Error; err != nil {
		return nil, err
	}

	groupNotifications := make(map[uint64][]uint64, len(ng))
	for _, n := range ngn {
		if _, ok := groupNotifications[n.NotificationGroupID]; !ok {
			groupNotifications[n.NotificationGroupID] = make([]uint64, 0)
		}
		groupNotifications[n.NotificationGroupID] = append(groupNotifications[n.NotificationGroupID], n.NotificationID)
	}

	isAdmin := callerIsAdmin(c)
	ngRes := make([]*model.NotificationGroupResponseItem, 0, len(ng))
	for _, n := range ng {
		if !isAdmin && !n.HasPermission(c) {
			continue
		}
		ngRes = append(ngRes, &model.NotificationGroupResponseItem{
			Group:         n,
			Notifications: groupNotifications[n.ID],
		})
	}

	return ngRes, nil
}

// New notification group
// @Summary New notification group
// @Schemes
// @Description New notification group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param body body model.NotificationGroupForm true "NotificationGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification-group [post]
func createNotificationGroup(c *gin.Context) (uint64, error) {
	var ngf model.NotificationGroupForm
	if err := c.ShouldBindJSON(&ngf); err != nil {
		return 0, err
	}
	ngf.Notifications = uniqueIDs(ngf.Notifications)

	if !singleton.NotificationShared.CheckPermission(c, slices.Values(ngf.Notifications)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	uid := getUid(c)

	var ng model.NotificationGroup
	ng.Name = ngf.Name
	ng.UserID = uid

	if err := ensureIDsExist(&model.Notification{}, ngf.Notifications, singleton.Localizer.ErrorT("have invalid notification id")); err != nil {
		return 0, err
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&ng).Error; err != nil {
			return err
		}
		return createNotificationGroupMembers(tx, uid, ng.ID, ngf.Notifications)
	})
	if err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.NotificationShared.UpdateGroup(&ng, ngf.Notifications)
	return ng.ID, nil
}

// Edit notification group
// @Summary Edit notification group
// @Schemes
// @Description Edit notification group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param id path uint true "ID"
// @Param body body model.NotificationGroupForm true "NotificationGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification-group/{id} [patch]
func updateNotificationGroup(c *gin.Context) (any, error) {
	id, err := paramID(c)
	if err != nil {
		return nil, err
	}

	var ngf model.NotificationGroupForm
	if err := c.ShouldBindJSON(&ngf); err != nil {
		return nil, err
	}

	if !singleton.NotificationShared.CheckPermission(c, slices.Values(ngf.Notifications)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var ngDB model.NotificationGroup
	if err := singleton.DB.First(&ngDB, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("group id %d does not exist", id)
	}

	if !ngDB.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	ngDB.Name = ngf.Name
	ngf.Notifications = uniqueIDs(ngf.Notifications)

	if err := ensureIDsExist(&model.Notification{}, ngf.Notifications, singleton.Localizer.ErrorT("have invalid notification id")); err != nil {
		return nil, err
	}

	uid := getUid(c)

	err = singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&ngDB).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_group_id = ?", id).Error; err != nil {
			return err
		}
		return createNotificationGroupMembers(tx, uid, ngDB.ID, ngf.Notifications)
	})
	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.NotificationShared.UpdateGroup(&ngDB, ngf.Notifications)
	return nil, nil
}

// createNotificationGroupMembers 在事务内逐条写入通知分组成员。
func createNotificationGroupMembers(tx *gorm.DB, uid, groupID uint64, notifications []uint64) error {
	for _, n := range notifications {
		member := model.NotificationGroupNotification{Common: model.Common{UserID: uid}, NotificationGroupID: groupID, NotificationID: n}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
	}
	return nil
}

// Batch delete notification group
// @Summary Batch delete notification group
// @Security BearerAuth
// @Schemes
// @Description Batch delete notification group
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/notification-group [post]
func batchDeleteNotificationGroup(c *gin.Context) (any, error) {
	var ngn []uint64
	if err := c.ShouldBindJSON(&ngn); err != nil {
		return nil, err
	}

	var ng []model.NotificationGroup
	if err := singleton.DB.Where("id in (?)", ngn).Find(&ng).Error; err != nil {
		return nil, err
	}

	for _, n := range ng {
		if !n.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	if err := singleton.DeleteNotificationGroups(ngn); err != nil {
		return nil, newGormError("%v", err)
	}
	return nil, nil
}
