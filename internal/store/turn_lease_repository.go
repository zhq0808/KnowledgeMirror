package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"healthAgent/internal/service"
)

// PostgresTurnLeaseRepository 使用共享连接池管理会话 turn 占用租约。
//
// Acquire/Release 都只用一个短事务：判断 + 写入在同一次 Begin/Commit 内完成，
// 调用方在事务外拿到结果后再去调 LLM、写 SSE，绝不把事务/行锁带出这两个方法。
type PostgresTurnLeaseRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresTurnLeaseRepository(pool *pgxpool.Pool) *PostgresTurnLeaseRepository {
	return &PostgresTurnLeaseRepository{pool: pool}
}

// Acquire 获取（或续期/复用）一个 Session 的 active turn 租约。
//
// 判断顺序：
//  1. 先看这个 client_message_id 是否已有记录——有就是"同一个 turn 在重连/续期"或"历史终态"，
//     不会走到下面的抢占逻辑，也就不会跟自己的旧记录撞唯一约束。
//  2. 没有自己的记录，才去看这个 Session 当前是否被别的 client_message_id 占着：
//     未过期 -> 冲突；已过期 -> 判定占用者已失联，标记 failed 腾出位置。
//  3. 插入新的 active 租约。
func (r *PostgresTurnLeaseRepository) Acquire(ctx context.Context, request service.AcquireTurnLeaseRequest) (service.AcquireTurnLeaseResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("开启获取租约事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now()
	messageResult, err := appendUserMessageTx(ctx, tx, service.AppendUserMessageRequest{
		UserID:          request.UserID,
		SessionID:       request.SessionID,
		ClientMessageID: request.ClientMessageID,
		Content:         request.Content,
	})
	if errors.Is(err, service.ErrSessionNotFound) {
		return service.AcquireTurnLeaseResult{}, service.ErrSessionNotFound
	}
	if err != nil {
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("写入 turn 用户消息失败: %w", err)
	}
	if !messageResult.Created && messageResult.Message.Content != request.Content {
		return service.AcquireTurnLeaseResult{}, service.ErrClientMessageConflict
	}

	own, err := scanTurnLease(tx.QueryRow(ctx, `
		SELECT id, session_id, user_id, client_message_id, status, attempt_no,
		       COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
		       lease_expires_at, created_at, updated_at
		FROM agent_turn_lease
		WHERE session_id = $1 AND client_message_id = $2
		FOR UPDATE`, request.SessionID, request.ClientMessageID))
	switch {
	case err == nil:
		if messageResult.Created {
			return service.AcquireTurnLeaseResult{}, errors.New("已有 turn 缺少对应的用户消息")
		}
		return r.acquireOwnExisting(ctx, tx, request, own, messageResult.Message, now)
	case !errors.Is(err, pgx.ErrNoRows):
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("查询自身租约记录失败: %w", err)
	}

	other, err := scanTurnLease(tx.QueryRow(ctx, `
		SELECT id, session_id, user_id, client_message_id, status, attempt_no,
		       COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
		       lease_expires_at, created_at, updated_at
		FROM agent_turn_lease
		WHERE session_id = $1 AND status = 'active'
		FOR UPDATE`, request.SessionID))
	switch {
	case err == nil:
		if other.LeaseExpiresAt.After(now) {
			return service.AcquireTurnLeaseResult{}, service.ErrTurnLeaseConflict
		}
		// 占用者的租约已过期（大概率进程崩溃/网络异常未释放），判定失败并腾出位置。
		if _, err := tx.Exec(ctx, `
			UPDATE agent_turn_lease SET status = 'failed'
			WHERE id = $1 AND status = 'active'`, other.ID); err != nil {
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("回收过期租约失败: %w", err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("查询会话当前租约失败: %w", err)
	}

	inserted, err := scanTurnLease(tx.QueryRow(ctx, `
		INSERT INTO agent_turn_lease (
			session_id, user_id, client_message_id, status, attempt_no, user_message_id, lease_expires_at
		)
		VALUES ($1, $2, $3, 'active', 1, $4, $5)
		RETURNING id, session_id, user_id, client_message_id, status, attempt_no,
		          COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
		          lease_expires_at, created_at, updated_at`,
		request.SessionID, request.UserID, request.ClientMessageID, messageResult.Message.MessageID, now.Add(request.LeaseDuration)))
	if err != nil {
		if isForeignKeyViolation(err) {
			return service.AcquireTurnLeaseResult{}, service.ErrSessionNotFound
		}
		if isUniqueViolation(err) {
			// 极小概率的竞态：两个请求几乎同时判定"没有别人占着"，只有一个能插入成功。
			return service.AcquireTurnLeaseResult{}, service.ErrTurnLeaseConflict
		}
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("写入 turn 租约失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.AcquireTurnLeaseResult{}, fmt.Errorf("提交获取租约事务失败: %w", err)
	}
	return service.AcquireTurnLeaseResult{Lease: inserted, UserMessage: messageResult.Message, Acquired: true}, nil
}

// acquireOwnExisting 处理"这个 client_message_id 已有记录"的三种情况：续期、失败后重试、已完成回放。
func (r *PostgresTurnLeaseRepository) acquireOwnExisting(
	ctx context.Context,
	tx pgx.Tx,
	request service.AcquireTurnLeaseRequest,
	existing service.TurnLease,
	userMessage service.UserMessage,
	now time.Time,
) (service.AcquireTurnLeaseResult, error) {
	switch existing.Status {
	case service.TurnLeaseActive:
		// 未过期的 active 已有执行者。重复 HTTP 请求只能得知“处理中”，不能替执行者续租，
		// 更不能再次调用 LLM；过期后才允许同一业务 turn 恢复执行。
		if existing.LeaseExpiresAt.After(now) {
			return service.AcquireTurnLeaseResult{}, service.ErrTurnInProgress
		}
		renewed, err := scanTurnLease(tx.QueryRow(ctx, `
			UPDATE agent_turn_lease
			SET lease_expires_at = $1, attempt_no = attempt_no + 1, user_message_id = $2
			WHERE id = $3
			RETURNING id, session_id, user_id, client_message_id, status, attempt_no,
			          COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
			          lease_expires_at, created_at, updated_at`,
			now.Add(request.LeaseDuration), userMessage.MessageID, existing.ID))
		if err != nil {
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("续期 turn 租约失败: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("提交续期租约事务失败: %w", err)
		}
		return service.AcquireTurnLeaseResult{Lease: renewed, UserMessage: userMessage, Acquired: true}, nil

	case service.TurnLeaseFailed:
		// failed：上一次处理确实失败了（LLM 报错/超时/落库失败等），这是一次合法的重试。
		// 必须原地把这一行从 failed 改回 active，不能插入新行——(session_id, client_message_id)
		// 是全局唯一约束，同一条消息永远只有一条租约记录。
		// 如果这时候 Session 已经被别的 client_message_id 占着（正常流程不该发生，这里只是防御性兜底），
		// 会撞上 session 级的部分唯一索引，统一识别成冲突，交给调用方按 409 处理。
		reopened, err := scanTurnLease(tx.QueryRow(ctx, `
			UPDATE agent_turn_lease
			SET status = 'active', lease_expires_at = $1, attempt_no = attempt_no + 1, user_message_id = $2
			WHERE id = $3
			RETURNING id, session_id, user_id, client_message_id, status, attempt_no,
			          COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
			          lease_expires_at, created_at, updated_at`,
			now.Add(request.LeaseDuration), userMessage.MessageID, existing.ID))
		if err != nil {
			if isUniqueViolation(err) {
				return service.AcquireTurnLeaseResult{}, service.ErrTurnLeaseConflict
			}
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("重新打开失败租约失败: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("提交重新打开租约事务失败: %w", err)
		}
		return service.AcquireTurnLeaseResult{Lease: reopened, UserMessage: userMessage, Acquired: true}, nil

	default:
		// completed：上一次已经跑出正确结果，绝不重新调用 LLM；交给调用方按 client_message_id
		// 查出已落库的 assistant 回复原样回放给客户端。
		if err := tx.Commit(ctx); err != nil {
			return service.AcquireTurnLeaseResult{}, fmt.Errorf("提交终态租约查询失败: %w", err)
		}
		return service.AcquireTurnLeaseResult{Lease: existing, UserMessage: userMessage, Acquired: false}, nil
	}
}

// Complete 在同一个短事务中写入 assistant 消息并把当前 attempt 推进到 completed。
// attempt_no 是 fencing token：已被过期恢复取代的旧执行者无法再提交结果。
func (r *PostgresTurnLeaseRepository) Complete(ctx context.Context, request service.CompleteTurnRequest) (service.AssistantMessage, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("开启完成 turn 事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 与 Acquire/appendUserMessageTx 保持一致的锁顺序：先 Session，后 turn。
	// 否则 Complete(turn -> session) 与 Acquire(session -> turn) 并发时会形成死锁环。
	var messageCount int
	if err := tx.QueryRow(ctx, `
		SELECT message_count
		FROM agent_memory_session
		WHERE session_id = $1 AND user_id = $2 AND status = 'active' AND deleted_at IS NULL
		FOR UPDATE`, request.SessionID, request.UserID).Scan(&messageCount); errors.Is(err, pgx.ErrNoRows) {
		return service.AssistantMessage{}, service.ErrSessionNotFound
	} else if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("锁定待完成会话失败: %w", err)
	}

	lease, err := scanTurnLease(tx.QueryRow(ctx, `
		SELECT id, session_id, user_id, client_message_id, status, attempt_no,
		       COALESCE(user_message_id::text, ''), COALESCE(result_message_id::text, ''),
		       lease_expires_at, created_at, updated_at
		FROM agent_turn_lease
		WHERE session_id = $1 AND user_id = $2 AND client_message_id = $3
		FOR UPDATE`, request.SessionID, request.UserID, request.ClientMessageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return service.AssistantMessage{}, service.ErrTurnLeaseLost
	}
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("锁定待完成 turn 失败: %w", err)
	}
	if lease.Status != service.TurnLeaseActive || lease.AttemptNo != request.AttemptNo ||
		lease.UserMessageID != request.UserMessageID {
		return service.AssistantMessage{}, service.ErrTurnLeaseLost
	}

	message, err := appendAssistantMessageTx(ctx, tx, service.AppendAssistantMessageRequest{
		UserID:             request.UserID,
		SessionID:          request.SessionID,
		ParentMessageID:    request.UserMessageID,
		Content:            request.Content,
		PromptVersion:      request.PromptVersion,
		ModelName:          request.ModelName,
		RetrievalRequestID: request.RetrievalRequestID,
	})
	if err != nil {
		return service.AssistantMessage{}, err
	}

	command, err := tx.Exec(ctx, `
		UPDATE agent_turn_lease
		SET status = 'completed', result_message_id = $1
		WHERE id = $2 AND status = 'active' AND attempt_no = $3`,
		message.MessageID, lease.ID, request.AttemptNo)
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("完成 turn 状态失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.AssistantMessage{}, service.ErrTurnLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return service.AssistantMessage{}, fmt.Errorf("提交完成 turn 事务失败: %w", err)
	}
	return message, nil
}

// Release 把当前 attempt 标记为 failed。成功路径必须走 Complete，以保证结果和 completed 原子提交。
func (r *PostgresTurnLeaseRepository) Release(ctx context.Context, request service.ReleaseTurnLeaseRequest) error {
	command, err := r.pool.Exec(ctx, `
		UPDATE agent_turn_lease
		SET status = $4
		WHERE session_id = $1 AND user_id = $2 AND client_message_id = $3
		  AND status = 'active' AND attempt_no = $5`,
		request.SessionID, request.UserID, request.ClientMessageID, string(request.Status), request.AttemptNo)
	if err != nil {
		return fmt.Errorf("释放 turn 租约失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		// 不当成硬错误：可能租约已被过期回收逻辑标记为 failed，或本来就不存在，释放本身应当是幂等的。
		return nil
	}
	return nil
}

func isForeignKeyViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23503"
}

func scanTurnLease(row pgx.Row) (service.TurnLease, error) {
	var lease service.TurnLease
	var status string
	err := row.Scan(
		&lease.ID,
		&lease.SessionID,
		&lease.UserID,
		&lease.ClientMessageID,
		&status,
		&lease.AttemptNo,
		&lease.UserMessageID,
		&lease.ResultMessageID,
		&lease.LeaseExpiresAt,
		&lease.CreatedAt,
		&lease.UpdatedAt,
	)
	lease.Status = service.TurnLeaseStatus(status)
	return lease, err
}
