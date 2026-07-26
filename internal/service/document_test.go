package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 内存版仓储：只保留断言需要的状态，不模拟 SQL 细节。
// ---------------------------------------------------------------------------

type fakeDocumentRepository struct {
	documents map[string]*fakeDocument
	requests  map[string]UploadRequestRecord // key: userID + "\x00" + idempotencyKey
	hashes    map[string]string              // key: userID + "\x00" + hex(sha256) → versionID
	sequence  int

	saveChunksErr error
	markedFailed  []string
}

type fakeDocument struct {
	document Document
	versions []DocumentVersion
	rawText  map[string]string
	chunks   map[string][]SourceChunk
	usages   []DocumentUsage
	deleted  bool
}

func newFakeDocumentRepository() *fakeDocumentRepository {
	return &fakeDocumentRepository{
		documents: map[string]*fakeDocument{},
		requests:  map[string]UploadRequestRecord{},
		hashes:    map[string]string{},
	}
}

func (r *fakeDocumentRepository) nextID(prefix string) string {
	r.sequence++
	return prefix + "-" + string(rune('a'+r.sequence-1))
}

func (r *fakeDocumentRepository) FindUploadRequest(_ context.Context, userID, key string) (UploadRequestRecord, bool, error) {
	record, found := r.requests[userID+"\x00"+key]
	return record, found, nil
}

func (r *fakeDocumentRepository) FindVersionByContentHash(_ context.Context, userID string, digest []byte) (string, bool, error) {
	versionID, found := r.hashes[userID+"\x00"+string(digest)]
	return versionID, found, nil
}

func (r *fakeDocumentRepository) CreateDocumentVersion(_ context.Context, params CreateDocumentVersionParams) (Document, DocumentVersion, error) {
	entry, exists := r.documents[params.DocumentID]
	if !exists {
		documentID := r.nextID("doc")
		entry = &fakeDocument{
			document: Document{
				DocumentID: documentID,
				UserID:     params.UserID,
				Title:      params.Title,
				// 默认来源“待确认”、类别“其他”，不由文件名推断。
				ContentOrigin: ContentOriginPending,
				DocumentKind:  DocumentKindOther,
				Status:        DocumentStatusParsing,
			},
			rawText: map[string]string{},
			chunks:  map[string][]SourceChunk{},
		}
		r.documents[documentID] = entry
	}
	if entry.document.UserID != params.UserID {
		return Document{}, DocumentVersion{}, ErrDocumentNotFound
	}

	version := DocumentVersion{
		VersionID:        r.nextID("ver"),
		DocumentID:       entry.document.DocumentID,
		VersionNo:        len(entry.versions) + 1,
		OriginalFilename: params.OriginalFilename,
		MIMEType:         params.MIMEType,
		SizeBytes:        params.SizeBytes,
		SHA256Hex:        string(params.SHA256),
		ParserVersion:    params.ParserVersion,
		CreatedAt:        time.Unix(0, 0),
	}
	entry.versions = append(entry.versions, version)
	entry.rawText[version.VersionID] = params.RawText
	entry.document.CurrentVersionID = version.VersionID
	entry.document.Status = DocumentStatusParsing
	// 新版本不继承旧用途，必须重新确认。
	entry.usages = nil

	r.hashes[params.UserID+"\x00"+string(params.SHA256)] = version.VersionID
	if params.IdempotencyKey != "" {
		r.requests[params.UserID+"\x00"+params.IdempotencyKey] = UploadRequestRecord{
			DocumentID:  entry.document.DocumentID,
			VersionID:   version.VersionID,
			RequestHash: slices.Clone(params.SHA256),
		}
	}
	return entry.document, version, nil
}

func (r *fakeDocumentRepository) SaveSourceChunks(_ context.Context, params SaveSourceChunksParams) error {
	if r.saveChunksErr != nil {
		return r.saveChunksErr
	}
	entry, found := r.documents[params.DocumentID]
	if !found || entry.document.UserID != params.UserID {
		return ErrDocumentNotFound
	}
	chunks := make([]SourceChunk, 0, len(params.Chunks))
	for _, chunk := range params.Chunks {
		chunks = append(chunks, SourceChunk{
			SourceChunkID:    r.nextID("chunk"),
			DocumentID:       params.DocumentID,
			VersionID:        params.VersionID,
			Ordinal:          chunk.Ordinal,
			HeadingPath:      chunk.HeadingPath,
			Content:          chunk.Content,
			StartOffset:      chunk.StartOffset,
			EndOffset:        chunk.EndOffset,
			TrustLevel:       SourceChunkTrustUnverified,
			RetrievalEnabled: params.RetrievalEnabled,
		})
	}
	entry.chunks[params.VersionID] = chunks
	entry.document.Status = params.Status
	entry.document.ParseError = ""
	parsedAt := params.ParsedAt
	entry.document.ParsedAt = &parsedAt
	return nil
}

func (r *fakeDocumentRepository) MarkParseFailed(_ context.Context, userID, documentID, message string) error {
	entry, found := r.documents[documentID]
	if !found || entry.document.UserID != userID {
		return ErrDocumentNotFound
	}
	entry.document.Status = DocumentStatusFailed
	entry.document.ParseError = message
	r.markedFailed = append(r.markedFailed, documentID)
	return nil
}

func (r *fakeDocumentRepository) lookup(userID, documentID string) (*fakeDocument, error) {
	entry, found := r.documents[documentID]
	if !found || entry.deleted || entry.document.UserID != userID {
		return nil, ErrDocumentNotFound
	}
	return entry, nil
}

func (r *fakeDocumentRepository) detail(entry *fakeDocument) DocumentDetail {
	var version DocumentVersion
	for _, candidate := range entry.versions {
		if candidate.VersionID == entry.document.CurrentVersionID {
			version = candidate
		}
	}
	return DocumentDetail{
		Document:   entry.document,
		Version:    version,
		Usages:     slices.Clone(entry.usages),
		ChunkCount: len(entry.chunks[entry.document.CurrentVersionID]),
	}
}

func (r *fakeDocumentRepository) GetDocumentDetail(_ context.Context, userID, documentID string) (DocumentDetail, error) {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return DocumentDetail{}, err
	}
	return r.detail(entry), nil
}

func (r *fakeDocumentRepository) GetVersionRawText(_ context.Context, userID, documentID, versionID string) (DocumentVersion, string, error) {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return DocumentVersion{}, "", err
	}
	raw, found := entry.rawText[versionID]
	if !found {
		return DocumentVersion{}, "", ErrDocumentNotFound
	}
	for _, version := range entry.versions {
		if version.VersionID == versionID {
			return version, raw, nil
		}
	}
	return DocumentVersion{}, "", ErrDocumentNotFound
}

func (r *fakeDocumentRepository) ListDocuments(_ context.Context, userID string, limit int) ([]DocumentListItem, error) {
	items := make([]DocumentListItem, 0, len(r.documents))
	for _, entry := range r.documents {
		if entry.deleted || entry.document.UserID != userID {
			continue
		}
		detail := r.detail(entry)
		items = append(items, DocumentListItem{
			Document:   detail.Document,
			Version:    detail.Version,
			Usages:     detail.Usages,
			ChunkCount: detail.ChunkCount,
		})
		if len(items) >= limit {
			break
		}
	}
	return items, nil
}

func (r *fakeDocumentRepository) ListVersions(_ context.Context, userID, documentID string) ([]DocumentVersion, error) {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return nil, err
	}
	return slices.Clone(entry.versions), nil
}

func (r *fakeDocumentRepository) ListSourceChunks(_ context.Context, userID, documentID, versionID string) ([]SourceChunk, error) {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return nil, err
	}
	return slices.Clone(entry.chunks[versionID]), nil
}

func (r *fakeDocumentRepository) UpdateDocumentMetadata(_ context.Context, params UpdateDocumentMetadataParams) error {
	entry, err := r.lookup(params.UserID, params.DocumentID)
	if err != nil {
		return err
	}
	if params.Title != nil {
		entry.document.Title = *params.Title
	}
	if params.ContentOrigin != nil {
		entry.document.ContentOrigin = *params.ContentOrigin
	}
	if params.DocumentKind != nil {
		entry.document.DocumentKind = *params.DocumentKind
	}
	if entry.document.Status != DocumentStatusFailed {
		entry.document.Status = documentStatus(
			entry.document.ContentOrigin,
			entry.usages,
			len(entry.chunks[entry.document.CurrentVersionID]),
		)
	}
	return nil
}

func (r *fakeDocumentRepository) ReplaceDocumentUsages(_ context.Context, params ReplaceDocumentUsagesParams) error {
	entry, err := r.lookup(params.UserID, params.DocumentID)
	if err != nil {
		return err
	}
	if len(entry.chunks[entry.document.CurrentVersionID]) == 0 {
		return &DocumentInputError{Message: "资料尚未解析成功，无法确认用途"}
	}
	usages := make([]DocumentUsage, 0, len(params.Purposes))
	confirmedAt := params.ConfirmedAt
	for _, purpose := range params.Purposes {
		usages = append(usages, DocumentUsage{Purpose: purpose, Enabled: true, ConfirmedAt: &confirmedAt})
	}
	entry.usages = usages
	entry.document.Status = documentStatus(
		entry.document.ContentOrigin,
		entry.usages,
		len(entry.chunks[entry.document.CurrentVersionID]),
	)

	// 片段级检索开关跟随资料级 ai_retrieval 用途：关闭授权后不留可召回残片。
	retrievalEnabled := slices.Contains(params.Purposes, DocumentPurposeAIRetrieval)
	versionID := entry.document.CurrentVersionID
	chunks := entry.chunks[versionID]
	for index := range chunks {
		chunks[index].RetrievalEnabled = retrievalEnabled
	}
	entry.chunks[versionID] = chunks
	return nil
}

func (r *fakeDocumentRepository) SetSourceChunkRetrieval(_ context.Context, userID, documentID, chunkID string, enabled bool) (SourceChunk, error) {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return SourceChunk{}, err
	}
	for versionID, chunks := range entry.chunks {
		for index := range chunks {
			if chunks[index].SourceChunkID == chunkID {
				chunks[index].RetrievalEnabled = enabled
				entry.chunks[versionID] = chunks
				return chunks[index], nil
			}
		}
	}
	return SourceChunk{}, ErrDocumentNotFound
}

func (r *fakeDocumentRepository) SoftDeleteDocument(_ context.Context, userID, documentID string) error {
	entry, err := r.lookup(userID, documentID)
	if err != nil {
		return err
	}
	entry.deleted = true
	return nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

const sampleMarkdown = "# 一级标题\n\n正文 A\n\n## 二级标题\n\n正文 B\n"

func newDocumentServiceForTest(t *testing.T) (*DocumentService, *fakeDocumentRepository) {
	t.Helper()
	repository := newFakeDocumentRepository()
	return NewDocumentService(repository, DefaultDocumentLimits()), repository
}

func uploadRequest(content, key string) UploadDocumentRequest {
	return UploadDocumentRequest{
		UserID:         "user-1",
		Filename:       "笔记.md",
		MIMEType:       "text/markdown",
		Content:        []byte(content),
		IdempotencyKey: key,
	}
}

func mustUpload(t *testing.T, s *DocumentService, request UploadDocumentRequest) UploadDocumentResult {
	t.Helper()
	result, err := s.Upload(context.Background(), request)
	if err != nil {
		t.Fatalf("Upload 出错: %v", err)
	}
	return result
}

// ---------------------------------------------------------------------------
// 默认值：上传不推断来源/类别/用途，也不产生掌握状态
// ---------------------------------------------------------------------------

func TestUploadLeavesOriginKindAndPurposesUnconfirmed(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)

	// 文件名刻意带有“学习笔记”字样，验证系统不会据此推断类别。
	request := uploadRequest(sampleMarkdown, "")
	request.Filename = "学习笔记.md"
	result := mustUpload(t, service, request)

	if got := result.Detail.Document.ContentOrigin; got != ContentOriginPending {
		t.Fatalf("ContentOrigin = %q, want %q（不得由文件名推断）", got, ContentOriginPending)
	}
	if got := result.Detail.Document.DocumentKind; got != DocumentKindOther {
		t.Fatalf("DocumentKind = %q, want %q（不得由文件名推断）", got, DocumentKindOther)
	}
	if len(result.Detail.Usages) != 0 {
		t.Fatalf("Usages = %v, want 空（用途必须由用户确认）", result.Detail.Usages)
	}
	// 新版本没有任何已确认用途，因此绝不能直接是 ready。
	if got := result.Detail.Document.Status; got != DocumentStatusPendingConfirmation {
		t.Fatalf("Status = %q, want %q", got, DocumentStatusPendingConfirmation)
	}
	if result.Detail.ChunkCount == 0 {
		t.Fatal("ChunkCount = 0, want > 0")
	}
}

func TestUploadTitleFallsBackToFilenameWithoutExtension(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)

	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))

	if got := result.Detail.Document.Title; got != "笔记" {
		t.Fatalf("Title = %q, want %q", got, "笔记")
	}
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

func TestUploadWithSameIdempotencyKeyDoesNotCreateSecondVersion(t *testing.T) {
	service, repository := newDocumentServiceForTest(t)

	first := mustUpload(t, service, uploadRequest(sampleMarkdown, "key-1"))
	second := mustUpload(t, service, uploadRequest(sampleMarkdown, "key-1"))

	if !second.IdempotentHit {
		t.Fatal("IdempotentHit = false, want true")
	}
	if second.Detail.Document.DocumentID != first.Detail.Document.DocumentID {
		t.Fatalf("重放返回了不同资料: %q vs %q",
			second.Detail.Document.DocumentID, first.Detail.Document.DocumentID)
	}
	versions, err := repository.ListVersions(context.Background(), "user-1", first.Detail.Document.DocumentID)
	if err != nil {
		t.Fatalf("ListVersions 出错: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("版本数 = %d, want 1（重复提交不得产生新版本）", len(versions))
	}
}

func TestUploadWithSameKeyDifferentContentIsRejected(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)

	mustUpload(t, service, uploadRequest(sampleMarkdown, "key-1"))

	_, err := service.Upload(context.Background(), uploadRequest("# 另一份内容\n\n正文\n", "key-1"))
	if !errors.Is(err, ErrDocumentIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrDocumentIdempotencyConflict", err)
	}
}

func TestUploadSameContentWithoutKeyReportsDuplicateButDoesNotMerge(t *testing.T) {
	service, repository := newDocumentServiceForTest(t)

	first := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	second := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))

	if second.DuplicateOfVersionID == "" {
		t.Fatal("DuplicateOfVersionID 为空, want 提示已有相同内容")
	}
	if second.Detail.Document.DocumentID == first.Detail.Document.DocumentID {
		t.Fatal("相同内容被静默合并成同一份资料，应由用户判断")
	}
	items, err := repository.ListDocuments(context.Background(), "user-1", 100)
	if err != nil {
		t.Fatalf("ListDocuments 出错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("资料数 = %d, want 2", len(items))
	}
}

// ---------------------------------------------------------------------------
// 输入与预算校验
// ---------------------------------------------------------------------------

func TestUploadRejectsInvalidInput(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)

	cases := []struct {
		name    string
		mutate  func(*UploadDocumentRequest)
		wantMsg string
	}{
		{"扩展名不支持", func(r *UploadDocumentRequest) { r.Filename = "note.txt" }, "只支持 Markdown"},
		{"文件名含路径", func(r *UploadDocumentRequest) { r.Filename = "../etc/passwd.md" }, "文件名不合法"},
		{"MIME 不支持", func(r *UploadDocumentRequest) { r.MIMEType = "application/pdf" }, "不支持的 MIME"},
		{"非 UTF-8", func(r *UploadDocumentRequest) { r.Content = []byte{0xff, 0xfe, 0x41} }, "UTF-8"},
		{"含空字节", func(r *UploadDocumentRequest) { r.Content = []byte("# 标题\x00正文") }, "空字节"},
		{"内容为空", func(r *UploadDocumentRequest) { r.Content = []byte("   \n\t ") }, "内容为空"},
		{"标题过长", func(r *UploadDocumentRequest) { r.Title = strings.Repeat("标", 201) }, "标题长度"},
		{"幂等键过长", func(r *UploadDocumentRequest) { r.IdempotencyKey = strings.Repeat("k", 129) }, "幂等键长度"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := uploadRequest(sampleMarkdown, "")
			testCase.mutate(&request)

			_, err := service.Upload(context.Background(), request)

			var inputError *DocumentInputError
			if !errors.As(err, &inputError) {
				t.Fatalf("err = %v, want *DocumentInputError", err)
			}
			if !strings.Contains(inputError.Message, testCase.wantMsg) {
				t.Fatalf("错误信息 = %q, want 包含 %q", inputError.Message, testCase.wantMsg)
			}
		})
	}
}

func TestUploadRejectsOversizedFileBeforeWriting(t *testing.T) {
	repository := newFakeDocumentRepository()
	limits := DefaultDocumentLimits()
	limits.MaxFileBytes = 32
	service := NewDocumentService(repository, limits)

	_, err := service.Upload(context.Background(), uploadRequest("# 标题\n\n"+strings.Repeat("正文", 100), ""))

	var inputError *DocumentInputError
	if !errors.As(err, &inputError) {
		t.Fatalf("err = %v, want *DocumentInputError", err)
	}
	if len(repository.documents) != 0 {
		t.Fatal("超限文件在库中留下了记录，校验必须发生在写库之前")
	}
}

func TestUploadRejectsExceededParseBudgetWithoutWriting(t *testing.T) {
	repository := newFakeDocumentRepository()
	limits := DefaultDocumentLimits()
	limits.Parse.MaxHeadings = 1
	service := NewDocumentService(repository, limits)

	_, err := service.Upload(context.Background(), uploadRequest("# 一\n\nA\n\n# 二\n\nB\n\n# 三\n\nC\n", ""))

	var inputError *DocumentInputError
	if !errors.As(err, &inputError) {
		t.Fatalf("err = %v, want *DocumentInputError", err)
	}
	if len(repository.documents) != 0 {
		t.Fatal("超预算文件在库中留下了记录")
	}
}

// ---------------------------------------------------------------------------
// 来源片段可追溯
// ---------------------------------------------------------------------------

func TestUploadedChunksTraceBackToVersionAndOffsets(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)

	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID
	versionID := result.Detail.Document.CurrentVersionID

	chunks, err := service.ListSourceChunks(context.Background(), "user-1", documentID, "")
	if err != nil {
		t.Fatalf("ListSourceChunks 出错: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("没有解析出来源片段")
	}

	runes := []rune(sampleMarkdown)
	for index, chunk := range chunks {
		if chunk.VersionID != versionID {
			t.Fatalf("chunks[%d].VersionID = %q, want %q", index, chunk.VersionID, versionID)
		}
		if len(chunk.HeadingPath) == 0 {
			t.Fatalf("chunks[%d] 缺少标题路径", index)
		}
		if got := string(runes[chunk.StartOffset:chunk.EndOffset]); got != chunk.Content {
			t.Fatalf("chunks[%d] 按偏移取回 %q, want %q", index, got, chunk.Content)
		}
		// 解析不提升可信级别，也不默认开放检索。
		if chunk.TrustLevel != SourceChunkTrustUnverified {
			t.Fatalf("chunks[%d].TrustLevel = %q, want %q", index, chunk.TrustLevel, SourceChunkTrustUnverified)
		}
		if chunk.RetrievalEnabled {
			t.Fatalf("chunks[%d] 默认可检索，应为 false", index)
		}
	}
}

// ---------------------------------------------------------------------------
// 用途确认
// ---------------------------------------------------------------------------

func TestConfirmUsagesRejectsArchiveOnlyCombinedWithOthers(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))

	_, err := service.ConfirmUsages(context.Background(), "user-1", result.Detail.Document.DocumentID,
		[]string{DocumentPurposeArchiveOnly, DocumentPurposeAIRetrieval})

	var inputError *DocumentInputError
	if !errors.As(err, &inputError) {
		t.Fatalf("err = %v, want *DocumentInputError", err)
	}
	if !strings.Contains(inputError.Message, "仅归档") {
		t.Fatalf("错误信息 = %q, want 包含 仅归档", inputError.Message)
	}
}

func TestConfirmUsagesAcceptsArchiveOnlyAlone(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))

	detail, err := service.ConfirmUsages(context.Background(), "user-1", result.Detail.Document.DocumentID,
		[]string{DocumentPurposeArchiveOnly})
	if err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}
	if detail.Document.Status != DocumentStatusArchived {
		t.Fatalf("Status = %q, want %q", detail.Document.Status, DocumentStatusArchived)
	}
}

func TestConfirmUsagesDeduplicatesAndRejectsUnknownPurpose(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	detail, err := service.ConfirmUsages(context.Background(), "user-1", documentID,
		[]string{DocumentPurposeLearn, DocumentPurposeLearn, "", DocumentPurposeGeneratePlan})
	if err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}
	if len(detail.Usages) != 2 {
		t.Fatalf("Usages 数量 = %d, want 2（应去重并忽略空值）", len(detail.Usages))
	}

	if _, err := service.ConfirmUsages(context.Background(), "user-1", documentID, []string{"mastered"}); err == nil {
		t.Fatal("接受了未定义的用途，应当拒绝")
	}
}

func TestConfirmUsagesRequiresParsedChunks(t *testing.T) {
	service, repository := newDocumentServiceForTest(t)
	repository.saveChunksErr = errors.New("数据库故障")

	_, err := service.Upload(context.Background(), uploadRequest(sampleMarkdown, ""))
	if err == nil {
		t.Fatal("落库失败时 Upload 应返回错误")
	}
	if len(repository.markedFailed) != 1 {
		t.Fatalf("markedFailed = %v, want 1 条", repository.markedFailed)
	}
	documentID := repository.markedFailed[0]

	if _, err := service.ConfirmUsages(context.Background(), "user-1", documentID, []string{DocumentPurposeLearn}); err == nil {
		t.Fatal("解析失败的资料不应允许确认用途")
	}
}

// ---------------------------------------------------------------------------
// 来源与类别修改
// ---------------------------------------------------------------------------

func TestUpdateMetadataAppliesUserConfirmationAndRejectsUnknownValues(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	origin := ContentOriginUserAuthored
	kind := DocumentKindLearningNote
	detail, err := service.UpdateMetadata(context.Background(), UpdateMetadataRequest{
		UserID: "user-1", DocumentID: documentID, ContentOrigin: &origin, DocumentKind: &kind,
	})
	if err != nil {
		t.Fatalf("UpdateMetadata 出错: %v", err)
	}
	if detail.Document.ContentOrigin != origin || detail.Document.DocumentKind != kind {
		t.Fatalf("确认未生效: origin=%q kind=%q", detail.Document.ContentOrigin, detail.Document.DocumentKind)
	}
	// 来源已确认但用途仍为空，状态不能提前变成 ready。
	if detail.Document.Status != DocumentStatusPendingConfirmation {
		t.Fatalf("Status = %q, want %q", detail.Document.Status, DocumentStatusPendingConfirmation)
	}

	badOrigin := "trusted_by_ai"
	if _, err := service.UpdateMetadata(context.Background(), UpdateMetadataRequest{
		UserID: "user-1", DocumentID: documentID, ContentOrigin: &badOrigin,
	}); err == nil {
		t.Fatal("接受了未定义的内容来源，应当拒绝")
	}
}

func TestDocumentBecomesReadyOnlyAfterOriginAndPurposeConfirmed(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	// 只确认用途、来源仍待确认 → 依然是待确认。
	detail, err := service.ConfirmUsages(context.Background(), "user-1", documentID, []string{DocumentPurposeLearn})
	if err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}
	if detail.Document.Status != DocumentStatusPendingConfirmation {
		t.Fatalf("Status = %q, want %q", detail.Document.Status, DocumentStatusPendingConfirmation)
	}

	origin := ContentOriginUserAuthored
	detail, err = service.UpdateMetadata(context.Background(), UpdateMetadataRequest{
		UserID: "user-1", DocumentID: documentID, ContentOrigin: &origin,
	})
	if err != nil {
		t.Fatalf("UpdateMetadata 出错: %v", err)
	}
	if detail.Document.Status != DocumentStatusReady {
		t.Fatalf("Status = %q, want %q", detail.Document.Status, DocumentStatusReady)
	}
}

// ---------------------------------------------------------------------------
// 片段级检索开关
// ---------------------------------------------------------------------------

func TestSetChunkRetrievalRequiresDocumentLevelPurpose(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	chunks, err := service.ListSourceChunks(context.Background(), "user-1", documentID, "")
	if err != nil {
		t.Fatalf("ListSourceChunks 出错: %v", err)
	}
	chunkID := chunks[0].SourceChunkID

	// 资料级未开启“供 AI 检索”时不允许开启片段级。
	if _, err := service.SetChunkRetrieval(context.Background(), "user-1", documentID, chunkID, true); !errors.Is(err, ErrDocumentRetrievalNotEnabled) {
		t.Fatalf("err = %v, want ErrDocumentRetrievalNotEnabled", err)
	}

	if _, err := service.ConfirmUsages(context.Background(), "user-1", documentID,
		[]string{DocumentPurposeAIRetrieval}); err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}

	chunk, err := service.SetChunkRetrieval(context.Background(), "user-1", documentID, chunkID, true)
	if err != nil {
		t.Fatalf("SetChunkRetrieval 出错: %v", err)
	}
	if !chunk.RetrievalEnabled {
		t.Fatal("RetrievalEnabled = false, want true")
	}

	// 关闭始终允许，用于剔除过期或含注入指令的片段。
	if _, err := service.SetChunkRetrieval(context.Background(), "user-1", documentID, chunkID, false); err != nil {
		t.Fatalf("关闭片段检索出错: %v", err)
	}
}

func TestRevokingRetrievalPurposeDisablesAllChunks(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID
	ctx := context.Background()

	if _, err := service.ConfirmUsages(ctx, "user-1", documentID, []string{DocumentPurposeAIRetrieval}); err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}
	// 撤回全部用途后，片段不能再留下可召回状态。
	if _, err := service.ConfirmUsages(ctx, "user-1", documentID, nil); err != nil {
		t.Fatalf("撤回用途出错: %v", err)
	}

	chunks, err := service.ListSourceChunks(ctx, "user-1", documentID, "")
	if err != nil {
		t.Fatalf("ListSourceChunks 出错: %v", err)
	}
	for index, chunk := range chunks {
		if chunk.RetrievalEnabled {
			t.Fatalf("撤回授权后 chunks[%d] 仍可检索", index)
		}
	}
}

// ---------------------------------------------------------------------------
// 新版本与重试
// ---------------------------------------------------------------------------

func TestNewVersionResetsPurposesAndChunkRetrieval(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	if _, err := service.ConfirmUsages(context.Background(), "user-1", documentID,
		[]string{DocumentPurposeAIRetrieval}); err != nil {
		t.Fatalf("ConfirmUsages 出错: %v", err)
	}

	second := uploadRequest("# 修订后的标题\n\n新的正文\n", "")
	second.DocumentID = documentID
	updated := mustUpload(t, service, second)

	if len(updated.Detail.Usages) != 0 {
		t.Fatalf("新版本继承了旧用途 %v，应重新确认", updated.Detail.Usages)
	}
	chunks, err := service.ListSourceChunks(context.Background(), "user-1", documentID, "")
	if err != nil {
		t.Fatalf("ListSourceChunks 出错: %v", err)
	}
	for index, chunk := range chunks {
		if chunk.RetrievalEnabled {
			t.Fatalf("新版本 chunks[%d] 默认可检索，应为 false", index)
		}
	}
}

func TestRetryParseRecoversFromChunkPersistenceFailure(t *testing.T) {
	service, repository := newDocumentServiceForTest(t)
	repository.saveChunksErr = errors.New("数据库故障")

	if _, err := service.Upload(context.Background(), uploadRequest(sampleMarkdown, "")); err == nil {
		t.Fatal("落库失败时 Upload 应返回错误")
	}
	documentID := repository.markedFailed[0]

	repository.saveChunksErr = nil
	detail, err := service.RetryParse(context.Background(), "user-1", documentID)
	if err != nil {
		t.Fatalf("RetryParse 出错: %v", err)
	}
	if detail.ChunkCount == 0 {
		t.Fatal("重试后仍没有来源片段")
	}
	if detail.Document.Status != DocumentStatusPendingConfirmation {
		t.Fatalf("Status = %q, want %q", detail.Document.Status, DocumentStatusPendingConfirmation)
	}
}

func TestRetryParseRejectsAlreadyParsedVersion(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))

	_, err := service.RetryParse(context.Background(), "user-1", result.Detail.Document.DocumentID)
	if !errors.Is(err, ErrDocumentParseSucceeded) {
		t.Fatalf("err = %v, want ErrDocumentParseSucceeded", err)
	}
}

// ---------------------------------------------------------------------------
// 用户隔离
// ---------------------------------------------------------------------------

func TestDocumentOperationsAreIsolatedByUser(t *testing.T) {
	service, _ := newDocumentServiceForTest(t)
	result := mustUpload(t, service, uploadRequest(sampleMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	ctx := context.Background()
	if _, err := service.Get(ctx, "user-2", documentID); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("跨用户读取 err = %v, want ErrDocumentNotFound", err)
	}
	if _, err := service.ListVersions(ctx, "user-2", documentID); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("跨用户查版本 err = %v, want ErrDocumentNotFound", err)
	}
	if _, err := service.ConfirmUsages(ctx, "user-2", documentID, []string{DocumentPurposeLearn}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("跨用户确认用途 err = %v, want ErrDocumentNotFound", err)
	}
	if err := service.Delete(ctx, "user-2", documentID); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("跨用户删除 err = %v, want ErrDocumentNotFound", err)
	}

	// 幂等键也按用户隔离：同键同内容不同用户不应命中他人记录。
	other := uploadRequest(sampleMarkdown, "key-1")
	other.UserID = "user-2"
	otherResult := mustUpload(t, service, other)
	if otherResult.IdempotentHit {
		t.Fatal("跨用户命中了他人的幂等记录")
	}
}

// ---------------------------------------------------------------------------
// 枚举与哈希
// ---------------------------------------------------------------------------

func TestPurposeAndKindEnumerationsAreStable(t *testing.T) {
	if got := len(ContentOrigins()); got != 4 {
		t.Fatalf("内容来源数量 = %d, want 4", got)
	}
	if got := len(DocumentKinds()); got != 7 {
		t.Fatalf("内容类别数量 = %d, want 7", got)
	}
	if got := len(DocumentPurposes()); got != 5 {
		t.Fatalf("资料用途数量 = %d, want 5", got)
	}
	// 返回副本，调用方修改不能污染内部枚举。
	ContentOrigins()[0] = "tampered"
	if ContentOrigins()[0] == "tampered" {
		t.Fatal("ContentOrigins 返回了内部切片")
	}
}

func TestSourceChunkContentHashMatchesSHA256(t *testing.T) {
	content := "正文 A"
	if SourceChunkContentHash(content) != sha256.Sum256([]byte(content)) {
		t.Fatal("片段内容哈希与 SHA-256 不一致")
	}
}
