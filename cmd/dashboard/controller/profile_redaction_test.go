package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestGetProfile_RedactsPasswordHash(t *testing.T) {
	defer setupTenancyTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}))

	passwordHash := "$2a$10$profilePasswordHashMustNotLeak012345678901"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
	c.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common:   model.Common{ID: 10},
		Username: "alice",
		Password: passwordHash,
		Role:     model.RoleMember,
	})

	commonHandler(getProfile)(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), passwordHash)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, true, body["success"])
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "alice", data["username"])
	require.NotContains(t, data, "password")
}

func TestUpdateProfile_UsesStoredPasswordHashAfterRedaction(t *testing.T) {
	defer setupTenancyTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}, &model.JWTSession{}))

	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserID := singleton.AgentSecretToUserId
	singleton.UserInfoMap = make(map[uint64]model.UserInfo)
	singleton.AgentSecretToUserId = make(map[string]uint64)
	defer func() {
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserID
	}()

	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	user := model.User{
		Common:      model.Common{ID: 10},
		Username:    "alice",
		Password:    string(oldHash),
		Role:        model.RoleMember,
		AgentSecret: "profile-update-agent-secret",
	}
	require.NoError(t, singleton.DB.Create(&user).Error)

	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{
		OriginalPassword: "wrong-password",
		NewUsername:      "mallory",
		NewPassword:      "new-password",
	}))
	require.Error(t, err)

	var unchanged model.User
	require.NoError(t, singleton.DB.First(&unchanged, user.ID).Error)
	require.Equal(t, "alice", unchanged.Username)
	require.Equal(t, string(oldHash), unchanged.Password)

	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{
		OriginalPassword: "old-password",
		NewUsername:      "alice-renamed",
		NewPassword:      "new-password",
	}))
	require.NoError(t, err)

	var after model.User
	require.NoError(t, singleton.DB.First(&after, user.ID).Error)
	require.Equal(t, "alice-renamed", after.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(after.Password), []byte("new-password")))
}

func profileUpdateContext(t *testing.T, user model.User, form model.ProfileForm) *gin.Context {
	t.Helper()

	body, err := json.Marshal(form)
	require.NoError(t, err)
	c := ctxAs(user.ID, user.Role)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/profile", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(model.CtxKeyAuthorizedUser, &user)
	return c
}
