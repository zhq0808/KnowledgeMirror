package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
)

func TestPostgresCoachFixedReviewLifecycle(t *testing.T) {
	if os.Getenv("INTERVIEW_AGENT_INTEGRATION_TEST") != "1" {
		t.Skip("set INTERVIEW_AGENT_INTEGRATION_TEST=1 to run PostgreSQL integration tests")
	}
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pool, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.RunMigrations(cfg.Postgres, os.DirFS("../../migrations"), "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	ctx := context.Background()
	userID := "usr_coach_03b_integration"
	sessionID := "session_coach_03b_integration"
	knowledgePointID := uuid.NewString()
	cleanupCoachLifecycleTest(t, pool, userID)
	defer cleanupCoachLifecycleTest(t, pool, userID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status) VALUES ($1, 0, 0);
		INSERT INTO agent_memory_session (session_id, user_id) VALUES ($2, $1);
		INSERT INTO knowledge_points (knowledge_point_id, user_id, title, created_by, updated_by)
		VALUES ($3, $1, 'Outbox', $1, $1)`, userID, sessionID, knowledgePointID); err != nil {
		t.Fatalf("seed owner/session/knowledge point: %v", err)
	}

	repo := store.NewPostgresCoachRepository(pool)
	taskDate := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	taskID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO coach_daily_tasks (
			coach_task_id, user_id, task_date, task_type, plan_role, status, source_key,
			question_text, knowledge_point_id, priority, created_by, updated_by
		) VALUES ($1, $2, $3, 'feynman_new', 'required', 'pending', $4, $5, $6, 1, $2, $2)`,
		taskID, userID, taskDate, "integration:new:"+taskID, "解释 Outbox", knowledgePointID); err != nil {
		t.Fatalf("insert initial task: %v", err)
	}
	if _, err := repo.StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: taskID, SessionID: sessionID}); err != nil {
		t.Fatalf("start initial task: %v", err)
	}

	firstMessageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 1, "初次回答有遗漏")
	gapTempID := uuid.NewString()
	gapKey := "key_points-outbox-relay"
	failed := coachLifecycleCommit(taskID, userID, sessionID, firstMessageID, taskDate, service.CoachAttemptOutcomeRetryRequired)
	failed.Gaps = []service.CoachGapEvidence{{
		AttemptGapID: uuid.NewString(), GapID: gapTempID, GapKey: gapKey,
		GapType: service.CoachGapTypeKnowledge, DiagnosticDimension: service.FeynmanDimensionKeyPoints,
		Title: "Outbox relay", Description: "未解释 relay", Severity: 4, IsFocus: true,
		RequiresCorrection: true, EvidenceJSON: []byte(`{"verdict":"omitted"}`),
	}}
	failed.PracticeState = coachLifecycleRetryState(userID, sessionID, taskID, firstMessageID)
	if _, err := repo.CommitAnalysis(ctx, failed); err != nil {
		t.Fatalf("commit initial failure: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE user_id = $1`, userID, 0)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM coach_task_pending_gaps WHERE user_id = $1 AND status = 'pending'`, userID, 1)

	secondMessageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 2, "纠正后完整回答")
	corrected := coachLifecycleCommit(taskID, userID, sessionID, secondMessageID, taskDate, service.CoachAttemptOutcomePassed)
	corrected.PracticeState = coachLifecycleIdleState(userID, sessionID, secondMessageID)
	if _, err := repo.CommitAnalysis(ctx, corrected); err != nil {
		t.Fatalf("commit immediate correction: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_review_cycles WHERE user_id = $1`, userID, 1)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE user_id = $1`, userID, 3)
	for stage, wantDate := range []string{"2026-08-08", "2026-08-10", "2026-08-14"} {
		var gotDate time.Time
		if err := pool.QueryRow(ctx, `
			SELECT scheduled_date FROM feynman_gap_reviews
			WHERE user_id = $1 AND stage = $2`, userID, stage+1).Scan(&gotDate); err != nil {
			t.Fatalf("read stage %d: %v", stage+1, err)
		}
		if gotDate.Format(time.DateOnly) != wantDate {
			t.Fatalf("stage %d date = %s, want %s", stage+1, gotDate.Format(time.DateOnly), wantDate)
		}
	}
	if _, err := repo.CommitAnalysis(ctx, corrected); err != nil {
		t.Fatalf("replay correction: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_review_cycles WHERE user_id = $1`, userID, 1)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE user_id = $1`, userID, 3)

	var gapID, sourceReviewID string
	if err := pool.QueryRow(ctx, `
		SELECT gap_id::text, gap_review_id::text FROM feynman_gap_reviews
		WHERE user_id = $1 AND stage = 1`, userID).Scan(&gapID, &sourceReviewID); err != nil {
		t.Fatalf("read first review: %v", err)
	}
	retestTaskID := insertCoachLifecycleRetestTask(t, pool, userID, knowledgePointID, taskDate.AddDate(0, 0, 1), gapID, sourceReviewID)
	if _, err := repo.StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: retestTaskID, SessionID: sessionID}); err != nil {
		t.Fatalf("start retest task: %v", err)
	}
	thirdMessageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 3, "复测仍遗漏目标且出现新问题")
	newGapTempID := uuid.NewString()
	retestFailed := coachLifecycleCommit(retestTaskID, userID, sessionID, thirdMessageID, taskDate.AddDate(0, 0, 1), service.CoachAttemptOutcomeRetryRequired)
	retestFailed.Gaps = []service.CoachGapEvidence{
		{AttemptGapID: uuid.NewString(), GapID: uuid.NewString(), ForceCanonicalGapID: gapID, GapKey: gapKey,
			GapType: service.CoachGapTypeRecall, DiagnosticDimension: service.FeynmanDimensionKeyPoints,
			Title: "Outbox relay", Description: "目标再次遗漏", Severity: 4, IsFocus: true,
			RequiresCorrection: true, EvidenceJSON: []byte(`{"verdict":"omitted"}`)},
		{AttemptGapID: uuid.NewString(), GapID: newGapTempID, GapKey: "causal_chain-outbox-polling",
			GapType: service.CoachGapTypeKnowledge, DiagnosticDimension: service.FeynmanDimensionCausalChain,
			Title: "polling causality", Description: "新阻断问题", Severity: 3,
			RequiresCorrection: true, EvidenceJSON: []byte(`{"verdict":"omitted"}`)},
	}
	retestFailed.ReviewDecision = service.CoachReviewDecision{IsRetest: true, TargetRecurred: true, CurrentReviewStatus: service.FeynmanGapReviewStatusFailed}
	retestFailed.PracticeState = coachLifecycleRetryState(userID, sessionID, retestTaskID, thirdMessageID)
	if _, err := repo.CommitAnalysis(ctx, retestFailed); err != nil {
		t.Fatalf("commit failed retest: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE gap_review_id = $1 AND status = 'failed'`, sourceReviewID, 1)

	fourthMessageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 4, "复测纠正后完整回答")
	retestCorrected := coachLifecycleCommit(retestTaskID, userID, sessionID, fourthMessageID, taskDate.AddDate(0, 0, 1), service.CoachAttemptOutcomePassed)
	retestCorrected.ReviewDecision = service.CoachReviewDecision{IsRetest: true, TargetRecurred: false, CurrentReviewStatus: service.FeynmanGapReviewStatusPassed}
	retestCorrected.PracticeState = coachLifecycleIdleState(userID, sessionID, fourthMessageID)
	if _, err := repo.CommitAnalysis(ctx, retestCorrected); err != nil {
		t.Fatalf("commit retest correction: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews r JOIN feynman_gap_review_cycles c USING (review_cycle_id) WHERE c.user_id = $1 AND c.anchor_date = DATE '2026-08-08'`, userID, 6)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE user_id = $1 AND status = 'cancelled'`, userID, 2)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM coach_task_pending_gaps WHERE user_id = $1 AND status = 'corrected'`, userID, 3)

	// The restarted stage 3 pass resolves the target only after stages 1/2 no longer remain active.
	var restartedStage1, restartedStage2, restartedStage3 string
	if err := pool.QueryRow(ctx, `
		SELECT max(gap_review_id::text) FILTER (WHERE stage = 1),
		       max(gap_review_id::text) FILTER (WHERE stage = 2),
		       max(gap_review_id::text) FILTER (WHERE stage = 3)
		FROM feynman_gap_reviews r
		JOIN feynman_gap_review_cycles c USING (review_cycle_id)
		WHERE c.user_id = $1 AND c.gap_id = $2 AND c.anchor_date = DATE '2026-08-08'`, userID, gapID).
		Scan(&restartedStage1, &restartedStage2, &restartedStage3); err != nil {
		t.Fatalf("read restarted target stages: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE feynman_gap_reviews
		SET status = 'passed', completed_attempt_id = $4, completed_at = now(), updated_by = user_id
		WHERE gap_review_id IN ($1, $2) AND user_id = $3`, restartedStage1, restartedStage2, userID, retestCorrected.Attempt.CoachAttemptID); err == nil {
		t.Fatal("two stages cannot reuse one completed_attempt_id")
	}
	// Seed passed earlier stages with distinct existing attempts while keeping append-only facts untouched.
	if _, err := pool.Exec(ctx, `
		UPDATE feynman_gap_reviews SET status = 'cancelled', completed_at = now(), updated_by = user_id
		WHERE gap_review_id IN ($1, $2) AND user_id = $3`, restartedStage1, restartedStage2, userID); err != nil {
		t.Fatalf("close restarted earlier stages: %v", err)
	}
	stage3TaskID := insertCoachLifecycleRetestTask(t, pool, userID, knowledgePointID, taskDate.AddDate(0, 0, 8), gapID, restartedStage3)
	if _, err := repo.StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: stage3TaskID, SessionID: sessionID}); err != nil {
		t.Fatalf("start restarted stage 3: %v", err)
	}
	stage3MessageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 5, "第三阶段通过")
	stage3Passed := coachLifecycleCommit(stage3TaskID, userID, sessionID, stage3MessageID, taskDate.AddDate(0, 0, 8), service.CoachAttemptOutcomePassed)
	stage3Passed.ReviewDecision = service.CoachReviewDecision{IsRetest: true, CurrentReviewStatus: service.FeynmanGapReviewStatusPassed}
	stage3Passed.PracticeState = coachLifecycleIdleState(userID, sessionID, stage3MessageID)
	if _, err := repo.CommitAnalysis(ctx, stage3Passed); err != nil {
		t.Fatalf("commit restarted stage 3 pass: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gaps WHERE gap_id = $1 AND status = 'resolved'`, gapID, 1)
}

func TestPostgresCoachStartRejectsAnotherActiveTaskWithTypedConflict(t *testing.T) {
	if os.Getenv("INTERVIEW_AGENT_INTEGRATION_TEST") != "1" {
		t.Skip("set INTERVIEW_AGENT_INTEGRATION_TEST=1 to run PostgreSQL integration tests")
	}
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pool, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.RunMigrations(cfg.Postgres, os.DirFS("../../migrations"), "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	ctx := context.Background()
	userID := "usr_coach_start_conflict"
	sessionID := "session_coach_start_conflict"
	cleanupCoachLifecycleTest(t, pool, userID)
	defer cleanupCoachLifecycleTest(t, pool, userID)
	firstID, secondID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status) VALUES ($1, 0, 0);
		INSERT INTO agent_memory_session (session_id, user_id) VALUES ($2, $1);
		INSERT INTO coach_daily_tasks (
			coach_task_id, user_id, task_date, task_type, plan_role, status, source_key,
			question_text, priority, created_by, updated_by
		) VALUES
			($3, $1, CURRENT_DATE, 'feynman_new', 'required', 'pending', $5, '第一题', 1, $1, $1),
			($4, $1, CURRENT_DATE, 'feynman_new', 'optional', 'pending', $6, '第二题', 2, $1, $1)`,
		userID, sessionID, firstID, secondID, "start-conflict:"+firstID, "start-conflict:"+secondID); err != nil {
		t.Fatalf("seed conflict tasks: %v", err)
	}
	repo := store.NewPostgresCoachRepository(pool)
	if _, err := repo.StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: firstID, SessionID: sessionID}); err != nil {
		t.Fatalf("start first task: %v", err)
	}
	if _, err := repo.StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: secondID, SessionID: sessionID}); !errors.Is(err, service.ErrCoachTaskNotStartable) {
		t.Fatalf("second start error = %v", err)
	}
}

func TestPostgresCoachPlannerSelectsMissedOverdueReview(t *testing.T) {
	if os.Getenv("INTERVIEW_AGENT_INTEGRATION_TEST") != "1" {
		t.Skip("set INTERVIEW_AGENT_INTEGRATION_TEST=1 to run PostgreSQL integration tests")
	}
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pool, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.RunMigrations(cfg.Postgres, os.DirFS("../../migrations"), "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	ctx := context.Background()
	userID := "usr_coach_03b_planner"
	sessionID := "session_coach_03b_planner"
	knowledgePointID := uuid.NewString()
	cleanupCoachLifecycleTest(t, pool, userID)
	defer cleanupCoachLifecycleTest(t, pool, userID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status) VALUES ($1, 0, 0);
		INSERT INTO agent_memory_session (session_id, user_id) VALUES ($2, $1);
		INSERT INTO knowledge_points (knowledge_point_id, user_id, title, created_by, updated_by)
		VALUES ($3, $1, 'Planner source', $1, $1)`, userID, sessionID, knowledgePointID); err != nil {
		t.Fatalf("seed planner owner: %v", err)
	}

	anchor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	taskID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO coach_daily_tasks (
			coach_task_id, user_id, task_date, task_type, plan_role, status, source_key,
			question_text, knowledge_point_id, priority, session_id, started_at, completed_at, created_by, updated_by
		) VALUES ($1, $2, $3, 'feynman_new', 'optional', 'completed', $4,
			'planner lineage', $5, 100, $6, now(), now(), $2, $2)`,
		taskID, userID, anchor, "planner-lineage:"+taskID, knowledgePointID, sessionID); err != nil {
		t.Fatalf("insert planner lineage task: %v", err)
	}
	messageID := insertCoachLifecycleMessage(t, pool, userID, sessionID, 1, "planner lineage")
	attemptID := uuid.NewString()
	gapID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO coach_attempts (
			coach_attempt_id, coach_task_id, user_id, session_id, answer_message_id,
			original_question_text, analysis_payload, outcome, prompt_version, model_name, created_by
		) VALUES ($1, $2, $3, $4, $5, 'planner lineage', '{}', 'retry_required', 'test', 'test', $3);
		INSERT INTO feynman_gaps (
			gap_id, user_id, knowledge_point_id, gap_key, gap_type, diagnostic_dimension,
			title, status, evidence_count, created_by, updated_by
		) VALUES ($6, $3, $7, $8, 'knowledge_gap', 'key_points', 'overdue target', 'open', 1, $3, $3);
		INSERT INTO coach_attempt_gaps (
			attempt_gap_id, coach_attempt_id, gap_id, user_id, gap_key, gap_type,
			diagnostic_dimension, classification, title, severity, is_focus, evidence_payload
		) VALUES ($9, $1, $6, $3, $8, 'knowledge_gap', 'key_points', 'new', 'overdue target', 4, true, '{}')`,
		attemptID, taskID, userID, sessionID, messageID, gapID, knowledgePointID, "key_points-overdue-target", uuid.NewString()); err != nil {
		t.Fatalf("insert planner lineage facts: %v", err)
	}
	cycleID := uuid.NewString()
	reviewID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO feynman_gap_review_cycles (
			review_cycle_id, gap_id, source_attempt_id, correction_attempt_id, coach_task_id, user_id,
			anchor_date, created_by, updated_by
		) VALUES ($1, $2, $3, $3, $4, $5, $6, $5, $5);
		INSERT INTO feynman_gap_reviews (
			gap_review_id, review_cycle_id, gap_id, source_attempt_id, user_id,
			stage, scheduled_date, scheduled_for, status, created_by, updated_by
		) VALUES ($7, $1, $2, $3, $5, 1, $8, $8::date::timestamp, 'scheduled', $5, $5)`,
		cycleID, gapID, attemptID, taskID, userID, anchor, reviewID, anchor.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("insert missed review: %v", err)
	}

	planDate := anchor.AddDate(0, 0, 5)
	firstTaskID := insertCoachLifecycleRetestTask(t, pool, userID, knowledgePointID, planDate, gapID, reviewID)
	if _, err := store.NewPostgresCoachRepository(pool).StartTaskInSession(ctx, service.StartCoachTaskParams{UserID: userID, CoachTaskID: firstTaskID, SessionID: sessionID}); err != nil {
		t.Fatalf("start review task before skip: %v", err)
	}
	if err := store.NewPostgresCoachRepository(pool).ControlTask(ctx, service.CoachTaskControlParams{
		UserID: userID, SessionID: sessionID, CoachTaskID: firstTaskID, Action: "skip", ControlledAt: planDate,
		PracticeState: service.FeynmanPracticeState{UserID: userID, SessionID: sessionID, State: service.FeynmanStateIdle},
	}); err != nil {
		t.Fatalf("skip review task: %v", err)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM feynman_gap_reviews WHERE gap_review_id = $1 AND status = 'missed'`, reviewID, 1)

	sameDay, err := store.NewPostgresCoachRepository(pool).EnsureDailyPlan(ctx, userID, planDate)
	if err != nil {
		t.Fatalf("ensure same skip date: %v", err)
	}
	if sameDay.Required != nil && sameDay.Required.SourceReviewID == reviewID {
		t.Fatalf("same-day skip must not replan same review: %+v", sameDay)
	}

	nextDate := planDate.AddDate(0, 0, 1)
	plan, err := store.NewPostgresCoachRepository(pool).EnsureDailyPlan(ctx, userID, nextDate)
	if err != nil {
		t.Fatalf("ensure later planner date: %v", err)
	}
	if plan.Required == nil || plan.Required.SourceReviewID != reviewID || plan.Required.SourceGapID != gapID {
		t.Fatalf("plan did not select missed overdue review later: %+v", plan)
	}
	again, err := store.NewPostgresCoachRepository(pool).EnsureDailyPlan(ctx, userID, nextDate)
	if err != nil {
		t.Fatalf("ensure planner replay: %v", err)
	}
	if again.Required == nil || again.Required.CoachTaskID != plan.Required.CoachTaskID {
		t.Fatalf("planner replay duplicated task: first=%+v second=%+v", plan, again)
	}
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM coach_daily_tasks WHERE source_review_id = $1 AND status <> 'skipped'`, reviewID, 1)
	assertCoachLifecycleCount(t, pool, `SELECT count(*) FROM coach_daily_tasks WHERE source_review_id = $1 AND status = 'skipped'`, reviewID, 1)
}

func coachLifecycleCommit(taskID, userID, sessionID, messageID string, taskDate time.Time, outcome string) service.CommitCoachAnalysisParams {
	return service.CommitCoachAnalysisParams{
		Attempt: service.CoachAttempt{
			CoachAttemptID: uuid.NewString(), CoachTaskID: taskID, UserID: userID, SessionID: sessionID,
			AnswerMessageID: messageID, OriginalQuestionText: "解释 Outbox", AnalysisJSON: []byte(`{"result":"sanitized"}`),
			Outcome: outcome, PromptVersion: "integration-v1", ModelName: "integration-model",
		},
		CorrectionDate: taskDate, CompletedAt: time.Now(),
	}
}

func coachLifecycleRetryState(userID, sessionID, taskID, messageID string) service.FeynmanPracticeState {
	return service.FeynmanPracticeState{
		UserID: userID, SessionID: sessionID, State: service.FeynmanStateAwaitingRetry,
		ActiveQuestionText: "解释 Outbox", QuestionOrigin: service.FeynmanQuestionOriginCoachTask,
		CoachTaskID: taskID, OriginalQuestionText: "解释 Outbox", RetryRequired: true,
		LastAnsweredMessageID: messageID, LastFeedback: "请重答", RoundNo: 2,
	}
}

func coachLifecycleIdleState(userID, sessionID, messageID string) service.FeynmanPracticeState {
	return service.FeynmanPracticeState{
		UserID: userID, SessionID: sessionID, State: service.FeynmanStateIdle,
		LastAnsweredMessageID: messageID, LastFeedback: "通过", RoundNo: 3,
	}
}

func insertCoachLifecycleMessage(t *testing.T, pool *pgxpool.Pool, userID, sessionID string, seq int, content string) string {
	t.Helper()
	messageID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO agent_memory_episodic (
			message_id, session_id, user_id, agent_id, seq, role, status, content
		) VALUES ($1, $2, $3, 'coach-test', $4, 'user', 'completed', $5)`,
		messageID, sessionID, userID, seq, content); err != nil {
		t.Fatalf("insert answer message: %v", err)
	}
	return messageID
}

func insertCoachLifecycleRetestTask(t *testing.T, pool *pgxpool.Pool, userID, knowledgePointID string, date time.Time, gapID, reviewID string) string {
	t.Helper()
	taskID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO coach_daily_tasks (
			coach_task_id, user_id, task_date, task_type, plan_role, status, source_key,
			question_text, knowledge_point_id, source_gap_id, source_review_id,
			priority, created_by, updated_by
		) VALUES ($1, $2, $3, 'feynman_retry', 'required', 'pending', $4,
			'解释 Outbox', $5, $6, $7, 1, $2, $2)`,
		taskID, userID, date, "integration:review:"+reviewID, knowledgePointID, gapID, reviewID); err != nil {
		t.Fatalf("insert retest task: %v", err)
	}
	return taskID
}

func assertCoachLifecycleCount(t *testing.T, pool *pgxpool.Pool, query string, arg any, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query, arg).Scan(&got); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d; query=%s", got, want, query)
	}
}

func cleanupCoachLifecycleTest(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cleanup: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("disable cleanup triggers: %v", err)
	}
	for _, statement := range []string{
		`DELETE FROM coach_task_pending_gaps WHERE user_id = $1`,
		`DELETE FROM feynman_gap_reviews WHERE user_id = $1`,
		`DELETE FROM feynman_gap_review_cycles WHERE user_id = $1`,
		`DELETE FROM coach_attempt_gaps WHERE user_id = $1`,
		`DELETE FROM coach_attempts WHERE user_id = $1`,
		`DELETE FROM feynman_practice_states WHERE user_id = $1`,
		`DELETE FROM coach_daily_tasks WHERE user_id = $1`,
		`DELETE FROM feynman_gaps WHERE user_id = $1`,
		`DELETE FROM agent_memory_episodic WHERE user_id = $1`,
		`DELETE FROM knowledge_points WHERE user_id = $1`,
		`DELETE FROM agent_memory_session WHERE user_id = $1`,
		`DELETE FROM agent_user WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, userID); err != nil {
			t.Fatalf("cleanup %q: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cleanup: %v", err)
	}
}
