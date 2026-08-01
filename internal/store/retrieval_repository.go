package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"KnowledgeMirror/internal/service"
)

// RetrievalExecutor 是检索仓储需要的最小 pgx 能力。
// *pgxpool.Pool 与 pgx.Tx 都满足它，因此固定检索集可以在一个最终回滚的事务里跑真 SQL，
// 既不依赖手写假仓储，也不会在开发库里留下数据。
type RetrievalExecutor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PostgresRetrievalRepository 实现知识库检索 v0 与检索审计。
//
// 三条约束在这一层兜底：
//   - 每条查询都带 user_id，跨用户召回不可能命中；
//   - 可召回集合完全由数据库事实推导（资料未删除 + 当前版本 + ai_retrieval 用途 + 片段开关），
//     不维护任何检索副本，因此撤回授权或删除资料后不存在“可召回残片”；
//   - 检索词只作为查询参数下发并预先转义 LIKE 通配符，不做任何 SQL 字符串拼接。
type PostgresRetrievalRepository struct {
	db RetrievalExecutor
}

func NewPostgresRetrievalRepository(db RetrievalExecutor) *PostgresRetrievalRepository {
	return &PostgresRetrievalRepository{db: db}
}

// likeEscaper 转义 LIKE 通配符：检索词来自用户输入，
// 不转义的话一个 "%" 就能把匹配退化成全表命中。
var likeEscaper = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

// searchSourceChunksSQL 的可召回条件是 AND 关系，缺一不可：
//
//	① c.user_id = $1                                 用户隔离
//	② d.deleted_at IS NULL                           资料未删除
//	③ c.document_version_id = d.current_version_id   只召回当前版本
//	④ c.retrieval_enabled                            片段级开关
//	⑤ document_usages(ai_retrieval, enabled)         资料级用途已确认
//
// 打分：标题路径命中权重 3（标题是作者亲手写的语义标签），正文命中权重 1，
// 整句命中额外 +2（精确匹配应稳定排在碎片匹配之前）。
// 排序必须带确定性 tie-break，否则同分片段每次顺序不同，固定检索集无法断言。
const searchSourceChunksSQL = `
WITH scored AS (
    SELECT
        c.source_chunk_id::text       AS source_chunk_id,
        c.document_id::text           AS document_id,
        c.document_version_id::text   AS version_id,
        d.title                       AS document_title,
        d.document_kind               AS document_kind,
        d.content_origin              AS content_origin,
        v.version_no                  AS version_no,
        c.ordinal                     AS ordinal,
        c.heading_path                AS heading_path,
        c.content                     AS content,
        c.trust_level                 AS trust_level,
        c.updated_at                  AS updated_at,
        t.term_score
            + CASE WHEN $3 <> '' AND c.content ILIKE '%' || $3 || '%' ESCAPE '\' THEN 2 ELSE 0 END
                                      AS score,
        t.matched_terms
    FROM source_chunks AS c
    JOIN documents AS d
      ON d.document_id = c.document_id AND d.user_id = c.user_id
    JOIN document_versions AS v
      ON v.version_id = c.document_version_id AND v.user_id = c.user_id
    CROSS JOIN LATERAL (
        SELECT
            COALESCE(SUM(
                CASE WHEN array_to_string(c.heading_path, ' ') ILIKE '%' || term || '%' ESCAPE '\' THEN 3 ELSE 0 END
              + CASE WHEN c.content ILIKE '%' || term || '%' ESCAPE '\' THEN 1 ELSE 0 END
            ), 0) AS term_score,
            COUNT(*) FILTER (
                WHERE array_to_string(c.heading_path, ' ') ILIKE '%' || term || '%' ESCAPE '\'
                   OR c.content ILIKE '%' || term || '%' ESCAPE '\'
            ) AS matched_terms
        FROM unnest($2::text[]) AS term
    ) AS t
    WHERE c.user_id = $1
      AND d.deleted_at IS NULL
      AND c.document_version_id = d.current_version_id
      AND c.retrieval_enabled
      AND EXISTS (
          SELECT 1
          FROM document_usages AS u
          WHERE u.document_version_id = c.document_version_id
            AND u.user_id = c.user_id
            AND u.purpose = 'ai_retrieval'
            AND u.enabled
      )
      AND (
          $4::uuid IS NULL
          OR EXISTS (
              SELECT 1
              FROM knowledge_point_sources AS kps
              WHERE kps.source_chunk_id = c.source_chunk_id
                AND kps.user_id = c.user_id
                AND kps.knowledge_point_id = $4::uuid
          )
      )
)
SELECT source_chunk_id, document_id, version_id, document_title, document_kind,
       content_origin, version_no, ordinal, heading_path, content, trust_level, score
FROM scored
WHERE matched_terms > 0
ORDER BY score DESC, updated_at DESC, source_chunk_id
LIMIT $5`

func (r *PostgresRetrievalRepository) SearchSourceChunks(ctx context.Context, params service.SearchSourceChunksParams) ([]service.RetrievalCandidate, error) {
	if strings.TrimSpace(params.UserID) == "" {
		// 防御性兜底：没有用户身份的检索永远不允许发出去。
		return nil, fmt.Errorf("检索缺少用户身份")
	}
	if len(params.Terms) == 0 {
		return nil, nil
	}

	terms := make([]string, 0, len(params.Terms))
	for _, term := range params.Terms {
		if escaped := likeEscaper.Replace(term); escaped != "" {
			terms = append(terms, escaped)
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}

	var knowledgePointID *string
	if id := strings.TrimSpace(params.KnowledgePointID); id != "" {
		knowledgePointID = &id
	}
	limit := params.Limit
	if limit <= 0 {
		limit = service.DefaultRetrievalLimits().MaxCandidates
	}

	rows, err := r.db.Query(ctx, searchSourceChunksSQL,
		params.UserID, terms, likeEscaper.Replace(params.Phrase), knowledgePointID, limit)
	if err != nil {
		return nil, fmt.Errorf("检索来源片段失败: %w", err)
	}
	defer rows.Close()

	candidates := make([]service.RetrievalCandidate, 0, limit)
	for rows.Next() {
		var candidate service.RetrievalCandidate
		if err := rows.Scan(
			&candidate.SourceChunkID, &candidate.DocumentID, &candidate.VersionID,
			&candidate.DocumentTitle, &candidate.DocumentKind, &candidate.ContentOrigin,
			&candidate.VersionNo, &candidate.Ordinal, &candidate.HeadingPath,
			&candidate.Content, &candidate.TrustLevel, &candidate.Score,
		); err != nil {
			return nil, fmt.Errorf("扫描检索结果失败: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取检索结果失败: %w", err)
	}
	return candidates, nil
}

// RecordRetrieval 在一个事务里写入检索请求与命中明细。
// 审计只保存片段 ID，不复制片段正文——正文的事实源永远是 source_chunks。
func (r *PostgresRetrievalRepository) RecordRetrieval(ctx context.Context, log service.RetrievalRequestLog) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启检索审计事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sessionID *string
	if id := strings.TrimSpace(log.SessionID); id != "" {
		sessionID = &id
	}
	var traceID *string
	if id := strings.TrimSpace(log.TraceID); id != "" {
		traceID = &id
	}
	var knowledgePointID *string
	if id := strings.TrimSpace(log.KnowledgePointID); id != "" {
		knowledgePointID = &id
	}
	terms := log.QueryTerms
	if terms == nil {
		terms = []string{}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO retrieval_requests (
			retrieval_request_id, user_id, session_id, trace_id, purpose, knowledge_point_id,
			query_text, query_terms, max_results, context_budget_chars,
			candidate_count, selected_count, excluded_count, prompt_chars, duration_ms, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		log.RequestID, log.UserID, sessionID, traceID, log.Purpose, knowledgePointID,
		log.QueryText, terms, log.MaxResults, log.ContextBudgetChars,
		log.CandidateCount, log.SelectedCount, log.ExcludedCount, log.PromptChars,
		log.DurationMillis, log.Status); err != nil {
		return fmt.Errorf("写入检索请求审计失败: %w", err)
	}

	for _, hit := range log.Hits {
		var reason *string
		if hit.ExcludedReason != "" {
			value := hit.ExcludedReason
			reason = &value
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO retrieval_hits (
				retrieval_request_id, source_chunk_id, user_id, document_id, document_version_id,
				ref, rank, score, included_in_prompt, excluded_reason, char_cost, truncated
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (retrieval_request_id, source_chunk_id) DO NOTHING`,
			log.RequestID, hit.SourceChunkID, log.UserID, hit.DocumentID, hit.VersionID,
			nullableString(hit.Ref), hit.Rank, hit.Score, hit.IncludedInPrompt, reason, hit.CharCost, hit.Truncated); err != nil {
			return fmt.Errorf("写入检索命中审计失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交检索审计事务失败: %w", err)
	}
	return nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// 编译期确认实现了服务层契约。
var _ service.RetrievalRepository = (*PostgresRetrievalRepository)(nil)
