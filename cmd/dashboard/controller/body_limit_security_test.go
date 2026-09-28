package controller

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// 回归：请求体无上限 —— 未认证 /login 的超大 JSON 被整体缓冲（64MiB 请求实测分配约 576MiB），
// 主题上传可无限写临时盘。limitRequestBody 统一设上限，主题上传单独放宽。

// countingBody 统计处理链实际读走的请求体字节数。
type countingBody struct {
	r io.Reader
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func hugeLoginBody(size int) io.Reader {
	return io.MultiReader(strings.NewReader(`{"username":"`),
		bytes.NewReader(bytes.Repeat([]byte("a"), size)), strings.NewReader(`","password":"x"}`))
}

func TestLimitRequestBody_RejectsDeclaredOversizedLogin(t *testing.T) {
	r := newLoginWAFRouter(t, "")
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, hugeLoginBody(2<<20))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestLimitRequestBody_CapsChunkedLoginBody(t *testing.T) {
	r := newLoginWAFRouter(t, "")
	body := &countingBody{r: hugeLoginBody(64 << 20)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", body)
	req.Header.Set("Content-Type", "application/json")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r.ServeHTTP(httptest.NewRecorder(), req)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("64MiB 未声明长度请求：读走 %d KiB，分配 %d MiB", body.n>>10, allocated>>20)
	require.LessOrEqual(t, body.n, defaultMaxRequestBodyBytes+64<<10, "读取量必须止于上限附近")
	require.Less(t, allocated, uint64(32<<20))
}

func TestLimitRequestBody_ThemeUploadHasOwnCap(t *testing.T) {
	orig := themeUploadMaxBodyBytes
	themeUploadMaxBodyBytes = 2 << 20
	t.Cleanup(func() { themeUploadMaxBodyBytes = orig })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(limitRequestBody)
	r.POST("/api/v1/theme/upload", func(c *gin.Context) {
		c.Set(model.CtxKeyAuthorizedUser, &model.User{Role: model.RoleAdmin})
	}, adminHandler(uploadTheme))
	pr, pw := io.Pipe()
	mp := multipart.NewWriter(pw)
	go func() {
		fw, _ := mp.CreateFormFile("file", "big.zip")
		_, _ = fw.Write(bytes.Repeat([]byte("Z"), 8<<20))
		_ = mp.Close()
		_ = pw.CloseWithError(io.ErrClosedPipe)
	}()
	body := &countingBody{r: pr}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/theme/upload", body)
	req.Header.Set("Content-Type", mp.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	_ = pr.Close()
	t.Logf("8MiB 上传（上限 2MiB）：读走 %d KiB，响应 %s", body.n>>10, w.Body.String())
	require.LessOrEqual(t, body.n, themeUploadMaxBodyBytes+64<<10)
	require.Contains(t, w.Body.String(), "too large")
}

func TestLoginRejectsFormEncodedCredentials(t *testing.T) {
	r := newLoginWAFRouter(t, "")
	form := url.Values{"Username": {"admin"}, "Password": {"correct-horse"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.NotContains(t, w.Body.String(), `"token"`, "跨站表单提交的登录不得成功（登录 CSRF）")
	require.Equal(t, http.StatusOK, postLogin(r, "127.0.0.1:1", "correct-horse").Code, "JSON 登录保持可用")
}
