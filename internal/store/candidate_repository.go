package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"healthAgent/internal/service"
)

// maxCandidateIDAttempts 限制 UUIDv7 主键冲突时的有限重试次数。
const maxCandidateIDAttempts = 5

// PostgresCandidateRepository 持久化候选内容、候选来源引用与正式知识点。
//
// 这一层兜底三条不可协商的约束：
//   - 每条 SQL 都带 user_id，跨用户读写不可能命中；
//   - 候选与来源引用在同一事务写入，数据库的延迟约束触发器保证「没有来源就没有候选」；
//   - 终态候选不可再改（数据库触发器拒绝任何对已处理候选的 UPDATE）。
type PostgresCandidateRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresCandidateRepository(pool *pgxpool.Pool) *PostgresCandidateRepository {
	return &PostgresCandidateRepository{pool: pool}
}

const candidateSelectSQL = `
	SELECT candidate_id::text, user_id,
	       COALESCE(document_id::text, ''), COALESCE(document_version_id::text, ''),
	       candidate_type, candidate_payload, status,
	       source_content_origin, trust_level,
	       COALESCE(target_knowledge_point_id::text, ''),
	       COALESCE(merged_into_candidate_id::text, ''),
	       COALESCE(confirmed_outcome, ''), COALESCE(decision_note, ''),
	       COALESCE(extractor_model, ''), COALESCE(extractor_version, ''),
	       confirmed_at, created_at, updated_at
	FROM content_candidates`

func scanCandidate(row rowScanner) (service.ContentCandidate, error) {
	var candidate service.ContentCandidate
	var payload []byte
	err := row.Scan(
		&candidate.CandidateID, &candidate.UserID,
		&candidate.DocumentID, &candidate.VersionID,
		&candidate.CandidateType, &payload, &candidate.Status,
		&candidate.SourceContentOrigin, &candidate.TrustLevel,
		&candidate.TargetKnowledgePointID, &candidate.MergedIntoCandidateID,
		&candidate.ConfirmedOutcome, &candidate.DecisionNote,
		&candidate.ExtractorModel, &candidate.ExtractorVersion,
		&candidate.ConfirmedAt, &candidate.CreatedAt, &candidate.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.ContentCandidate{}, service.ErrCandidateNotFound
	}
	if err != nil {
		return service.ContentCandidate{}, fmt.Errorf("扫描候选内容失败: %w", err)
	}
	if err := json.Unmarshal(payload, &candidate.Payload); err != nil {
		return service.ContentCandidate{}, fmt.Errorf("解析候选正文失败: %w", err)
	}
	return candidate, nil
}

// ---------------------------------------------------------------------------
// 抽取落库
// ---------------------------------------------------------------------------

// SaveCandidates 在一个事务里写入本批候选及其来源引用。
//
// 与已有待确认候选重复的条目通过部分唯一索引被跳过（DO NOTHING），
// 因此重复点击「抽取」不会堆出一模一样的待确认项，也不会报错。
func (r *PostgresCandidateRepository) SaveCandidates(ctx context.Context, params service.SaveCandidatesParams) ([]service.ContentCandidate, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启候选抽取事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	saved := make([]service.ContentCandidate, 0, len(params.Candidates))
	for _, candidate := range params.Candidates {
		payload, err := json.Marshal(candidate.Payload)
		if err != nil {
			return nil, fmt.Errorf("序列化候选正文失败: %w", err)
		}
		candidateID, inserted, err := insertCandidateWithRetry(ctx, tx, params, candidate, payload)
		if err != nil {
			return nil, err
		}
		if !inserted {
			continue
		}
		for _, source := range candidate.Sources {
			if _, err := tx.Exec(ctx, `
				INSERT INTO content_candidate_sources (
					candidate_id, source_chunk_id, user_id, source_order, evidence_quote,
					created_by, updated_by
				)
				VALUES ($1, $2, $3, $4, NULLIF($5, ''), $3, $3)`,
				candidateID, source.SourceChunkID, params.UserID, source.SourceOrder, source.EvidenceQuote); err != nil {
				return nil, fmt.Errorf("写入候选来源引用失败: %w", err)
			}
		}
		stored, err := scanCandidate(tx.QueryRow(ctx, candidateSelectSQL+`
			WHERE candidate_id = $1
			  AND user_id = $2`, candidateID, params.UserID))
		if err != nil {
			return nil, err
		}
		stored.Sources = candidate.Sources
		saved = append(saved, stored)
	}

	// 提交时才会触发「候选必须引用来源片段」的延迟约束。
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交候选抽取事务失败: %w", err)
	}
	return saved, nil
}

func insertCandidateWithRetry(
	ctx context.Context,
	tx pgx.Tx,
	params service.SaveCandidatesParams,
	candidate service.NewCandidate,
	payload []byte,
) (string, bool, error) {
	for range maxCandidateIDAttempts {
		candidateID, err := service.NewCandidateID()
		if err != nil {
			return "", false, err
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return "", false, fmt.Errorf("开启候选保存点失败: %w", err)
		}
		// status 固定 pending、trust_level 固定 unverified：
		// 抽取这条链路没有任何办法产出「已确认」或「可信」的数据。
		var storedID string
		err = savepoint.QueryRow(ctx, `
			INSERT INTO content_candidates (
				candidate_id, user_id, document_id, document_version_id,
				candidate_type, candidate_payload, status,
				source_content_origin, trust_level, dedupe_hash,
				extractor_model, extractor_version, created_by, updated_by
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, 'unverified', $8,
			        NULLIF($9, ''), NULLIF($10, ''), $2, $2)
			ON CONFLICT (user_id, document_version_id, dedupe_hash)
				WHERE status = 'pending' AND dedupe_hash IS NOT NULL
				DO NOTHING
			RETURNING candidate_id::text`,
			candidateID, params.UserID, params.DocumentID, params.VersionID,
			candidate.CandidateType, payload, params.ContentOrigin, candidate.DedupeHash,
			params.ExtractorModel, params.ExtractorVersion).Scan(&storedID)
		if errors.Is(err, pgx.ErrNoRows) {
			// 命中去重：同一版本已有内容相同的待确认候选。
			if err := savepoint.Commit(ctx); err != nil {
				return "", false, fmt.Errorf("提交候选保存点失败: %w", err)
			}
			return "", false, nil
		}
		if err != nil {
			_ = savepoint.Rollback(ctx)
			if isUniqueViolation(err) {
				continue
			}
			return "", false, fmt.Errorf("写入候选内容失败: %w", err)
		}
		if err := savepoint.Commit(ctx); err != nil {
			return "", false, fmt.Errorf("提交候选保存点失败: %w", err)
		}
		return storedID, true, nil
	}
	return "", false, errors.New("连续多次 candidate_id 唯一冲突，放弃写入")
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (r *PostgresCandidateRepository) GetCandidate(ctx context.Context, userID, candidateID string) (service.ContentCandidate, error) {
	candidate, err := scanCandidate(r.pool.QueryRow(ctx, candidateSelectSQL+`
		WHERE candidate_id = $1
		  AND user_id = $2`, candidateID, userID))
	if err != nil {
		return service.ContentCandidate{}, err
	}
	sources, err := r.listSources(ctx, userID, []string{candidateID})
	if err != nil {
		return service.ContentCandidate{}, err
	}
	candidate.Sources = sources[candidateID]
	return candidate, nil
}

func (r *PostgresCandidateRepository) ListCandidates(ctx context.Context, userID string, query service.CandidateQuery) ([]service.ContentCandidate, error) {
	conditions := []string{"user_id = $1"}
	args := []any{userID}
	if query.DocumentID != "" {
		args = append(args, query.DocumentID)
		conditions = append(conditions, fmt.Sprintf("document_id = $%d", len(args)))
	}
	if query.Status != "" {
		args = append(args, query.Status)
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(query.CandidateTypes) > 0 {
		args = append(args, query.CandidateTypes)
		conditions = append(conditions, fmt.Sprintf("candidate_type = ANY($%d::text[])", len(args)))
	}
	args = append(args, query.Limit)

	rows, err := r.pool.Query(ctx, candidateSelectSQL+`
		WHERE `+strings.Join(conditions, "\n		  AND ")+`
		ORDER BY created_at DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("查询候选列表失败: %w", err)
	}
	defer rows.Close()

	candidates := make([]service.ContentCandidate, 0, 16)
	candidateIDs := make([]string, 0, 16)
	for rows.Next() {
		candidate, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
		candidateIDs = append(candidateIDs, candidate.CandidateID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选列表失败: %w", err)
	}
	if len(candidates) == 0 {
		return candidates, nil
	}

	sources, err := r.listSources(ctx, userID, candidateIDs)
	if err != nil {
		return nil, err
	}
	for index := range candidates {
		candidates[index].Sources = sources[candidates[index].CandidateID]
	}
	return candidates, nil
}

// listSources 一次性取回多条候选的来源引用，避免列表接口出现 N+1 查询。
func (r *PostgresCandidateRepository) listSources(ctx context.Context, userID string, candidateIDs []string) (map[string][]service.CandidateSource, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT candidate_id::text, source_chunk_id::text, source_order, COALESCE(evidence_quote, '')
		FROM content_candidate_sources
		WHERE user_id = $1
		  AND candidate_id = ANY($2::uuid[])
		ORDER BY candidate_id, source_order`, userID, candidateIDs)
	if err != nil {
		return nil, fmt.Errorf("查询候选来源引用失败: %w", err)
	}
	defer rows.Close()

	sources := make(map[string][]service.CandidateSource, len(candidateIDs))
	for rows.Next() {
		var candidateID string
		var source service.CandidateSource
		if err := rows.Scan(&candidateID, &source.SourceChunkID, &source.SourceOrder, &source.EvidenceQuote); err != nil {
			return nil, fmt.Errorf("扫描候选来源引用失败: %w", err)
		}
		sources[candidateID] = append(sources[candidateID], source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选来源引用失败: %w", err)
	}
	return sources, nil
}

func (r *PostgresCandidateRepository) ListKnowledgePoints(ctx context.Context, userID string, limit int) ([]service.KnowledgePoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT knowledge_point_id::text, user_id, title, COALESCE(description, ''),
		       status, created_at, updated_at
		FROM knowledge_points
		WHERE user_id = $1
		  AND deleted_at IS NULL
		ORDER BY updated_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询知识点列表失败: %w", err)
	}
	defer rows.Close()

	points := make([]service.KnowledgePoint, 0, 16)
	for rows.Next() {
		var point service.KnowledgePoint
		if err := rows.Scan(&point.KnowledgePointID, &point.UserID, &point.Title,
			&point.Description, &point.Status, &point.CreatedAt, &point.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描知识点失败: %w", err)
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历知识点列表失败: %w", err)
	}
	return points, nil
}

// ---------------------------------------------------------------------------
// 用户处理
// ---------------------------------------------------------------------------

func (r *PostgresCandidateRepository) UpdateCandidatePayload(
	ctx context.Context,
	userID, candidateID string,
	payload service.CandidatePayload,
	dedupeHash []byte,
) (service.ContentCandidate, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return service.ContentCandidate{}, fmt.Errorf("序列化候选正文失败: %w", err)
	}
	command, err := r.pool.Exec(ctx, `
		UPDATE content_candidates
		SET candidate_payload = $1,
		    dedupe_hash = $2,
		    updated_by = $3
		WHERE candidate_id = $4
		  AND user_id = $3
		  AND status = 'pending'`, encoded, dedupeHash, userID, candidateID)
	if err != nil {
		if isUniqueViolation(err) {
			return service.ContentCandidate{}, &service.CandidateInputError{Message: "已存在内容相同的待确认候选"}
		}
		return service.ContentCandidate{}, fmt.Errorf("修改候选内容失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ContentCandidate{}, r.explainMissingPending(ctx, userID, candidateID)
	}
	return r.GetCandidate(ctx, userID, candidateID)
}

// ResolveCandidate 把待确认候选推进到终态。
// `status = 'pending'` 是并发下的乐观条件：两次并发处理只有一次能成功。
func (r *PostgresCandidateRepository) ResolveCandidate(ctx context.Context, params service.ResolveCandidateParams) (service.ContentCandidate, error) {
	var encoded []byte
	if params.Payload != nil {
		var err error
		encoded, err = json.Marshal(*params.Payload)
		if err != nil {
			return service.ContentCandidate{}, fmt.Errorf("序列化候选正文失败: %w", err)
		}
	}
	command, err := r.pool.Exec(ctx, `
		UPDATE content_candidates AS candidate
		SET status = $1,
		    confirmed_outcome = $2,
		    trust_level = $3,
		    merged_into_candidate_id = NULLIF($4, '')::uuid,
		    decision_note = NULLIF($5, ''),
		    candidate_payload = COALESCE($6, candidate_payload),
		    confirmed_by = $7,
		    confirmed_at = $8,
		    updated_by = $7
		WHERE candidate_id = $9
		  AND user_id = $7
		  AND status = 'pending'
		  AND (
		      NULLIF($4, '') IS NULL
		      OR EXISTS (
		          SELECT 1
		          FROM content_candidates AS target
		          WHERE target.candidate_id = NULLIF($4, '')::uuid
		            AND target.user_id = $7
		            AND target.status = 'pending'
		            AND target.candidate_type = candidate.candidate_type
		      )
		  )`,
		params.Status, params.Outcome, params.TrustLevel, params.MergedIntoCandidateID,
		params.DecisionNote, encoded, params.UserID, params.ResolvedAt, params.CandidateID)
	if err != nil {
		return service.ContentCandidate{}, fmt.Errorf("处理候选内容失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ContentCandidate{}, r.explainResolveFailure(ctx, params)
	}
	return r.GetCandidate(ctx, params.UserID, params.CandidateID)
}

func (r *PostgresCandidateRepository) explainResolveFailure(ctx context.Context, params service.ResolveCandidateParams) error {
	var sourceStatus string
	err := r.pool.QueryRow(ctx, `
		SELECT status
		FROM content_candidates
		WHERE candidate_id = $1
		  AND user_id = $2`, params.CandidateID, params.UserID).Scan(&sourceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.ErrCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("查询候选状态失败: %w", err)
	}
	if sourceStatus != service.CandidateStatusPending {
		return service.ErrCandidateResolved
	}
	if params.MergedIntoCandidateID != "" {
		// 来源仍待确认时，原子更新失败只能说明合并目标已离开待确认状态
		// 或不再满足同用户、同类型条件；统一按并发冲突处理并让客户端刷新。
		return service.ErrCandidateResolved
	}
	return service.ErrCandidateNotFound
}

// ConfirmKnowledgePointCandidate 在一个事务里完成：创建或关联知识点 → 关联来源片段 → 推进候选状态。
//
// 这里刻意不写任何掌握状态：新知识点进入知识库后，UI 显示「暂无证据」，
// 掌握等级要等到用户产生真实输出证据才可能出现。
func (r *PostgresCandidateRepository) ConfirmKnowledgePointCandidate(
	ctx context.Context,
	params service.ConfirmKnowledgePointParams,
) (service.ContentCandidate, service.KnowledgePoint, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, fmt.Errorf("开启候选确认事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var point service.KnowledgePoint
	status := service.CandidateStatusConfirmed
	outcome := service.CandidateOutcomeKnowledgePointCreated
	if params.KnowledgePointID == "" {
		point, err = insertKnowledgePointWithRetry(ctx, tx, params)
		if err != nil {
			return service.ContentCandidate{}, service.KnowledgePoint{}, err
		}
	} else {
		// 关联已有项：不新建、不覆盖标题，避免候选反向改写用户已确认的知识点。
		point, err = scanKnowledgePoint(tx.QueryRow(ctx, `
			SELECT knowledge_point_id::text, user_id, title, COALESCE(description, ''),
			       status, created_at, updated_at
			FROM knowledge_points
			WHERE knowledge_point_id = $1
			  AND user_id = $2
			  AND deleted_at IS NULL
			FOR UPDATE`, params.KnowledgePointID, params.UserID))
		if err != nil {
			return service.ContentCandidate{}, service.KnowledgePoint{}, err
		}
		status = service.CandidateStatusLinked
		outcome = service.CandidateOutcomeKnowledgePointLinked
	}

	// 知识点的来源片段直接继承候选的引用：结论永远能回到原文。
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_point_sources (
			knowledge_point_id, source_chunk_id, user_id, relation_type, created_by, updated_by
		)
		SELECT $1, s.source_chunk_id, $2, 'reference', $2, $2
		FROM content_candidate_sources AS s
		WHERE s.candidate_id = $3
		  AND s.user_id = $2
		ON CONFLICT (knowledge_point_id, source_chunk_id) DO NOTHING`,
		point.KnowledgePointID, params.UserID, params.CandidateID); err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, fmt.Errorf("关联知识点来源失败: %w", err)
	}

	encoded, err := json.Marshal(params.Payload)
	if err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, fmt.Errorf("序列化候选正文失败: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE content_candidates
		SET status = $1,
		    confirmed_outcome = $2,
		    trust_level = $3,
		    target_knowledge_point_id = $4,
		    decision_note = NULLIF($5, ''),
		    candidate_payload = $6,
		    confirmed_by = $7,
		    confirmed_at = $8,
		    updated_by = $7
		WHERE candidate_id = $9
		  AND user_id = $7
		  AND candidate_type = 'knowledge_point'
		  AND status = 'pending'`,
		status, outcome, params.TrustLevel, point.KnowledgePointID, params.DecisionNote,
		encoded, params.UserID, params.ResolvedAt, params.CandidateID)
	if err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, fmt.Errorf("确认知识点候选失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ContentCandidate{}, service.KnowledgePoint{},
			r.explainMissingPending(ctx, params.UserID, params.CandidateID)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, fmt.Errorf("提交候选确认事务失败: %w", err)
	}

	candidate, err := r.GetCandidate(ctx, params.UserID, params.CandidateID)
	if err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, err
	}
	return candidate, point, nil
}

func insertKnowledgePointWithRetry(ctx context.Context, tx pgx.Tx, params service.ConfirmKnowledgePointParams) (service.KnowledgePoint, error) {
	for range maxCandidateIDAttempts {
		knowledgePointID, err := service.NewKnowledgePointID()
		if err != nil {
			return service.KnowledgePoint{}, err
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return service.KnowledgePoint{}, fmt.Errorf("开启知识点保存点失败: %w", err)
		}
		point, err := scanKnowledgePoint(savepoint.QueryRow(ctx, `
			INSERT INTO knowledge_points (
				knowledge_point_id, user_id, title, description, status, created_by, updated_by
			)
			VALUES ($1, $2, $3, NULLIF($4, ''), 'active', $2, $2)
			RETURNING knowledge_point_id::text, user_id, title, COALESCE(description, ''),
			          status, created_at, updated_at`,
			knowledgePointID, params.UserID, params.Title, params.Description))
		if err != nil {
			_ = savepoint.Rollback(ctx)
			if isUniqueViolation(err) {
				continue
			}
			return service.KnowledgePoint{}, fmt.Errorf("创建知识点失败: %w", err)
		}
		if err := savepoint.Commit(ctx); err != nil {
			return service.KnowledgePoint{}, fmt.Errorf("提交知识点保存点失败: %w", err)
		}
		return point, nil
	}
	return service.KnowledgePoint{}, errors.New("连续多次 knowledge_point_id 唯一冲突，放弃写入")
}

func scanKnowledgePoint(row rowScanner) (service.KnowledgePoint, error) {
	var point service.KnowledgePoint
	err := row.Scan(&point.KnowledgePointID, &point.UserID, &point.Title,
		&point.Description, &point.Status, &point.CreatedAt, &point.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.KnowledgePoint{}, service.ErrCandidateNotFound
	}
	if err != nil {
		return service.KnowledgePoint{}, fmt.Errorf("扫描知识点失败: %w", err)
	}
	return point, nil
}

// explainMissingPending 区分「候选不存在/不属于我」与「候选已被处理过」。
// 两者的用户处置完全不同：前者是找错了，后者是别处已经做过决定。
func (r *PostgresCandidateRepository) explainMissingPending(ctx context.Context, userID, candidateID string) error {
	var status string
	err := r.pool.QueryRow(ctx, `
		SELECT status
		FROM content_candidates
		WHERE candidate_id = $1
		  AND user_id = $2`, candidateID, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.ErrCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("查询候选状态失败: %w", err)
	}
	if status != service.CandidateStatusPending {
		return service.ErrCandidateResolved
	}
	return service.ErrCandidateNotFound
}
