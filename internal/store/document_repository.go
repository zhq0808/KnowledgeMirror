package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/service"
)

// maxDocumentIDAttempts 限制 UUIDv7 主键冲突时的有限重试次数。
const maxDocumentIDAttempts = 5

// PostgresDocumentRepository 持久化资料、版本、用途与来源片段。
//
// 三条不可协商的约束在这一层兜底：
//   - 每条 SQL 都带 user_id，跨用户读写不可能命中；
//   - document_versions 追加不覆盖（数据库触发器同样禁止 UPDATE/DELETE）；
//   - source_chunks 的血缘字段不可改，只允许改检索开关与可信级别。
type PostgresDocumentRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresDocumentRepository(pool *pgxpool.Pool) *PostgresDocumentRepository {
	return &PostgresDocumentRepository{pool: pool}
}

// ---------------------------------------------------------------------------
// 上传
// ---------------------------------------------------------------------------

func (r *PostgresDocumentRepository) FindUploadRequest(ctx context.Context, userID, idempotencyKey string) (service.UploadRequestRecord, bool, error) {
	var record service.UploadRequestRecord
	err := r.pool.QueryRow(ctx, `
		SELECT document_id::text, version_id::text, request_hash
		FROM document_upload_requests
		WHERE user_id = $1
		  AND idempotency_key = $2`, userID, idempotencyKey).
		Scan(&record.DocumentID, &record.VersionID, &record.RequestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.UploadRequestRecord{}, false, nil
	}
	if err != nil {
		return service.UploadRequestRecord{}, false, fmt.Errorf("查询上传幂等记录失败: %w", err)
	}
	return record, true, nil
}

func (r *PostgresDocumentRepository) FindVersionByContentHash(ctx context.Context, userID string, sha256 []byte) (string, bool, error) {
	var versionID string
	err := r.pool.QueryRow(ctx, `
		SELECT v.version_id::text
		FROM document_versions AS v
		JOIN documents AS d ON d.document_id = v.document_id AND d.user_id = v.user_id
		WHERE v.user_id = $1
		  AND v.sha256 = $2
		  AND d.deleted_at IS NULL
		ORDER BY v.created_at DESC
		LIMIT 1`, userID, sha256).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("查询重复内容失败: %w", err)
	}
	return versionID, true, nil
}

// CreateDocumentVersion 在一个事务里创建（或复用）资料主记录、追加版本并写入幂等记录。
func (r *PostgresDocumentRepository) CreateDocumentVersion(ctx context.Context, params service.CreateDocumentVersionParams) (service.Document, service.DocumentVersion, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.Document{}, service.DocumentVersion{}, fmt.Errorf("开启资料上传事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	documentID := params.DocumentID
	if documentID == "" {
		documentID, err = insertDocumentWithRetry(ctx, tx, params)
		if err != nil {
			return service.Document{}, service.DocumentVersion{}, err
		}
	} else {
		// 锁住资料行，串行化同一份资料的版本号分配。
		var locked string
		err = tx.QueryRow(ctx, `
			SELECT document_id::text
			FROM documents
			WHERE document_id = $1
			  AND user_id = $2
			  AND deleted_at IS NULL
			FOR UPDATE`, documentID, params.UserID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return service.Document{}, service.DocumentVersion{}, service.ErrDocumentNotFound
		}
		if err != nil {
			return service.Document{}, service.DocumentVersion{}, fmt.Errorf("锁定资料失败: %w", err)
		}
	}

	var nextVersionNo int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_no), 0) + 1
		FROM document_versions
		WHERE document_id = $1
		  AND user_id = $2`, documentID, params.UserID).Scan(&nextVersionNo); err != nil {
		return service.Document{}, service.DocumentVersion{}, fmt.Errorf("计算资料版本号失败: %w", err)
	}

	version, err := insertVersionWithRetry(ctx, tx, documentID, nextVersionNo, params)
	if err != nil {
		return service.Document{}, service.DocumentVersion{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE documents
		SET current_version_id = $1,
		    status = 'parsing',
		    parse_error = NULL,
		    parsed_at = NULL,
		    updated_by = $2
		WHERE document_id = $3
		  AND user_id = $2`, version.VersionID, params.UserID, documentID); err != nil {
		return service.Document{}, service.DocumentVersion{}, fmt.Errorf("更新资料当前版本失败: %w", err)
	}

	if params.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO document_upload_requests (user_id, idempotency_key, request_hash, document_id, version_id)
			VALUES ($1, $2, $3, $4, $5)`,
			params.UserID, params.IdempotencyKey, params.SHA256, documentID, version.VersionID); err != nil {
			if isUniqueViolation(err) {
				// 并发重放：另一个请求已经用同一个幂等键落库，本次整体回滚。
				return service.Document{}, service.DocumentVersion{}, service.ErrDocumentIdempotencyConflict
			}
			return service.Document{}, service.DocumentVersion{}, fmt.Errorf("写入上传幂等记录失败: %w", err)
		}
	}

	document, err := scanDocument(tx.QueryRow(ctx, documentSelectSQL+`
		WHERE d.document_id = $1
		  AND d.user_id = $2`, documentID, params.UserID))
	if err != nil {
		return service.Document{}, service.DocumentVersion{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return service.Document{}, service.DocumentVersion{}, fmt.Errorf("提交资料上传事务失败: %w", err)
	}
	return document, version, nil
}

// insertDocumentWithRetry 生成 UUIDv7 主键并在唯一冲突时用保存点有限重试。
func insertDocumentWithRetry(ctx context.Context, tx pgx.Tx, params service.CreateDocumentVersionParams) (string, error) {
	for range maxDocumentIDAttempts {
		documentID, err := service.NewDocumentID()
		if err != nil {
			return "", err
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return "", fmt.Errorf("开启资料保存点失败: %w", err)
		}
		// 来源默认“待确认”、类别默认“其他”、用途为空：不由文件名擅自判断。
		_, err = savepoint.Exec(ctx, `
			INSERT INTO documents (
				document_id, user_id, title, content_origin, document_kind, status,
				created_by, updated_by
			)
			VALUES ($1, $2, $3, 'pending_confirmation', 'other', 'parsing', $2, $2)`,
			documentID, params.UserID, params.Title)
		if err != nil {
			_ = savepoint.Rollback(ctx)
			if isUniqueViolation(err) {
				continue
			}
			return "", fmt.Errorf("写入资料失败: %w", err)
		}
		if err := savepoint.Commit(ctx); err != nil {
			return "", fmt.Errorf("提交资料保存点失败: %w", err)
		}
		return documentID, nil
	}
	return "", errors.New("连续多次 document_id 唯一冲突，放弃写入")
}

func insertVersionWithRetry(ctx context.Context, tx pgx.Tx, documentID string, versionNo int, params service.CreateDocumentVersionParams) (service.DocumentVersion, error) {
	for range maxDocumentIDAttempts {
		versionID, err := service.NewDocumentVersionID()
		if err != nil {
			return service.DocumentVersion{}, err
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return service.DocumentVersion{}, fmt.Errorf("开启资料版本保存点失败: %w", err)
		}
		version, err := scanVersion(savepoint.QueryRow(ctx, `
			INSERT INTO document_versions (
				version_id, document_id, user_id, version_no, original_filename,
				mime_type, size_bytes, sha256, raw_text, parser_version,
				created_by, updated_by
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $3, $3)
			RETURNING version_id::text, document_id::text, version_no, original_filename,
			          mime_type, size_bytes, sha256, parser_version, created_at`,
			versionID, documentID, params.UserID, versionNo, params.OriginalFilename,
			params.MIMEType, params.SizeBytes, params.SHA256, params.RawText, params.ParserVersion))
		if err != nil {
			_ = savepoint.Rollback(ctx)
			if isVersionIDConflict(err) {
				continue
			}
			return service.DocumentVersion{}, fmt.Errorf("写入资料版本失败: %w", err)
		}
		if err := savepoint.Commit(ctx); err != nil {
			return service.DocumentVersion{}, fmt.Errorf("提交资料版本保存点失败: %w", err)
		}
		return version, nil
	}
	return service.DocumentVersion{}, errors.New("连续多次 version_id 唯一冲突，放弃写入")
}

// isVersionIDConflict 只把 version_id 主键/归属唯一冲突当成可重试碰撞，
// 版本号冲突（并发追加）不在此列，必须整体失败重来。
func isVersionIDConflict(err error) bool {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23505" {
		return false
	}
	return pgError.ConstraintName == "document_versions_pkey" ||
		pgError.ConstraintName == "uk_document_versions_owner"
}

// ---------------------------------------------------------------------------
// 解析结果
// ---------------------------------------------------------------------------

// SaveSourceChunks 在一个事务里写入全部来源片段并推进资料状态。
// 片段是全有或全无：source_chunks 被触发器禁止删除，不能留下半截解析结果。
func (r *PostgresDocumentRepository) SaveSourceChunks(ctx context.Context, params service.SaveSourceChunksParams) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启来源片段事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existing int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM source_chunks
		WHERE document_version_id = $1
		  AND user_id = $2`, params.VersionID, params.UserID).Scan(&existing); err != nil {
		return fmt.Errorf("统计已有来源片段失败: %w", err)
	}

	if existing == 0 {
		for _, chunk := range params.Chunks {
			chunkID, err := service.NewSourceChunkID()
			if err != nil {
				return err
			}
			contentHash := service.SourceChunkContentHash(chunk.Content)
			if _, err := tx.Exec(ctx, `
				INSERT INTO source_chunks (
					source_chunk_id, document_version_id, document_id, user_id, ordinal,
					heading_path, content, start_offset, end_offset, content_hash,
					trust_level, retrieval_enabled, created_by, updated_by
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'unverified', $11, $4, $4)`,
				chunkID, params.VersionID, params.DocumentID, params.UserID, chunk.Ordinal,
				chunk.HeadingPath, chunk.Content, chunk.StartOffset, chunk.EndOffset,
				contentHash[:], params.RetrievalEnabled); err != nil {
				return fmt.Errorf("写入来源片段失败: %w", err)
			}
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE documents
		SET status = $1,
		    parse_error = NULL,
		    parsed_at = $2,
		    updated_by = $3
		WHERE document_id = $4
		  AND user_id = $3
		  AND deleted_at IS NULL`,
		params.Status, params.ParsedAt, params.UserID, params.DocumentID); err != nil {
		return fmt.Errorf("更新资料解析状态失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交来源片段事务失败: %w", err)
	}
	return nil
}

func (r *PostgresDocumentRepository) MarkParseFailed(ctx context.Context, userID, documentID, message string) error {
	if message == "" {
		message = "解析失败"
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'failed',
		    parse_error = $1,
		    updated_by = $2
		WHERE document_id = $3
		  AND user_id = $2
		  AND deleted_at IS NULL`, message, userID, documentID); err != nil {
		return fmt.Errorf("标记资料解析失败状态失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

const documentSelectSQL = `
	SELECT d.document_id::text, d.user_id, d.title, d.content_origin, d.document_kind,
	       d.status, COALESCE(d.current_version_id::text, ''), COALESCE(d.parse_error, ''),
	       d.parsed_at, d.created_at, d.updated_at
	FROM documents AS d`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDocument(row rowScanner) (service.Document, error) {
	var document service.Document
	err := row.Scan(
		&document.DocumentID, &document.UserID, &document.Title,
		&document.ContentOrigin, &document.DocumentKind, &document.Status,
		&document.CurrentVersionID, &document.ParseError, &document.ParsedAt,
		&document.CreatedAt, &document.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.Document{}, service.ErrDocumentNotFound
	}
	if err != nil {
		return service.Document{}, fmt.Errorf("扫描资料失败: %w", err)
	}
	return document, nil
}

func scanVersion(row rowScanner) (service.DocumentVersion, error) {
	var version service.DocumentVersion
	var digest []byte
	err := row.Scan(
		&version.VersionID, &version.DocumentID, &version.VersionNo,
		&version.OriginalFilename, &version.MIMEType, &version.SizeBytes,
		&digest, &version.ParserVersion, &version.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.DocumentVersion{}, service.ErrDocumentNotFound
	}
	if err != nil {
		return service.DocumentVersion{}, fmt.Errorf("扫描资料版本失败: %w", err)
	}
	version.SHA256Hex = hex.EncodeToString(digest)
	return version, nil
}

const versionSelectSQL = `
	SELECT version_id::text, document_id::text, version_no, original_filename,
	       mime_type, size_bytes, sha256, parser_version, created_at
	FROM document_versions`

func (r *PostgresDocumentRepository) GetDocumentDetail(ctx context.Context, userID, documentID string) (service.DocumentDetail, error) {
	document, err := scanDocument(r.pool.QueryRow(ctx, documentSelectSQL+`
		WHERE d.document_id = $1
		  AND d.user_id = $2
		  AND d.deleted_at IS NULL`, documentID, userID))
	if err != nil {
		return service.DocumentDetail{}, err
	}
	if document.CurrentVersionID == "" {
		return service.DocumentDetail{Document: document}, nil
	}

	version, err := scanVersion(r.pool.QueryRow(ctx, versionSelectSQL+`
		WHERE version_id = $1
		  AND user_id = $2`, document.CurrentVersionID, userID))
	if err != nil {
		return service.DocumentDetail{}, err
	}
	usages, err := r.listUsages(ctx, userID, document.CurrentVersionID)
	if err != nil {
		return service.DocumentDetail{}, err
	}
	chunkCount, err := r.countChunks(ctx, userID, document.CurrentVersionID)
	if err != nil {
		return service.DocumentDetail{}, err
	}
	return service.DocumentDetail{
		Document:   document,
		Version:    version,
		Usages:     usages,
		ChunkCount: chunkCount,
	}, nil
}

func (r *PostgresDocumentRepository) GetVersionRawText(ctx context.Context, userID, documentID, versionID string) (service.DocumentVersion, string, error) {
	if versionID == "" {
		return service.DocumentVersion{}, "", service.ErrDocumentNotFound
	}
	var version service.DocumentVersion
	var digest []byte
	var rawText string
	err := r.pool.QueryRow(ctx, `
		SELECT v.version_id::text, v.document_id::text, v.version_no, v.original_filename,
		       v.mime_type, v.size_bytes, v.sha256, v.parser_version, v.created_at, v.raw_text
		FROM document_versions AS v
		JOIN documents AS d ON d.document_id = v.document_id AND d.user_id = v.user_id
		WHERE v.version_id = $1
		  AND v.document_id = $2
		  AND v.user_id = $3
		  AND d.deleted_at IS NULL`, versionID, documentID, userID).
		Scan(&version.VersionID, &version.DocumentID, &version.VersionNo,
			&version.OriginalFilename, &version.MIMEType, &version.SizeBytes,
			&digest, &version.ParserVersion, &version.CreatedAt, &rawText)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.DocumentVersion{}, "", service.ErrDocumentNotFound
	}
	if err != nil {
		return service.DocumentVersion{}, "", fmt.Errorf("查询资料原文失败: %w", err)
	}
	version.SHA256Hex = hex.EncodeToString(digest)
	return version, rawText, nil
}

func (r *PostgresDocumentRepository) ListDocuments(ctx context.Context, userID string, limit int) ([]service.DocumentListItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.document_id::text, d.user_id, d.title, d.content_origin, d.document_kind,
		       d.status, COALESCE(d.current_version_id::text, ''), COALESCE(d.parse_error, ''),
		       d.parsed_at, d.created_at, d.updated_at,
		       v.version_id::text, v.document_id::text, v.version_no, v.original_filename,
		       v.mime_type, v.size_bytes, v.sha256, v.parser_version, v.created_at,
		       COALESCE((
		           SELECT json_agg(json_build_object(
		               'purpose', u.purpose,
		               'enabled', u.enabled,
		               'confirmed_at', u.confirmed_at
		           ) ORDER BY u.purpose)::text
		           FROM document_usages AS u
		           WHERE u.document_version_id = d.current_version_id
		             AND u.user_id = d.user_id
		             AND u.enabled
		       ), '[]'),
		       (SELECT COUNT(*)
		        FROM source_chunks AS c
		        WHERE c.document_version_id = d.current_version_id
		          AND c.user_id = d.user_id)
		FROM documents AS d
		JOIN document_versions AS v
		  ON v.version_id = d.current_version_id
		 AND v.document_id = d.document_id
		 AND v.user_id = d.user_id
		WHERE d.user_id = $1
		  AND d.deleted_at IS NULL
		ORDER BY d.updated_at DESC, d.document_id DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询资料列表失败: %w", err)
	}
	defer rows.Close()
	items := make([]service.DocumentListItem, 0, limit)
	for rows.Next() {
		var item service.DocumentListItem
		var digest []byte
		var usagesJSON string
		if err := rows.Scan(
			&item.Document.DocumentID, &item.Document.UserID, &item.Document.Title,
			&item.Document.ContentOrigin, &item.Document.DocumentKind, &item.Document.Status,
			&item.Document.CurrentVersionID, &item.Document.ParseError, &item.Document.ParsedAt,
			&item.Document.CreatedAt, &item.Document.UpdatedAt,
			&item.Version.VersionID, &item.Version.DocumentID, &item.Version.VersionNo,
			&item.Version.OriginalFilename, &item.Version.MIMEType, &item.Version.SizeBytes,
			&digest, &item.Version.ParserVersion, &item.Version.CreatedAt,
			&usagesJSON, &item.ChunkCount,
		); err != nil {
			return nil, fmt.Errorf("扫描资料列表失败: %w", err)
		}
		item.Version.SHA256Hex = hex.EncodeToString(digest)
		var usages []struct {
			Purpose     string     `json:"purpose"`
			Enabled     bool       `json:"enabled"`
			ConfirmedAt *time.Time `json:"confirmed_at"`
		}
		if err := json.Unmarshal([]byte(usagesJSON), &usages); err != nil {
			return nil, fmt.Errorf("解析资料用途失败: %w", err)
		}
		item.Usages = make([]service.DocumentUsage, 0, len(usages))
		for _, usage := range usages {
			item.Usages = append(item.Usages, service.DocumentUsage{
				Purpose: usage.Purpose, Enabled: usage.Enabled, ConfirmedAt: usage.ConfirmedAt,
			})
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资料列表失败: %w", err)
	}
	return items, nil
}

func (r *PostgresDocumentRepository) ListVersions(ctx context.Context, userID, documentID string) ([]service.DocumentVersion, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM documents
			WHERE document_id = $1 AND user_id = $2 AND deleted_at IS NULL
		)`, documentID, userID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("查询资料归属失败: %w", err)
	}
	if !exists {
		return nil, service.ErrDocumentNotFound
	}

	rows, err := r.pool.Query(ctx, versionSelectSQL+`
		WHERE document_id = $1
		  AND user_id = $2
		ORDER BY version_no DESC`, documentID, userID)
	if err != nil {
		return nil, fmt.Errorf("查询资料版本列表失败: %w", err)
	}
	defer rows.Close()

	versions := make([]service.DocumentVersion, 0, 4)
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资料版本列表失败: %w", err)
	}
	return versions, nil
}

func (r *PostgresDocumentRepository) ListSourceChunks(ctx context.Context, userID, documentID, versionID string) ([]service.SourceChunk, error) {
	if versionID == "" {
		return nil, service.ErrDocumentNotFound
	}
	rows, err := r.pool.Query(ctx, `
		SELECT c.source_chunk_id::text, c.document_id::text, c.document_version_id::text,
		       c.ordinal, c.heading_path, c.content, c.start_offset, c.end_offset,
		       c.trust_level, c.retrieval_enabled
		FROM source_chunks AS c
		JOIN documents AS d ON d.document_id = c.document_id AND d.user_id = c.user_id
		WHERE c.document_version_id = $1
		  AND c.document_id = $2
		  AND c.user_id = $3
		  AND d.deleted_at IS NULL
		ORDER BY c.ordinal`, versionID, documentID, userID)
	if err != nil {
		return nil, fmt.Errorf("查询来源片段失败: %w", err)
	}
	defer rows.Close()

	chunks := make([]service.SourceChunk, 0, 32)
	for rows.Next() {
		var chunk service.SourceChunk
		if err := rows.Scan(
			&chunk.SourceChunkID, &chunk.DocumentID, &chunk.VersionID,
			&chunk.Ordinal, &chunk.HeadingPath, &chunk.Content,
			&chunk.StartOffset, &chunk.EndOffset,
			&chunk.TrustLevel, &chunk.RetrievalEnabled,
		); err != nil {
			return nil, fmt.Errorf("扫描来源片段失败: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历来源片段失败: %w", err)
	}
	return chunks, nil
}

func (r *PostgresDocumentRepository) listUsages(ctx context.Context, userID, versionID string) ([]service.DocumentUsage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT purpose, enabled, confirmed_at
		FROM document_usages
		WHERE document_version_id = $1
		  AND user_id = $2
		ORDER BY purpose`, versionID, userID)
	if err != nil {
		return nil, fmt.Errorf("查询资料用途失败: %w", err)
	}
	defer rows.Close()

	usages := make([]service.DocumentUsage, 0, 5)
	for rows.Next() {
		var usage service.DocumentUsage
		if err := rows.Scan(&usage.Purpose, &usage.Enabled, &usage.ConfirmedAt); err != nil {
			return nil, fmt.Errorf("扫描资料用途失败: %w", err)
		}
		usages = append(usages, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资料用途失败: %w", err)
	}
	return usages, nil
}

func (r *PostgresDocumentRepository) countChunks(ctx context.Context, userID, versionID string) (int, error) {
	var count int
	if err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM source_chunks
		WHERE document_version_id = $1
		  AND user_id = $2`, versionID, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计来源片段失败: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// 修改
// ---------------------------------------------------------------------------

func (r *PostgresDocumentRepository) UpdateDocumentMetadata(ctx context.Context, params service.UpdateDocumentMetadataParams) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启资料元数据事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockCurrentDocumentVersion(ctx, tx, params.UserID, params.DocumentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE documents
		SET title = COALESCE($1, title),
		    content_origin = COALESCE($2, content_origin),
		    document_kind = COALESCE($3, document_kind),
		    updated_by = $4
		WHERE document_id = $5
		  AND user_id = $4
		  AND deleted_at IS NULL`,
		params.Title, params.ContentOrigin, params.DocumentKind, params.UserID, params.DocumentID); err != nil {
		return fmt.Errorf("更新资料元数据失败: %w", err)
	}
	if err := recomputeDocumentStatus(ctx, tx, params.UserID, params.DocumentID, true); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交资料元数据事务失败: %w", err)
	}
	return nil
}

// ReplaceDocumentUsages 用提交的用途集合整体覆盖当前版本的用途。
//
// 顺序很重要：先关闭不在集合里的用途，再开启集合内的用途，
// 这样数据库里的 archive_only 互斥触发器不会在中间态误报。
// ai_retrieval 的开关同时联动整份版本的片段检索开关：
// 用户撤回资料级授权后，片段必须立刻不可召回。
func (r *PostgresDocumentRepository) ReplaceDocumentUsages(ctx context.Context, params service.ReplaceDocumentUsagesParams) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启资料用途事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	versionID, err := lockCurrentDocumentVersion(ctx, tx, params.UserID, params.DocumentID)
	if err != nil {
		return err
	}
	var chunkCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM source_chunks
		WHERE document_version_id = $1
		  AND user_id = $2`, versionID, params.UserID).Scan(&chunkCount); err != nil {
		return fmt.Errorf("统计当前版本来源片段失败: %w", err)
	}
	if chunkCount == 0 {
		return &service.DocumentInputError{Message: "资料尚未解析成功，无法确认用途"}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE document_usages
		SET enabled = FALSE,
		    confirmed_by = $1,
		    confirmed_at = $2,
		    updated_by = $1
		WHERE document_version_id = $3
		  AND user_id = $1
		  AND NOT (purpose = ANY($4::text[]))`,
		params.UserID, params.ConfirmedAt, versionID, params.Purposes); err != nil {
		return fmt.Errorf("关闭资料用途失败: %w", err)
	}

	for _, purpose := range params.Purposes {
		usageID, err := service.NewDocumentUsageID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO document_usages (
				usage_id, document_version_id, document_id, user_id, purpose, enabled,
				confirmed_by, confirmed_at, created_by, updated_by
			)
			VALUES ($1, $2, $3, $4, $5, TRUE, $4, $6, $4, $4)
			ON CONFLICT (document_version_id, purpose) DO UPDATE
			SET enabled = TRUE,
			    confirmed_by = EXCLUDED.confirmed_by,
			    confirmed_at = EXCLUDED.confirmed_at,
			    updated_by = EXCLUDED.updated_by`,
			usageID, versionID, params.DocumentID, params.UserID, purpose, params.ConfirmedAt); err != nil {
			return fmt.Errorf("确认资料用途失败: %w", err)
		}
	}

	retrievalEnabled := false
	for _, purpose := range params.Purposes {
		if purpose == service.DocumentPurposeAIRetrieval {
			retrievalEnabled = true
			break
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE source_chunks
		SET retrieval_enabled = $1,
		    updated_by = $2
		WHERE document_version_id = $3
		  AND user_id = $2
		  AND retrieval_enabled <> $1`,
		retrievalEnabled, params.UserID, versionID); err != nil {
		return fmt.Errorf("同步来源片段检索开关失败: %w", err)
	}

	if err := recomputeDocumentStatus(ctx, tx, params.UserID, params.DocumentID, false); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交资料用途事务失败: %w", err)
	}
	return nil
}

// lockCurrentDocumentVersion 串行化同一资料的版本、元数据和用途变更。
func lockCurrentDocumentVersion(ctx context.Context, tx pgx.Tx, userID, documentID string) (string, error) {
	var versionID string
	err := tx.QueryRow(ctx, `
		SELECT current_version_id::text
		FROM documents
		WHERE document_id = $1
		  AND user_id = $2
		  AND deleted_at IS NULL
		FOR UPDATE`, documentID, userID).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", service.ErrDocumentNotFound
	}
	if err != nil {
		return "", fmt.Errorf("锁定资料失败: %w", err)
	}
	return versionID, nil
}

// recomputeDocumentStatus 只从当前版本的数据库事实推导状态。
// 元数据编辑不应把解析失败资料改回 parsing，重试解析仍是 failed 的唯一出口。
func recomputeDocumentStatus(ctx context.Context, tx pgx.Tx, userID, documentID string, preserveFailed bool) error {
	command, err := tx.Exec(ctx, `
		UPDATE documents AS d
		SET status = CASE
		        WHEN $3 AND d.status = 'failed' THEN 'failed'
		        WHEN NOT EXISTS (
		            SELECT 1 FROM source_chunks AS c
		            WHERE c.document_version_id = d.current_version_id AND c.user_id = d.user_id
		        ) THEN 'parsing'
		        WHEN EXISTS (
		            SELECT 1 FROM document_usages AS u
		            WHERE u.document_version_id = d.current_version_id
		              AND u.user_id = d.user_id
		              AND u.purpose = 'archive_only' AND u.enabled
		        ) THEN 'archived'
		        WHEN d.content_origin = 'pending_confirmation' OR NOT EXISTS (
		            SELECT 1 FROM document_usages AS u
		            WHERE u.document_version_id = d.current_version_id
		              AND u.user_id = d.user_id AND u.enabled
		        ) THEN 'pending_confirmation'
		        ELSE 'ready'
		    END,
		    updated_by = $1
		WHERE d.document_id = $2
		  AND d.user_id = $1
		  AND d.deleted_at IS NULL`, userID, documentID, preserveFailed)
	if err != nil {
		return fmt.Errorf("重算资料状态失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ErrDocumentNotFound
	}
	return nil
}

func (r *PostgresDocumentRepository) SetSourceChunkRetrieval(ctx context.Context, userID, documentID, chunkID string, enabled bool) (service.SourceChunk, error) {
	var chunk service.SourceChunk
	err := r.pool.QueryRow(ctx, `
		UPDATE source_chunks
		SET retrieval_enabled = $1,
		    updated_by = $2
		WHERE source_chunk_id = $3
		  AND document_id = $4
		  AND user_id = $2
		RETURNING source_chunk_id::text, document_id::text, document_version_id::text,
		          ordinal, heading_path, content, start_offset, end_offset,
		          trust_level, retrieval_enabled`, enabled, userID, chunkID, documentID).
		Scan(&chunk.SourceChunkID, &chunk.DocumentID, &chunk.VersionID,
			&chunk.Ordinal, &chunk.HeadingPath, &chunk.Content,
			&chunk.StartOffset, &chunk.EndOffset,
			&chunk.TrustLevel, &chunk.RetrievalEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.SourceChunk{}, service.ErrDocumentNotFound
	}
	if err != nil {
		return service.SourceChunk{}, fmt.Errorf("更新来源片段检索开关失败: %w", err)
	}
	return chunk, nil
}

// SoftDeleteDocument 只标记删除并关闭检索，不物理删除版本与来源片段：
// 血缘数据要留给已有引用追溯，但删除后必须立刻无法被 AI 召回。
func (r *PostgresDocumentRepository) SoftDeleteDocument(ctx context.Context, userID, documentID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启资料删除事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	command, err := tx.Exec(ctx, `
		UPDATE documents
		SET deleted_at = $1,
		    updated_by = $2
		WHERE document_id = $3
		  AND user_id = $2
		  AND deleted_at IS NULL`, time.Now(), userID, documentID)
	if err != nil {
		return fmt.Errorf("删除资料失败: %w", err)
	}
	if command.RowsAffected() == 0 {
		return service.ErrDocumentNotFound
	}

	if _, err := tx.Exec(ctx, `
		UPDATE document_usages
		SET enabled = FALSE,
		    updated_by = $1
		WHERE document_id = $2
		  AND user_id = $1
		  AND enabled`, userID, documentID); err != nil {
		return fmt.Errorf("关闭已删除资料的用途失败: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE source_chunks
		SET retrieval_enabled = FALSE,
		    updated_by = $1
		WHERE document_id = $2
		  AND user_id = $1
		  AND retrieval_enabled`, userID, documentID); err != nil {
		return fmt.Errorf("关闭已删除资料的检索开关失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交资料删除事务失败: %w", err)
	}
	return nil
}
