package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/service"
)

// PostgresFeynmanRepository 持久化语音费曼练习尝试、录音转写与知识点 Rubric。
//
// 与资料治理模块一致的不可协商约束：
//   - 每条 SQL 都带 user_id，跨用户读写不可能命中；
//   - feynman_audio_tasks / feynman_transcript_confirmations / knowledge_point_rubrics
//     全部 Append-only（数据库触发器同样禁止 UPDATE/DELETE）；
//   - feynman_attempts 一旦写入 confirmation_id 即永久只读（数据库触发器兜底）。
//
// 全部业务 ID（attempt_id/audio_task_id/confirmation_id/rubric_id）由 service 层预先生成，
// 本层只负责按给定 ID 写入，不做 UUID 冲突重试。
type PostgresFeynmanRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresFeynmanRepository 构造语音费曼练习仓储。
func NewPostgresFeynmanRepository(pool *pgxpool.Pool) *PostgresFeynmanRepository {
	return &PostgresFeynmanRepository{pool: pool}
}

// ---------------------------------------------------------------------------
// 练习尝试
// ---------------------------------------------------------------------------

const feynmanAttemptDetailSelectSQL = `
	SELECT
		a.attempt_id::text, a.user_id, a.knowledge_point_id::text, a.idempotency_key,
		COALESCE(a.active_audio_task_id::text, ''), COALESCE(a.confirmation_id::text, ''),
		a.created_at, a.updated_at,

		COALESCE(t.audio_task_id::text, ''), COALESCE(t.attempt_id::text, ''), COALESCE(t.user_id, ''),
		COALESCE(t.attempt_no, 0), COALESCE(t.status, ''), COALESCE(t.mime_type, ''),
		COALESCE(t.size_bytes, 0), t.duration_ms, t.sha256,
		COALESCE(t.stt_provider, ''), COALESCE(t.stt_model, ''), COALESCE(t.stt_request_id, ''),
		COALESCE(t.raw_transcript, ''), COALESCE(t.transcript_error, ''), t.created_at, t.updated_at,

		COALESCE(c.confirmation_id::text, ''), COALESCE(c.attempt_id::text, ''), COALESCE(c.audio_task_id::text, ''),
		COALESCE(c.user_id, ''), COALESCE(c.raw_transcript, ''), COALESCE(c.confirmed_transcript, ''),
		c.edited, COALESCE(c.confirmed_by, ''), c.confirmed_at
	FROM feynman_attempts AS a
	LEFT JOIN feynman_audio_tasks AS t
	  ON t.audio_task_id = a.active_audio_task_id
	 AND t.attempt_id = a.attempt_id
	 AND t.user_id = a.user_id
	LEFT JOIN feynman_transcript_confirmations AS c
	  ON c.confirmation_id = a.confirmation_id
	 AND c.attempt_id = a.attempt_id
	 AND c.user_id = a.user_id`

func scanFeynmanAttemptDetail(row rowScanner) (service.FeynmanAttemptDetail, error) {
	var (
		attempt                             service.FeynmanAttempt
		activeAudioTaskID, confirmationID   string
		audioTaskID, audioAttemptID         string
		audioUserID, audioStatus, audioMIME string
		audioAttemptNo                      int
		audioSizeBytes                      int64
		audioDurationMs                     *int
		audioSHA256                         []byte
		sttProvider, sttModel, sttRequestID string
		rawTranscript, transcriptError      string
		audioCreatedAt, audioUpdatedAt      *time.Time

		confConfirmationID, confAttemptID, confAudioTaskID string
		confUserID, confRawTranscript, confConfirmedText   string
		confEdited                                         *bool
		confConfirmedBy                                    string
		confConfirmedAt                                    *time.Time
	)

	err := row.Scan(
		&attempt.AttemptID, &attempt.UserID, &attempt.KnowledgePointID, &attempt.IdempotencyKey,
		&activeAudioTaskID, &confirmationID, &attempt.CreatedAt, &attempt.UpdatedAt,

		&audioTaskID, &audioAttemptID, &audioUserID, &audioAttemptNo, &audioStatus, &audioMIME,
		&audioSizeBytes, &audioDurationMs, &audioSHA256,
		&sttProvider, &sttModel, &sttRequestID, &rawTranscript, &transcriptError, &audioCreatedAt, &audioUpdatedAt,

		&confConfirmationID, &confAttemptID, &confAudioTaskID, &confUserID,
		&confRawTranscript, &confConfirmedText, &confEdited, &confConfirmedBy, &confConfirmedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanAttemptNotFound
	}
	if err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("扫描练习尝试失败: %w", err)
	}

	attempt.ActiveAudioTaskID = activeAudioTaskID
	attempt.ConfirmationID = confirmationID
	detail := service.FeynmanAttemptDetail{Attempt: attempt}

	if audioTaskID != "" {
		task := &service.FeynmanAudioTask{
			AudioTaskID:     audioTaskID,
			AttemptID:       audioAttemptID,
			UserID:          audioUserID,
			AttemptNo:       audioAttemptNo,
			Status:          audioStatus,
			MIMEType:        audioMIME,
			SizeBytes:       audioSizeBytes,
			DurationMs:      audioDurationMs,
			SHA256:          audioSHA256,
			STTProvider:     sttProvider,
			STTModel:        sttModel,
			STTRequestID:    sttRequestID,
			RawTranscript:   rawTranscript,
			TranscriptError: transcriptError,
		}
		if audioCreatedAt != nil {
			task.CreatedAt = *audioCreatedAt
		}
		if audioUpdatedAt != nil {
			task.UpdatedAt = *audioUpdatedAt
		}
		detail.ActiveAudioTask = task
	}

	if confConfirmationID != "" {
		confirmation := &service.FeynmanTranscriptConfirmation{
			ConfirmationID:      confConfirmationID,
			AttemptID:           confAttemptID,
			AudioTaskID:         confAudioTaskID,
			UserID:              confUserID,
			RawTranscript:       confRawTranscript,
			ConfirmedTranscript: confConfirmedText,
			ConfirmedBy:         confConfirmedBy,
		}
		if confEdited != nil {
			confirmation.Edited = *confEdited
		}
		if confConfirmedAt != nil {
			confirmation.ConfirmedAt = *confConfirmedAt
		}
		detail.Confirmation = confirmation
	}

	return detail, nil
}

// GetAttemptDetail 返回练习尝试及其当前音频任务、确认记录（如有）。
func (r *PostgresFeynmanRepository) GetAttemptDetail(ctx context.Context, userID, attemptID string) (service.FeynmanAttemptDetail, error) {
	return scanFeynmanAttemptDetail(r.pool.QueryRow(ctx, feynmanAttemptDetailSelectSQL+`
		WHERE a.attempt_id = $1
		  AND a.user_id = $2`, attemptID, userID))
}

// FindAttemptByIdempotencyKey 查询是否已存在同一幂等键的练习尝试。
func (r *PostgresFeynmanRepository) FindAttemptByIdempotencyKey(ctx context.Context, userID, idempotencyKey string) (service.FeynmanAttemptDetail, bool, error) {
	detail, err := scanFeynmanAttemptDetail(r.pool.QueryRow(ctx, feynmanAttemptDetailSelectSQL+`
		WHERE a.user_id = $1
		  AND a.idempotency_key = $2`, userID, idempotencyKey))
	if errors.Is(err, service.ErrFeynmanAttemptNotFound) {
		return service.FeynmanAttemptDetail{}, false, nil
	}
	if err != nil {
		return service.FeynmanAttemptDetail{}, false, err
	}
	return detail, true, nil
}

// CreateAttempt 创建一条新的练习尝试；attempt_id 由 service 层预先生成。
func (r *PostgresFeynmanRepository) CreateAttempt(ctx context.Context, params service.CreateFeynmanAttemptParams) (service.FeynmanAttemptDetail, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO feynman_attempts (
			attempt_id, user_id, knowledge_point_id, idempotency_key, created_by, updated_by
		)
		VALUES ($1, $2, $3, $4, $2, $2)`,
		params.AttemptID, params.UserID, params.KnowledgePointID, params.IdempotencyKey)
	if err != nil {
		if isForeignKeyViolation(err) {
			return service.FeynmanAttemptDetail{}, service.ErrFeynmanKnowledgePointNotFound
		}
		if isFeynmanIdempotencyConflict(err) {
			return service.FeynmanAttemptDetail{}, service.ErrFeynmanIdempotencyConflict
		}
		if isUniqueViolation(err) {
			return service.FeynmanAttemptDetail{}, fmt.Errorf("attempt_id 冲突: %w", err)
		}
		return service.FeynmanAttemptDetail{}, fmt.Errorf("写入练习尝试失败: %w", err)
	}
	return r.GetAttemptDetail(ctx, params.UserID, params.AttemptID)
}

func isFeynmanIdempotencyConflict(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "uk_feynman_attempts_idempotency"
}

// isRaisedException 判断错误是否来自触发器主动 RAISE EXCEPTION（ERRCODE = '55000'），
// 例如对已确认的练习尝试再次写入 active_audio_task_id / confirmation_id。
func isRaisedException(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "55000"
}

// ClaimAudioTask 在 attempt 行锁内完成去重、失败/超时接管、序号分配和 active 指针更新。
func (r *PostgresFeynmanRepository) ClaimAudioTask(ctx context.Context, params service.ClaimFeynmanAudioTaskParams) (service.FeynmanAttemptDetail, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.FeynmanAttemptDetail{}, false, fmt.Errorf("开启录音任务事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hasConfirmation bool
	err = tx.QueryRow(ctx, `
		SELECT confirmation_id IS NOT NULL
		FROM feynman_attempts
		WHERE attempt_id = $1
		  AND user_id = $2
		FOR UPDATE`, params.AttemptID, params.UserID).Scan(&hasConfirmation)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanAttemptDetail{}, false, service.ErrFeynmanAttemptNotFound
	}
	if err != nil {
		return service.FeynmanAttemptDetail{}, false, fmt.Errorf("锁定练习尝试失败: %w", err)
	}
	if hasConfirmation {
		return service.FeynmanAttemptDetail{}, false, service.ErrFeynmanAttemptConfirmed
	}

	var existingID, existingStatus string
	var existingUpdatedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT audio_task_id::text, status, updated_at
		FROM feynman_audio_tasks
		WHERE attempt_id = $1 AND user_id = $2 AND sha256 = $3`,
		params.AttemptID, params.UserID, params.SHA256).
		Scan(&existingID, &existingStatus, &existingUpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanAttemptDetail{}, false, fmt.Errorf("查询录音去重记录失败: %w", err)
	}

	claimed := false
	audioTaskID := params.AudioTaskID
	if err == nil {
		audioTaskID = existingID
		stale := existingStatus == service.FeynmanAudioStatusTranscribing && existingUpdatedAt.Before(params.StaleBefore)
		if existingStatus == service.FeynmanAudioStatusFailed || stale {
			if stale {
				if _, err := tx.Exec(ctx, `
					UPDATE feynman_audio_tasks
					SET status = 'failed', transcript_error = '转写任务超时，等待重试'
					WHERE audio_task_id = $1 AND attempt_id = $2 AND user_id = $3`,
					audioTaskID, params.AttemptID, params.UserID); err != nil {
					return service.FeynmanAttemptDetail{}, false, fmt.Errorf("标记超时录音任务失败: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE feynman_audio_tasks
				SET status = 'transcribing', stt_provider = NULLIF($1, ''),
				    stt_model = NULL, stt_request_id = NULL, raw_transcript = NULL, transcript_error = NULL
				WHERE audio_task_id = $2 AND attempt_id = $3 AND user_id = $4`,
				params.STTProvider, audioTaskID, params.AttemptID, params.UserID); err != nil {
				return service.FeynmanAttemptDetail{}, false, fmt.Errorf("接管录音任务失败: %w", err)
			}
			claimed = true
		}
	} else {
		var nextAttemptNo int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(attempt_no), 0) + 1
			FROM feynman_audio_tasks
			WHERE attempt_id = $1 AND user_id = $2`, params.AttemptID, params.UserID).Scan(&nextAttemptNo); err != nil {
			return service.FeynmanAttemptDetail{}, false, fmt.Errorf("计算录音序号失败: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO feynman_audio_tasks (
				audio_task_id, attempt_id, user_id, attempt_no, status, mime_type, size_bytes,
				duration_ms, sha256, audio_data, stt_provider, created_by
			) VALUES ($1, $2, $3, $4, 'uploaded', $5, $6, $7, $8, $9, NULLIF($10, ''), $3)`,
			params.AudioTaskID, params.AttemptID, params.UserID, nextAttemptNo,
			params.MIMEType, params.SizeBytes, params.DurationMs, params.SHA256, params.AudioData,
			params.STTProvider); err != nil {
			return service.FeynmanAttemptDetail{}, false, fmt.Errorf("写入录音任务失败: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE feynman_audio_tasks SET status = 'transcribing'
			WHERE audio_task_id = $1 AND attempt_id = $2 AND user_id = $3`,
			params.AudioTaskID, params.AttemptID, params.UserID); err != nil {
			return service.FeynmanAttemptDetail{}, false, fmt.Errorf("开始录音转写失败: %w", err)
		}
		claimed = true
	}

	if claimed {
		if _, err := tx.Exec(ctx, `
		UPDATE feynman_attempts
		SET active_audio_task_id = $1,
		    updated_by = $2
		WHERE attempt_id = $3
		  AND user_id = $2`, audioTaskID, params.UserID, params.AttemptID); err != nil {
			if isRaisedException(err) {
				return service.FeynmanAttemptDetail{}, false, service.ErrFeynmanAttemptConfirmed
			}
			return service.FeynmanAttemptDetail{}, false, fmt.Errorf("更新练习尝试失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return service.FeynmanAttemptDetail{}, false, fmt.Errorf("提交录音任务事务失败: %w", err)
	}
	detail, err := r.GetAttemptDetail(ctx, params.UserID, params.AttemptID)
	return detail, claimed, err
}

// CompleteAudioTask 写入指定任务终态，不触碰 attempt.active_audio_task_id。
func (r *PostgresFeynmanRepository) CompleteAudioTask(ctx context.Context, params service.CompleteFeynmanAudioTaskParams) (service.FeynmanAttemptDetail, error) {
	command, err := r.pool.Exec(ctx, `
		UPDATE feynman_audio_tasks
		SET status = $1,
		    stt_provider = NULLIF($2, ''), stt_model = NULLIF($3, ''), stt_request_id = NULLIF($4, ''),
		    raw_transcript = NULLIF($5, ''), transcript_error = NULLIF($6, '')
		WHERE audio_task_id = $7 AND attempt_id = $8 AND user_id = $9 AND status = 'transcribing'`,
		params.Status, params.STTProvider, params.STTModel, params.STTRequestID,
		params.RawTranscript, params.TranscriptError,
		params.AudioTaskID, params.AttemptID, params.UserID)
	if err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("完成录音任务失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("录音任务状态已变化，无法完成")
	}
	return r.GetAttemptDetail(ctx, params.UserID, params.AttemptID)
}

// ConfirmTranscript 在一个事务内写入确认记录，并把 attempt.confirmation_id 定住。
func (r *PostgresFeynmanRepository) ConfirmTranscript(ctx context.Context, params service.ConfirmFeynmanTranscriptParams) (service.FeynmanAttemptDetail, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("开启转写确认事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		hasConfirmation   bool
		activeAudioTaskID string
	)
	err = tx.QueryRow(ctx, `
		SELECT confirmation_id IS NOT NULL, COALESCE(active_audio_task_id::text, '')
		FROM feynman_attempts
		WHERE attempt_id = $1
		  AND user_id = $2
		FOR UPDATE`, params.AttemptID, params.UserID).Scan(&hasConfirmation, &activeAudioTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanAttemptNotFound
	}
	if err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("锁定练习尝试失败: %w", err)
	}
	if hasConfirmation {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanAttemptConfirmed
	}
	if activeAudioTaskID == "" {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanNoActiveAudio
	}

	var (
		audioStatus   string
		rawTranscript string
	)
	err = tx.QueryRow(ctx, `
		SELECT status, COALESCE(raw_transcript, '')
		FROM feynman_audio_tasks
		WHERE audio_task_id = $1
		  AND attempt_id = $2
		  AND user_id = $3`, activeAudioTaskID, params.AttemptID, params.UserID).Scan(&audioStatus, &rawTranscript)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanNoActiveAudio
	}
	if err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("查询当前录音任务失败: %w", err)
	}
	if audioStatus != service.FeynmanAudioStatusTranscribed {
		return service.FeynmanAttemptDetail{}, service.ErrFeynmanAudioNotReady
	}

	edited := rawTranscript != params.ConfirmedTranscript
	if _, err := tx.Exec(ctx, `
		INSERT INTO feynman_transcript_confirmations (
			confirmation_id, attempt_id, audio_task_id, user_id,
			raw_transcript, confirmed_transcript, edited, confirmed_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		params.ConfirmationID, params.AttemptID, activeAudioTaskID, params.UserID,
		rawTranscript, params.ConfirmedTranscript, edited, params.ConfirmedBy); err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("写入转写确认记录失败: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE feynman_attempts
		SET confirmation_id = $1,
		    updated_by = $2
		WHERE attempt_id = $3
		  AND user_id = $2`, params.ConfirmationID, params.UserID, params.AttemptID); err != nil {
		if isRaisedException(err) {
			return service.FeynmanAttemptDetail{}, service.ErrFeynmanAttemptConfirmed
		}
		return service.FeynmanAttemptDetail{}, fmt.Errorf("更新练习尝试失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.FeynmanAttemptDetail{}, fmt.Errorf("提交转写确认事务失败: %w", err)
	}
	return r.GetAttemptDetail(ctx, params.UserID, params.AttemptID)
}

// ---------------------------------------------------------------------------
// 知识点 Rubric
// ---------------------------------------------------------------------------

func scanKnowledgePointRubric(row rowScanner) (service.KnowledgePointRubric, error) {
	var (
		rubric       service.KnowledgePointRubric
		criteriaJSON []byte
	)
	err := row.Scan(
		&rubric.RubricID, &rubric.KnowledgePointID, &rubric.UserID, &rubric.VersionNo,
		&rubric.TemplateVersion, &criteriaJSON, &rubric.CreatedBy, &rubric.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.KnowledgePointRubric{}, pgx.ErrNoRows
	}
	if err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("扫描知识点 Rubric 失败: %w", err)
	}
	if err := json.Unmarshal(criteriaJSON, &rubric.Criteria); err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("解析知识点 Rubric 维度失败: %w", err)
	}
	return rubric, nil
}

// GetActiveRubric 返回知识点当前生效的 Rubric 版本；不存在返回 found=false。
func (r *PostgresFeynmanRepository) GetActiveRubric(ctx context.Context, userID, knowledgePointID string) (service.KnowledgePointRubric, bool, error) {
	rubric, err := scanKnowledgePointRubric(r.pool.QueryRow(ctx, `
		SELECT r.rubric_id::text, r.knowledge_point_id::text, r.user_id, r.version_no,
		       r.template_version, r.criteria, r.created_by, r.created_at
		FROM knowledge_points AS kp
		JOIN knowledge_point_rubrics AS r
		  ON r.rubric_id = kp.current_rubric_version_id
		 AND r.knowledge_point_id = kp.knowledge_point_id
		 AND r.user_id = kp.user_id
		WHERE kp.knowledge_point_id = $1
		  AND kp.user_id = $2
		  AND kp.deleted_at IS NULL`, knowledgePointID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return service.KnowledgePointRubric{}, false, nil
	}
	if err != nil {
		return service.KnowledgePointRubric{}, false, err
	}
	return rubric, true, nil
}

// InitializeRubric 在知识点行锁内只初始化一次默认版本。
func (r *PostgresFeynmanRepository) InitializeRubric(ctx context.Context, params service.CreateRubricVersionParams) (service.KnowledgePointRubric, error) {
	return r.createRubricVersion(ctx, params, true)
}

// CreateRubricVersion 创建新的 Rubric 版本并把知识点的当前版本指针移过去。
func (r *PostgresFeynmanRepository) CreateRubricVersion(ctx context.Context, params service.CreateRubricVersionParams) (service.KnowledgePointRubric, error) {
	return r.createRubricVersion(ctx, params, false)
}

func (r *PostgresFeynmanRepository) createRubricVersion(ctx context.Context, params service.CreateRubricVersionParams, onlyIfAbsent bool) (service.KnowledgePointRubric, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("开启 Rubric 版本事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked, currentRubricID string
	err = tx.QueryRow(ctx, `
		SELECT knowledge_point_id::text, COALESCE(current_rubric_version_id::text, '')
		FROM knowledge_points
		WHERE knowledge_point_id = $1
		  AND user_id = $2
		  AND deleted_at IS NULL
		FOR UPDATE`, params.KnowledgePointID, params.UserID).Scan(&locked, &currentRubricID)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.KnowledgePointRubric{}, service.ErrFeynmanKnowledgePointNotFound
	}
	if err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("锁定知识点失败: %w", err)
	}
	if onlyIfAbsent && currentRubricID != "" {
		rubric, err := scanKnowledgePointRubric(tx.QueryRow(ctx, `
			SELECT rubric_id::text, knowledge_point_id::text, user_id, version_no,
			       template_version, criteria, created_by, created_at
			FROM knowledge_point_rubrics
			WHERE rubric_id = $1 AND knowledge_point_id = $2 AND user_id = $3`,
			currentRubricID, params.KnowledgePointID, params.UserID))
		if err != nil {
			return service.KnowledgePointRubric{}, fmt.Errorf("查询已初始化 Rubric 失败: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return service.KnowledgePointRubric{}, fmt.Errorf("提交 Rubric 查询事务失败: %w", err)
		}
		return rubric, nil
	}

	var nextVersionNo int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_no), 0) + 1
		FROM knowledge_point_rubrics
		WHERE knowledge_point_id = $1
		  AND user_id = $2`, params.KnowledgePointID, params.UserID).Scan(&nextVersionNo); err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("计算 Rubric 版本号失败: %w", err)
	}

	criteriaJSON, err := json.Marshal(params.Criteria)
	if err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("序列化 Rubric 维度失败: %w", err)
	}

	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO knowledge_point_rubrics (
			rubric_id, knowledge_point_id, user_id, version_no, template_version, criteria, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $3)
		RETURNING created_at`,
		params.RubricID, params.KnowledgePointID, params.UserID, nextVersionNo,
		params.TemplateVersion, criteriaJSON).Scan(&createdAt); err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("写入 Rubric 版本失败: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_points
		SET current_rubric_version_id = $1
		WHERE knowledge_point_id = $2
		  AND user_id = $3`, params.RubricID, params.KnowledgePointID, params.UserID); err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("更新知识点当前 Rubric 指针失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.KnowledgePointRubric{}, fmt.Errorf("提交 Rubric 版本事务失败: %w", err)
	}

	return service.KnowledgePointRubric{
		RubricID:         params.RubricID,
		KnowledgePointID: params.KnowledgePointID,
		UserID:           params.UserID,
		VersionNo:        nextVersionNo,
		TemplateVersion:  params.TemplateVersion,
		Criteria:         params.Criteria,
		CreatedBy:        params.CreatedBy,
		CreatedAt:        createdAt,
	}, nil
}
