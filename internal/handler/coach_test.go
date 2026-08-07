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

	"KnowledgeMirror/internal/service"
)

type handlerCoachRepository struct {
	plan          service.CoachDailyPlan
	progress      service.CoachProgress
	gaps          []service.FeynmanGap
	ensureCalls   int
	progressCalls int
	gapCalls      int
}

func (r *handlerCoachRepository) EnsureDailyPlan(_ context.Context, _ string, date time.Time) (service.CoachDailyPlan, error) {
	r.ensureCalls++
	result := r.plan
	result.Date = date
	return result, nil
}
func (r *handlerCoachRepository) GetProgress(_ context.Context, _ string, from, to time.Time) (service.CoachProgress, error) {
	r.progressCalls++
	result := r.progress
	result.From, result.To = from, to
	return result, nil
}
func (r *handlerCoachRepository) ListGaps(_ context.Context, _ string, _ string, _ int) ([]service.FeynmanGap, error) {
	r.gapCalls++
	return r.gaps, nil
}

func newCoachHandlerTestContext(method, target, userID string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.ReleaseMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(method, target, nil)
	if userID != "" {
		request = request.WithContext(context.WithValue(request.Context(), userIDKey, userID))
	}
	ctx.Request = request
	return ctx, recorder
}

func newCoachHandlerServer(repo service.CoachPlanRepository) *Server {
	return &Server{
		coach: service.NewCoachService(repo, func() time.Time {
			return time.Date(2026, 8, 7, 10, 0, 0, 0, time.Local)
		}),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestCoachTodayHandlerReturnsRequiredOptionalAndEmptyState(t *testing.T) {
	required := service.CoachDailyTask{
		CoachTaskID: "task-required", TaskDate: time.Date(2026, 8, 7, 0, 0, 0, 0, time.Local),
		TaskType: service.CoachTaskTypeFeynmanNew, PlanRole: service.CoachPlanRoleRequired,
		Status: service.CoachTaskStatusPending, QuestionText: service.CoachNewTopicQuestionPrefix + "Kafka",
	}
	repo := &handlerCoachRepository{plan: service.CoachDailyPlan{Required: &required, Optional: []service.CoachDailyTask{}}}
	server := newCoachHandlerServer(repo)
	ctx, recorder := newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/today?date=2026-08-07", "user-1")
	server.coachTodayHandler(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data coachTodayReply `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Data.Required == nil || response.Data.Required.PlanRole != service.CoachPlanRoleRequired || response.Data.Optional == nil {
		t.Fatalf("today response = %+v", response.Data)
	}
}

func TestCoachTodayHandlerMarksCarriedActiveTask(t *testing.T) {
	active := service.CoachDailyTask{
		CoachTaskID: "task-carried", TaskDate: time.Date(2026, 8, 6, 0, 0, 0, 0, time.Local),
		TaskType: service.CoachTaskTypeFeynmanNew, PlanRole: service.CoachPlanRoleRequired,
		Status: service.CoachTaskStatusAwaitingRetry, QuestionText: "原题",
	}
	repo := &handlerCoachRepository{plan: service.CoachDailyPlan{ActiveTask: &active}}
	server := newCoachHandlerServer(repo)
	ctx, recorder := newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/today?date=2026-08-07", "user-1")
	server.coachTodayHandler(ctx)
	var response struct {
		Data coachTodayReply `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Data.ActiveTask == nil || !response.Data.ActiveTask.CarriedOver || response.Data.ActiveTask.Date != "2026-08-06" {
		t.Fatalf("active task = %+v", response.Data.ActiveTask)
	}
}

func TestCoachHandlersValidateAuthenticationAndQueries(t *testing.T) {
	repo := &handlerCoachRepository{}
	server := newCoachHandlerServer(repo)

	ctx, recorder := newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/today", "")
	server.coachTodayHandler(ctx)
	if recorder.Code != http.StatusUnauthorized || repo.ensureCalls != 0 {
		t.Fatalf("unauth today status=%d calls=%d", recorder.Code, repo.ensureCalls)
	}

	ctx, recorder = newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/progress?from=2026-01-01&to=2026-08-07", "user-1")
	server.coachProgressHandler(ctx)
	if recorder.Code != http.StatusBadRequest || repo.progressCalls != 0 {
		t.Fatalf("invalid progress status=%d calls=%d", recorder.Code, repo.progressCalls)
	}

	ctx, recorder = newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/gaps?status=bad&limit=50", "user-1")
	server.coachGapsHandler(ctx)
	if recorder.Code != http.StatusBadRequest || repo.gapCalls != 0 {
		t.Fatalf("invalid gaps status=%d calls=%d", recorder.Code, repo.gapCalls)
	}
}

func TestCoachReplyIncludesRetryCountsAndReviewDate(t *testing.T) {
	date := time.Date(2026, 8, 8, 0, 0, 0, 0, time.Local)
	progress := toCoachProgressReply(service.CoachProgress{
		From: date, To: date, AwaitingRetry: 2,
		Days: []service.CoachProgressDay{{Date: date, InProgress: 1, AwaitingRetry: 2}},
	})
	if progress.AwaitingRetry != 2 || len(progress.Days) != 1 || progress.Days[0].InProgress != 1 || progress.Days[0].AwaitingRetry != 2 {
		t.Fatalf("progress reply = %+v", progress)
	}
	compatAt := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	reviewDate := time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local)
	gap := toCoachGapReply(service.FeynmanGap{NextReviewAt: &compatAt, NextReviewDate: &reviewDate})
	if gap.NextReviewDate == nil || *gap.NextReviewDate != "2026-08-10" || gap.NextReviewAt == nil {
		t.Fatalf("gap reply = %+v", gap)
	}
	compatOnly := toCoachGapReply(service.FeynmanGap{NextReviewAt: &compatAt})
	if compatOnly.NextReviewDate == nil || *compatOnly.NextReviewDate != "2026-08-09" {
		t.Fatalf("compat gap reply = %+v", compatOnly)
	}
}

func TestCoachProgressAndGapsHandlersReturnStableEmptyArrays(t *testing.T) {
	repo := &handlerCoachRepository{progress: service.CoachProgress{Days: []service.CoachProgressDay{}}, gaps: []service.FeynmanGap{}}
	server := newCoachHandlerServer(repo)

	ctx, recorder := newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/progress?from=2026-08-01&to=2026-08-07", "user-1")
	server.coachProgressHandler(ctx)
	if recorder.Code != http.StatusOK || !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("progress status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	ctx, recorder = newCoachHandlerTestContext(http.MethodGet, "/api/v1/coach/gaps?status=open&limit=50", "user-1")
	server.coachGapsHandler(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("gaps status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []coachGapReply `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Data == nil {
		t.Fatalf("gaps decode/data = %+v, %v", response.Data, err)
	}
}
