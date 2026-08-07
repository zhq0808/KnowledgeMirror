package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

func TestGetFeynmanPracticeStateHandlerExposesCoachFields(t *testing.T) {
	const sessionID = "session_0123456789abcdef0123456789abcdef"
	practiceRepository := &handlerPracticeRepository{state: service.FeynmanPracticeState{
		State:                service.FeynmanStateAwaitingRetry,
		ActiveQuestionText:   "请重新解释 Outbox",
		RoundNo:              2,
		CoachTaskID:          "01900000-0000-7000-8000-000000000123",
		OriginalQuestionText: "为什么要使用 Outbox？",
		RetryRequired:        true,
	}}
	server := &Server{
		sessions: service.NewSessionService(&handlerSessionRepository{owners: map[string]string{
			sessionID: "usr_owner",
		}}),
		practice: service.NewFeynmanDialogService(practiceRepository, nil, nil, service.FeynmanDialogLimits{}, slog.New(slog.NewTextHandler(io.Discard, nil))),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/feynman/practice-state?session_id="+sessionID, nil)
	ginContext.Request = request.WithContext(context.WithValue(request.Context(), userIDKey, "usr_owner"))
	server.getFeynmanPracticeStateHandler(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data feynmanPracticeStateResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Data.CoachTaskID != "01900000-0000-7000-8000-000000000123" ||
		response.Data.OriginalQuestion != "为什么要使用 Outbox？" ||
		!response.Data.RetryRequired {
		t.Fatalf("practice response=%+v, want coach fields", response.Data)
	}
}
