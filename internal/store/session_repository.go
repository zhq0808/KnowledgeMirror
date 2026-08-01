package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/service"
)

// PostgresSessionRepository 使用共享连接池持久化会话及其用户归属。
type PostgresSessionRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresSessionRepository(pool *pgxpool.Pool) *PostgresSessionRepository {
	return &PostgresSessionRepository{pool: pool}
}

func (r *PostgresSessionRepository) CreateSession(ctx context.Context, userID, sessionID string) (bool, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2)`, sessionID, userID)
	if err != nil {
		if isUniqueViolation(err) {
			return false, nil
		}
		return false, fmt.Errorf("写入会话失败: %w", err)
	}
	return true, nil
}

func (r *PostgresSessionRepository) OwnsSession(ctx context.Context, userID, sessionID string) (bool, error) {
	var owned bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM agent_memory_session
			WHERE session_id = $1
			  AND user_id = $2
			  AND deleted_at IS NULL
		)`, sessionID, userID).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("查询会话归属失败: %w", err)
	}
	return owned, nil
}

func (r *PostgresSessionRepository) OwnsActiveSession(ctx context.Context, userID, sessionID string) (bool, error) {
	var owned bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM agent_memory_session
			WHERE session_id = $1
			  AND user_id = $2
			  AND status = 'active'
			  AND deleted_at IS NULL
		)`, sessionID, userID).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("查询会话归属失败: %w", err)
	}
	return owned, nil
}

// ListSessions 按最近活跃时间（无活跃消息时退回创建时间）倒序返回该用户未删除的会话。
func (r *PostgresSessionRepository) ListSessions(ctx context.Context, userID string, limit int) ([]service.SessionListItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT session_id, COALESCE(title, ''), status, message_count, last_message_at, created_at
		FROM agent_memory_session
		WHERE user_id = $1
		  AND deleted_at IS NULL
		ORDER BY COALESCE(last_message_at, created_at) DESC,
		         created_at DESC,
		         session_id DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表失败: %w", err)
	}
	defer rows.Close()

	items := make([]service.SessionListItem, 0, limit)
	for rows.Next() {
		var item service.SessionListItem
		if err := rows.Scan(
			&item.SessionID,
			&item.Title,
			&item.Status,
			&item.MessageCount,
			&item.LastMessageAt,
			&item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描会话列表失败: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}
	return items, nil
}
