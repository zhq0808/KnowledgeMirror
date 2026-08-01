package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"KnowledgeMirror/internal/service"
)

// maxMessageIDAttempts 限制 message_id 生成的有限冲突重试次数。UUIDv7 有 122 位随机，
// 碰撞概率极低，这里用保存点包住 INSERT，仅在命中 message_id 唯一冲突时重新生成重试。
const maxMessageIDAttempts = 5

// PostgresMessageRepository 使用共享连接池持久化对话消息。
type PostgresMessageRepository struct {
	pool MessageExecutor
}

type MessageExecutor interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewPostgresMessageRepository(pool MessageExecutor) *PostgresMessageRepository {
	return &PostgresMessageRepository{pool: pool}
}

// AppendUserMessage 在短事务内串行化同一 session 的 seq 分配，并保证客户端消息幂等。
func (r *PostgresMessageRepository) AppendUserMessage(ctx context.Context, request service.AppendUserMessageRequest) (service.AppendUserMessageResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.AppendUserMessageResult{}, fmt.Errorf("开启用户消息事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := appendUserMessageTx(ctx, tx, request)
	if err != nil {
		return service.AppendUserMessageResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return service.AppendUserMessageResult{}, fmt.Errorf("提交用户消息事务失败: %w", err)
	}
	return result, nil
}

// appendUserMessageTx 在调用方提供的短事务中锁定 Session、从 next_message_seq 分配 seq，
// 并幂等写入用户消息。message_id 由后端生成 UUIDv7。调用方负责提交或回滚事务。
func appendUserMessageTx(ctx context.Context, tx pgx.Tx, request service.AppendUserMessageRequest) (service.AppendUserMessageResult, error) {
	var nextSeq int64
	err := tx.QueryRow(ctx, `
		SELECT next_message_seq
		FROM agent_memory_session
		WHERE session_id = $1
		  AND user_id = $2
		  AND status = 'active'
		  AND deleted_at IS NULL
		FOR UPDATE`, request.SessionID, request.UserID).Scan(&nextSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.AppendUserMessageResult{}, service.ErrSessionNotFound
	}
	if err != nil {
		return service.AppendUserMessageResult{}, fmt.Errorf("锁定会话失败: %w", err)
	}

	existing, err := scanUserMessage(tx.QueryRow(ctx, `
		SELECT message_id::text, user_id, session_id, client_message_id::text, seq,
		       COALESCE(content, ''), created_at
		FROM agent_memory_episodic
		WHERE user_id = $1
		  AND session_id = $2
		  AND client_message_id = $3
		  AND role = 'user'`, request.UserID, request.SessionID, request.ClientMessageID))
	if err == nil {
		return service.AppendUserMessageResult{Message: existing, Created: false}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return service.AppendUserMessageResult{}, fmt.Errorf("查询幂等用户消息失败: %w", err)
	}

	message, err := insertEpisodicWithRetry(ctx, tx, func(sp pgx.Tx, messageID string) (service.UserMessage, error) {
		return scanUserMessage(sp.QueryRow(ctx, `
			INSERT INTO agent_memory_episodic (
				message_id, session_id, user_id, agent_id, seq, role, status,
				content, client_message_id
			)
			VALUES ($1, $2, $3, $4, $5, 'user', 'completed', $6, $7)
			RETURNING message_id::text, user_id, session_id, client_message_id::text, seq,
			          COALESCE(content, ''), created_at`,
			messageID,
			request.SessionID,
			request.UserID,
			service.InterviewAgentID,
			nextSeq,
			request.Content,
			request.ClientMessageID,
		))
	})
	if err != nil {
		return service.AppendUserMessageResult{}, fmt.Errorf("写入用户消息失败: %w", err)
	}

	if err := advanceSessionCounters(ctx, tx, request.SessionID, request.UserID, message.CreatedAt); err != nil {
		return service.AppendUserMessageResult{}, err
	}

	return service.AppendUserMessageResult{Message: message, Created: true}, nil
}

// AppendAssistantMessage 将完整回复追加到同一 session 的消息流水中。
func (r *PostgresMessageRepository) AppendAssistantMessage(ctx context.Context, request service.AppendAssistantMessageRequest) (service.AssistantMessage, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("开启 assistant 消息事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	message, err := appendAssistantMessageTx(ctx, tx, request)
	if err != nil {
		return service.AssistantMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return service.AssistantMessage{}, fmt.Errorf("提交 assistant 消息事务失败: %w", err)
	}
	return message, nil
}

// appendAssistantMessageTx 在调用方提供的短事务中追加 assistant 消息并推进 Session 序号/计数。
// ParentMessageID 为空时 parent_message_id 保持 NULL；调用方负责提交或回滚事务。
func appendAssistantMessageTx(ctx context.Context, tx pgx.Tx, request service.AppendAssistantMessageRequest) (service.AssistantMessage, error) {
	var nextSeq int64
	err := tx.QueryRow(ctx, `
		SELECT next_message_seq
		FROM agent_memory_session
		WHERE session_id = $1
		  AND user_id = $2
		  AND status = 'active'
		  AND deleted_at IS NULL
		FOR UPDATE`, request.SessionID, request.UserID).Scan(&nextSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.AssistantMessage{}, service.ErrSessionNotFound
	}
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("锁定会话失败: %w", err)
	}

	// parent_message_id 是 UUID 列；空字符串不能直接写入，用 nil 让 pgx 发 NULL。
	var parentMessageID any
	if request.ParentMessageID != "" {
		parentMessageID = request.ParentMessageID
	}

	message, err := insertEpisodicWithRetry(ctx, tx, func(sp pgx.Tx, messageID string) (service.AssistantMessage, error) {
		return scanAssistantMessage(sp.QueryRow(ctx, `
			INSERT INTO agent_memory_episodic (
				message_id, session_id, user_id, agent_id, seq, parent_message_id, role, status,
				content, meta_data
			)
			VALUES (
				$1, $2, $3, $4, $5, $6, 'assistant', 'completed', $7,
				jsonb_strip_nulls(jsonb_build_object(
					'prompt_version', $8::text,
					'model', $9::text,
					'retrieval_request_id', NULLIF($10::text, '')
				))
			)
			RETURNING message_id::text, user_id, session_id, seq,
			          COALESCE(content, ''), created_at`,
			messageID,
			request.SessionID,
			request.UserID,
			service.InterviewAgentID,
			nextSeq,
			parentMessageID,
			request.Content,
			request.PromptVersion,
			request.ModelName,
			request.RetrievalRequestID,
		))
	})
	if err != nil {
		return service.AssistantMessage{}, fmt.Errorf("写入 assistant 消息失败: %w", err)
	}

	if err := advanceSessionCounters(ctx, tx, request.SessionID, request.UserID, message.CreatedAt); err != nil {
		return service.AssistantMessage{}, err
	}

	return message, nil
}

// advanceSessionCounters 在同一短事务内推进 Session 的序号分配器和消息计数：
// next_message_seq 只增不减（事务回滚时一并回滚，不消耗序号），message_count 仅作统计。
func advanceSessionCounters(ctx context.Context, tx pgx.Tx, sessionID, userID string, lastMessageAt time.Time) error {
	command, err := tx.Exec(ctx, `
		UPDATE agent_memory_session
		SET next_message_seq = next_message_seq + 1,
		    message_count = message_count + 1,
		    last_message_at = $3
		WHERE session_id = $1
		  AND user_id = $2`, sessionID, userID, lastMessageAt)
	if err != nil {
		return fmt.Errorf("更新会话序号与计数失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("更新会话序号与计数影响行数异常: %d", command.RowsAffected())
	}
	return nil
}

// insertEpisodicWithRetry 在 tx 内用 SAVEPOINT 反复尝试：每次生成一个 UUIDv7 message_id 并执行 insert，
// 仅当命中 message_id 唯一冲突时回滚保存点并重新生成；其它错误直接返回。
func insertEpisodicWithRetry[T any](ctx context.Context, tx pgx.Tx, insert func(sp pgx.Tx, messageID string) (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < maxMessageIDAttempts; attempt++ {
		messageID, err := service.NewMessageID()
		if err != nil {
			return zero, err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return zero, fmt.Errorf("开启 message_id 保存点失败: %w", err)
		}
		value, err := insert(sp, messageID)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isMessageIDConflict(err) {
				continue
			}
			return zero, err
		}
		if err := sp.Commit(ctx); err != nil {
			return zero, fmt.Errorf("提交 message_id 保存点失败: %w", err)
		}
		return value, nil
	}
	return zero, errors.New("连续多次 message_id 唯一冲突，放弃写入")
}

// isMessageIDConflict 只识别 message_id 相关的唯一冲突，不把 client_message_id / session_seq 等
// 其它唯一冲突当成可重试的 message_id 碰撞。
func isMessageIDConflict(err error) bool {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23505" {
		return false
	}
	return pgError.ConstraintName == "uk_ame_message_id" || pgError.ConstraintName == "uk_ame_message_user"
}

// LoadRecent 读取最近的 completed 对话消息，再按 seq 正序返回给上层组装上下文。
func (r *PostgresMessageRepository) LoadRecent(ctx context.Context, userID, sessionID string, limit int) ([]service.ConversationMessage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT seq, role, content
		FROM (
			SELECT seq, role, content
			FROM agent_memory_episodic
			WHERE user_id = $1
			  AND session_id = $2
			  AND status = 'completed'
			  AND deleted_at IS NULL
			  AND role IN ('user', 'assistant')
			  AND content IS NOT NULL
			ORDER BY seq DESC
			LIMIT $3
		) AS recent
		ORDER BY seq ASC`, userID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取最近对话历史失败: %w", err)
	}
	defer rows.Close()

	messages := make([]service.ConversationMessage, 0, limit)
	for rows.Next() {
		var message service.ConversationMessage
		if err := rows.Scan(&message.Seq, &message.Role, &message.Content); err != nil {
			return nil, fmt.Errorf("扫描对话历史失败: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历对话历史失败: %w", err)
	}
	return messages, nil
}

// FindAssistantReplyByID 按 completed turn 保存的结果消息 UUID 查询 assistant 回复。
func (r *PostgresMessageRepository) FindAssistantReplyByID(ctx context.Context, userID, sessionID, messageID string) (service.AssistantMessage, bool, error) {
	row := r.pool.QueryRow(ctx, messageWithRetrievalSQL+`
		WHERE message.message_id = $1
		  AND message.user_id = $2
		  AND message.session_id = $3
		  AND message.role = 'assistant'
		  AND message.status = 'completed'
		  AND message.deleted_at IS NULL`, messageID, userID, sessionID)
	message, err := scanAssistantMessageWithRetrieval(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.AssistantMessage{}, false, nil
	}
	if err != nil {
		return service.AssistantMessage{}, false, fmt.Errorf("查询 assistant 回放消息失败: %w", err)
	}
	message.UserID = userID
	message.SessionID = sessionID
	return message, true, nil
}

// ListMessages 按 seq 升序返回该会话已完成、未删除的 user/assistant 消息，供"切换会话后
// 加载完整历史"这个场景使用。userID 是调用方已经校验过归属的可信用户，这里的 WHERE 条件
// 再带一层 user_id 过滤，是防止上层校验被绕过的第二道防线，不是唯一防线。
func (r *PostgresMessageRepository) ListMessages(ctx context.Context, userID, sessionID string) ([]service.SessionMessage, error) {
	rows, err := r.pool.Query(ctx, messageWithRetrievalSQL+`
		JOIN agent_memory_session AS session
		  ON session.session_id = message.session_id
		 AND session.user_id = message.user_id
		WHERE message.user_id = $1
		  AND message.session_id = $2
		  AND session.deleted_at IS NULL
		  AND message.status = 'completed'
		  AND message.deleted_at IS NULL
		  AND message.role IN ('user', 'assistant')
		ORDER BY message.seq ASC`, userID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询会话消息失败: %w", err)
	}
	defer rows.Close()

	messages := make([]service.SessionMessage, 0)
	for rows.Next() {
		var message service.SessionMessage
		var retrievalID, retrievalStatus, sourcesJSON string
		var candidateCount, quarantinedCount int
		if err := rows.Scan(
			&message.MessageID, &message.Role, &message.Content, &message.Seq,
			&retrievalID, &retrievalStatus, &candidateCount, &quarantinedCount,
			&sourcesJSON, &message.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描会话消息失败: %w", err)
		}
		message.Retrieval, err = decodeMessageRetrieval(retrievalID, retrievalStatus, candidateCount, quarantinedCount, sourcesJSON)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话消息失败: %w", err)
	}
	return messages, nil
}

const messageWithRetrievalSQL = `
		SELECT message.message_id::text, message.role, COALESCE(message.content, ''), message.seq,
		       COALESCE(retrieval.retrieval_request_id::text, ''),
		       COALESCE(retrieval.status, ''),
		       COALESCE(retrieval.candidate_count, 0),
		       COALESCE((
		           SELECT COUNT(*)
		           FROM retrieval_hits AS quarantined
		           WHERE quarantined.retrieval_request_id = retrieval.retrieval_request_id
		             AND quarantined.excluded_reason = 'prompt_injection'
		       ), 0),
		       COALESCE((
		           SELECT json_agg(json_build_object(
		               'ref', hit.ref,
		               'source_chunk_id', hit.source_chunk_id::text,
		               'document_id', hit.document_id::text,
		               'document_title', document.title,
		               'version_no', version.version_no,
		               'heading_path', chunk.heading_path,
		               'content_origin', document.content_origin,
		               'trust_level', chunk.trust_level,
		               'truncated', hit.truncated
		           ) ORDER BY hit.rank)::text
		           FROM retrieval_hits AS hit
		           JOIN source_chunks AS chunk
		             ON chunk.source_chunk_id = hit.source_chunk_id
		            AND chunk.user_id = hit.user_id
		           JOIN documents AS document
		             ON document.document_id = hit.document_id
		            AND document.user_id = hit.user_id
		           JOIN document_versions AS version
		             ON version.version_id = hit.document_version_id
		            AND version.user_id = hit.user_id
		           WHERE hit.retrieval_request_id = retrieval.retrieval_request_id
		             AND hit.included_in_prompt
		       ), '[]'),
		       message.created_at
		FROM agent_memory_episodic AS message
		LEFT JOIN retrieval_requests AS retrieval
		  ON retrieval.retrieval_request_id = NULLIF(message.meta_data->>'retrieval_request_id', '')::uuid
		 AND retrieval.user_id = message.user_id`

type messageRetrievalSourceJSON struct {
	Ref           string   `json:"ref"`
	SourceChunkID string   `json:"source_chunk_id"`
	DocumentID    string   `json:"document_id"`
	DocumentTitle string   `json:"document_title"`
	VersionNo     int      `json:"version_no"`
	HeadingPath   []string `json:"heading_path"`
	ContentOrigin string   `json:"content_origin"`
	TrustLevel    string   `json:"trust_level"`
	Truncated     bool     `json:"truncated"`
}

func decodeMessageRetrieval(requestID, status string, candidateCount, quarantinedCount int, sourcesJSON string) (*service.MessageRetrieval, error) {
	if requestID == "" {
		return nil, nil
	}
	var encoded []messageRetrievalSourceJSON
	if err := json.Unmarshal([]byte(sourcesJSON), &encoded); err != nil {
		return nil, fmt.Errorf("解析消息检索来源失败: %w", err)
	}
	sources := make([]service.SessionRetrievalSource, 0, len(encoded))
	for _, source := range encoded {
		sources = append(sources, service.SessionRetrievalSource{
			Ref:           source.Ref,
			SourceChunkID: source.SourceChunkID,
			DocumentID:    source.DocumentID,
			DocumentTitle: source.DocumentTitle,
			VersionNo:     source.VersionNo,
			HeadingPath:   source.HeadingPath,
			OriginLabel:   service.ContentOriginLabel(source.ContentOrigin),
			TrustLabel:    service.TrustLevelLabel(source.TrustLevel),
			Truncated:     source.Truncated,
		})
	}
	return &service.MessageRetrieval{
		RequestID:        requestID,
		Status:           status,
		CandidateCount:   candidateCount,
		QuarantinedCount: quarantinedCount,
		Sources:          sources,
	}, nil
}

func scanAssistantMessageWithRetrieval(row pgx.Row) (service.AssistantMessage, error) {
	var message service.AssistantMessage
	var role, retrievalID, retrievalStatus, sourcesJSON string
	var candidateCount, quarantinedCount int
	err := row.Scan(
		&message.MessageID, &role, &message.Content, &message.Seq,
		&retrievalID, &retrievalStatus, &candidateCount, &quarantinedCount,
		&sourcesJSON, &message.CreatedAt,
	)
	if err != nil {
		return message, err
	}
	message.Retrieval, err = decodeMessageRetrieval(retrievalID, retrievalStatus, candidateCount, quarantinedCount, sourcesJSON)
	return message, err
}

func scanUserMessage(row pgx.Row) (service.UserMessage, error) {
	var message service.UserMessage
	err := row.Scan(
		&message.MessageID,
		&message.UserID,
		&message.SessionID,
		&message.ClientMessageID,
		&message.Seq,
		&message.Content,
		&message.CreatedAt,
	)
	return message, err
}

func scanAssistantMessage(row pgx.Row) (service.AssistantMessage, error) {
	var message service.AssistantMessage
	err := row.Scan(
		&message.MessageID,
		&message.UserID,
		&message.SessionID,
		&message.Seq,
		&message.Content,
		&message.CreatedAt,
	)
	return message, err
}
