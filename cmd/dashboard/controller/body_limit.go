package controller

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/service/singleton"
)

// 请求体上限。gin 的 MaxMultipartMemory 只是 multipart 内存缓冲阈值、net/http 也没有默认上限：
// 未认证的 /api/v1/login 就能用超大 JSON 撑爆内存（64MiB 请求实测分配约 576MiB），
// 主题上传则可无限写临时盘。这里对所有 HTTP 请求统一设上限。
const defaultMaxRequestBodyBytes int64 = 1 << 20

// themeUploadMaxBodyBytes 主题上传允许的最大请求体：主题包上限 + multipart 头部余量（变量便于单测调小）。
var themeUploadMaxBodyBytes int64 = singleton.MaxThemeArchiveSize + 1<<20

var errRequestBodyTooLarge = errors.New("request body too large")

// maxRequestBodyBytes 返回路径对应的请求体上限。
func maxRequestBodyBytes(path string) int64 {
	if path == "/api/v1/theme/upload" {
		return themeUploadMaxBodyBytes
	}
	return defaultMaxRequestBodyBytes
}

// limitRequestBody 声明长度超限直接 413；未声明长度（chunked）交给 MaxBytesReader 读到上限即报错。
func limitRequestBody(c *gin.Context) {
	limit := maxRequestBodyBytes(c.Request.URL.Path)
	if c.Request.ContentLength > limit {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, newErrorResponse(errRequestBodyTooLarge))
		return
	}
	if c.Request.Body != nil && c.Request.Body != http.NoBody {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	}
	c.Next()
}
