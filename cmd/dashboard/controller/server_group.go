package controller

import (
	"slices"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List server group
// @Summary List server group
// @Schemes
// @Description List server group
// @Security BearerAuth
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.ServerGroupResponseItem]
// @Router /server-group [get]
func listServerGroup(c *gin.Context) ([]*model.ServerGroupResponseItem, error) {
	var sg []model.ServerGroup
	if err := singleton.DB.Find(&sg).Error; err != nil {
		return nil, err
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	isAdmin := isMember && callerIsAdmin(c)
	pat := model.PATFromContext(c)
	patLimited := pat != nil && patHasServerWhitelist(c)

	visibleServerIDs := make(map[uint64]struct{})
	if !isMember {
		for _, server := range singleton.ServerShared.GetSortedListForGuest() {
			visibleServerIDs[server.ID] = struct{}{}
		}
	}

	groupServers := make(map[uint64][]uint64, 0)
	var sgs []model.ServerGroupServer
	if err := singleton.DB.Find(&sgs).Error; err != nil {
		return nil, err
	}
	for _, s := range sgs {
		if !isMember {
			if _, ok := visibleServerIDs[s.ServerId]; !ok {
				continue
			}
		}
		if pat != nil && !pat.CanAccessServer(s.ServerId) {
			continue
		}
		if _, ok := groupServers[s.ServerGroupId]; !ok {
			groupServers[s.ServerGroupId] = make([]uint64, 0)
		}
		groupServers[s.ServerGroupId] = append(groupServers[s.ServerGroupId], s.ServerId)
	}

	var sgRes []*model.ServerGroupResponseItem
	for _, s := range sg {
		if isMember && !isAdmin && !s.HasPermission(c) {
			continue
		}
		if !isMember && len(groupServers[s.ID]) == 0 {
			continue
		}
		if patLimited && len(groupServers[s.ID]) == 0 {
			continue
		}
		sgRes = append(sgRes, &model.ServerGroupResponseItem{
			Group:   s,
			Servers: groupServers[s.ID],
		})
	}

	return sgRes, nil
}

// New server group
// @Summary New server group
// @Schemes
// @Description New server group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param body body model.ServerGroupForm true "ServerGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /server-group [post]
func createServerGroup(c *gin.Context) (uint64, error) {
	var sgf model.ServerGroupForm
	if err := c.ShouldBindJSON(&sgf); err != nil {
		return 0, err
	}
	sgf.Servers = uniqueIDs(sgf.Servers)

	if !singleton.ServerShared.CheckPermission(c, slices.Values(sgf.Servers)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	uid := getUid(c)

	var sg model.ServerGroup
	sg.Name = sgf.Name
	sg.UserID = uid

	if err := ensureIDsExist(&model.Server{}, sgf.Servers, singleton.Localizer.ErrorT("have invalid server id")); err != nil {
		return 0, err
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&sg).Error; err != nil {
			return err
		}
		return createServerGroupMembers(tx, uid, sg.ID, sgf.Servers)
	})
	if err != nil {
		return 0, newGormError("%v", err)
	}

	return sg.ID, nil
}

// Edit server group
// @Summary Edit server group
// @Schemes
// @Description Edit server group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param id path uint true "ID"
// @Param body body model.ServerGroupForm true "ServerGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /server-group/{id} [patch]
func updateServerGroup(c *gin.Context) (any, error) {
	id, err := paramID(c)
	if err != nil {
		return nil, err
	}

	var sg model.ServerGroupForm
	if err := c.ShouldBindJSON(&sg); err != nil {
		return nil, err
	}
	sg.Servers = uniqueIDs(sg.Servers)

	if !singleton.ServerShared.CheckPermission(c, slices.Values(sg.Servers)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var sgDB model.ServerGroup
	if err := singleton.DB.First(&sgDB, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("group id %d does not exist", id)
	}

	if !sgDB.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	if !patGroupMembershipAccessAllowed(c, sgDB.ID) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	sgDB.Name = sg.Name

	if err := ensureIDsExist(&model.Server{}, sg.Servers, singleton.Localizer.ErrorT("have invalid server id")); err != nil {
		return nil, err
	}

	uid := getUid(c)

	err = singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&sgDB).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_group_id = ?", id).Error; err != nil {
			return err
		}
		return createServerGroupMembers(tx, uid, sgDB.ID, sg.Servers)
	})
	if err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}

// createServerGroupMembers 在事务内逐条写入分组成员（空列表时不写，避免 GORM 空切片报错）。
func createServerGroupMembers(tx *gorm.DB, uid, groupID uint64, servers []uint64) error {
	for _, s := range servers {
		member := model.ServerGroupServer{Common: model.Common{UserID: uid}, ServerGroupId: groupID, ServerId: s}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
	}
	return nil
}

// Batch delete server group
// @Summary Batch delete server group
// @Security BearerAuth
// @Schemes
// @Description Batch delete server group
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/server-group [post]
func batchDeleteServerGroup(c *gin.Context) (any, error) {
	var sgs []uint64
	if err := c.ShouldBindJSON(&sgs); err != nil {
		return nil, err
	}

	var sg []model.ServerGroup
	if err := singleton.DB.Where("id in (?)", sgs).Find(&sg).Error; err != nil {
		return nil, err
	}

	for _, s := range sg {
		if !s.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	if pat := model.PATFromContext(c); pat != nil && patHasServerWhitelist(c) {
		var members []model.ServerGroupServer
		if err := singleton.DB.Where("server_group_id in (?)", sgs).Find(&members).Error; err != nil {
			return nil, err
		}
		for _, m := range members {
			if !pat.CanAccessServer(m.ServerId) {
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
		}
	}

	if err := deleteWithMembers(&model.ServerGroup{}, &model.ServerGroupServer{}, "server_group_id in (?)", sgs); err != nil {
		return nil, err
	}
	return nil, nil
}
