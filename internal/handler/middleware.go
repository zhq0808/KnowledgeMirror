package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// ctxKey 是 context 键类型，避免键冲突。
type ctxKey string

const (
	traceIDKey ctxKey = "trace_id"
	userIDKey  ctxKey = "user_id"
)

// newTraceID 生成一个随机的 trace id（16 字节 hex）。
func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极少发生；退化为时间戳，保证不为空。
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// TraceIDFromContext 从 context 取出 trace id。
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// UserIDFromContext 返回认证 middleware 写入的可信用户 ID。
func UserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDKey).(string)
	return userID, ok && userID != ""
}

// authMiddleware 验证 HttpOnly Guest Cookie，并把可信 user_id 写入请求 context。
func authMiddleware(identity *service.IdentityService, cookieName string, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(cookieName)
		if err != nil || rawToken == "" {
			fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
			c.Abort()
			return
		}

		userID, err := identity.AuthenticateGuest(c.Request.Context(), rawToken)
		if errors.Is(err, service.ErrUnauthenticated) {
			fail(c, http.StatusUnauthorized, CodeUnauthorized, "身份凭证无效或已过期")
			c.Abort()
			return
		}
		if err != nil {
			log.Error("认证 Guest 身份失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
			fail(c, http.StatusInternalServerError, CodeInternal, "身份服务暂时不可用")
			c.Abort()
			return
		}

		ctx := context.WithValue(c.Request.Context(), userIDKey, userID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// traceMiddleware 为每个请求生成 trace id，写入 context 与响应头。
func traceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tid := c.GetHeader("X-Trace-Id")
		if tid == "" {
			tid = newTraceID()
		}
		ctx := context.WithValue(c.Request.Context(), traceIDKey, tid)
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Trace-Id", tid)
		c.Next()
	}
}

// recoverMiddleware 捕获 panic，避免单个请求崩掉整个服务。
// 注意：不记录请求体，避免泄露简历、面试回答等敏感用户数据。
func recoverMiddleware(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("请求处理 panic",
					"trace_id", TraceIDFromContext(c.Request.Context()),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"panic", rec,
				)
				fail(c, http.StatusInternalServerError, CodeInternal, "内部错误")
				c.Abort()
			}
		}()
		c.Next()
	}
}

// accessLogMiddleware 记录请求耗时与状态码。仅记录安全字段，
// 绝不记录 query/body/header 中可能含用户隐私的内容。
func accessLogMiddleware(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("request",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}

// bodyLimitMiddleware 限制请求体大小，防止大请求/慢连接拖住服务。
func bodyLimitMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func bodyLimitMiddlewareExcept(maxBytes int64, method, path string) gin.HandlerFunc {
	limited := bodyLimitMiddleware(maxBytes)
	return func(c *gin.Context) {
		if c.Request.Method == method && c.Request.URL.Path == path {
			c.Next()
			return
		}
		limited(c)
	}
}
