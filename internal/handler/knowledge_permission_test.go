package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// ---------------------------------------------------------------------------
// HTTP 边界的权限测试。
//
// 这里验证的不是服务层规则（那在 internal/service 里已经测过），
// 而是接口层有没有做对两件事：
//  1. 用户身份只来自认证中间件，绝不接受请求体或 query 里的 user_id；
//  2. 他人的资源一律映射成 404，不返回 403，也不返回 500——403 本身就是「存在」的确认。
// ---------------------------------------------------------------------------

const (
	ownerUserID      = "usr_owner"
	attackerUserID   = "usr_attacker"
	ownedDocumentID  = "11111111-1111-4111-8111-111111111111"
	ownedCandidateID = "22222222-2222-4222-8222-222222222222"
	otherCandidateID = "33333333-3333-4333-8333-333333333333"
)

// ownerScopedDocumentRepository 只认 ownerUserID：其余用户一律 ErrDocumentNotFound。
// 它同时记录服务层实际传下来的 user_id，用来证明接口层没有采信客户端自报身份。
type ownerScopedDocumentRepository struct {
	askedUserIDs []string
}

func (r *ownerScopedDocumentRepository) check(userID string) error {
	r.askedUserIDs = append(r.askedUserIDs, userID)
	if userID != ownerUserID {
		return service.ErrDocumentNotFound
	}
	return nil
}

func (r *ownerScopedDocumentRepository) FindUploadRequest(context.Context, string, string) (service.UploadRequestRecord, bool, error) {
	return service.UploadRequestRecord{}, false, nil
}

func (r *ownerScopedDocumentRepository) FindVersionByContentHash(context.Context, string, []byte) (string, bool, error) {
	return "", false, nil
}

func (r *ownerScopedDocumentRepository) CreateDocumentVersion(context.Context, service.CreateDocumentVersionParams) (service.Document, service.DocumentVersion, error) {
	return service.Document{}, service.DocumentVersion{}, service.ErrDocumentNotFound
}

func (r *ownerScopedDocumentRepository) SaveSourceChunks(context.Context, service.SaveSourceChunksParams) error {
	return nil
}

func (r *ownerScopedDocumentRepository) MarkParseFailed(context.Context, string, string, string) error {
	return nil
}

func (r *ownerScopedDocumentRepository) GetDocumentDetail(_ context.Context, userID, documentID string) (service.DocumentDetail, error) {
	if err := r.check(userID); err != nil {
		return service.DocumentDetail{}, err
	}
	return service.DocumentDetail{
		Document: service.Document{
			DocumentID:       documentID,
			UserID:           userID,
			Title:            "机主的资料",
			ContentOrigin:    service.ContentOriginUserAuthored,
			DocumentKind:     service.DocumentKindLearningNote,
			Status:           service.DocumentStatusPendingConfirmation,
			CurrentVersionID: "44444444-4444-4444-8444-444444444444",
		},
		ChunkCount: 1,
	}, nil
}

func (r *ownerScopedDocumentRepository) GetVersionRawText(_ context.Context, userID, _, _ string) (service.DocumentVersion, string, error) {
	if err := r.check(userID); err != nil {
		return service.DocumentVersion{}, "", err
	}
	return service.DocumentVersion{}, "# 机主的原文", nil
}

func (r *ownerScopedDocumentRepository) ListDocuments(_ context.Context, userID string, _ int) ([]service.DocumentListItem, error) {
	if err := r.check(userID); err != nil {
		return nil, nil
	}
	return []service.DocumentListItem{{Document: service.Document{DocumentID: ownedDocumentID, Title: "机主的资料"}}}, nil
}

func (r *ownerScopedDocumentRepository) ListVersions(_ context.Context, userID, _ string) ([]service.DocumentVersion, error) {
	if err := r.check(userID); err != nil {
		return nil, err
	}
	return []service.DocumentVersion{{VersionID: "44444444-4444-4444-8444-444444444444", VersionNo: 1}}, nil
}

func (r *ownerScopedDocumentRepository) ListSourceChunks(_ context.Context, userID, _, _ string) ([]service.SourceChunk, error) {
	if err := r.check(userID); err != nil {
		return nil, err
	}
	return []service.SourceChunk{{SourceChunkID: "55555555-5555-4555-8555-555555555555", Content: "机主的片段"}}, nil
}

func (r *ownerScopedDocumentRepository) UpdateDocumentMetadata(_ context.Context, params service.UpdateDocumentMetadataParams) error {
	return r.check(params.UserID)
}

func (r *ownerScopedDocumentRepository) ReplaceDocumentUsages(_ context.Context, params service.ReplaceDocumentUsagesParams) error {
	return r.check(params.UserID)
}

func (r *ownerScopedDocumentRepository) SetSourceChunkRetrieval(_ context.Context, userID, _, _ string, _ bool) (service.SourceChunk, error) {
	if err := r.check(userID); err != nil {
		return service.SourceChunk{}, err
	}
	return service.SourceChunk{}, nil
}

func (r *ownerScopedDocumentRepository) SoftDeleteDocument(_ context.Context, userID, _ string) error {
	return r.check(userID)
}

// ownerScopedCandidateRepository 同理：只有 ownerUserID 能看到候选与知识点。
type ownerScopedCandidateRepository struct {
	askedUserIDs []string
	resolved     bool
}

func (r *ownerScopedCandidateRepository) candidate(userID string) (service.ContentCandidate, error) {
	r.askedUserIDs = append(r.askedUserIDs, userID)
	if userID != ownerUserID {
		return service.ContentCandidate{}, service.ErrCandidateNotFound
	}
	return service.ContentCandidate{
		CandidateID:         ownedCandidateID,
		UserID:              ownerUserID,
		DocumentID:          ownedDocumentID,
		CandidateType:       service.CandidateTypeKnowledgePoint,
		Payload:             service.CandidatePayload{Title: "机主的候选"},
		Status:              service.CandidateStatusPending,
		SourceContentOrigin: service.ContentOriginUserAuthored,
		TrustLevel:          service.CandidateTrustUnverified,
	}, nil
}

func (r *ownerScopedCandidateRepository) SaveCandidates(context.Context, service.SaveCandidatesParams) ([]service.ContentCandidate, error) {
	return nil, nil
}

func (r *ownerScopedCandidateRepository) GetCandidate(_ context.Context, userID, _ string) (service.ContentCandidate, error) {
	return r.candidate(userID)
}

func (r *ownerScopedCandidateRepository) ListCandidates(_ context.Context, userID string, _ service.CandidateQuery) ([]service.ContentCandidate, error) {
	candidate, err := r.candidate(userID)
	if err != nil {
		return nil, nil // 列表接口下，他人数据表现为“空列表”，不是错误
	}
	return []service.ContentCandidate{candidate}, nil
}

func (r *ownerScopedCandidateRepository) UpdateCandidatePayload(_ context.Context, userID, _ string, payload service.CandidatePayload, _ []byte) (service.ContentCandidate, error) {
	candidate, err := r.candidate(userID)
	if err != nil {
		return service.ContentCandidate{}, err
	}
	r.resolved = true
	candidate.Payload = payload
	return candidate, nil
}

func (r *ownerScopedCandidateRepository) ResolveCandidate(_ context.Context, params service.ResolveCandidateParams) (service.ContentCandidate, error) {
	candidate, err := r.candidate(params.UserID)
	if err != nil {
		return service.ContentCandidate{}, err
	}
	r.resolved = true
	candidate.Status = params.Status
	candidate.ConfirmedOutcome = params.Outcome
	return candidate, nil
}

func (r *ownerScopedCandidateRepository) ConfirmKnowledgePointCandidate(_ context.Context, params service.ConfirmKnowledgePointParams) (service.ContentCandidate, service.KnowledgePoint, error) {
	candidate, err := r.candidate(params.UserID)
	if err != nil {
		return service.ContentCandidate{}, service.KnowledgePoint{}, err
	}
	r.resolved = true
	candidate.Status = service.CandidateStatusConfirmed
	candidate.ConfirmedOutcome = service.CandidateOutcomeKnowledgePointCreated
	return candidate, service.KnowledgePoint{KnowledgePointID: "66666666-6666-4666-8666-666666666666", UserID: params.UserID}, nil
}

func (r *ownerScopedCandidateRepository) ListKnowledgePoints(_ context.Context, userID string, _ int) ([]service.KnowledgePoint, error) {
	r.askedUserIDs = append(r.askedUserIDs, userID)
	if userID != ownerUserID {
		return nil, nil
	}
	return []service.KnowledgePoint{{KnowledgePointID: "66666666-6666-4666-8666-666666666666", UserID: ownerUserID, Title: "机主的知识点"}}, nil
}

// ---------------------------------------------------------------------------
// 测试装配
// ---------------------------------------------------------------------------

type knowledgeTestServer struct {
	server     *Server
	documents  *ownerScopedDocumentRepository
	candidates *ownerScopedCandidateRepository
}

func newKnowledgeTestServer(t *testing.T) *knowledgeTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	documentRepository := &ownerScopedDocumentRepository{}
	candidateRepository := &ownerScopedCandidateRepository{}
	documentService := service.NewDocumentService(documentRepository, service.DefaultDocumentLimits())
	return &knowledgeTestServer{
		server: &Server{
			documents:  documentService,
			candidates: service.NewCandidateService(candidateRepository, documentService, nil, service.CandidateLimits{}),
			log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		documents:  documentRepository,
		candidates: candidateRepository,
	}
}

// call 以指定用户身份调用一个 handler，并返回响应。
func (k *knowledgeTestServer) call(
	handler gin.HandlerFunc, userID, method, target, body string, params gin.Params,
) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if userID != "" {
		request = request.WithContext(context.WithValue(request.Context(), userIDKey, userID))
	}
	ginContext.Request = request
	ginContext.Params = params
	handler(ginContext)
	return recorder
}

func documentParams() gin.Params {
	return gin.Params{{Key: "document_id", Value: ownedDocumentID}}
}

func candidateParams() gin.Params {
	return gin.Params{{Key: "candidate_id", Value: ownedCandidateID}}
}

// ---------------------------------------------------------------------------
// 跨用户读取、修改、删除
// ---------------------------------------------------------------------------

func TestCrossUserDocumentAccessReturnsNotFound(t *testing.T) {
	cases := []struct {
		name    string
		handler func(*Server) gin.HandlerFunc
		method  string
		body    string
		params  gin.Params
	}{
		{"读取资料", func(s *Server) gin.HandlerFunc { return s.getDocumentHandler }, http.MethodGet, "", documentParams()},
		{"修改资料", func(s *Server) gin.HandlerFunc { return s.updateDocumentHandler }, http.MethodPatch, `{"title":"被篡改"}`, documentParams()},
		{"确认用途", func(s *Server) gin.HandlerFunc { return s.confirmDocumentUsagesHandler }, http.MethodPut, `{"purposes":["learn"]}`, documentParams()},
		{"删除资料", func(s *Server) gin.HandlerFunc { return s.deleteDocumentHandler }, http.MethodDelete, "", documentParams()},
		{"查看版本历史", func(s *Server) gin.HandlerFunc { return s.listDocumentVersionsHandler }, http.MethodGet, "", documentParams()},
		{"查看来源片段", func(s *Server) gin.HandlerFunc { return s.listDocumentChunksHandler }, http.MethodGet, "", documentParams()},
		{"重试解析", func(s *Server) gin.HandlerFunc { return s.retryDocumentParseHandler }, http.MethodPost, "", documentParams()},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newKnowledgeTestServer(t)

			recorder := rig.call(testCase.handler(rig.server), attackerUserID,
				testCase.method, "/api/v1/documents/"+ownedDocumentID, testCase.body, testCase.params)

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
			}
			// 404 的响应体不能泄露资料标题或原文。
			if strings.Contains(recorder.Body.String(), "机主") {
				t.Fatalf("响应泄露了他人资料内容: %s", recorder.Body.String())
			}
			for _, asked := range rig.documents.askedUserIDs {
				if asked != attackerUserID {
					t.Fatalf("服务层收到的 user_id = %q, want %q（接口层不得采信客户端身份）", asked, attackerUserID)
				}
			}
		})
	}
}

func TestCrossUserCandidateAccessReturnsNotFound(t *testing.T) {
	cases := []struct {
		name    string
		handler func(*Server) gin.HandlerFunc
		method  string
		body    string
	}{
		{"读取候选", func(s *Server) gin.HandlerFunc { return s.getCandidateHandler }, http.MethodGet, ""},
		{"修改候选", func(s *Server) gin.HandlerFunc { return s.updateCandidateHandler }, http.MethodPatch, `{"title":"被篡改"}`},
		{"确认候选", func(s *Server) gin.HandlerFunc { return s.confirmCandidateHandler }, http.MethodPost, `{}`},
		{"合并候选", func(s *Server) gin.HandlerFunc { return s.mergeCandidateHandler }, http.MethodPost, `{"into_candidate_id":"` + otherCandidateID + `"}`},
		{"归档候选", func(s *Server) gin.HandlerFunc { return s.archiveCandidateHandler }, http.MethodPost, `{}`},
		{"拒绝候选", func(s *Server) gin.HandlerFunc { return s.rejectCandidateHandler }, http.MethodPost, `{}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newKnowledgeTestServer(t)

			recorder := rig.call(testCase.handler(rig.server), attackerUserID,
				testCase.method, "/api/v1/candidates/"+ownedCandidateID, testCase.body, candidateParams())

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
			}
			if rig.candidates.resolved {
				t.Fatal("跨用户请求改动了他人候选")
			}
			if strings.Contains(recorder.Body.String(), "机主") {
				t.Fatalf("响应泄露了他人候选内容: %s", recorder.Body.String())
			}
		})
	}
}

// 列表接口对他人数据表现为空列表，而不是报错或返回他人条目。
func TestCrossUserListsReturnEmpty(t *testing.T) {
	rig := newKnowledgeTestServer(t)

	for name, handler := range map[string]gin.HandlerFunc{
		"资料列表":  rig.server.listDocumentsHandler,
		"候选列表":  rig.server.listCandidatesHandler,
		"知识点列表": rig.server.listKnowledgePointsHandler,
	} {
		recorder := rig.call(handler, attackerUserID, http.MethodGet, "/api/v1/x", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", name, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "机主") {
			t.Fatalf("%s 返回了他人数据: %s", name, recorder.Body.String())
		}
	}
}

// 机主自己访问必须正常，否则上面的 404 可能是「谁都 404」的假通过。
func TestOwnerAccessSucceeds(t *testing.T) {
	rig := newKnowledgeTestServer(t)

	recorder := rig.call(rig.server.getDocumentHandler, ownerUserID,
		http.MethodGet, "/api/v1/documents/"+ownedDocumentID, "", documentParams())
	if recorder.Code != http.StatusOK {
		t.Fatalf("机主读取资料 status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "机主的资料") {
		t.Fatalf("机主没拿到自己的资料: %s", recorder.Body.String())
	}

	recorder = rig.call(rig.server.getCandidateHandler, ownerUserID,
		http.MethodGet, "/api/v1/candidates/"+ownedCandidateID, "", candidateParams())
	if recorder.Code != http.StatusOK {
		t.Fatalf("机主读取候选 status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
}

// 未认证请求一律 401，不进入服务层。
func TestKnowledgeEndpointsRequireAuthentication(t *testing.T) {
	rig := newKnowledgeTestServer(t)

	for name, handler := range map[string]gin.HandlerFunc{
		"资料详情": rig.server.getDocumentHandler,
		"候选详情": rig.server.getCandidateHandler,
		"候选列表": rig.server.listCandidatesHandler,
		"知识点":  rig.server.listKnowledgePointsHandler,
	} {
		recorder := rig.call(handler, "", http.MethodGet, "/api/v1/x", "", append(documentParams(), candidateParams()...))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", name, recorder.Code)
		}
	}
	if len(rig.documents.askedUserIDs) != 0 || len(rig.candidates.askedUserIDs) != 0 {
		t.Fatal("未认证请求进入了服务层")
	}
}

// 抽取未启用时返回 503，而不是 500：这是能力缺失，不是服务器故障。
func TestExtractWithoutExtractorReturnsServiceUnavailable(t *testing.T) {
	rig := newKnowledgeTestServer(t)

	recorder := rig.call(rig.server.extractDocumentCandidatesHandler, ownerUserID,
		http.MethodPost, "/api/v1/documents/"+ownedDocumentID+"/candidates/extract", "", documentParams())

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
}
