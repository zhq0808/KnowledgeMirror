package handler

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

// uuidPattern 校验路径中的资料/版本/片段 ID，避免把任意字符串送进 SQL 的 UUID 转换。
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// uploadFormField 是 multipart/form-data 中的文件字段名。
const uploadFormField = "file"

// idempotencyHeader 是上传幂等键请求头。同键同内容重放返回首次结果，不产生新版本。
const idempotencyHeader = "Idempotency-Key"

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

type documentReply struct {
	DocumentID    string  `json:"document_id"`
	Title         string  `json:"title"`
	ContentOrigin string  `json:"content_origin"`
	DocumentKind  string  `json:"document_kind"`
	Status        string  `json:"status"`
	ParseError    string  `json:"parse_error,omitempty"`
	ParsedAt      *string `json:"parsed_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`

	CurrentVersion *documentVersionReply `json:"current_version,omitempty"`
	Purposes       []string              `json:"purposes"`
	ChunkCount     int                   `json:"chunk_count"`
}

type documentVersionReply struct {
	VersionID        string `json:"version_id"`
	VersionNo        int    `json:"version_no"`
	OriginalFilename string `json:"original_filename"`
	MIMEType         string `json:"mime_type"`
	SizeBytes        int64  `json:"size_bytes"`
	SHA256           string `json:"sha256"`
	ParserVersion    string `json:"parser_version"`
	CreatedAt        string `json:"created_at"`
}

type sourceChunkReply struct {
	SourceChunkID    string   `json:"source_chunk_id"`
	VersionID        string   `json:"version_id"`
	Ordinal          int      `json:"ordinal"`
	HeadingPath      []string `json:"heading_path"`
	Content          string   `json:"content"`
	StartOffset      int64    `json:"start_offset"`
	EndOffset        int64    `json:"end_offset"`
	TrustLevel       string   `json:"trust_level"`
	RetrievalEnabled bool     `json:"retrieval_enabled"`
}

type uploadDocumentReply struct {
	Document      documentReply `json:"document"`
	IdempotentHit bool          `json:"idempotent_hit"`
	// DuplicateOfVersionID 表示同一用户已存在内容完全相同的版本。
	// 这里只提示，不做静默合并——是否算同一份资料由用户决定。
	DuplicateOfVersionID string `json:"duplicate_of_version_id,omitempty"`
}

type versionContentReply struct {
	Version documentVersionReply `json:"version"`
	RawText string               `json:"raw_text"`
}

type updateDocumentRequest struct {
	Title         *string `json:"title"`
	ContentOrigin *string `json:"content_origin"`
	DocumentKind  *string `json:"document_kind"`
}

type confirmUsagesRequest struct {
	Purposes []string `json:"purposes"`
}

type updateChunkRetrievalRequest struct {
	RetrievalEnabled *bool `json:"retrieval_enabled"`
}

func toDocumentReply(detail service.DocumentDetail) documentReply {
	reply := documentReply{
		DocumentID:    detail.Document.DocumentID,
		Title:         detail.Document.Title,
		ContentOrigin: detail.Document.ContentOrigin,
		DocumentKind:  detail.Document.DocumentKind,
		Status:        detail.Document.Status,
		ParseError:    detail.Document.ParseError,
		CreatedAt:     detail.Document.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     detail.Document.UpdatedAt.Format(time.RFC3339),
		Purposes:      enabledPurposes(detail.Usages),
		ChunkCount:    detail.ChunkCount,
	}
	if detail.Document.ParsedAt != nil {
		parsedAt := detail.Document.ParsedAt.Format(time.RFC3339)
		reply.ParsedAt = &parsedAt
	}
	if detail.Version.VersionID != "" {
		version := toVersionReply(detail.Version)
		reply.CurrentVersion = &version
	}
	return reply
}

func toVersionReply(version service.DocumentVersion) documentVersionReply {
	return documentVersionReply{
		VersionID:        version.VersionID,
		VersionNo:        version.VersionNo,
		OriginalFilename: version.OriginalFilename,
		MIMEType:         version.MIMEType,
		SizeBytes:        version.SizeBytes,
		SHA256:           version.SHA256Hex,
		ParserVersion:    version.ParserVersion,
		CreatedAt:        version.CreatedAt.Format(time.RFC3339),
	}
}

func toChunkReply(chunk service.SourceChunk) sourceChunkReply {
	headingPath := chunk.HeadingPath
	if headingPath == nil {
		headingPath = []string{}
	}
	return sourceChunkReply{
		SourceChunkID:    chunk.SourceChunkID,
		VersionID:        chunk.VersionID,
		Ordinal:          chunk.Ordinal,
		HeadingPath:      headingPath,
		Content:          chunk.Content,
		StartOffset:      chunk.StartOffset,
		EndOffset:        chunk.EndOffset,
		TrustLevel:       chunk.TrustLevel,
		RetrievalEnabled: chunk.RetrievalEnabled,
	}
}

// enabledPurposes 只返回用户已确认启用的用途。未确认的用途不出现在响应里，
// 避免前端把“AI 建议”误当成“已生效”。
func enabledPurposes(usages []service.DocumentUsage) []string {
	purposes := make([]string, 0, len(usages))
	for _, usage := range usages {
		if usage.Enabled {
			purposes = append(purposes, usage.Purpose)
		}
	}
	return purposes
}

// ---------------------------------------------------------------------------
// 公共校验
// ---------------------------------------------------------------------------

// requireDocumentUser 取出可信 user_id 与合法的 document_id。
func (s *Server) requireDocumentUser(c *gin.Context) (userID, documentID string, okToProceed bool) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return "", "", false
	}
	documentID = c.Param("document_id")
	if !uuidPattern.MatchString(documentID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "资料ID格式错误")
		return "", "", false
	}
	return userID, documentID, true
}

// failDocumentError 把服务层错误映射成稳定的 HTTP 语义。
// 不属于当前用户的资料一律按 404 处理，不泄露“该资料存在”。
func (s *Server) failDocumentError(c *gin.Context, action string, err error) {
	var inputError *service.DocumentInputError
	switch {
	case errors.As(err, &inputError):
		fail(c, http.StatusBadRequest, CodeBadRequest, inputError.Message)
	case errors.Is(err, service.ErrDocumentNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "资料不存在")
	case errors.Is(err, service.ErrDocumentIdempotencyConflict):
		fail(c, http.StatusConflict, CodeConflict, "幂等键已用于其它上传内容")
	case errors.Is(err, service.ErrDocumentParseSucceeded):
		fail(c, http.StatusConflict, CodeConflict, "当前版本已解析成功，无需重试")
	case errors.Is(err, service.ErrDocumentRetrievalNotEnabled):
		fail(c, http.StatusConflict, CodeConflict, "请先把资料用途设为“供 AI 检索”，再调整单个片段")
	default:
		s.log.Error(action, "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, action)
	}
}

// ---------------------------------------------------------------------------
// 上传与解析
// ---------------------------------------------------------------------------

// uploadDocumentHandler 接收 multipart/form-data 的 Markdown 文件。
//
// 上传只产生资料、版本和来源片段，不产生知识点，也不改变任何掌握状态。
// 来源默认“待确认”、类别默认“其他”、用途为空，必须由用户显式确认后才生效。
func (s *Server) uploadDocumentHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	fileHeader, err := c.FormFile(uploadFormField)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "请求体大小超过上限")
			return
		}
		fail(c, http.StatusBadRequest, CodeBadRequest, "请通过 file 字段上传 Markdown 文件")
		return
	}
	if fileHeader.Size > s.documents.Limits().MaxFileBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "文件大小超过上限")
		return
	}

	opened, err := fileHeader.Open()
	if err != nil {
		s.failDocumentError(c, "读取上传文件失败", err)
		return
	}
	defer func() { _ = opened.Close() }()

	// 再套一层上限：Size 来自客户端声明，不能作为唯一防线。
	content, err := io.ReadAll(io.LimitReader(opened, s.documents.Limits().MaxFileBytes+1))
	if err != nil {
		s.failDocumentError(c, "读取上传文件失败", err)
		return
	}
	if int64(len(content)) > s.documents.Limits().MaxFileBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "文件大小超过上限")
		return
	}

	documentID := strings.TrimSpace(c.PostForm("document_id"))
	if documentID != "" && !uuidPattern.MatchString(documentID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "资料ID格式错误")
		return
	}

	result, err := s.documents.Upload(c.Request.Context(), service.UploadDocumentRequest{
		UserID:         userID,
		DocumentID:     documentID,
		Title:          c.PostForm("title"),
		Filename:       fileHeader.Filename,
		MIMEType:       fileHeader.Header.Get("Content-Type"),
		Content:        content,
		IdempotencyKey: c.GetHeader(idempotencyHeader),
	})
	if err != nil {
		s.failDocumentError(c, "上传资料失败", err)
		return
	}

	ok(c, uploadDocumentReply{
		Document:             toDocumentReply(result.Detail),
		IdempotentHit:        result.IdempotentHit,
		DuplicateOfVersionID: result.DuplicateOfVersionID,
	})
}

// retryDocumentParseHandler 重新解析当前版本。原文已经落库，重试不需要重新上传文件。
func (s *Server) retryDocumentParseHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	detail, err := s.documents.RetryParse(c.Request.Context(), userID, documentID)
	if err != nil {
		s.failDocumentError(c, "重新解析资料失败", err)
		return
	}
	ok(c, toDocumentReply(detail))
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (s *Server) listDocumentsHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	items, err := s.documents.List(c.Request.Context(), userID)
	if err != nil {
		s.failDocumentError(c, "查询资料列表失败", err)
		return
	}
	reply := make([]documentReply, 0, len(items))
	for _, item := range items {
		reply = append(reply, toDocumentReply(service.DocumentDetail{
			Document:   item.Document,
			Version:    item.Version,
			Usages:     item.Usages,
			ChunkCount: item.ChunkCount,
		}))
	}
	ok(c, reply)
}

func (s *Server) getDocumentHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	detail, err := s.documents.Get(c.Request.Context(), userID, documentID)
	if err != nil {
		s.failDocumentError(c, "查询资料失败", err)
		return
	}
	ok(c, toDocumentReply(detail))
}

// listDocumentVersionsHandler 返回全部历史版本。版本只新增不覆盖，旧版本永远可追溯。
func (s *Server) listDocumentVersionsHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	versions, err := s.documents.ListVersions(c.Request.Context(), userID, documentID)
	if err != nil {
		s.failDocumentError(c, "查询资料版本失败", err)
		return
	}
	reply := make([]documentVersionReply, 0, len(versions))
	for _, version := range versions {
		reply = append(reply, toVersionReply(version))
	}
	ok(c, reply)
}

func (s *Server) getDocumentVersionHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	versionID := c.Param("version_id")
	if !uuidPattern.MatchString(versionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "版本ID格式错误")
		return
	}
	version, rawText, err := s.documents.GetVersionContent(c.Request.Context(), userID, documentID, versionID)
	if err != nil {
		s.failDocumentError(c, "查询资料原文失败", err)
		return
	}
	ok(c, versionContentReply{Version: toVersionReply(version), RawText: rawText})
}

// listDocumentChunksHandler 返回来源片段。每个片段都带标题路径和原文偏移，
// 后续的候选内容、Agent 回答和评估都要靠它回到具体原文位置。
func (s *Server) listDocumentChunksHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	versionID := strings.TrimSpace(c.Query("version_id"))
	if versionID != "" && !uuidPattern.MatchString(versionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "版本ID格式错误")
		return
	}
	chunks, err := s.documents.ListSourceChunks(c.Request.Context(), userID, documentID, versionID)
	if err != nil {
		s.failDocumentError(c, "查询来源片段失败", err)
		return
	}
	reply := make([]sourceChunkReply, 0, len(chunks))
	for _, chunk := range chunks {
		reply = append(reply, toChunkReply(chunk))
	}
	ok(c, reply)
}

// ---------------------------------------------------------------------------
// 用户确认
// ---------------------------------------------------------------------------

// updateDocumentHandler 是来源与类别的唯一生效入口。
// AI 只能在别处给出建议，任何建议都必须经由用户在这里提交才会改变数据。
func (s *Server) updateDocumentHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	var request updateDocumentRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	detail, err := s.documents.UpdateMetadata(c.Request.Context(), service.UpdateMetadataRequest{
		UserID:        userID,
		DocumentID:    documentID,
		Title:         request.Title,
		ContentOrigin: request.ContentOrigin,
		DocumentKind:  request.DocumentKind,
	})
	if err != nil {
		s.failDocumentError(c, "修改资料失败", err)
		return
	}
	ok(c, toDocumentReply(detail))
}

// confirmDocumentUsagesHandler 整体覆盖当前版本的用途集合（多选）。
// 传空数组表示撤回全部用途；“仅归档”不能与其它用途并存。
func (s *Server) confirmDocumentUsagesHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	var request confirmUsagesRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	detail, err := s.documents.ConfirmUsages(c.Request.Context(), userID, documentID, request.Purposes)
	if err != nil {
		s.failDocumentError(c, "确认资料用途失败", err)
		return
	}
	ok(c, toDocumentReply(detail))
}

// updateDocumentChunkHandler 做片段级的检索纳入/排除，用于剔除过期内容、
// AI 幻觉段落或含 Prompt Injection 的片段。前提是资料级已开启“供 AI 检索”。
func (s *Server) updateDocumentChunkHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	chunkID := c.Param("chunk_id")
	if !uuidPattern.MatchString(chunkID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "片段ID格式错误")
		return
	}
	var request updateChunkRetrievalRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.RetrievalEnabled == nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请提供 retrieval_enabled")
		return
	}
	chunk, err := s.documents.SetChunkRetrieval(c.Request.Context(), userID, documentID, chunkID, *request.RetrievalEnabled)
	if err != nil {
		s.failDocumentError(c, "更新来源片段失败", err)
		return
	}
	ok(c, toChunkReply(chunk))
}

func (s *Server) deleteDocumentHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	if err := s.documents.Delete(c.Request.Context(), userID, documentID); err != nil {
		s.failDocumentError(c, "删除资料失败", err)
		return
	}
	ok(c, gin.H{"document_id": documentID})
}
