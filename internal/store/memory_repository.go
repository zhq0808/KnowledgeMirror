package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/service"
)

// maxMemoryIDAttempts 限制 memory_id / history_id 生成的有限冲突重试次数。
// UUIDv7 碰撞概率极低，这里用保存点包住写入，仅在命中主键唯一冲突时重新生成重试。
const maxMemoryIDAttempts = 5

// PostgresMemoryRepository 使用共享连接池持久化用户长期记忆及其变更历史。
//
// 所有写入都在一个短事务内完成：先在事务里锁定并校验抽取游标/租约，再应用 meta/history/history_source，
// 最后推进游标；LLM 调用发生在事务外，不把长事务/行锁带进本方法。
type PostgresMemoryRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresMemoryRepository(pool *pgxpool.Pool) *PostgresMemoryRepository {
	return &PostgresMemoryRepository{pool: pool}
}

// ListCurrentMemories 按 user_id + agent_id 返回未删除的当前记忆，按更新时间倒序。
// maxCount <= 0 表示不下推数量限制（字符预算由服务层再裁剪）。
func (r *PostgresMemoryRepository) ListCurrentMemories(ctx context.Context, userID, agentID string, maxCount int) ([]service.Memory, error) {
	const baseQuery = `
		SELECT memory_id::text, user_id, agent_id, memory_type, memory_value,
		       confidence::float8, COALESCE(latest_source_message_id::text, ''), version, created_at, updated_at
		FROM agent_memory_meta
		WHERE user_id = $1 AND agent_id = $2 AND deleted_at IS NULL
		ORDER BY updated_at DESC, memory_id DESC`

	var rows pgx.Rows
	var err error
	if maxCount > 0 {
		rows, err = r.pool.Query(ctx, baseQuery+"\n\t\tLIMIT $3", userID, agentID, maxCount)
	} else {
		rows, err = r.pool.Query(ctx, baseQuery, userID, agentID)
	}
	if err != nil {
		return nil, fmt.Errorf("查询当前记忆失败: %w", err)
	}
	defer rows.Close()

	memories := make([]service.Memory, 0)
	for rows.Next() {
		var memory service.Memory
		if err := rows.Scan(
			&memory.MemoryID,
			&memory.UserID,
			&memory.AgentID,
			&memory.MemoryType,
			&memory.MemoryValue,
			&memory.Confidence,
			&memory.LatestSourceMessageID,
			&memory.Version,
			&memory.CreatedAt,
			&memory.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描当前记忆失败: %w", err)
		}
		memories = append(memories, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历当前记忆失败: %w", err)
	}
	return memories, nil
}

// ApplyExtraction 在一个短事务内落库全部操作并推进抽取游标。
//
// 事务顺序：
//  1. 锁定并校验抽取游标/租约：lease_token 与传入一致，且 last_extracted_result_seq 恰为 from_result_seq，
//     否则说明旧执行者晚到或已被接管，整批拒绝（ErrExtractionCursorConflict）。
//  2. 逐条应用 ADD/UPDATE/DELETE，同时追加 history 与全部 history_source；UPDATE/DELETE 用 version 乐观锁。
//  3. 推进游标到 to_result_seq，并清空租约与失败信息。
//
// 任一步失败整体回滚：meta / history / history_source / 游标要么一起成功，要么一起失败。
func (r *PostgresMemoryRepository) ApplyExtraction(ctx context.Context, request service.ApplyExtractionRequest) (service.ApplyExtractionResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.ApplyExtractionResult{}, fmt.Errorf("开启记忆抽取事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. 锁定抽取状态行并校验租约与游标。锁在事务提交前一直持有，串行化同一 Session 的并发提交。
	var currentSeq int64
	var leaseToken *string
	err = tx.QueryRow(ctx, `
		SELECT last_extracted_result_seq, lease_token::text
		FROM agent_memory_extraction_state
		WHERE session_id = $1 AND user_id = $2
		FOR UPDATE`, request.SessionID, request.UserID).Scan(&currentSeq, &leaseToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.ApplyExtractionResult{}, service.ErrExtractionCursorConflict
	}
	if err != nil {
		return service.ApplyExtractionResult{}, fmt.Errorf("锁定抽取游标失败: %w", err)
	}
	if leaseToken == nil || *leaseToken != request.LeaseToken || currentSeq != request.FromResultSeq {
		return service.ApplyExtractionResult{}, service.ErrExtractionCursorConflict
	}

	// 2. 逐条应用操作。
	var result service.ApplyExtractionResult
	for _, operation := range request.Operations {
		switch operation.Action {
		case service.MemoryActionAdd:
			if err := applyMemoryAdd(ctx, tx, request, operation); err != nil {
				return service.ApplyExtractionResult{}, err
			}
			result.Added++
		case service.MemoryActionUpdate:
			if err := applyMemoryUpdate(ctx, tx, request, operation); err != nil {
				return service.ApplyExtractionResult{}, err
			}
			result.Updated++
		case service.MemoryActionDelete:
			if err := applyMemoryDelete(ctx, tx, request, operation); err != nil {
				return service.ApplyExtractionResult{}, err
			}
			result.Deleted++
		default:
			return service.ApplyExtractionResult{}, fmt.Errorf("未知记忆操作类型: %s", operation.Action)
		}
	}

	// 3. 推进游标并清空租约/失败信息。行已被 FOR UPDATE 锁定并校验，此处必然影响 1 行。
	command, err := tx.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET last_extracted_result_seq = $3,
		    lease_token = NULL,
		    lease_until = NULL,
		    consecutive_failures = 0,
		    next_retry_at = NULL,
		    last_error_code = NULL
		WHERE session_id = $1 AND user_id = $2`,
		request.SessionID, request.UserID, request.ToResultSeq)
	if err != nil {
		return service.ApplyExtractionResult{}, fmt.Errorf("推进抽取游标失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.ApplyExtractionResult{}, service.ErrExtractionCursorConflict
	}

	if err := tx.Commit(ctx); err != nil {
		return service.ApplyExtractionResult{}, fmt.Errorf("提交记忆抽取事务失败: %w", err)
	}
	result.ToResultSeq = request.ToResultSeq
	return result, nil
}

// applyMemoryAdd 插入一条新的当前记忆（version 1）并追加 0->1 历史与全部来源。
// memory_id / history_id 由后端生成 UUIDv7，仅在命中主键唯一冲突时用保存点重试。
func applyMemoryAdd(ctx context.Context, tx pgx.Tx, request service.ApplyExtractionRequest, operation service.ResolvedOperation) error {
	for attempt := 0; attempt < maxMemoryIDAttempts; attempt++ {
		memoryID, err := service.NewMemoryID()
		if err != nil {
			return err
		}
		historyID, err := service.NewHistoryID()
		if err != nil {
			return err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("开启记忆 ADD 保存点失败: %w", err)
		}
		err = func() error {
			if _, execErr := sp.Exec(ctx, `
				INSERT INTO agent_memory_meta (
					memory_id, user_id, agent_id, memory_type, memory_value, confidence,
					latest_source_message_id, extractor_model, extractor_version, version
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1)`,
				memoryID, request.UserID, request.AgentID, operation.MemoryType, operation.MemoryValue,
				operation.Confidence, nullableUUID(operation.LatestSourceMessageID),
				request.ExtractorModel, request.ExtractorVersion); execErr != nil {
				return execErr
			}
			newValue := operation.MemoryValue
			return insertMemoryHistory(ctx, sp, request, operation, memoryID, historyID,
				service.MemoryActionAdd, 0, 1, nil, &newValue)
		}()
		if err != nil {
			_ = sp.Rollback(ctx)
			if isMemoryUUIDConflict(err) {
				continue
			}
			return mapMemoryWriteError(err)
		}
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("提交记忆 ADD 保存点失败: %w", err)
		}
		return nil
	}
	return errors.New("连续多次 memory_id/history_id 唯一冲突，放弃写入")
}

// applyMemoryUpdate 按 (memory_id,user_id,agent_id,version) 乐观锁更新当前记忆，并追加 UPDATE 历史与来源。
func applyMemoryUpdate(ctx context.Context, tx pgx.Tx, request service.ApplyExtractionRequest, operation service.ResolvedOperation) error {
	var oldValue string
	var newVersion int64
	err := tx.QueryRow(ctx, `
		WITH locked AS (
			SELECT memory_value
			FROM agent_memory_meta
			WHERE memory_id = $1 AND user_id = $2 AND agent_id = $3 AND version = $4 AND deleted_at IS NULL
			FOR UPDATE
		), updated AS (
			UPDATE agent_memory_meta AS m
			SET memory_value = $5,
			    memory_type = $6,
			    confidence = $7,
			    latest_source_message_id = $8,
			    extractor_model = $9,
			    extractor_version = $10,
			    version = m.version + 1
			FROM locked
			WHERE m.memory_id = $1 AND m.user_id = $2 AND m.agent_id = $3 AND m.version = $4 AND m.deleted_at IS NULL
			RETURNING m.version
		)
		SELECT locked.memory_value, updated.version FROM locked, updated`,
		operation.MemoryID, request.UserID, request.AgentID, operation.ExpectedVersion,
		operation.MemoryValue, operation.MemoryType, operation.Confidence,
		nullableUUID(operation.LatestSourceMessageID), request.ExtractorModel, request.ExtractorVersion).
		Scan(&oldValue, &newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		// 版本不匹配或已被软删除：旧结果不能覆盖新版本。
		return service.ErrMemoryVersionConflict
	}
	if err != nil {
		return fmt.Errorf("更新当前记忆失败: %w", err)
	}

	newValue := operation.MemoryValue
	return insertMemoryHistoryWithRetry(ctx, tx, request, operation,
		service.MemoryActionUpdate, operation.ExpectedVersion, newVersion, &oldValue, &newValue)
}

// applyMemoryDelete 软删除当前记忆（version 加 1），并追加 DELETE 历史与来源。
func applyMemoryDelete(ctx context.Context, tx pgx.Tx, request service.ApplyExtractionRequest, operation service.ResolvedOperation) error {
	var oldValue string
	var newVersion int64
	err := tx.QueryRow(ctx, `
		WITH locked AS (
			SELECT memory_value
			FROM agent_memory_meta
			WHERE memory_id = $1 AND user_id = $2 AND agent_id = $3 AND version = $4 AND deleted_at IS NULL
			FOR UPDATE
		), updated AS (
			UPDATE agent_memory_meta AS m
			SET deleted_at = now(),
			    latest_source_message_id = $5,
			    extractor_model = $6,
			    extractor_version = $7,
			    version = m.version + 1
			FROM locked
			WHERE m.memory_id = $1 AND m.user_id = $2 AND m.agent_id = $3 AND m.version = $4 AND m.deleted_at IS NULL
			RETURNING m.version
		)
		SELECT locked.memory_value, updated.version FROM locked, updated`,
		operation.MemoryID, request.UserID, request.AgentID, operation.ExpectedVersion,
		nullableUUID(operation.LatestSourceMessageID), request.ExtractorModel, request.ExtractorVersion).
		Scan(&oldValue, &newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.ErrMemoryVersionConflict
	}
	if err != nil {
		return fmt.Errorf("软删除当前记忆失败: %w", err)
	}

	return insertMemoryHistoryWithRetry(ctx, tx, request, operation,
		service.MemoryActionDelete, operation.ExpectedVersion, newVersion, &oldValue, nil)
}

// insertMemoryHistoryWithRetry 为 UPDATE/DELETE 追加历史与来源；history_id 由后端生成，
// 仅在命中 history 主键唯一冲突时用保存点重试。同版本重复写入（uk_hist_memory_version）不重试，视为版本冲突。
func insertMemoryHistoryWithRetry(
	ctx context.Context,
	tx pgx.Tx,
	request service.ApplyExtractionRequest,
	operation service.ResolvedOperation,
	action string,
	fromVersion, toVersion int64,
	oldValue, newValue *string,
) error {
	for attempt := 0; attempt < maxMemoryIDAttempts; attempt++ {
		historyID, err := service.NewHistoryID()
		if err != nil {
			return err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("开启记忆历史保存点失败: %w", err)
		}
		err = insertMemoryHistory(ctx, sp, request, operation, operation.MemoryID, historyID,
			action, fromVersion, toVersion, oldValue, newValue)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isHistoryIDConflict(err) {
				continue
			}
			return mapMemoryWriteError(err)
		}
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("提交记忆历史保存点失败: %w", err)
		}
		return nil
	}
	return errors.New("连续多次 history_id 唯一冲突，放弃写入")
}

// insertMemoryHistory 追加一条历史事件及其全部来源证据。memoryID 与 historyID 由调用方给定。
func insertMemoryHistory(
	ctx context.Context,
	sp pgx.Tx,
	request service.ApplyExtractionRequest,
	operation service.ResolvedOperation,
	memoryID, historyID, action string,
	fromVersion, toVersion int64,
	oldValue, newValue *string,
) error {
	if _, err := sp.Exec(ctx, `
		INSERT INTO agent_memory_history (
			history_id, memory_id, user_id, agent_id, action, from_version, to_version,
			old_memory_value, new_memory_value, memory_type, confidence, extractor_model, extractor_version
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		historyID, memoryID, request.UserID, request.AgentID, action, fromVersion, toVersion,
		oldValue, newValue, nullableText(operation.MemoryType), operation.Confidence,
		request.ExtractorModel, request.ExtractorVersion); err != nil {
		return err
	}
	for _, source := range operation.Sources {
		if _, err := sp.Exec(ctx, `
			INSERT INTO agent_memory_history_source (
				history_id, source_order, user_id, source_message_id, evidence_quote
			)
			VALUES ($1, $2, $3, $4, $5)`,
			historyID, source.SourceOrder, request.UserID, source.MessageID, source.EvidenceQuote); err != nil {
			return err
		}
	}
	return nil
}

// nullableUUID 把空字符串转成 NULL，供可空 UUID 列（如 latest_source_message_id）写入。
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// nullableText 把空字符串转成 NULL，供可空文本列写入。
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// isMemoryUUIDConflict 只识别 memory / history 主键的唯一冲突，用于 ADD 时的 UUID 重生成重试。
func isMemoryUUIDConflict(err error) bool {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23505" {
		return false
	}
	return pgError.ConstraintName == "agent_memory_meta_pkey" || pgError.ConstraintName == "agent_memory_history_pkey"
}

// isHistoryIDConflict 只识别 history 主键的唯一冲突，用于 UPDATE/DELETE 时的 history_id 重生成重试。
func isHistoryIDConflict(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "agent_memory_history_pkey"
}

// mapMemoryWriteError 把数据库约束错误翻译成领域错误：
//   - 同一记忆同一版本重复历史（uk_hist_memory_version）-> 版本冲突。
//   - 来源/记忆归属外键（source_message_id、latest_source_message_id 等）-> 来源越权。
func mapMemoryWriteError(err error) error {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		return err
	}
	switch pgError.Code {
	case "23505":
		if pgError.ConstraintName == "uk_hist_memory_version" {
			return service.ErrMemoryVersionConflict
		}
	case "23503":
		return service.ErrMemorySourceForbidden
	}
	return err
}
