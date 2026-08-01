package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
)

type handlerGuestRecord struct {
	userID    string
	expiresAt time.Time
}

type handlerIdentityRepository struct {
	byHash      map[string]handlerGuestRecord
	createCalls int
}

func (r *handlerIdentityRepository) FindActiveGuest(_ context.Context, tokenHash []byte, now time.Time) (string, time.Time, bool, error) {
	record, found := r.byHash[string(tokenHash)]
	if !found || !record.expiresAt.After(now) {
		return "", time.Time{}, false, nil
	}
	return record.userID, record.expiresAt, true, nil
}

func (r *handlerIdentityRepository) CreateGuest(_ context.Context, userID string, tokenHash []byte, expiresAt time.Time) (bool, error) {
	r.createCalls++
	key := string(tokenHash)
	if _, exists := r.byHash[key]; exists {
		return false, nil
	}
	r.byHash[key] = handlerGuestRecord{userID: userID, expiresAt: expiresAt}
	return true, nil
}

func TestGuestHandlerCreatesCookieAndReusesIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &handlerIdentityRepository{byHash: make(map[string]handlerGuestRecord)}
	identityService := service.NewIdentityService(repository, 24*time.Hour)
	server := &Server{
		identity: identityService,
		identityConfig: config.IdentityConfig{
			GuestCookieName: "interview_guest_test",
			CookieSecure:    false,
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	firstRecorder := httptest.NewRecorder()
	firstContext, _ := gin.CreateTestContext(firstRecorder)
	firstContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/guest", nil)
	server.guestHandler(firstContext)

	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusOK)
	}
	firstReply := decodeGuestReply(t, firstRecorder)
	if !firstReply.Created || firstReply.UserID == "" {
		t.Fatalf("first reply = %+v, want newly created Guest", firstReply)
	}

	cookies := firstRecorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "interview_guest_test" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v, want named HttpOnly SameSite=Lax cookie", cookie)
	}
	if cookie.Path != "/" || cookie.MaxAge <= 0 || cookie.Value == "" {
		t.Fatalf("cookie path/max-age/value are invalid: %+v", cookie)
	}

	secondRecorder := httptest.NewRecorder()
	secondContext, _ := gin.CreateTestContext(secondRecorder)
	secondContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/guest", nil)
	secondContext.Request.AddCookie(cookie)
	server.guestHandler(secondContext)

	secondReply := decodeGuestReply(t, secondRecorder)
	if secondReply.Created || secondReply.UserID != firstReply.UserID {
		t.Fatalf("second reply = %+v, want restored user %q", secondReply, firstReply.UserID)
	}
	if repository.createCalls != 1 {
		t.Fatalf("CreateGuest() calls = %d, want 1", repository.createCalls)
	}
}

func decodeGuestReply(t *testing.T, recorder *httptest.ResponseRecorder) guestReply {
	t.Helper()
	var response struct {
		Data guestReply `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.Data
}
