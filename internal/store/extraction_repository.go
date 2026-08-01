package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"KnowledgeMirror/internal/service"
)

// LookupSessionUser 按 session_id 返回归属用户；会话不存在或已删除时 found=false。
func (r *PostgresMemoryRepository) LookupSessionUser(ctx context.Context, sessionID string) (string, bool, error) {
	var userID string
	err := r.pool.QueryRow(ctx, `
		SELECT user_id
		FROM agent_memory_session
		WHERE session_id = $1 AND deleted_at IS NULL`, sessionID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("查询抽取会话用户失败: %w", err)
	}
	return userID, true, nil
}

// AcquireExtractionLease 在短事务内抢占该 Session 的抽取执行权。
//
// 判定顺序：确保 state 行存在 -> 锁定该行 -> 未过期租约或未到重试时间则放弃 ->
// 计算最新 completed 结果 seq，无积压则放弃 -> 盖上新 lease_token/lease_until 并返回工作窗口。
func (r *PostgresMemoryRepository) AcquireExtractionLease(ctx context.Context, sessionID, userID string, leaseDuration time.Duration) (service.ExtractionLease, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.ExtractionLease{}, false, fmt.Errorf("开启抢占抽取租约事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 兼容历史 Session 缺少 state：按游标 0 创建。会话不存在时外键报错，交由上层丢弃。
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_memory_extraction_state (session_id, user_id, last_extracted_result_seq)
		VALUES ($1, $2, 0)
		ON CONFLICT (session_id) DO NOTHING`, sessionID, userID); err != nil {
		if isForeignKeyViolation(err) {
			return service.ExtractionLease{}, false, nil
		}
		return service.ExtractionLease{}, false, fmt.Errorf("初始化抽取状态失败: %w", err)
	}

	var stateUserID string
	var lastExtracted int64
	var leaseUntil, nextRetryAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT user_id, last_extracted_result_seq, lease_until, next_retry_at
		FROM agent_memory_extraction_state
		WHERE session_id = $1
		FOR UPDATE`, sessionID).Scan(&stateUserID, &lastExtracted, &leaseUntil, &nextRetryAt); err != nil {
		return service.ExtractionLease{}, false, fmt.Errorf("锁定抽取状态失败: %w", err)
	}
	if stateUserID != userID {
		// 归属不一致（理论上不会发生，session_id 唯一映射一个用户），保守放弃。
		return service.ExtractionLease{}, false, nil
	}

	now := time.Now()
	if leaseUntil != nil && leaseUntil.After(now) {
		return service.ExtractionLease{}, false, nil // 已有未过期租约
	}
	if nextRetryAt != nil && nextRetryAt.After(now) {
		return service.ExtractionLease{}, false, nil // 尚未到重试时间
	}

	var latestCompleted int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(result_msg.seq), 0)
		FROM agent_turn_lease AS t
		JOIN agent_memory_episodic AS result_msg ON result_msg.message_id = t.result_message_id
		WHERE t.session_id = $1 AND t.user_id = $2 AND t.status = 'completed'`,
		sessionID, userID).Scan(&latestCompleted); err != nil {
		return service.ExtractionLease{}, false, fmt.Errorf("计算最新完成结果 seq 失败: %w", err)
	}
	if latestCompleted <= lastExtracted {
		return service.ExtractionLease{}, false, nil // 无积压
	}

	leaseToken, err := service.NewLeaseToken()
	if err != nil {
		return service.ExtractionLease{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET lease_token = $2, lease_until = $3
		WHERE session_id = $1`, sessionID, leaseToken, now.Add(leaseDuration)); err != nil {
		return service.ExtractionLease{}, false, fmt.Errorf("盖抽取租约失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.ExtractionLease{}, false, fmt.Errorf("提交抢占抽取租约事务失败: %w", err)
	}
	return service.ExtractionLease{
		SessionID:     sessionID,
		UserID:        userID,
		LeaseToken:    leaseToken,
		FromResultSeq: lastExtracted,
		ToResultSeq:   latestCompleted,
	}, true, nil
}

// LoadExtractionBatch 读取 result seq 落在 (fromSeq, toSeq] 的 completed turns 的一问一答，按结果 seq 升序。
func (r *PostgresMemoryRepository) LoadExtractionBatch(ctx context.Context, sessionID, userID string, fromSeq, toSeq int64) ([]service.ExtractionTurn, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT user_msg.message_id::text, COALESCE(user_msg.content, ''), user_msg.seq,
		       result_msg.message_id::text, COALESCE(result_msg.content, ''), result_msg.seq
		FROM agent_turn_lease AS t
		JOIN agent_memory_episodic AS user_msg   ON user_msg.message_id   = t.user_message_id
		JOIN agent_memory_episodic AS result_msg ON result_msg.message_id = t.result_message_id
		WHERE t.session_id = $1 AND t.user_id = $2 AND t.status = 'completed'
		  AND result_msg.seq > $3 AND result_msg.seq <= $4
		  AND user_msg.deleted_at IS NULL AND result_msg.deleted_at IS NULL
		ORDER BY result_msg.seq ASC`, sessionID, userID, fromSeq, toSeq)
	if err != nil {
		return nil, fmt.Errorf("读取抽取批次失败: %w", err)
	}
	defer rows.Close()

	turns := make([]service.ExtractionTurn, 0)
	for rows.Next() {
		var turn service.ExtractionTurn
		turn.UserMessage.Role = "user"
		turn.AssistantMessage.Role = "assistant"
		if err := rows.Scan(
			&turn.UserMessage.MessageID,
			&turn.UserMessage.Content,
			&turn.UserMessage.Seq,
			&turn.AssistantMessage.MessageID,
			&turn.AssistantMessage.Content,
			&turn.AssistantMessage.Seq,
		); err != nil {
			return nil, fmt.Errorf("扫描抽取批次失败: %w", err)
		}
		turn.ResultSeq = turn.AssistantMessage.Seq
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历抽取批次失败: %w", err)
	}
	return turns, nil
}

// RecordExtractionFailure 清空租约、累加失败次数并按指数退避+抖动设置 next_retry_at。
// lease_token 不匹配（已被接管）时不做改动，返回 nil，避免旧执行者的失败覆盖新执行者的进度。
func (r *PostgresMemoryRepository) RecordExtractionFailure(ctx context.Context, sessionID, userID, leaseToken, errorCode string, baseBackoff, maxBackoff time.Duration) error {
	// 退避在 SQL 内基于旧的 consecutive_failures 计算：base * 2^failures，封顶 max，再乘 [0.5,1) 抖动。
	_, err := r.pool.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET lease_token = NULL,
		    lease_until = NULL,
		    consecutive_failures = consecutive_failures + 1,
		    last_error_code = $4,
		    next_retry_at = now() + make_interval(secs =>
		        LEAST($6::float8, $5::float8 * power(2, consecutive_failures)) * (0.5 + random() * 0.5))
		WHERE session_id = $1 AND user_id = $2 AND lease_token = $3`,
		sessionID, userID, leaseToken, errorCode, baseBackoff.Seconds(), maxBackoff.Seconds())
	if err != nil {
		return fmt.Errorf("记录抽取失败退避失败: %w", err)
	}
	return nil
}

// ScanExtractionBacklog 返回有积压且当前可执行的 Session：
// completed 结果 seq 超过已抽取游标（含尚无 state 行的历史 Session，按 0 计），且无未过期租约、已过重试时间。
func (r *PostgresMemoryRepository) ScanExtractionBacklog(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT completed.session_id
		FROM (
			SELECT t.session_id, MAX(result_msg.seq) AS latest_seq
			FROM agent_turn_lease AS t
			JOIN agent_memory_episodic AS result_msg ON result_msg.message_id = t.result_message_id
			WHERE t.status = 'completed'
			GROUP BY t.session_id
		) AS completed
		JOIN agent_memory_session AS s
		  ON s.session_id = completed.session_id AND s.deleted_at IS NULL
		LEFT JOIN agent_memory_extraction_state AS st
		  ON st.session_id = completed.session_id
		WHERE completed.latest_seq > COALESCE(st.last_extracted_result_seq, 0)
		  AND (st.lease_until IS NULL OR st.lease_until <= now())
		  AND (st.next_retry_at IS NULL OR st.next_retry_at <= now())
		ORDER BY completed.session_id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("补扫抽取积压失败: %w", err)
	}
	defer rows.Close()

	sessions := make([]string, 0)
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, fmt.Errorf("扫描抽取积压失败: %w", err)
		}
		sessions = append(sessions, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历抽取积压失败: %w", err)
	}
	return sessions, nil
}
