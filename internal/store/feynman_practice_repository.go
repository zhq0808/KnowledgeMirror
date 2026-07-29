package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"healthAgent/internal/service"
)

// PostgresFeynmanPracticeRepository 持久化会话级的对话式费曼练习状态。
//
// 这张表是“当前投影”而不是历史账本：一个会话只有一行，随着练习推进被覆盖。
// 历史（问了什么、答了什么、给了什么反馈）本来就完整落在 agent_memory_episodic 的
// 消息流里，没必要再复制一份账本；这里只保存恢复对话所必需的最小状态。
//
// 不可协商约束：每条 SQL 都带 user_id，跨用户读写不可能命中。
type PostgresFeynmanPracticeRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresFeynmanPracticeRepository 构造练习状态仓储。
func NewPostgresFeynmanPracticeRepository(pool *pgxpool.Pool) *PostgresFeynmanPracticeRepository {
	return &PostgresFeynmanPracticeRepository{pool: pool}
}

const feynmanPracticeStateSelectSQL = `
	SELECT session_id, user_id, state, active_question_text, question_origin,
	       COALESCE(last_answered_message_id::text, ''), last_feedback, round_no, updated_at
	FROM feynman_practice_states
	WHERE session_id = $1 AND user_id = $2`

// Load 读取会话当前练习状态；无记录时返回 found=false，由服务层按 idle 处理。
func (r *PostgresFeynmanPracticeRepository) Load(ctx context.Context, userID, sessionID string) (service.FeynmanPracticeState, bool, error) {
	var state service.FeynmanPracticeState
	err := r.pool.QueryRow(ctx, feynmanPracticeStateSelectSQL, sessionID, userID).Scan(
		&state.SessionID, &state.UserID, &state.State, &state.ActiveQuestionText, &state.QuestionOrigin,
		&state.LastAnsweredMessageID, &state.LastFeedback, &state.RoundNo, &state.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanPracticeState{}, false, nil
	}
	if err != nil {
		return service.FeynmanPracticeState{}, false, fmt.Errorf("查询费曼练习状态失败: %w", err)
	}
	return state, true, nil
}

const feynmanPracticeStateUpsertSQL = `
	INSERT INTO feynman_practice_states (
		session_id, user_id, state, active_question_text, question_origin,
		last_answered_message_id, last_feedback, round_no
	) VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, $7, $8)
	ON CONFLICT (session_id) DO UPDATE SET
		state                    = EXCLUDED.state,
		active_question_text     = EXCLUDED.active_question_text,
		question_origin          = EXCLUDED.question_origin,
		last_answered_message_id = EXCLUDED.last_answered_message_id,
		last_feedback            = EXCLUDED.last_feedback,
		round_no                 = EXCLUDED.round_no
	WHERE feynman_practice_states.user_id = EXCLUDED.user_id`

// Save 以 session_id 为粒度整体覆盖当前状态。
//
// ON CONFLICT 上那句 user_id 相等的条件是越权兜底：session_id 是主键，
// 如果换个 user_id 来写同一个会话，这里会静默不更新而不是改掉别人的状态；
// 影响行数为 0 即视为越权，直接报错，不让调用方以为写成功了。
func (r *PostgresFeynmanPracticeRepository) Save(ctx context.Context, state service.FeynmanPracticeState) error {
	tag, err := r.pool.Exec(ctx, feynmanPracticeStateUpsertSQL,
		state.SessionID, state.UserID, state.State, state.ActiveQuestionText, state.QuestionOrigin,
		state.LastAnsweredMessageID, state.LastFeedback, state.RoundNo,
	)
	if err != nil {
		return fmt.Errorf("保存费曼练习状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("保存费曼练习状态失败: 会话 %s 不属于当前用户", state.SessionID)
	}
	return nil
}
