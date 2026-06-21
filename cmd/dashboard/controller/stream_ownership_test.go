package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

func ensureLocalizerForStreamTests(t *testing.T) {
	t.Helper()
	if singleton.Localizer == nil {
		singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	}
	// upgrader stays nil — these tests must reject the caller BEFORE WS upgrade.
	// If a test ever reaches the upgrade path it will panic on nil upgrader,
	// surfacing the regression.
}

// decodeCommonResponseError returns Success and Error of a CommonResponse[any].
func decodeCommonResponseError(t *testing.T, body []byte) (bool, string) {
	t.Helper()
	var resp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, string(body))
	}
	return resp.Success, resp.Error
}

func setAuthUser(c *gin.Context, userID uint64, role model.Role) {
	c.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common: model.Common{ID: userID},
		Role:   role,
	})
}

// JWT cookie security: SigningAlgorithm must be pinned to HS256 (defense
// against future algorithm-confusion regressions in the library) and the
// JWT cookie must use SameSite=Lax so cross-site GET navigations don't
// silently mint requests with the user's session. HttpOnly/Secure are NOT
// asserted here because the frontend currently reads `!!document.cookie`
// to display login state and many deployments terminate TLS at a proxy —
// flipping those would break user-visible behaviour and is tracked
// separately.
func TestJWTInitParamsPinsAlgorithmAndSameSite(t *testing.T) {
	ensureLocalizerForStreamTests(t)
	if singleton.Conf == nil {
		singleton.Conf = &singleton.ConfigClass{
			Config: &model.Config{JWTSecretKey: "test-secret-for-jwt-config-assertions"},
		}
	}
	params := initParams()
	if params.SigningAlgorithm != "HS256" {
		t.Fatalf("SigningAlgorithm must be pinned to HS256, got %q", params.SigningAlgorithm)
	}
	if params.CookieSameSite != http.SameSiteLaxMode {
		t.Fatalf("CookieSameSite must be Lax for OAuth-callback compatibility + CSRF safety, got %v", params.CookieSameSite)
	}
}

// nz-o2s carries the OAuth2 state binding that authenticates the callback.
// The frontend never reads it, so HttpOnly is safe to enable and shuts the
// door on XSS attempting to steal the state.
func TestWriteOauth2StateCookieIsHttpOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	writeOauth2StateCookie(c, "test-key")

	header := w.Header().Get("Set-Cookie")
	if !strings.Contains(header, "nz-o2s=test-key") {
		t.Fatalf("expected nz-o2s cookie in response, got %q", header)
	}
	if !strings.Contains(header, "HttpOnly") {
		t.Fatalf("nz-o2s must be HttpOnly to prevent XSS reading OAuth state, got %q", header)
	}
}
