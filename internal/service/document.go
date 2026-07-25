package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 资料的三个独立维度：内容来源 / 内容类别 / 使用用途。
// 它们互不推导，也都不推导掌握状态：上传、解析、确认用途都不产生掌握证据。
// ---------------------------------------------------------------------------

// 内容来源：判断作者与可信边界。默认 pending_confirmation，不由文件名擅自判断。
const (
	ContentOriginUserAuthored = "user_authored"
	ContentOriginAIGenerated  = "ai_generated"
	ContentOriginExternal     = "external"
	ContentOriginPending      = "pending_confirmation"
)

// 内容类别：决定后续用什么候选抽取策略。默认 other。
const (
	DocumentKindLearningNote      = "learning_note"
	DocumentKindLearningTodo      = "learning_todo"
	DocumentKindTechnicalMaterial = "technical_material"
	DocumentKindTargetJD          = "target_jd"
	DocumentKindProjectFact       = "project_fact"
	DocumentKindInterviewReview   = "interview_review"
	DocumentKindOther             = "other"
)

// 使用用途：允许多选，但 archive_only 与其余用途互斥。默认为空（未确认任何用途）。
const (
	DocumentPurposeLearn         = "learn"
	DocumentPurposeAIRetrieval   = "ai_retrieval"
	DocumentPurposeGeneratePlan  = "generate_plan"
	DocumentPurposeFactReference = "fact_reference"
	DocumentPurposeArchiveOnly   = "archive_only"
)

// 资料生命周期状态。
const (
	DocumentStatusParsing             = "parsing"
	DocumentStatusPendingConfirmation = "pending_confirmation"
	DocumentStatusReady               = "ready"
	DocumentStatusFailed              = "failed"
	DocumentStatusArchived            = "archived"
)

// 来源片段可信级别。解析和 AI 整理都不会自动提升它。
const (
	SourceChunkTrustUnverified    = "unverified"
	SourceChunkTrustUserConfirmed = "user_confirmed"
	SourceChunkTrustTrusted       = "trusted"
)

var (
	validContentOrigins = []string{
		ContentOriginUserAuthored, ContentOriginAIGenerated,
		ContentOriginExternal, ContentOriginPending,
	}
	validDocumentKinds = []string{
		DocumentKindLearningNote, DocumentKindLearningTodo, DocumentKindTechnicalMaterial,
		DocumentKindTargetJD, DocumentKindProjectFact, DocumentKindInterviewReview,
		DocumentKindOther,
	}
	validDocumentPurposes = []string{
		DocumentPurposeLearn, DocumentPurposeAIRetrieval, DocumentPurposeGeneratePlan,
		DocumentPurposeFactReference, DocumentPurposeArchiveOnly,
	}
)

// ContentOrigins / DocumentKinds / DocumentPurposes 供接口层枚举与校验复用。
func ContentOrigins() []string   { return slices.Clone(validContentOrigins) }
func DocumentKinds() []string    { return slices.Clone(validDocumentKinds) }
func DocumentPurposes() []string { return slices.Clone(validDocumentPurposes) }

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

var (
	// ErrDocumentNotFound 表示资料不存在、已删除或不属于当前用户；对外一律 404，不区分。
	ErrDocumentNotFound = errors.New("资料不存在")
	// ErrDocumentIdempotencyConflict 表示同一幂等键被用于内容不同的上传请求。
	ErrDocumentIdempotencyConflict = errors.New("幂等键已用于其它上传内容")
	// ErrDocumentRetrievalNotEnabled 表示资料级还没有开启“供 AI 检索”，不能先做片段级开关。
	ErrDocumentRetrievalNotEnabled = errors.New("资料尚未开启供 AI 检索用途")
	// ErrDocumentParseSucceeded 表示当前版本已解析成功，无需重试。
	ErrDocumentParseSucceeded = errors.New("当前版本已解析成功")
)

// DocumentInputError 是可以安全回显给用户的输入/预算类错误，接口层映射为 400。
type DocumentInputError struct{ Message string }

func (e *DocumentInputError) Error() string { return e.Message }

func invalidDocumentInput(format string, args ...any) error {
	return &DocumentInputError{Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// 领域模型
// ---------------------------------------------------------------------------

type Document struct {
	DocumentID       string
	UserID           string
	Title            string
	ContentOrigin    string
	DocumentKind     string
	Status           string
	CurrentVersionID string
	ParseError       string
	ParsedAt         *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type DocumentVersion struct {
	VersionID        string
	DocumentID       string
	VersionNo        int
	OriginalFilename string
	MIMEType         string
	SizeBytes        int64
	SHA256Hex        string
	ParserVersion    string
	CreatedAt        time.Time
}

type DocumentUsage struct {
	Purpose     string
	Enabled     bool
	ConfirmedAt *time.Time
}

type SourceChunk struct {
	SourceChunkID    string
	DocumentID       string
	VersionID        string
	Ordinal          int
	HeadingPath      []string
	Content          string
	StartOffset      int64
	EndOffset        int64
	TrustLevel       string
	RetrievalEnabled bool
}

// DocumentDetail 是资料详情：主记录 + 当前版本 + 用途 + 片段计数。
type DocumentDetail struct {
	Document   Document
	Version    DocumentVersion
	Usages     []DocumentUsage
	ChunkCount int
}

// DocumentListItem 是资料列表项，不包含原文。
type DocumentListItem struct {
	Document   Document
	Version    DocumentVersion
	Usages     []DocumentUsage
	ChunkCount int
}

// UploadDocumentResult 描述一次上传动作的结果。
// DuplicateOfVersionID 只是“同一用户已有相同内容”的提示，不做静默合并。
type UploadDocumentResult struct {
	Detail               DocumentDetail
	IdempotentHit        bool
	DuplicateOfVersionID string
}

// ---------------------------------------------------------------------------
// 仓储契约
// ---------------------------------------------------------------------------

type CreateDocumentVersionParams struct {
	UserID           string
	DocumentID       string // 为空表示新建资料；非空表示给已有资料追加版本
	Title            string
	OriginalFilename string
	MIMEType         string
	SizeBytes        int64
	SHA256           []byte
	RawText          string
	ParserVersion    string
	IdempotencyKey   string
}

type SaveSourceChunksParams struct {
	UserID     string
	DocumentID string
	VersionID  string
	Chunks     []MarkdownChunk
	// RetrievalEnabled 跟随当前版本的 ai_retrieval 用途；新版本默认关闭。
	RetrievalEnabled bool
	Status           string
	ParsedAt         time.Time
}

type UpdateDocumentMetadataParams struct {
	UserID        string
	DocumentID    string
	Title         *string
	ContentOrigin *string
	DocumentKind  *string
	Status        string
}

type ReplaceDocumentUsagesParams struct {
	UserID      string
	DocumentID  string
	VersionID   string
	Purposes    []string
	Status      string
	ConfirmedAt time.Time
}

type UploadRequestRecord struct {
	DocumentID  string
	VersionID   string
	RequestHash []byte
}

// DocumentRepository 是资料治理需要的最小持久化能力。所有方法都必须按 user_id 隔离。
type DocumentRepository interface {
	FindUploadRequest(ctx context.Context, userID, idempotencyKey string) (UploadRequestRecord, bool, error)
	FindVersionByContentHash(ctx context.Context, userID string, sha256 []byte) (string, bool, error)
	CreateDocumentVersion(ctx context.Context, params CreateDocumentVersionParams) (Document, DocumentVersion, error)
	SaveSourceChunks(ctx context.Context, params SaveSourceChunksParams) error
	MarkParseFailed(ctx context.Context, userID, documentID, message string) error

	GetDocumentDetail(ctx context.Context, userID, documentID string) (DocumentDetail, error)
	GetVersionRawText(ctx context.Context, userID, documentID, versionID string) (DocumentVersion, string, error)
	ListDocuments(ctx context.Context, userID string, limit int) ([]DocumentListItem, error)
	ListVersions(ctx context.Context, userID, documentID string) ([]DocumentVersion, error)
	ListSourceChunks(ctx context.Context, userID, documentID, versionID string) ([]SourceChunk, error)

	UpdateDocumentMetadata(ctx context.Context, params UpdateDocumentMetadataParams) error
	ReplaceDocumentUsages(ctx context.Context, params ReplaceDocumentUsagesParams) error
	SetSourceChunkRetrieval(ctx context.Context, userID, documentID, chunkID string, enabled bool) (SourceChunk, error)
	SoftDeleteDocument(ctx context.Context, userID, documentID string) error
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

// DocumentLimits 是上传入口的硬限制。
type DocumentLimits struct {
	MaxFileBytes  int64
	MaxTitleChars int
	Parse         MarkdownParseLimits
}

func DefaultDocumentLimits() DocumentLimits {
	return DocumentLimits{
		MaxFileBytes:  1 << 20, // 1 MiB：Markdown v0 原文直接存 PostgreSQL TEXT
		MaxTitleChars: 200,
		Parse:         DefaultMarkdownParseLimits(),
	}
}

// defaultDocumentListLimit 是资料列表默认条数上限，首版不做游标分页。
const defaultDocumentListLimit = 100

// allowedMarkdownExtensions 是 v0 唯一支持的扩展名。
var allowedMarkdownExtensions = []string{".md", ".markdown"}

// allowedMarkdownMIMETypes 覆盖各浏览器/操作系统对 .md 的不同判定。
// 浏览器对 Markdown 的 MIME 判定并不统一，这里放行文本类与未知类，
// 真正的把关是扩展名 + UTF-8 校验 + AST 解析，而不是客户端自报的 MIME。
var allowedMarkdownMIMETypes = []string{
	"text/markdown",
	"text/x-markdown",
	"text/plain",
	"application/markdown",
	"application/octet-stream",
	"", // 部分客户端不带 Content-Type
}

// DocumentService 编排资料上传、解析、用途确认与检索开关。
//
// 它守住三条边界：
//  1. 上传和解析只产生资料与来源片段，不产生知识点，也不产生任何掌握状态；
//  2. 内容来源、类别、用途的默认值全部是“未确认”，只有用户显式提交才生效；
//  3. 片段级检索开关必须建立在资料级 ai_retrieval 用途之上。
type DocumentService struct {
	repository DocumentRepository
	limits     DocumentLimits
	now        func() time.Time
}

func NewDocumentService(repository DocumentRepository, limits DocumentLimits) *DocumentService {
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = DefaultDocumentLimits().MaxFileBytes
	}
	if limits.MaxTitleChars <= 0 {
		limits.MaxTitleChars = DefaultDocumentLimits().MaxTitleChars
	}
	return &DocumentService{repository: repository, limits: limits, now: time.Now}
}

// Limits 暴露生效中的上传限制，供接口层做前置校验与错误提示。
func (s *DocumentService) Limits() DocumentLimits { return s.limits }

// UploadDocumentRequest 是一次 Markdown 上传动作。
type UploadDocumentRequest struct {
	UserID         string
	DocumentID     string // 可选：给已有资料追加新版本
	Title          string // 可选：留空时取文件名（仅作展示标题，不推断来源与类别）
	Filename       string
	MIMEType       string
	Content        []byte
	IdempotencyKey string
}

// Upload 保存一份 Markdown 资料并解析成来源片段。
//
// 流程刻意分成两个事务：
//   - 事务一写入资料与版本（status=parsing），保证原文这一“事实源”先落库；
//   - 事务二写入来源片段并推进状态。
//
// 内容与预算校验全部发生在事务一之前，所以非法文件不会留下任何数据；
// 只有数据库故障才会留下 failed 状态，而那正是重试接口要处理的情况。
func (s *DocumentService) Upload(ctx context.Context, request UploadDocumentRequest) (UploadDocumentResult, error) {
	if request.UserID == "" {
		return UploadDocumentResult{}, errors.New("缺少用户身份")
	}

	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if utf8.RuneCountInString(idempotencyKey) > 128 {
		return UploadDocumentResult{}, invalidDocumentInput("幂等键长度不能超过 128 个字符")
	}

	rawText, err := s.decodeMarkdown(request)
	if err != nil {
		return UploadDocumentResult{}, err
	}
	title, err := s.resolveTitle(request)
	if err != nil {
		return UploadDocumentResult{}, err
	}

	// 先解析：标题数量、片段数量、单段长度等预算在落库前就必须满足。
	chunks, err := ParseMarkdown(rawText, s.limits.Parse)
	if err != nil {
		return UploadDocumentResult{}, invalidDocumentInput("解析 Markdown 失败: %s", err.Error())
	}

	digest := sha256.Sum256(request.Content)

	if idempotencyKey != "" {
		record, found, err := s.repository.FindUploadRequest(ctx, request.UserID, idempotencyKey)
		if err != nil {
			return UploadDocumentResult{}, fmt.Errorf("查询上传幂等记录失败: %w", err)
		}
		if found {
			if !bytes.Equal(record.RequestHash, digest[:]) {
				return UploadDocumentResult{}, ErrDocumentIdempotencyConflict
			}
			detail, err := s.repository.GetDocumentDetail(ctx, request.UserID, record.DocumentID)
			if err != nil {
				return UploadDocumentResult{}, err
			}
			return UploadDocumentResult{Detail: detail, IdempotentHit: true}, nil
		}
	}

	// 同一用户的相同内容只做提示，不静默合并成同一份资料。
	duplicateVersionID, _, err := s.repository.FindVersionByContentHash(ctx, request.UserID, digest[:])
	if err != nil {
		return UploadDocumentResult{}, fmt.Errorf("查询重复内容失败: %w", err)
	}

	document, version, err := s.repository.CreateDocumentVersion(ctx, CreateDocumentVersionParams{
		UserID:           request.UserID,
		DocumentID:       request.DocumentID,
		Title:            title,
		OriginalFilename: strings.TrimSpace(request.Filename),
		MIMEType:         normalizeMIME(request.MIMEType),
		SizeBytes:        int64(len(request.Content)),
		SHA256:           digest[:],
		RawText:          rawText,
		ParserVersion:    MarkdownParserVersion,
		IdempotencyKey:   idempotencyKey,
	})
	if err != nil {
		return UploadDocumentResult{}, err
	}

	if err := s.persistChunks(ctx, request.UserID, document, version.VersionID, chunks, false); err != nil {
		return UploadDocumentResult{}, err
	}

	detail, err := s.repository.GetDocumentDetail(ctx, request.UserID, document.DocumentID)
	if err != nil {
		return UploadDocumentResult{}, err
	}
	return UploadDocumentResult{Detail: detail, DuplicateOfVersionID: duplicateVersionID}, nil
}

// persistChunks 写入来源片段并推进状态；失败时把资料标记为 failed 供重试。
func (s *DocumentService) persistChunks(
	ctx context.Context,
	userID string,
	document Document,
	versionID string,
	chunks []MarkdownChunk,
	retrievalEnabled bool,
) error {
	// 新版本还没有任何已确认用途，所以状态一定是“待确认”，不会是 ready。
	status := documentStatus(document.ContentOrigin, nil, len(chunks))
	err := s.repository.SaveSourceChunks(ctx, SaveSourceChunksParams{
		UserID:           userID,
		DocumentID:       document.DocumentID,
		VersionID:        versionID,
		Chunks:           chunks,
		RetrievalEnabled: retrievalEnabled,
		Status:           status,
		ParsedAt:         s.now(),
	})
	if err == nil {
		return nil
	}
	// 尽力标记失败原因；标记失败本身不覆盖原始错误。
	if markErr := s.repository.MarkParseFailed(ctx, userID, document.DocumentID, err.Error()); markErr != nil {
		return fmt.Errorf("保存来源片段失败: %w（标记失败状态也失败: %v）", err, markErr)
	}
	return fmt.Errorf("保存来源片段失败: %w", err)
}

// RetryParse 重新解析当前版本。原文是事实源，所以重试不需要用户重新上传文件。
func (s *DocumentService) RetryParse(ctx context.Context, userID, documentID string) (DocumentDetail, error) {
	detail, err := s.repository.GetDocumentDetail(ctx, userID, documentID)
	if err != nil {
		return DocumentDetail{}, err
	}
	if detail.ChunkCount > 0 && detail.Document.Status != DocumentStatusFailed {
		return DocumentDetail{}, ErrDocumentParseSucceeded
	}

	_, rawText, err := s.repository.GetVersionRawText(ctx, userID, documentID, detail.Document.CurrentVersionID)
	if err != nil {
		return DocumentDetail{}, err
	}
	chunks, err := ParseMarkdown(rawText, s.limits.Parse)
	if err != nil {
		message := fmt.Sprintf("解析 Markdown 失败: %s", err.Error())
		if markErr := s.repository.MarkParseFailed(ctx, userID, documentID, message); markErr != nil {
			return DocumentDetail{}, fmt.Errorf("标记解析失败状态失败: %w", markErr)
		}
		return DocumentDetail{}, invalidDocumentInput("%s", message)
	}

	if err := s.persistChunks(ctx, userID, detail.Document, detail.Document.CurrentVersionID, chunks,
		purposeEnabled(detail.Usages, DocumentPurposeAIRetrieval)); err != nil {
		return DocumentDetail{}, err
	}
	return s.repository.GetDocumentDetail(ctx, userID, documentID)
}

// Get 返回资料详情。
func (s *DocumentService) Get(ctx context.Context, userID, documentID string) (DocumentDetail, error) {
	return s.repository.GetDocumentDetail(ctx, userID, documentID)
}

// List 返回当前用户未删除的资料列表。
func (s *DocumentService) List(ctx context.Context, userID string) ([]DocumentListItem, error) {
	return s.repository.ListDocuments(ctx, userID, defaultDocumentListLimit)
}

// ListVersions 返回资料的全部历史版本，最新在前。版本只新增不覆盖。
func (s *DocumentService) ListVersions(ctx context.Context, userID, documentID string) ([]DocumentVersion, error) {
	return s.repository.ListVersions(ctx, userID, documentID)
}

// GetVersionContent 返回指定版本的原文，用于“查看原文”。
func (s *DocumentService) GetVersionContent(ctx context.Context, userID, documentID, versionID string) (DocumentVersion, string, error) {
	return s.repository.GetVersionRawText(ctx, userID, documentID, versionID)
}

// ListSourceChunks 返回指定版本的来源片段；versionID 为空时取当前版本。
func (s *DocumentService) ListSourceChunks(ctx context.Context, userID, documentID, versionID string) ([]SourceChunk, error) {
	if versionID == "" {
		detail, err := s.repository.GetDocumentDetail(ctx, userID, documentID)
		if err != nil {
			return nil, err
		}
		versionID = detail.Document.CurrentVersionID
	}
	return s.repository.ListSourceChunks(ctx, userID, documentID, versionID)
}

// UpdateMetadataRequest 是用户对来源、类别、标题的确认。
// 三个字段都用指针：nil 表示本次不修改，而不是“清空”。
type UpdateMetadataRequest struct {
	UserID        string
	DocumentID    string
	Title         *string
	ContentOrigin *string
	DocumentKind  *string
}

// UpdateMetadata 应用用户确认后的来源与类别。
// AI 只能在别处提出建议，任何建议都必须经过这个入口才会生效。
func (s *DocumentService) UpdateMetadata(ctx context.Context, request UpdateMetadataRequest) (DocumentDetail, error) {
	detail, err := s.repository.GetDocumentDetail(ctx, request.UserID, request.DocumentID)
	if err != nil {
		return DocumentDetail{}, err
	}

	origin := detail.Document.ContentOrigin
	if request.ContentOrigin != nil {
		if !slices.Contains(validContentOrigins, *request.ContentOrigin) {
			return DocumentDetail{}, invalidDocumentInput("不支持的内容来源: %s", *request.ContentOrigin)
		}
		origin = *request.ContentOrigin
	}
	if request.DocumentKind != nil && !slices.Contains(validDocumentKinds, *request.DocumentKind) {
		return DocumentDetail{}, invalidDocumentInput("不支持的内容类别: %s", *request.DocumentKind)
	}
	var title *string
	if request.Title != nil {
		normalized := strings.TrimSpace(*request.Title)
		if normalized == "" {
			return DocumentDetail{}, invalidDocumentInput("标题不能为空")
		}
		if utf8.RuneCountInString(normalized) > s.limits.MaxTitleChars {
			return DocumentDetail{}, invalidDocumentInput("标题长度不能超过 %d 个字符", s.limits.MaxTitleChars)
		}
		title = &normalized
	}

	err = s.repository.UpdateDocumentMetadata(ctx, UpdateDocumentMetadataParams{
		UserID:        request.UserID,
		DocumentID:    request.DocumentID,
		Title:         title,
		ContentOrigin: request.ContentOrigin,
		DocumentKind:  request.DocumentKind,
		Status:        documentStatus(origin, detail.Usages, detail.ChunkCount),
	})
	if err != nil {
		return DocumentDetail{}, err
	}
	return s.repository.GetDocumentDetail(ctx, request.UserID, request.DocumentID)
}

// ConfirmUsages 用用户提交的用途集合整体覆盖当前版本的用途。
// 传空集合表示撤回全部用途，资料回到“待确认”。
func (s *DocumentService) ConfirmUsages(ctx context.Context, userID, documentID string, purposes []string) (DocumentDetail, error) {
	normalized, err := normalizePurposes(purposes)
	if err != nil {
		return DocumentDetail{}, err
	}
	detail, err := s.repository.GetDocumentDetail(ctx, userID, documentID)
	if err != nil {
		return DocumentDetail{}, err
	}
	if detail.ChunkCount == 0 {
		return DocumentDetail{}, invalidDocumentInput("资料尚未解析成功，无法确认用途")
	}

	usages := make([]DocumentUsage, 0, len(normalized))
	for _, purpose := range normalized {
		usages = append(usages, DocumentUsage{Purpose: purpose, Enabled: true})
	}

	err = s.repository.ReplaceDocumentUsages(ctx, ReplaceDocumentUsagesParams{
		UserID:      userID,
		DocumentID:  documentID,
		VersionID:   detail.Document.CurrentVersionID,
		Purposes:    normalized,
		Status:      documentStatus(detail.Document.ContentOrigin, usages, detail.ChunkCount),
		ConfirmedAt: s.now(),
	})
	if err != nil {
		return DocumentDetail{}, err
	}
	return s.repository.GetDocumentDetail(ctx, userID, documentID)
}

// SetChunkRetrieval 单独把某个来源片段排除出或纳入 AI 检索范围。
// 资料级用途是前提：没有开启 ai_retrieval 时不允许做片段级开关，避免出现
// “资料没授权但片段可召回”的越权状态。
func (s *DocumentService) SetChunkRetrieval(ctx context.Context, userID, documentID, chunkID string, enabled bool) (SourceChunk, error) {
	detail, err := s.repository.GetDocumentDetail(ctx, userID, documentID)
	if err != nil {
		return SourceChunk{}, err
	}
	if enabled && !purposeEnabled(detail.Usages, DocumentPurposeAIRetrieval) {
		return SourceChunk{}, ErrDocumentRetrievalNotEnabled
	}
	return s.repository.SetSourceChunkRetrieval(ctx, userID, documentID, chunkID, enabled)
}

// Delete 软删除资料。版本、来源片段等血缘数据保留，但资料不再出现在列表与检索中。
func (s *DocumentService) Delete(ctx context.Context, userID, documentID string) error {
	return s.repository.SoftDeleteDocument(ctx, userID, documentID)
}

// ---------------------------------------------------------------------------
// 后端生成的业务 ID 与内容哈希
// ---------------------------------------------------------------------------

// NewDocumentID 生成资料主记录 ID。
func NewDocumentID() (string, error) { return newUUIDv7("document_id") }

// NewDocumentVersionID 生成资料版本 ID。
func NewDocumentVersionID() (string, error) { return newUUIDv7("version_id") }

// NewDocumentUsageID 生成资料用途记录 ID。
func NewDocumentUsageID() (string, error) { return newUUIDv7("usage_id") }

// NewSourceChunkID 生成来源片段 ID。
func NewSourceChunkID() (string, error) { return newUUIDv7("source_chunk_id") }

// SourceChunkContentHash 计算片段正文哈希，用于跨版本识别“内容没变的片段”。
func SourceChunkContentHash(content string) [32]byte {
	return sha256.Sum256([]byte(content))
}

// ---------------------------------------------------------------------------
// 内部校验
// ---------------------------------------------------------------------------

// decodeMarkdown 校验扩展名、MIME、大小和 UTF-8，返回去掉 BOM 的原文。
func (s *DocumentService) decodeMarkdown(request UploadDocumentRequest) (string, error) {
	filename := strings.TrimSpace(request.Filename)
	if filename == "" {
		return "", invalidDocumentInput("缺少文件名")
	}
	if strings.ContainsAny(filename, `/\`) || strings.Contains(filename, "..") {
		return "", invalidDocumentInput("文件名不合法")
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if !slices.Contains(allowedMarkdownExtensions, extension) {
		return "", invalidDocumentInput("当前只支持 Markdown 文件（%s）", strings.Join(allowedMarkdownExtensions, "、"))
	}
	if !slices.Contains(allowedMarkdownMIMETypes, normalizeMIME(request.MIMEType)) {
		return "", invalidDocumentInput("不支持的 MIME 类型: %s", request.MIMEType)
	}
	if int64(len(request.Content)) > s.limits.MaxFileBytes {
		return "", invalidDocumentInput("文件大小超过上限 %d 字节", s.limits.MaxFileBytes)
	}

	rawText := strings.TrimPrefix(string(request.Content), "\ufeff")
	if !utf8.ValidString(rawText) {
		return "", invalidDocumentInput("文件必须是 UTF-8 编码的文本")
	}
	if strings.ContainsRune(rawText, '\x00') {
		return "", invalidDocumentInput("文件包含空字节，疑似二进制内容")
	}
	if strings.TrimSpace(rawText) == "" {
		return "", invalidDocumentInput("文件内容为空")
	}
	return rawText, nil
}

// resolveTitle 只把文件名当作展示标题的兜底，绝不据此推断来源、类别或用途。
func (s *DocumentService) resolveTitle(request UploadDocumentRequest) (string, error) {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		base := filepath.Base(strings.TrimSpace(request.Filename))
		title = strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
	}
	if title == "" {
		return "", invalidDocumentInput("标题不能为空")
	}
	if utf8.RuneCountInString(title) > s.limits.MaxTitleChars {
		return "", invalidDocumentInput("标题长度不能超过 %d 个字符", s.limits.MaxTitleChars)
	}
	return title, nil
}

// normalizePurposes 去重、校验取值，并强制“仅归档”与其它用途互斥。
func normalizePurposes(purposes []string) ([]string, error) {
	normalized := make([]string, 0, len(purposes))
	for _, purpose := range purposes {
		purpose = strings.TrimSpace(purpose)
		if purpose == "" {
			continue
		}
		if !slices.Contains(validDocumentPurposes, purpose) {
			return nil, invalidDocumentInput("不支持的资料用途: %s", purpose)
		}
		if !slices.Contains(normalized, purpose) {
			normalized = append(normalized, purpose)
		}
	}
	if slices.Contains(normalized, DocumentPurposeArchiveOnly) && len(normalized) > 1 {
		return nil, invalidDocumentInput("“仅归档”不能与其它用途同时选择")
	}
	return normalized, nil
}

// documentStatus 由“来源是否确认 + 用途是否确认 + 是否解析出片段”共同推导。
// 它只描述资料自身的生命周期，不表达任何掌握状态。
func documentStatus(origin string, usages []DocumentUsage, chunkCount int) string {
	if chunkCount == 0 {
		return DocumentStatusParsing
	}
	if purposeEnabled(usages, DocumentPurposeArchiveOnly) {
		return DocumentStatusArchived
	}
	if origin == ContentOriginPending || !anyPurposeEnabled(usages) {
		return DocumentStatusPendingConfirmation
	}
	return DocumentStatusReady
}

func purposeEnabled(usages []DocumentUsage, purpose string) bool {
	for _, usage := range usages {
		if usage.Purpose == purpose && usage.Enabled {
			return true
		}
	}
	return false
}

func anyPurposeEnabled(usages []DocumentUsage) bool {
	for _, usage := range usages {
		if usage.Enabled {
			return true
		}
	}
	return false
}

// normalizeMIME 去掉 `; charset=utf-8` 之类的参数并统一小写。
func normalizeMIME(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if mediaType, _, err := mime.ParseMediaType(value); err == nil {
		return strings.ToLower(mediaType)
	}
	return strings.ToLower(value)
}
