package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
)

func TestPostgresFeynmanRepositoryAttemptAndAudioLifecycle(t *testing.T) {
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

	const userID = "usr_feynman_repository_test"
	knowledgePointID := uuid.Must(uuid.NewV7()).String()
	ctx := context.Background()
	cleanupFeynmanRepositoryTest(t, pool, userID)
	defer cleanupFeynmanRepositoryTest(t, pool, userID)
	seedFeynmanKnowledgePoint(t, pool, userID, knowledgePointID)

	repo := store.NewPostgresFeynmanRepository(pool)

	attemptID, err := service.NewFeynmanAttemptID()
	if err != nil {
		t.Fatalf("new attempt id: %v", err)
	}
	created, err := repo.CreateAttempt(ctx, service.CreateFeynmanAttemptParams{
		AttemptID:        attemptID,
		UserID:           userID,
		KnowledgePointID: knowledgePointID,
		IdempotencyKey:   "idem-1",
	})
	if err != nil {
		t.Fatalf("CreateAttempt() error = %v", err)
	}
	if created.Attempt.AttemptID != attemptID || created.ActiveAudioTask != nil || created.Confirmation != nil {
		t.Fatalf("CreateAttempt() = %+v, want fresh attempt with no audio/confirmation", created)
	}

	// 同一用户重复用同一幂等键创建，仓储层应报冲突，由 service 层负责回查复用。
	otherAttemptID, err := service.NewFeynmanAttemptID()
	if err != nil {
		t.Fatalf("new attempt id: %v", err)
	}
	if _, err := repo.CreateAttempt(ctx, service.CreateFeynmanAttemptParams{
		AttemptID:        otherAttemptID,
		UserID:           userID,
		KnowledgePointID: knowledgePointID,
		IdempotencyKey:   "idem-1",
	}); err != service.ErrFeynmanIdempotencyConflict {
		t.Fatalf("CreateAttempt() duplicate key error = %v, want ErrFeynmanIdempotencyConflict", err)
	}

	found, ok, err := repo.FindAttemptByIdempotencyKey(ctx, userID, "idem-1")
	if err != nil || !ok || found.Attempt.AttemptID != attemptID {
		t.Fatalf("FindAttemptByIdempotencyKey() = (%+v, %t, %v), want (attempt %s, true, nil)", found, ok, err, attemptID)
	}

	// 未知知识点应报 ErrFeynmanKnowledgePointNotFound。
	unknownAttemptID, err := service.NewFeynmanAttemptID()
	if err != nil {
		t.Fatalf("new attempt id: %v", err)
	}
	if _, err := repo.CreateAttempt(ctx, service.CreateFeynmanAttemptParams{
		AttemptID:        unknownAttemptID,
		UserID:           userID,
		KnowledgePointID: uuid.Must(uuid.NewV7()).String(),
		IdempotencyKey:   "idem-unknown",
	}); err != service.ErrFeynmanKnowledgePointNotFound {
		t.Fatalf("CreateAttempt() unknown knowledge point error = %v, want ErrFeynmanKnowledgePointNotFound", err)
	}

	// 上传首个录音：先原子占位进入 transcribing，再写入转写终态。
	audioTaskID, err := service.NewFeynmanAudioTaskID()
	if err != nil {
		t.Fatalf("new audio task id: %v", err)
	}
	sha := []byte("0123456789abcdef0123456789abcdef") // 32 字节，满足 sha256 长度约束
	claimed, won, err := repo.ClaimAudioTask(ctx, service.ClaimFeynmanAudioTaskParams{
		AudioTaskID: audioTaskID,
		AttemptID:   attemptID,
		UserID:      userID,
		MIMEType:    "audio/webm",
		SizeBytes:   11,
		SHA256:      sha,
		AudioData:   []byte("audio-bytes"),
		STTProvider: "local_placeholder",
		StaleBefore: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("ClaimAudioTask() error = %v", err)
	}
	if !won || claimed.ActiveAudioTask == nil || claimed.ActiveAudioTask.Status != service.FeynmanAudioStatusTranscribing {
		t.Fatalf("ClaimAudioTask() = (%+v, won=%t), want active transcribing task", claimed, won)
	}
	afterUpload, err := repo.CompleteAudioTask(ctx, service.CompleteFeynmanAudioTaskParams{
		AudioTaskID:   audioTaskID,
		AttemptID:     attemptID,
		UserID:        userID,
		Status:        service.FeynmanAudioStatusTranscribed,
		RawTranscript: "hello world",
		STTProvider:   "local_placeholder",
		STTModel:      "placeholder-v0",
		STTRequestID:  "req-1",
	})
	if err != nil {
		t.Fatalf("CompleteAudioTask() error = %v", err)
	}
	if afterUpload.ActiveAudioTask.RawTranscript != "hello world" {
		t.Fatalf("raw transcript = %q, want %q", afterUpload.ActiveAudioTask.RawTranscript, "hello world")
	}

	// 完全相同的字节应命中去重，不获得 STT 执行权。
	dedupe, won, err := repo.ClaimAudioTask(ctx, service.ClaimFeynmanAudioTaskParams{
		AudioTaskID: uuid.Must(uuid.NewV7()).String(), AttemptID: attemptID, UserID: userID,
		MIMEType: "audio/webm", SizeBytes: 11, SHA256: sha, AudioData: []byte("audio-bytes"),
		STTProvider: "local_placeholder", StaleBefore: time.Now().Add(-time.Minute),
	})
	if err != nil || won || dedupe.ActiveAudioTask == nil || dedupe.ActiveAudioTask.AudioTaskID != audioTaskID {
		t.Fatalf("ClaimAudioTask() duplicate = (%+v, won=%t, err=%v), want existing task %s", dedupe, won, err, audioTaskID)
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM feynman_audio_task_events WHERE audio_task_id = $1`, audioTaskID).Scan(&eventCount); err != nil {
		t.Fatalf("count audio task events: %v", err)
	}
	if eventCount != 3 {
		t.Fatalf("audio task event count = %d, want uploaded/transcribing/transcribed", eventCount)
	}

	detail, err := repo.GetAttemptDetail(ctx, userID, attemptID)
	if err != nil {
		t.Fatalf("GetAttemptDetail() error = %v", err)
	}
	if detail.Status() != service.FeynmanAttemptStatusTranscribed {
		t.Fatalf("Status() = %q, want transcribed", detail.Status())
	}

	// 确认转写后练习永久只读。
	confirmationID, err := service.NewFeynmanConfirmationID()
	if err != nil {
		t.Fatalf("new confirmation id: %v", err)
	}
	confirmed, err := repo.ConfirmTranscript(ctx, service.ConfirmFeynmanTranscriptParams{
		ConfirmationID:      confirmationID,
		AttemptID:           attemptID,
		UserID:              userID,
		ConfirmedTranscript: "hello world, corrected",
		ConfirmedBy:         userID,
	})
	if err != nil {
		t.Fatalf("ConfirmTranscript() error = %v", err)
	}
	if confirmed.Confirmation == nil || !confirmed.Confirmation.Edited {
		t.Fatalf("Confirmation = %+v, want non-nil with Edited=true", confirmed.Confirmation)
	}
	if confirmed.Status() != service.FeynmanAttemptStatusTranscriptConfirmed {
		t.Fatalf("Status() = %q, want transcript_confirmed", confirmed.Status())
	}

	// 已确认的练习不能再上传录音或再次确认。
	secondAudioTaskID, err := service.NewFeynmanAudioTaskID()
	if err != nil {
		t.Fatalf("new audio task id: %v", err)
	}
	if _, _, err := repo.ClaimAudioTask(ctx, service.ClaimFeynmanAudioTaskParams{
		AudioTaskID: secondAudioTaskID,
		AttemptID:   attemptID,
		UserID:      userID,
		MIMEType:    "audio/webm",
		SizeBytes:   5,
		SHA256:      []byte("abcdefghijklmnopqrstuvwxyz012345"),
		AudioData:   []byte("more"),
	}); err != service.ErrFeynmanAttemptConfirmed {
		t.Fatalf("ClaimAudioTask() after confirm error = %v, want ErrFeynmanAttemptConfirmed", err)
	}
	if _, err := repo.ConfirmTranscript(ctx, service.ConfirmFeynmanTranscriptParams{
		ConfirmationID:      confirmationID,
		AttemptID:           attemptID,
		UserID:              userID,
		ConfirmedTranscript: "again",
		ConfirmedBy:         userID,
	}); err != service.ErrFeynmanAttemptConfirmed {
		t.Fatalf("ConfirmTranscript() after confirm error = %v, want ErrFeynmanAttemptConfirmed", err)
	}
}

func TestPostgresFeynmanRepositoryRubricVersioning(t *testing.T) {
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

	const userID = "usr_feynman_rubric_test"
	knowledgePointID := uuid.Must(uuid.NewV7()).String()
	ctx := context.Background()
	cleanupFeynmanRepositoryTest(t, pool, userID)
	defer cleanupFeynmanRepositoryTest(t, pool, userID)
	seedFeynmanKnowledgePoint(t, pool, userID, knowledgePointID)

	repo := store.NewPostgresFeynmanRepository(pool)

	if _, found, err := repo.GetActiveRubric(ctx, userID, knowledgePointID); err != nil || found {
		t.Fatalf("GetActiveRubric() before any version = (found=%t, err=%v), want (false, nil)", found, err)
	}

	rubricV1ID, err := service.NewKnowledgePointRubricID()
	if err != nil {
		t.Fatalf("new rubric id: %v", err)
	}
	v1, err := repo.CreateRubricVersion(ctx, service.CreateRubricVersionParams{
		RubricID:         rubricV1ID,
		KnowledgePointID: knowledgePointID,
		UserID:           userID,
		TemplateVersion:  service.FeynmanRubricTemplateV1,
		Criteria:         service.DefaultRubricCriteria(),
		CreatedBy:        userID,
	})
	if err != nil {
		t.Fatalf("CreateRubricVersion() v1 error = %v", err)
	}
	if v1.VersionNo != 1 || len(v1.Criteria) != 5 {
		t.Fatalf("v1 = %+v, want VersionNo=1 with 5 criteria", v1)
	}

	active, found, err := repo.GetActiveRubric(ctx, userID, knowledgePointID)
	if err != nil || !found || active.RubricID != rubricV1ID {
		t.Fatalf("GetActiveRubric() after v1 = (%+v, %t, %v), want rubric %s", active, found, err, rubricV1ID)
	}
	if active.Criteria[0].Dimension != service.DefaultRubricCriteria()[0].Dimension {
		t.Fatalf("criteria did not round-trip through JSONB: %+v", active.Criteria)
	}

	rubricV2ID, err := service.NewKnowledgePointRubricID()
	if err != nil {
		t.Fatalf("new rubric id: %v", err)
	}
	v2, err := repo.CreateRubricVersion(ctx, service.CreateRubricVersionParams{
		RubricID:         rubricV2ID,
		KnowledgePointID: knowledgePointID,
		UserID:           userID,
		TemplateVersion:  service.FeynmanRubricTemplateV1,
		Criteria:         service.DefaultRubricCriteria(),
		CreatedBy:        userID,
	})
	if err != nil {
		t.Fatalf("CreateRubricVersion() v2 error = %v", err)
	}
	if v2.VersionNo != 2 {
		t.Fatalf("v2.VersionNo = %d, want 2", v2.VersionNo)
	}

	active, found, err = repo.GetActiveRubric(ctx, userID, knowledgePointID)
	if err != nil || !found || active.RubricID != rubricV2ID {
		t.Fatalf("GetActiveRubric() after v2 = (%+v, %t, %v), want rubric %s", active, found, err, rubricV2ID)
	}
}

func TestPostgresFeynmanRepositoryConcurrentRubricInitialization(t *testing.T) {
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

	const userID = "usr_feynman_rubric_concurrent_test"
	knowledgePointID := uuid.Must(uuid.NewV7()).String()
	ctx := context.Background()
	cleanupFeynmanRepositoryTest(t, pool, userID)
	defer cleanupFeynmanRepositoryTest(t, pool, userID)
	seedFeynmanKnowledgePoint(t, pool, userID, knowledgePointID)
	repo := store.NewPostgresFeynmanRepository(pool)

	const workers = 8
	results := make(chan service.KnowledgePointRubric, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			rubric, initializeErr := repo.InitializeRubric(ctx, service.CreateRubricVersionParams{
				RubricID:         uuid.Must(uuid.NewV7()).String(),
				KnowledgePointID: knowledgePointID,
				UserID:           userID,
				TemplateVersion:  service.FeynmanRubricTemplateV1,
				Criteria:         service.DefaultRubricCriteria(),
				CreatedBy:        userID,
			})
			if initializeErr != nil {
				errorsFound <- initializeErr
				return
			}
			results <- rubric
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for initializeErr := range errorsFound {
		t.Errorf("InitializeRubric() concurrent error = %v", initializeErr)
	}

	var expectedID string
	for rubric := range results {
		if rubric.VersionNo != 1 {
			t.Errorf("concurrent rubric version = %d, want 1", rubric.VersionNo)
		}
		if expectedID == "" {
			expectedID = rubric.RubricID
		} else if rubric.RubricID != expectedID {
			t.Errorf("concurrent rubric id = %s, want %s", rubric.RubricID, expectedID)
		}
	}
	var versionCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_point_rubrics
		WHERE knowledge_point_id = $1 AND user_id = $2`, knowledgePointID, userID).Scan(&versionCount); err != nil {
		t.Fatalf("count rubric versions: %v", err)
	}
	if versionCount != 1 {
		t.Fatalf("rubric version count = %d, want 1", versionCount)
	}
}

func TestPostgresFeynmanRepositoryEvaluationDecisionRace(t *testing.T) {
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

	const userID = "usr_feynman_evaluation_race_test"
	knowledgePointID := uuid.Must(uuid.NewV7()).String()
	ctx := context.Background()
	cleanupFeynmanRepositoryTest(t, pool, userID)
	defer cleanupFeynmanRepositoryTest(t, pool, userID)
	seedFeynmanKnowledgePoint(t, pool, userID, knowledgePointID)
	repo := store.NewPostgresFeynmanRepository(pool)

	rubricID := uuid.Must(uuid.NewV7()).String()
	if _, err := repo.InitializeRubric(ctx, service.CreateRubricVersionParams{
		RubricID: rubricID, KnowledgePointID: knowledgePointID, UserID: userID,
		TemplateVersion: service.FeynmanRubricTemplateV1, Criteria: service.DefaultRubricCriteria(), CreatedBy: userID,
	}); err != nil {
		t.Fatalf("initialize rubric: %v", err)
	}
	attemptID := uuid.Must(uuid.NewV7()).String()
	if _, err := repo.CreateAttempt(ctx, service.CreateFeynmanAttemptParams{
		AttemptID: attemptID, UserID: userID, KnowledgePointID: knowledgePointID, IdempotencyKey: "decision-race",
	}); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	audioTaskID := uuid.Must(uuid.NewV7()).String()
	if _, claimed, err := repo.ClaimAudioTask(ctx, service.ClaimFeynmanAudioTaskParams{
		AudioTaskID: audioTaskID, AttemptID: attemptID, UserID: userID, MIMEType: "audio/webm",
		SizeBytes: 5, SHA256: []byte("0123456789abcdef0123456789abcdef"), AudioData: []byte("audio"),
	}); err != nil || !claimed {
		t.Fatalf("claim audio = (claimed=%t, err=%v)", claimed, err)
	}
	if _, err := repo.CompleteAudioTask(ctx, service.CompleteFeynmanAudioTaskParams{
		AudioTaskID: audioTaskID, AttemptID: attemptID, UserID: userID,
		Status: service.FeynmanAudioStatusTranscribed, RawTranscript: "confirmed output",
	}); err != nil {
		t.Fatalf("complete audio: %v", err)
	}
	confirmationID := uuid.Must(uuid.NewV7()).String()
	if _, err := repo.ConfirmTranscript(ctx, service.ConfirmFeynmanTranscriptParams{
		ConfirmationID: confirmationID, AttemptID: attemptID, UserID: userID,
		ConfirmedTranscript: "confirmed output", ConfirmedBy: userID,
	}); err != nil {
		t.Fatalf("confirm transcript: %v", err)
	}

	evaluationID := uuid.Must(uuid.NewV7()).String()
	if _, claimed, err := repo.ClaimEvaluation(ctx, service.ClaimFeynmanEvaluationParams{
		EvaluationID: evaluationID, AttemptID: attemptID, ConfirmationID: confirmationID,
		RubricID: rubricID, KnowledgePointID: knowledgePointID, UserID: userID,
		PromptVersion: "test-v1", ModelName: "test-model",
		ConfirmedTranscriptHash: []byte("0123456789abcdef0123456789abcdef"),
	}); err != nil || !claimed {
		t.Fatalf("claim evaluation = (claimed=%t, err=%v)", claimed, err)
	}
	payload := &service.FeynmanEvaluationPayload{
		Summary: "test", InsufficientSources: true,
		EvidenceCandidate: service.FeynmanEvidenceCandidate{Claim: "claim", Rationale: "reason", EvidenceScope: service.FeynmanEvidenceScopeLearning},
	}
	if _, err := repo.CompleteEvaluation(ctx, service.CompleteFeynmanEvaluationParams{
		EvaluationID: evaluationID, UserID: userID, Status: service.FeynmanEvaluationStatusProposed,
		Payload: payload, Sources: []service.FeynmanSourceSnapshot{},
	}); err != nil {
		t.Fatalf("complete evaluation: %v", err)
	}

	decisionErrors := make(chan error, 2)
	for range 2 {
		go func() {
			_, decisionErr := repo.DecideEvaluation(ctx, service.DecideFeynmanEvaluationParams{
				DecisionID: uuid.Must(uuid.NewV7()).String(), EvaluationID: evaluationID, UserID: userID,
				Decision: service.FeynmanEvaluationDecisionConfirmed, FinalPayload: payload,
			})
			decisionErrors <- decisionErr
		}()
	}
	var succeeded, alreadyDecided int
	for range 2 {
		decisionErr := <-decisionErrors
		switch {
		case decisionErr == nil:
			succeeded++
		case errors.Is(decisionErr, service.ErrFeynmanEvaluationDecided):
			alreadyDecided++
		default:
			t.Errorf("DecideEvaluation() race error = %v", decisionErr)
		}
	}
	if succeeded != 1 || alreadyDecided != 1 {
		t.Fatalf("decision race results = success %d, already decided %d; want 1 and 1", succeeded, alreadyDecided)
	}

	if _, err := repo.DecideEvaluation(ctx, service.DecideFeynmanEvaluationParams{
		DecisionID: uuid.Must(uuid.NewV7()).String(), EvaluationID: uuid.Must(uuid.NewV7()).String(),
		UserID: userID, Decision: service.FeynmanEvaluationDecisionRejected,
	}); !errors.Is(err, service.ErrFeynmanEvaluationNotReady) {
		t.Fatalf("zero-row decision error = %v, want ErrFeynmanEvaluationNotReady", err)
	}
}

func seedFeynmanKnowledgePoint(t *testing.T, pool *pgxpool.Pool, userID, knowledgePointID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0)`, userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_points (knowledge_point_id, user_id, title, created_by, updated_by)
		VALUES ($1, $2, 'feynman repository test topic', $2, $2)`,
		knowledgePointID, userID); err != nil {
		t.Fatalf("seed knowledge point: %v", err)
	}
}

// cleanupFeynmanRepositoryTest 按照子表 -> 主表的顺序在一个事务内删除。
// 这些表全部是 Append-only（触发器同样禁止 DELETE），测试专用清理必须用
// `SET LOCAL session_replication_role = replica` 临时关闭触发器；该设置
// 随事务结束自动还原，不会影响连接池里其它连接。
func cleanupFeynmanRepositoryTest(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("cleanup begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("cleanup disable triggers: %v", err)
	}

	statements := []string{
		`DELETE FROM feynman_evaluation_decisions WHERE user_id = $1`,
		`DELETE FROM feynman_evaluations WHERE user_id = $1`,
		`DELETE FROM feynman_transcript_confirmations WHERE user_id = $1`,
		`DELETE FROM feynman_audio_task_events WHERE user_id = $1`,
		`DELETE FROM feynman_audio_tasks WHERE user_id = $1`,
		`DELETE FROM feynman_attempts WHERE user_id = $1`,
		`DELETE FROM knowledge_point_rubrics WHERE user_id = $1`,
		`DELETE FROM knowledge_points WHERE user_id = $1`,
		`DELETE FROM agent_user WHERE user_id = $1`,
	}
	for _, sql := range statements {
		if _, err := tx.Exec(ctx, sql, userID); err != nil {
			t.Fatalf("cleanup exec %q: %v", sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("cleanup commit tx: %v", err)
	}
}
