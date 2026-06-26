package controller

import (
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// Get profile
// @Summary Get profile
// @Security BearerAuth
// @Schemes
// @Description Get profile
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[model.Profile]
// @Router /profile [get]
func getProfile(c *gin.Context) (*model.Profile, error) {
	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	var ob []model.Oauth2Bind
	if err := singleton.DB.Where("user_id = ?", auth.(*model.User).ID).Find(&ob).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	var obMap = make(map[string]string)
	for _, v := range ob {
		obMap[v.Provider] = v.OpenID
	}
	return &model.Profile{
		User:       *auth.(*model.User),
		LoginIP:    c.GetString(model.CtxKeyRealIPStr),
		Oauth2Bind: obMap,
	}, nil
}

// Update password for current user
// @Summary Update password for current user
// @Security BearerAuth
// @Schemes
// @Description Update password for current user
// @Tags auth required
// @Accept json
// @param request body model.ProfileForm true "password"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /profile [post]
func updateProfile(c *gin.Context) (any, error) {
	var pf model.ProfileForm
	if err := c.ShouldBindJSON(&pf); err != nil {
		return 0, err
	}

	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	user := *auth.(*model.User)
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(pf.OriginalPassword)); err != nil {
		return nil, singleton.Localizer.ErrorT("incorrect password")
	}
	if err := checkRejectPasswordAllowed(user.ID, pf.RejectPassword); err != nil {
		return nil, err
	}
	if err := applyProfileChanges(&user, &pf); err != nil {
		return nil, err
	}
	if err := singleton.DB.Save(&user).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnUserUpdate(&user)
	if err := singleton.RevokeJWTSessionsByUser(user.ID); err != nil {
		return nil, newGormError("%v", err)
	}
	return nil, nil
}

// checkRejectPasswordAllowed 开启「禁止密码登录」前，账号必须至少绑定一个 OAuth2。
func checkRejectPasswordAllowed(userID uint64, reject bool) error {
	if !reject {
		return nil
	}
	var bindCount int64
	if err := singleton.DB.Model(&model.Oauth2Bind{}).
		Where("user_id = ?", userID).Count(&bindCount).Error; err != nil {
		return newGormError("%v", err)
	}
	if bindCount < 1 {
		return singleton.Localizer.ErrorT("you don't have any oauth2 bindings")
	}
	return nil
}

// applyProfileChanges 按「留空即不修改」语义更新用户名/密码，避免空值覆盖把账号写坏。
func applyProfileChanges(user *model.User, pf *model.ProfileForm) error {
	if pf.NewUsername != "" {
		user.Username = pf.NewUsername
	}
	if pf.NewPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(pf.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		user.Password = string(hash)
	}
	user.RejectPassword = pf.RejectPassword
	user.TokenVersion += 1
	return nil
}

// List user
// @Summary List user
// @Security BearerAuth
// @Schemes
// @Description List user
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.User]
// @Router /user [get]
func listUser(c *gin.Context) ([]model.User, error) {
	var users []model.User
	if err := singleton.DB.Omit("password").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// Create user
// @Summary Create user
// @Security BearerAuth
// @Schemes
// @Description Create user
// @Tags admin required
// @Accept json
// @param request body model.UserForm true "User Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /user [post]
func createUser(c *gin.Context) (uint64, error) {
	var uf model.UserForm
	if err := c.ShouldBindJSON(&uf); err != nil {
		return 0, err
	}

	if len(uf.Password) < 6 {
		return 0, singleton.Localizer.ErrorT("password length must be greater than 6")
	}
	if uf.Username == "" {
		return 0, singleton.Localizer.ErrorT("username can't be empty")
	}
	if uf.Role > model.RoleMember {
		return 0, singleton.Localizer.ErrorT("invalid role")
	}

	var u model.User
	u.Username = uf.Username
	u.Role = uf.Role

	hash, err := bcrypt.GenerateFromPassword([]byte(uf.Password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	u.Password = string(hash)

	if err := singleton.DB.Create(&u).Error; err != nil {
		return 0, err
	}

	singleton.OnUserUpdate(&u)
	return u.ID, nil
}

// Batch delete users
// @Summary Batch delete users
// @Security BearerAuth
// @Schemes
// @Description Batch delete users
// @Tags admin required
// @Accept json
// @param request body []uint true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/user [post]
func batchDeleteUser(c *gin.Context) (any, error) {
	var ids []uint64
	if err := c.ShouldBindJSON(&ids); err != nil {
		return nil, err
	}
	auth := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	if slices.Contains(ids, auth.ID) {
		return nil, singleton.Localizer.ErrorT("can't delete yourself")
	}

	err := singleton.OnUserDelete(ids, newGormError)
	return nil, err
}

// List online users
// @Summary List online users
// @Security BearerAuth
// @Schemes
// @Description List online users
// @Tags auth required
// @Param limit query uint false "Page limit"
// @Param offset query uint false "Page offset"
// @Produce json
// @Success 200 {object} model.PaginatedResponse[[]model.OnlineUser, model.OnlineUser]
// @Router /online-user [get]
func listOnlineUser(c *gin.Context) (*model.Value[[]*model.OnlineUser], error) {
	var isAdmin bool
	u, ok := c.Get(model.CtxKeyAuthorizedUser)
	if ok {
		isAdmin = u.(*model.User).Role.IsAdmin()
	}
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = 25
	}

	offset, err := strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}

	all := onlineSessions()
	users := paginateOnline(all, offset, limit)
	if !isAdmin {
		users = desensitizeOnline(users)
	}

	return &model.Value[[]*model.OnlineUser]{
		Value: users,
		Pagination: model.Pagination{
			Offset: offset,
			Limit:  limit,
			Total:  int64(len(all)),
		},
	}, nil
}

// onlineSessions 返回有效(未过期未吊销)登录会话,按 IP 去重,空 IP 跳过。
func onlineSessions() []*model.OnlineUser {
	var sessions []model.JWTSession
	singleton.DB.Where("expires_at > ? AND revoked_at IS NULL", time.Now()).
		Order("created_at ASC").Find(&sessions)
	seen := make(map[string]bool)
	out := make([]*model.OnlineUser, 0, len(sessions))
	for _, s := range sessions {
		if s.IP == "" || seen[s.IP] {
			continue
		}
		seen[s.IP] = true
		out = append(out, &model.OnlineUser{UserID: s.UserID, IP: s.IP, ConnectedAt: s.CreatedAt})
	}
	return out
}

// paginateOnline 对会话列表做 offset/limit 切片。
func paginateOnline(all []*model.OnlineUser, offset, limit int) []*model.OnlineUser {
	if offset >= len(all) {
		return nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end]
}

// desensitizeOnline 对非管理员脱敏在线用户 IP。
func desensitizeOnline(users []*model.OnlineUser) []*model.OnlineUser {
	out := make([]*model.OnlineUser, 0, len(users))
	for _, user := range users {
		out = append(out, &model.OnlineUser{
			UserID:      user.UserID,
			IP:          utils.IPDesensitize(user.IP),
			ConnectedAt: user.ConnectedAt,
		})
	}
	return out
}

// Batch block online user
// @Summary Batch block online user
// @Security BearerAuth
// @Schemes
// @Description Batch block online user
// @Tags admin required
// @Accept json
// @Param request body []string true "block list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /online-user/batch-block [post]
func batchBlockOnlineUser(c *gin.Context) (any, error) {
	var list []string
	if err := c.ShouldBindJSON(&list); err != nil {
		return nil, err
	}

	if err := singleton.BlockByIPs(utils.Unique(list)); err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
