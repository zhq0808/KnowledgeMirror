package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

func TestAuthMiddlewareWritesTrustedUserToContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &handlerIdentityRepository{byHash: make(map[string]handlerGuestRecord)}
	identityService := service.NewIdentityService(repository, time.Hour)
	identity, err := identityService.EnsureGuest(t.Context(), "")
	if err != nil {
		t.Fatalf("EnsureGuest() error = %v", err)
	}

	engine := gin.New()
	engine.Use(authMiddleware(identityService, "interview_guest_test", slog.New(slog.NewTextHandler(io.Discard, nil))))
	engine.GET("/protected", func(c *gin.Context) {
		userID, ok := UserIDFromContext(c.Request.Context())
		if !ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.String(http.StatusOK, userID)
	})

	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.AddCookie(&http.Cookie{Name: "interview_guest_test", Value: identity.Token})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != identity.UserID {
		t.Fatalf("response = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), identity.UserID)
	}
}

func TestAuthMiddlewareRejectsMissingAndInvalidCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &handlerIdentityRepository{byHash: make(map[string]handlerGuestRecord)}
	identityService := service.NewIdentityService(repository, time.Hour)
	engine := gin.New()
	engine.Use(authMiddleware(identityService, "interview_guest_test", slog.New(slog.NewTextHandler(io.Discard, nil))))
	engine.GET("/protected", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, testCase := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{name: "missing"},
		{name: "invalid", cookie: &http.Cookie{Name: "interview_guest_test", Value: "invalid"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if testCase.cookie != nil {
				request.AddCookie(testCase.cookie)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
		})
	}
}
