package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"healthAgent/internal/service"
)

// PostgresVoiceCaptureRepository 持久化通用语音输入的录音与转写结果。
//
// 不可协商约束：每条 SQL 都带 user_id，跨用户读写不可能命中；
// 原始转写与音频字节由数据库触发器守卫，应用层写错也改不掉历史。
type PostgresVoiceCaptureRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresVoiceCaptureRepository 构造语音录音仓储。
func NewPostgresVoiceCaptureRepository(pool *pgxpool.Pool) *PostgresVoiceCaptureRepository {
	return &PostgresVoiceCaptureRepository{pool: pool}
}

const voiceCaptureColumnsSQL = `
	SELECT capture_id::text, user_id, session_id, status, mime_type, size_bytes, duration_ms,
	       COALESCE(stt_provider, ''), COALESCE(stt_model, ''), COALESCE(stt_request_id, ''),
	       COALESCE(raw_transcript, ''), confidence::float8, ambiguous_terms,
	       needs_confirmation, confirmation_reason, COALESCE(transcript_error, ''),
	       COALESCE(message_id::text, ''), created_at, updated_at
	FROM voice_captures`

type voiceCaptureRow interface {
	Scan(dest ...any) error
}

func scanVoiceCapture(row voiceCaptureRow) (service.VoiceCapture, error) {
	var capture service.VoiceCapture
	var ambiguousJSON []byte
	if err := row.Scan(
		&capture.CaptureID, &capture.UserID, &capture.SessionID, &capture.Status,
		&capture.MIMEType, &capture.SizeBytes, &capture.DurationMs,
		&capture.STTProvider, &capture.STTModel, &capture.STTRequestID,
		&capture.RawTranscript, &capture.Confidence, &ambiguousJSON,
		&capture.NeedsConfirmation, &capture.ConfirmationReason, &capture.TranscriptError,
		&capture.MessageID, &capture.CreatedAt, &capture.UpdatedAt,
	); err != nil {
		return service.VoiceCapture{}, err
	}
	if len(ambiguousJSON) > 0 {
		if err := json.Unmarshal(ambiguousJSON, &capture.AmbiguousTerms); err != nil {
			return service.VoiceCapture{}, fmt.Errorf("解析语音歧义术语失败: %w", err)
		}
	}
	return capture, nil
}

func marshalAmbiguousTerms(terms []service.AmbiguousTerm) ([]byte, error) {
	if len(terms) == 0 {
		return []byte("[]"), nil
	}
	encoded, err := json.Marshal(terms)
	if err != nil {
		return nil, fmt.Errorf("序列化语音歧义术语失败: %w", err)
	}
	return encoded, nil
}

// Claim 抢占一次转写任务。
//
// 三条分支：
//  1. 同一会话内相同字节已存在且仍有效 -> won=false，调用方直接复用，不再调用 STT；
//  2. 上一次调用崩在 transcribing（超过 StaleBefore）-> 就地接管重试；
//  3. 全新录音 -> 插入并直接置为 transcribing。
//
// 去重键是 (user_id, session_id, sha256)：录音字节带噪声，两次真实录音不可能逐字节相同，
// 能命中的只有「同一次上传被重放」，正是要挡掉的重复计费。
func (r *PostgresVoiceCaptureRepository) Claim(ctx context.Context, params service.ClaimVoiceCaptureParams) (service.VoiceCapture, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.VoiceCapture{}, false, fmt.Errorf("开启语音录音事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owned bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_memory_session
			WHERE session_id = $1 AND user_id = $2 AND status = 'active' AND deleted_at IS NULL
		)`, params.SessionID, params.UserID).Scan(&owned); err != nil {
		return service.VoiceCapture{}, false, fmt.Errorf("校验会话归属失败: %w", err)
	}
	if !owned {
		return service.VoiceCapture{}, false, service.ErrSessionNotFound
	}

	var existingID, existingStatus string
	var existingStale bool
	err = tx.QueryRow(ctx, `
		SELECT capture_id::text, status, (status = 'transcribing' AND updated_at < $4)
		FROM voice_captures
		WHERE user_id = $1 AND session_id = $2 AND sha256 = $3
		FOR UPDATE`,
		params.UserID, params.SessionID, params.SHA256, params.StaleBefore).
		Scan(&existingID, &existingStatus, &existingStale)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return service.VoiceCapture{}, false, fmt.Errorf("查询语音去重记录失败: %w", err)
	}

	captureID := params.CaptureID
	won := true
	switch {
	case err == nil && !existingStale:
		// 已有记录且没卡住：直接复用（含已失败的记录 —— 重放同一段失败录音仍然应该失败，
		// 用户重新录一次自然是另一段字节，不会命中这里）。
		captureID = existingID
		won = false
	case err == nil:
		captureID = existingID
		if _, err := tx.Exec(ctx, `
			UPDATE voice_captures
			SET status = 'transcribing', stt_provider = NULLIF($1, ''),
			    stt_model = NULL, stt_request_id = NULL, raw_transcript = NULL,
			    transcript_error = NULL, confidence = NULL, ambiguous_terms = '[]'::jsonb,
			    needs_confirmation = TRUE, confirmation_reason = ''
			WHERE capture_id = $2::uuid AND user_id = $3`,
			params.STTProvider, captureID, params.UserID); err != nil {
			return service.VoiceCapture{}, false, fmt.Errorf("接管语音转写任务失败: %w", err)
		}
	default:
		if _, err := tx.Exec(ctx, `
			INSERT INTO voice_captures (
				capture_id, user_id, session_id, status, mime_type, size_bytes,
				duration_ms, sha256, audio_data, stt_provider, created_by
			) VALUES ($1::uuid, $2, $3, 'transcribing', $4, $5, $6, $7, $8, NULLIF($9, ''), $2)`,
			captureID, params.UserID, params.SessionID, params.MIMEType, params.SizeBytes,
			params.DurationMs, params.SHA256, params.AudioData, params.STTProvider); err != nil {
			return service.VoiceCapture{}, false, fmt.Errorf("写入语音录音失败: %w", err)
		}
	}

	capture, err := scanVoiceCapture(tx.QueryRow(ctx,
		voiceCaptureColumnsSQL+` WHERE capture_id = $1::uuid AND user_id = $2`, captureID, params.UserID))
	if err != nil {
		return service.VoiceCapture{}, false, fmt.Errorf("读取语音录音失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return service.VoiceCapture{}, false, fmt.Errorf("提交语音录音事务失败: %w", err)
	}
	return capture, won, nil
}

// Complete 写入一次转写的终态结果，仅当记录仍停在 transcribing 时生效。
//
// 影响行数为 0 说明别的请求已经把它写成终态了：这时读回已有结果返回，
// 而不是报错 —— 用户要的是那段文字，谁写进去的不重要。
func (r *PostgresVoiceCaptureRepository) Complete(ctx context.Context, params service.CompleteVoiceCaptureParams) (service.VoiceCapture, error) {
	ambiguousJSON, err := marshalAmbiguousTerms(params.AmbiguousTerms)
	if err != nil {
		return service.VoiceCapture{}, err
	}

	_, err = r.pool.Exec(ctx, `
		UPDATE voice_captures
		SET status = $1,
		    stt_provider = NULLIF($2, ''), stt_model = NULLIF($3, ''), stt_request_id = NULLIF($4, ''),
		    raw_transcript = NULLIF($5, ''), transcript_error = NULLIF($6, ''),
		    confidence = $7, ambiguous_terms = $8::jsonb,
		    needs_confirmation = $9, confirmation_reason = $10
		WHERE capture_id = $11::uuid AND user_id = $12 AND status = 'transcribing'`,
		params.Status, params.STTProvider, params.STTModel, params.STTRequestID,
		params.RawTranscript, params.TranscriptError,
		params.Confidence, ambiguousJSON,
		params.NeedsConfirmation, params.ConfirmationReason,
		params.CaptureID, params.UserID)
	if err != nil {
		return service.VoiceCapture{}, fmt.Errorf("完成语音转写失败: %w", err)
	}

	capture, err := scanVoiceCapture(r.pool.QueryRow(ctx,
		voiceCaptureColumnsSQL+` WHERE capture_id = $1::uuid AND user_id = $2`, params.CaptureID, params.UserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return service.VoiceCapture{}, service.ErrVoiceCaptureNotFound
	}
	if err != nil {
		return service.VoiceCapture{}, fmt.Errorf("读取语音录音失败: %w", err)
	}
	return capture, nil
}

// Get 按 (user_id, session_id, capture_id) 读取一条录音记录。
func (r *PostgresVoiceCaptureRepository) Get(ctx context.Context, userID, sessionID, captureID string) (service.VoiceCapture, error) {
	capture, err := scanVoiceCapture(r.pool.QueryRow(ctx,
		voiceCaptureColumnsSQL+` WHERE capture_id = $1::uuid AND user_id = $2 AND session_id = $3`,
		captureID, userID, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return service.VoiceCapture{}, service.ErrVoiceCaptureNotFound
	}
	if err != nil {
		return service.VoiceCapture{}, fmt.Errorf("读取语音录音失败: %w", err)
	}
	return capture, nil
}

// BindMessage 把转写记录绑定到它最终发出的那条消息上。
//
// WHERE 里带 (message_id IS NULL OR message_id = $4) 让重复绑定同一条消息保持幂等；
// 绑到另一条消息则命中 0 行 —— 一段转写只能对应一次发送，历史不允许被改写。
func (r *PostgresVoiceCaptureRepository) BindMessage(ctx context.Context, userID, sessionID, captureID, messageID string) error {
	command, err := r.pool.Exec(ctx, `
		UPDATE voice_captures
		SET message_id = $4::uuid
		WHERE capture_id = $1::uuid
		  AND user_id = $2
		  AND session_id = $3
		  AND status = 'transcribed'
		  AND (message_id IS NULL OR message_id = $4::uuid)`,
		captureID, userID, sessionID, messageID)
	if err != nil {
		return fmt.Errorf("绑定语音消息失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ErrVoiceCaptureNotFound
	}
	return nil
}
