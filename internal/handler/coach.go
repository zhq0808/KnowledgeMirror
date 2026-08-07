package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

type coachTaskReply struct {
	CoachTaskID      string  `json:"coach_task_id"`
	Date             string  `json:"date"`
	TaskType         string  `json:"task_type"`
	PlanRole         string  `json:"plan_role"`
	Status           string  `json:"status"`
	Question         string  `json:"question"`
	KnowledgePointID string  `json:"knowledge_point_id,omitempty"`
	SourceGapID      string  `json:"source_gap_id,omitempty"`
	SessionID        string  `json:"session_id,omitempty"`
	CarriedOver      bool    `json:"carried_over"`
	StartedAt        *string `json:"started_at,omitempty"`
	CompletedAt      *string `json:"completed_at,omitempty"`
}

type coachEmptyStateReply struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Action     string `json:"action"`
	ActionPath string `json:"action_path"`
}

type coachTodayReply struct {
	Date          string                `json:"date"`
	Required      *coachTaskReply       `json:"required"`
	Optional      []coachTaskReply      `json:"optional"`
	ActiveTask    *coachTaskReply       `json:"active_task,omitempty"`
	TerminalTasks []coachTaskReply      `json:"terminal_tasks"`
	EmptyState    *coachEmptyStateReply `json:"empty_state,omitempty"`
}

type coachProgressDayReply struct {
	Date              string `json:"date"`
	RequiredTotal     int    `json:"required_total"`
	RequiredCompleted int    `json:"required_completed"`
	OptionalTotal     int    `json:"optional_total"`
	OptionalCompleted int    `json:"optional_completed"`
	Pending           int    `json:"pending"`
	InProgress        int    `json:"in_progress"`
	AwaitingRetry     int    `json:"awaiting_retry"`
	Completed         int    `json:"completed"`
	Skipped           int    `json:"skipped"`
}

type coachProgressReply struct {
	From              string                  `json:"from"`
	To                string                  `json:"to"`
	RequiredTotal     int                     `json:"required_total"`
	RequiredCompleted int                     `json:"required_completed"`
	OptionalTotal     int                     `json:"optional_total"`
	OptionalCompleted int                     `json:"optional_completed"`
	Pending           int                     `json:"pending"`
	InProgress        int                     `json:"in_progress"`
	AwaitingRetry     int                     `json:"awaiting_retry"`
	Completed         int                     `json:"completed"`
	Skipped           int                     `json:"skipped"`
	Days              []coachProgressDayReply `json:"days"`
}

type coachGapReply struct {
	GapID            string  `json:"gap_id"`
	KnowledgePointID string  `json:"knowledge_point_id,omitempty"`
	GapKey           string  `json:"gap_key"`
	GapType          string  `json:"gap_type"`
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Status           string  `json:"status"`
	EvidenceCount    int     `json:"evidence_count"`
	FirstSeenAt      string  `json:"first_seen_at"`
	LastSeenAt       string  `json:"last_seen_at"`
	NextReviewDate   *string `json:"next_review_date,omitempty"`
	NextReviewAt     *string `json:"next_review_at,omitempty"`
}

func (s *Server) coachTodayHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	plan, err := s.coach.Today(c.Request.Context(), userID, c.Query("date"))
	if err != nil {
		s.failCoachError(c, "查询今日教练计划失败", err)
		return
	}
	ok(c, toCoachTodayReply(plan))
}

func (s *Server) coachProgressHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	progress, err := s.coach.Progress(c.Request.Context(), userID, c.Query("from"), c.Query("to"))
	if err != nil {
		s.failCoachError(c, "查询教练进度失败", err)
		return
	}
	ok(c, toCoachProgressReply(progress))
}

func (s *Server) coachGapsHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	gaps, err := s.coach.Gaps(c.Request.Context(), userID, c.Query("status"), c.Query("limit"))
	if err != nil {
		s.failCoachError(c, "查询教练薄弱点失败", err)
		return
	}
	reply := make([]coachGapReply, 0, len(gaps))
	for _, gap := range gaps {
		reply = append(reply, toCoachGapReply(gap))
	}
	ok(c, reply)
}

func (s *Server) failCoachError(c *gin.Context, action string, err error) {
	if errors.Is(err, service.ErrCoachQueryInput) {
		fail(c, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	s.log.Error(action, "error", err)
	fail(c, http.StatusInternalServerError, CodeInternal, action)
}

func toCoachTodayReply(plan service.CoachDailyPlan) coachTodayReply {
	reply := coachTodayReply{
		Date:          plan.Date.Format(time.DateOnly),
		Optional:      make([]coachTaskReply, 0, len(plan.Optional)),
		TerminalTasks: make([]coachTaskReply, 0, len(plan.TerminalTasks)),
	}
	if plan.Required != nil {
		task := toCoachTaskReply(*plan.Required, plan.Date)
		reply.Required = &task
	}
	for _, item := range plan.Optional {
		reply.Optional = append(reply.Optional, toCoachTaskReply(item, plan.Date))
	}
	for _, item := range plan.TerminalTasks {
		reply.TerminalTasks = append(reply.TerminalTasks, toCoachTaskReply(item, plan.Date))
	}
	if plan.ActiveTask != nil {
		task := toCoachTaskReply(*plan.ActiveTask, plan.Date)
		reply.ActiveTask = &task
	}
	if plan.EmptyState != nil {
		reply.EmptyState = &coachEmptyStateReply{
			Code: plan.EmptyState.Code, Message: plan.EmptyState.Message,
			Action: plan.EmptyState.Action, ActionPath: plan.EmptyState.ActionPath,
		}
	}
	return reply
}

func toCoachTaskReply(task service.CoachDailyTask, planDate time.Time) coachTaskReply {
	reply := coachTaskReply{
		CoachTaskID: task.CoachTaskID, Date: task.TaskDate.Format(time.DateOnly), TaskType: task.TaskType,
		PlanRole: task.PlanRole, Status: task.Status, Question: task.QuestionText,
		KnowledgePointID: task.KnowledgePointID, SourceGapID: task.SourceGapID, SessionID: task.SessionID,
		CarriedOver: daysBetweenDates(task.TaskDate, planDate) > 0,
	}
	if task.StartedAt != nil {
		value := task.StartedAt.Format(time.RFC3339)
		reply.StartedAt = &value
	}
	if task.CompletedAt != nil {
		value := task.CompletedAt.Format(time.RFC3339)
		reply.CompletedAt = &value
	}
	return reply
}

func daysBetweenDates(from, to time.Time) int {
	fromYear, fromMonth, fromDay := from.Date()
	toYear, toMonth, toDay := to.Date()
	fromUTC := time.Date(fromYear, fromMonth, fromDay, 0, 0, 0, 0, time.UTC)
	toUTC := time.Date(toYear, toMonth, toDay, 0, 0, 0, 0, time.UTC)
	return int(toUTC.Sub(fromUTC) / (24 * time.Hour))
}

func toCoachProgressReply(progress service.CoachProgress) coachProgressReply {
	reply := coachProgressReply{
		From: progress.From.Format(time.DateOnly), To: progress.To.Format(time.DateOnly),
		RequiredTotal: progress.RequiredTotal, RequiredCompleted: progress.RequiredCompleted,
		OptionalTotal: progress.OptionalTotal, OptionalCompleted: progress.OptionalCompleted,
		Pending: progress.Pending, InProgress: progress.InProgress, AwaitingRetry: progress.AwaitingRetry,
		Completed: progress.Completed, Skipped: progress.Skipped,
		Days: make([]coachProgressDayReply, 0, len(progress.Days)),
	}
	for _, day := range progress.Days {
		reply.Days = append(reply.Days, coachProgressDayReply{
			Date: day.Date.Format(time.DateOnly), RequiredTotal: day.RequiredTotal,
			RequiredCompleted: day.RequiredCompleted, OptionalTotal: day.OptionalTotal,
			OptionalCompleted: day.OptionalCompleted, Pending: day.Pending,
			InProgress: day.InProgress, AwaitingRetry: day.AwaitingRetry,
			Completed: day.Completed, Skipped: day.Skipped,
		})
	}
	return reply
}

func toCoachGapReply(gap service.FeynmanGap) coachGapReply {
	reply := coachGapReply{
		GapID: gap.GapID, KnowledgePointID: gap.KnowledgePointID, GapKey: gap.GapKey,
		GapType: gap.GapType, Title: gap.Title, Description: gap.Description, Status: gap.Status,
		EvidenceCount: gap.EvidenceCount, FirstSeenAt: gap.FirstSeenAt.Format(time.RFC3339),
		LastSeenAt: gap.LastSeenAt.Format(time.RFC3339),
	}
	nextReviewDate := gap.NextReviewDate
	if nextReviewDate == nil {
		nextReviewDate = gap.NextReviewAt
	}
	if nextReviewDate != nil {
		value := nextReviewDate.Format(time.DateOnly)
		reply.NextReviewDate = &value
	}
	if gap.NextReviewAt != nil {
		value := gap.NextReviewAt.Format(time.RFC3339)
		reply.NextReviewAt = &value
	}
	return reply
}
