package main

import (
	"crypto/subtle"
	"bytes"
	"encoding/json"
	"io"
	"time"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// authMiddleware 静态 Bearer Token 鉴权中间件，Token 为空时关闭鉴权。
func authMiddleware(token string) gin.HandlerFunc {
	expectedToken := []byte(token)

	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}

		scheme, credentials, found := strings.Cut(c.GetHeader("Authorization"), " ")
		credentials = strings.TrimLeft(credentials, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(credentials), expectedToken) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			respondError(c, http.StatusUnauthorized, "UNAUTHORIZED", "未授权", nil)
			c.Abort()
			return
		}

		c.Next()
	}
}

// corsMiddleware CORS 中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// errorHandlingMiddleware 错误处理中间件
func errorHandlingMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		logrus.Errorf("服务器内部错误: %v, path: %s", recovered, c.Request.URL.Path)

		respondError(c, http.StatusInternalServerError, "INTERNAL_ERROR",
			"服务器内部错误", recovered)
	})
}

// isSafeReadRequest only recognizes known read-only MCP calls. It rewinds
// request bodies so the downstream MCP SDK sees exactly the same bytes.
// Unknown calls and writes are never placed in the queue.
func isSafeReadRequest(c *gin.Context) bool {
	if c.Request.Method == http.MethodGet { return true }
	if c.Request.Method != http.MethodPost || !strings.HasPrefix(c.Request.URL.Path, "/mcp") {
		return c.Request.Method == http.MethodGet
	}
	if c.Request.Body == nil { return false }
	const maxPeek = 16384
	prefix, err := io.ReadAll(io.LimitReader(c.Request.Body, maxPeek+1))
	c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), c.Request.Body))
	if err != nil || len(prefix) > maxPeek { return false }
	var request struct {
		Method string `json:"method"`
		Params struct { Name string `json:"name"` } `json:"params"`
	}
	if json.Unmarshal(prefix, &request) != nil { return false }
	if request.Method == "tools/list" || request.Method == "initialize" || request.Method == "ping" { return true }
	if request.Method != "tools/call" { return false }
	switch request.Params.Name {
	case "check_login_status", "get_feed_detail", "get_feeds_list", "search_feeds", "get_user_profile", "get_user_profile_feeds":
		return true
	default:
		return false
	}
}

// Serializes Chromium work on the 1GB service. Only read-only calls wait
// briefly for the slot. All writes fail fast to prevent delayed duplicate posts.
func browserTrafficLimiter() gin.HandlerFunc {
	slots := make(chan struct{}, 1)
	return func(c *gin.Context) {
		if isSafeReadRequest(c) {
			timer := time.NewTimer(20*time.Second)
			defer timer.Stop()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				c.Next()
				return
			case <-c.Request.Context().Done():
				c.AbortWithStatus(http.StatusRequestTimeout)
				return
			case <-timer.C:
				c.Header("Retry-After", "5")
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": "只读请求等待浏览器资源超时，请稍后再试",
				})
				return
			}
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			c.Next()
		default:
			c.Header("Retry-After", "5")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "浏览器正在处理另一项请求，请稍后重试",
			})
		}
	}
}
