package controller

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func newFrontendFallbackTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{
		ConfigDashboard: model.ConfigDashboard{
			UserTemplate: "user-dist",
		},
	}}
	t.Cleanup(func() { singleton.Conf = originalConf })

	writeFrontendFallbackTestFile(t, "admin-dist/index.html", "<html>admin index</html>")
	writeFrontendFallbackTestFile(t, "admin-dist/assets/app.js", "console.log('admin asset')")
	writeFrontendFallbackTestFile(t, "user-dist/index.html", "<html>user index</html>")
	writeFrontendFallbackTestFile(t, "data/config.yaml", "jwt_secret_key: traversal-secret")

	r := gin.New()
	r.NoRoute(fallbackToFrontend(testFrontendDist{}))
	return r
}

func writeFrontendFallbackTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

type testFrontendDist struct{}

func (testFrontendDist) Open(string) (fs.File, error) {
	return nil, fs.ErrNotExist
}

func performFrontendFallbackRequest(t *testing.T, router *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	router.ServeHTTP(w, req)
	return w
}

func TestFallbackToFrontendBlocksDashboardTraversal(t *testing.T) {
	t.Chdir(t.TempDir())
	router := newFrontendFallbackTestRouter(t)

	tests := []string{
		"/dashboard../data/config.yaml",
		"/dashboard%2e%2e/data/config.yaml",
		"/dashboard%2e%2e%2fdata%2fconfig.yaml",
		"/dashboard/../data/config.yaml",
		"/dashboard/%2e%2e/data/config.yaml",
		"/dashboard../assets/app.js",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			w := performFrontendFallbackRequest(t, router, target)
			body := w.Body.String()
			if strings.Contains(body, "traversal-secret") || strings.Contains(body, "jwt_secret_key") || strings.Contains(body, "admin asset") {
				t.Fatalf("%s leaked protected content with status %d: %q", target, w.Code, body)
			}
		})
	}
}

func TestFallbackToFrontendBlocksUserTraversal(t *testing.T) {
	t.Chdir(t.TempDir())
	router := newFrontendFallbackTestRouter(t)

	tests := []string{
		"/../data/config.yaml",
		"/%2e%2e/data/config.yaml",
		"/%2e%2e%2fdata%2fconfig.yaml",
		"/../admin-dist/assets/app.js",
		"/%2e%2e/admin-dist/assets/app.js",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			w := performFrontendFallbackRequest(t, router, target)
			body := w.Body.String()
			if strings.Contains(body, "traversal-secret") || strings.Contains(body, "jwt_secret_key") || strings.Contains(body, "admin asset") {
				t.Fatalf("%s leaked protected content with status %d: %q", target, w.Code, body)
			}
		})
	}
}

func TestFallbackToFrontendPreservesDashboardRoutes(t *testing.T) {
	t.Chdir(t.TempDir())
	router := newFrontendFallbackTestRouter(t)

	w := performFrontendFallbackRequest(t, router, "/dashboard")
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("/dashboard status = %d, want %d", w.Code, http.StatusMovedPermanently)
	}
	if location := w.Header().Get("Location"); location != "/dashboard/" {
		t.Fatalf("/dashboard Location = %q, want /dashboard/", location)
	}

	w = performFrontendFallbackRequest(t, router, "/dashboard/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin index") {
		t.Fatalf("/dashboard/ status = %d body = %q, want admin index", w.Code, w.Body.String())
	}

	w = performFrontendFallbackRequest(t, router, "/dashboard/assets/app.js")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin asset") {
		t.Fatalf("/dashboard/assets/app.js status = %d body = %q, want admin asset", w.Code, w.Body.String())
	}
}

// TestFallbackToFrontendAdminIgnoresThemeDir 管理端固定内置：<ThemeDir>/admin-dist（旧版面板更新残留）不得覆盖内置产物；
// 访客内置主题仍优先读 <ThemeDir> 下的面板更新版。
func TestFallbackToFrontendAdminIgnoresThemeDir(t *testing.T) {
	t.Chdir(t.TempDir())
	router := newFrontendFallbackTestRouter(t)

	themeDir := t.TempDir()
	origThemeDir := singleton.ThemeDir
	singleton.ThemeDir = themeDir
	t.Cleanup(func() { singleton.ThemeDir = origThemeDir })
	writeFrontendFallbackTestFile(t, filepath.Join(themeDir, "admin-dist/index.html"), "<html>stale admin</html>")
	writeFrontendFallbackTestFile(t, filepath.Join(themeDir, "admin-dist/assets/app.js"), "console.log('stale admin')")
	writeFrontendFallbackTestFile(t, filepath.Join(themeDir, "user-dist/index.html"), "<html>updated user</html>")

	for target, want := range map[string]string{
		"/dashboard/":              "admin index",
		"/dashboard/settings":      "admin index",
		"/dashboard/assets/app.js": "admin asset",
		"/":                        "updated user",
	} {
		w := performFrontendFallbackRequest(t, router, target)
		if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%s status = %d body = %q, want %q", target, w.Code, body, want)
		}
	}
}
