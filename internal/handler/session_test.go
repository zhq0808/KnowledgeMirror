package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

type handlerSessionRepository struct {
	owners     map[string]string
	inactive   map[string]bool
	listResult []service.SessionListItem
	listErr    error
}

func (r *handlerSessionRepository) CreateSession(_ context.Context, userID, sessionID string) (bool, error) {
	if _, exists := r.owners[sessionID]; exists {
		return false, nil
	}
	r.owners[sessionID] = userID
	return true, nil
}

func (r *handlerSessionRepository) OwnsSession(_ context.Context, userID, sessionID string) (bool, error) {
	return r.owners[sessionID] == userID, nil
}

func (r *handlerSessionRepository) OwnsActiveSession(_ context.Context, userID, sessionID string) (bool, error) {
	return r.owners[sessionID] == userID && !r.inactive[sessionID], nil
}

func (r *handlerSessionRepository) ListSessions(_ context.Context, _ string, _ int) ([]service.SessionListItem, error) {
	return r.listResult, r.listErr
}

func TestCreateSessionHandlerUsesAuthenticatedUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &handlerSessionRepository{owners: make(map[string]string)}
	server := &Server{sessions: service.NewSessionService(repository)}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	request = request.WithContext(context.WithValue(request.Context(), userIDKey, "usr_owner"))
	ginContext.Request = request

	server.createSessionHandler(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var ownerFound bool
	for sessionID, userID := range repository.owners {
		if !validSessionID(sessionID) {
			t.Fatalf("session ID = %q, want valid backend format", sessionID)
		}
		ownerFound = userID == "usr_owner"
	}
	if !ownerFound {
		t.Fatal("created session is not owned by authenticated user")
	}
}

func TestValidSessionID(t *testing.T) {
	if !validSessionID("session_0123456789abcdef0123456789abcdef") {
		t.Fatal("backend session ID was rejected")
	}
	for _, invalid := range []string{"", "session_bad", "session_01234567-89ab-cdef-0123-456789abcdef"} {
		if validSessionID(invalid) {
			t.Fatalf("invalid session ID %q was accepted", invalid)
		}
	}
}

func TestListSessionsHandlerReturnsAuthenticatedUsersSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	lastMessageAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	repository := &handlerSessionRepository{
		owners: make(map[string]string),
		listResult: []service.SessionListItem{
			{
				SessionID:     "session_0123456789abcdef0123456789abcdef",
				Title:         "体检异常咨询",
				Status:        "active",
				MessageCount:  4,
				LastMessageAt: &lastMessageAt,
				CreatedAt:     lastMessageAt,
			},
		},
	}
	server := &Server{sessions: service.NewSessionService(repository), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	request = request.WithContext(context.WithValue(request.Context(), userIDKey, "usr_owner"))
	ginContext.Request = request

	server.listSessionsHandler(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "session_0123456789abcdef0123456789abcdef") || !strings.Contains(body, "体检异常咨询") {
		t.Fatalf("body = %q, want the repository's session item", body)
	}
}

func TestListSessionsHandlerRequiresAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{sessions: service.NewSessionService(&handlerSessionRepository{owners: make(map[string]string)})}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)

	server.listSessionsHandler(ginContext)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestListSessionsHandlerFailsOnRepositoryError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &handlerSessionRepository{owners: make(map[string]string), listErr: errors.New("database unavailable")}
	server := &Server{sessions: service.NewSessionService(repository), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	request = request.WithContext(context.WithValue(request.Context(), userIDKey, "usr_owner"))
	ginContext.Request = request

	server.listSessionsHandler(ginContext)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

// performListSessionMessagesRequest 直接调用 handler，模拟经过路由后 gin 会把 URL 里的
// :session_id 填进 ginContext.Params 这件事——单测不走真实路由，所以要手动补上。
func performListSessionMessagesRequest(server *Server, sessionID, userID string, authenticated bool) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", nil)
	if authenticated {
		request = request.WithContext(context.WithValue(request.Context(), userIDKey, userID))
	}
	ginContext.Request = request
	ginContext.Params = gin.Params{{Key: "session_id", Value: sessionID}}

	server.listSessionMessagesHandler(ginContext)
	return recorder
}

func TestListSessionMessagesHandlerReturnsOwnedSessionMessages(t *testing.T) {
	const sessionID = "session_0123456789abcdef0123456789abcdef"
	messageRepository := &handlerMessageRepository{
		sessionMessages: []service.SessionMessage{
			{MessageID: "m1", Role: "user", Content: "早上体检报告有点异常", Seq: 1},
			{
				MessageID: "m2", Role: "assistant", Content: "具体是哪项指标异常呢？", Seq: 2,
				Retrieval: &service.MessageRetrieval{
					RequestID: "00000000-0000-4000-8000-000000000099",
					Status:    service.RetrievalStatusOK, CandidateCount: 1,
					Sources: []service.SessionRetrievalSource{{
						Ref: "S1", SourceChunkID: "chunk-1", DocumentID: "doc-1",
						DocumentTitle: "体检资料", VersionNo: 2, OriginLabel: "用户笔记", TrustLabel: "用户已确认",
					}},
				},
			},
		},
	}
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{owners: map[string]string{sessionID: "usr_owner"}}),
		messages: service.NewMessageService(messageRepository),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	recorder := performListSessionMessagesRequest(server, sessionID, "usr_owner", true)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	userIndex := strings.Index(body, "早上体检报告有点异常")
	assistantIndex := strings.Index(body, "具体是哪项指标异常呢？")
	if userIndex < 0 || assistantIndex < 0 || userIndex >= assistantIndex {
		t.Fatalf("body = %q, want both messages in seq order", body)
	}
	if !strings.Contains(body, `"request_id":"00000000-0000-4000-8000-000000000099"`) ||
		!strings.Contains(body, `"document_title":"体检资料"`) ||
		!strings.Contains(body, `"ref":"S1"`) {
		t.Fatalf("body = %q, want persisted retrieval sources", body)
	}
}

func TestListSessionMessagesHandlerAllowsArchivedOwnedSession(t *testing.T) {
	const sessionID = "session_0123456789abcdef0123456789abcdef"
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{
			owners:   map[string]string{sessionID: "usr_owner"},
			inactive: map[string]bool{sessionID: true},
		}),
		messages: service.NewMessageService(&handlerMessageRepository{
			sessionMessages: []service.SessionMessage{{MessageID: "m1", Role: "user", Content: "归档消息", Seq: 1}},
		}),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	recorder := performListSessionMessagesRequest(server, sessionID, "usr_owner", true)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "归档消息") {
		t.Fatalf("status = %d, body = %q; want archived history readable", recorder.Code, recorder.Body.String())
	}
}

// 结果恢复协议同款的"归属"校验：session_id 存在，但不属于当前认证用户，必须统一 404，
// 不能泄露"这个会话确实存在，只是不是你的"。
func TestListSessionMessagesHandlerReturnsNotFoundForOtherUsersSession(t *testing.T) {
	const sessionID = "session_0123456789abcdef0123456789abcdef"
	messageRepository := &handlerMessageRepository{
		sessionMessages: []service.SessionMessage{{MessageID: "m1", Role: "user", Content: "别人的面试回答", Seq: 1}},
	}
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{owners: map[string]string{sessionID: "usr_other"}}),
		messages: service.NewMessageService(messageRepository),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	recorder := performListSessionMessagesRequest(server, sessionID, "usr_owner", true)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "别人的面试回答") {
		t.Fatalf("body = %q, must not leak another user's message content", recorder.Body.String())
	}
}

func TestListSessionMessagesHandlerRejectsInvalidSessionID(t *testing.T) {
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{owners: make(map[string]string)}),
		messages: service.NewMessageService(&handlerMessageRepository{}),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	recorder := performListSessionMessagesRequest(server, "not-a-valid-session-id", "usr_owner", true)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestListSessionMessagesHandlerRequiresAuthentication(t *testing.T) {
	const sessionID = "session_0123456789abcdef0123456789abcdef"
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{owners: map[string]string{sessionID: "usr_owner"}}),
		messages: service.NewMessageService(&handlerMessageRepository{}),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	recorder := performListSessionMessagesRequest(server, sessionID, "", false)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}
