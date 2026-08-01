package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	appLogger "KnowledgeMirror/internal/logger"
)

func TestTraceMiddlewarePropagatesTraceIDToContextLogger(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	var logs bytes.Buffer
	log := slog.New(&appLogger.ContextHandler{
		Handler: slog.NewJSONHandler(&logs, nil),
	})
	engine := gin.New()
	engine.Use(traceMiddleware())
	engine.GET("/trace", func(c *gin.Context) {
		log.InfoContext(c.Request.Context(), "trace probe")
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/trace", nil)
	request.Header.Set("X-Trace-Id", "trace-handler-123")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Header().Get("X-Trace-Id") != "trace-handler-123" {
		t.Fatalf("response trace ID = %q", recorder.Header().Get("X-Trace-Id"))
	}
	if !strings.Contains(logs.String(), `"trace_id":"trace-handler-123"`) {
		t.Fatalf("context log missing request trace ID: %s", logs.String())
	}
}
