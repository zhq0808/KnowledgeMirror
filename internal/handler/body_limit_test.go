package handler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUploadRouteUsesIndependentBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(bodyLimitMiddlewareExcept(maxBodyBytes, http.MethodPost, "/api/v1/documents"))
	engine.POST("/api/v1/regular", readBodyHandler)
	engine.POST(
		"/api/v1/documents",
		bodyLimitMiddleware(documentBodyLimitBytes(1<<20)),
		readBodyHandler,
	)

	tests := []struct {
		name       string
		path       string
		bodyBytes  int
		wantStatus int
	}{
		{name: "普通请求仍受全局限制", path: "/api/v1/regular", bodyBytes: 3 << 20, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "上传请求绕过全局限制", path: "/api/v1/documents", bodyBytes: 3 << 20, wantStatus: http.StatusNoContent},
		{name: "上传请求受独立限制", path: "/api/v1/documents", bodyBytes: 5 << 20, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(strings.Repeat("x", test.bodyBytes)))
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

func TestDocumentBodyLimitTracksConfiguredFileLimit(t *testing.T) {
	if got := documentBodyLimitBytes(1 << 20); got != 4<<20 {
		t.Fatalf("default body limit = %d, want %d", got, 4<<20)
	}
	if got := documentBodyLimitBytes(6 << 20); got != 7<<20 {
		t.Fatalf("configured body limit = %d, want %d", got, 7<<20)
	}
}

func readBodyHandler(c *gin.Context) {
	_, err := io.ReadAll(c.Request.Body)
	if err == nil {
		c.Status(http.StatusNoContent)
		return
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		c.Status(http.StatusRequestEntityTooLarge)
		return
	}
	c.Status(http.StatusInternalServerError)
}
