package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 这组测试守的是产品红线，不是实现细节：
// AI 只能提候选，正式知识点/计划/事实/生产证据都必须由用户显式确认。
// ---------------------------------------------------------------------------

type fakeCandidateDocuments struct {
	detail DocumentDetail
	chunks []SourceChunk
	getErr error
}

func (f *fakeCandidateDocuments) Get(context.Context, string, string) (DocumentDetail, error) {
	if f.getErr != nil {
		return DocumentDetail{}, f.getErr
	}
	return f.detail, nil
}

func (f *fakeCandidateDocuments) ListSourceChunks(context.Context, string, string, string) ([]SourceChunk, error) {
	return f.chunks, nil
}

type fakeCandidateExtractor struct {
	proposals []CandidateProposal
	err       error
	lastInput CandidateExtractionInput
	calls     int
}

func (f *fakeCandidateExtractor) Extract(_ context.Context, input CandidateExtractionInput) ([]CandidateProposal, error) {
	f.lastInput = input
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.proposals, nil
}

// fakeCandidateRepository 只做最小的内存实现，用来观察服务层到底写了什么。
type fakeCandidateRepository struct {
	candidates      map[string]ContentCandidate
	knowledgePoints map[string]KnowledgePoint
	saved           SaveCandidatesParams
	resolved        ResolveCandidateParams
	confirmedKP     ConfirmKnowledgePointParams
	nextID          int

	// saveErr / resolveErr 用来模拟数据库故障，验证故障时不会留下半成品数据。
	saveErr       error
	resolveErr    error
	beforeResolve func(*fakeCandidateRepository, ResolveCandidateParams)
}

func newFakeCandidateRepository() *fakeCandidateRepository {
	return &fakeCandidateRepository{
		candidates:      map[string]ContentCandidate{},
		knowledgePoints: map[string]KnowledgePoint{},
	}
}

func (f *fakeCandidateRepository) put(candidate ContentCandidate) ContentCandidate {
	if candidate.CandidateID == "" {
		f.nextID++
		candidate.CandidateID = string(rune('a'+f.nextID)) + "-candidate"
	}
	f.candidates[candidate.CandidateID] = candidate
	return candidate
}

func (f *fakeCandidateRepository) SaveCandidates(_ context.Context, params SaveCandidatesParams) ([]ContentCandidate, error) {
	f.saved = params
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	saved := make([]ContentCandidate, 0, len(params.Candidates))
	for _, item := range params.Candidates {
		saved = append(saved, f.put(ContentCandidate{
			UserID:     params.UserID,
			DocumentID: params.DocumentID,
			VersionID:  params.VersionID,

			CandidateType:       item.CandidateType,
			Payload:             item.Payload,
			Status:              CandidateStatusPending,
			SourceContentOrigin: params.ContentOrigin,
			TrustLevel:          CandidateTrustUnverified,
			Sources:             item.Sources,
		}))
	}
	return saved, nil
}

// GetCandidate 按 user_id 隔离：他人的候选一律按「不存在」处理，不泄露存在性。
func (f *fakeCandidateRepository) GetCandidate(_ context.Context, userID, candidateID string) (ContentCandidate, error) {
	candidate, found := f.candidates[candidateID]
	if !found || candidate.UserID != userID {
		return ContentCandidate{}, ErrCandidateNotFound
	}
	return candidate, nil
}

func (f *fakeCandidateRepository) ListCandidates(_ context.Context, userID string, _ CandidateQuery) ([]ContentCandidate, error) {
	list := make([]ContentCandidate, 0, len(f.candidates))
	for _, candidate := range f.candidates {
		if candidate.UserID == userID {
			list = append(list, candidate)
		}
	}
	return list, nil
}

func (f *fakeCandidateRepository) UpdateCandidatePayload(_ context.Context, userID, candidateID string, payload CandidatePayload, _ []byte) (ContentCandidate, error) {
	candidate, found := f.candidates[candidateID]
	if !found || candidate.UserID != userID {
		return ContentCandidate{}, ErrCandidateNotFound
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, ErrCandidateResolved
	}
	candidate.Payload = payload
	f.candidates[candidateID] = candidate
	return candidate, nil
}

func (f *fakeCandidateRepository) ResolveCandidate(_ context.Context, params ResolveCandidateParams) (ContentCandidate, error) {
	f.resolved = params
	if f.resolveErr != nil {
		return ContentCandidate{}, f.resolveErr
	}
	if f.beforeResolve != nil {
		f.beforeResolve(f, params)
	}
	candidate, found := f.candidates[params.CandidateID]
	if !found || candidate.UserID != params.UserID {
		return ContentCandidate{}, ErrCandidateNotFound
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, ErrCandidateResolved
	}
	if params.MergedIntoCandidateID != "" {
		target, found := f.candidates[params.MergedIntoCandidateID]
		if !found || target.UserID != params.UserID || target.Status != CandidateStatusPending || target.CandidateType != candidate.CandidateType {
			return ContentCandidate{}, ErrCandidateResolved
		}
	}
	candidate.Status = params.Status
	candidate.ConfirmedOutcome = params.Outcome
	candidate.TrustLevel = params.TrustLevel
	candidate.MergedIntoCandidateID = params.MergedIntoCandidateID
	candidate.DecisionNote = params.DecisionNote
	if params.Payload != nil {
		candidate.Payload = *params.Payload
	}
	f.candidates[params.CandidateID] = candidate
	return candidate, nil
}

func (f *fakeCandidateRepository) ConfirmKnowledgePointCandidate(_ context.Context, params ConfirmKnowledgePointParams) (ContentCandidate, KnowledgePoint, error) {
	f.confirmedKP = params
	if f.resolveErr != nil {
		return ContentCandidate{}, KnowledgePoint{}, f.resolveErr
	}
	candidate, found := f.candidates[params.CandidateID]
	if !found || candidate.UserID != params.UserID {
		return ContentCandidate{}, KnowledgePoint{}, ErrCandidateNotFound
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, KnowledgePoint{}, ErrCandidateResolved
	}

	knowledgePointID := params.KnowledgePointID
	status := CandidateStatusLinked
	outcome := CandidateOutcomeKnowledgePointLinked
	if knowledgePointID == "" {
		f.nextID++
		knowledgePointID = "kp-" + string(rune('a'+f.nextID))
		status = CandidateStatusConfirmed
		outcome = CandidateOutcomeKnowledgePointCreated
	}
	knowledgePoint, found := f.knowledgePoints[knowledgePointID]
	if found && knowledgePoint.UserID != params.UserID {
		// 关联已有项必须按 user_id 过滤，他人知识点按不存在处理。
		return ContentCandidate{}, KnowledgePoint{}, ErrCandidateNotFound
	}
	if !found {
		knowledgePoint = KnowledgePoint{
			KnowledgePointID: knowledgePointID,
			UserID:           params.UserID,
			Title:            params.Title,
			Description:      params.Description,
			Status:           "active",
		}
		f.knowledgePoints[knowledgePointID] = knowledgePoint
	}

	candidate.Status = status
	candidate.ConfirmedOutcome = outcome
	candidate.TrustLevel = params.TrustLevel
	candidate.TargetKnowledgePointID = knowledgePointID
	candidate.Payload = params.Payload
	candidate.DecisionNote = params.DecisionNote
	f.candidates[params.CandidateID] = candidate
	return candidate, knowledgePoint, nil
}

func (f *fakeCandidateRepository) ListKnowledgePoints(_ context.Context, userID string, _ int) ([]KnowledgePoint, error) {
	list := make([]KnowledgePoint, 0, len(f.knowledgePoints))
	for _, knowledgePoint := range f.knowledgePoints {
		if knowledgePoint.UserID == userID {
			list = append(list, knowledgePoint)
		}
	}
	return list, nil
}

func usages(purposes ...string) []DocumentUsage {
	list := make([]DocumentUsage, 0, len(purposes))
	for _, purpose := range purposes {
		list = append(list, DocumentUsage{Purpose: purpose, Enabled: true})
	}
	return list
}

func candidateFixture(kind string, purposes []string) *fakeCandidateDocuments {
	return &fakeCandidateDocuments{
		detail: DocumentDetail{
			Document: Document{
				DocumentID:       "doc-1",
				Title:            "资料",
				ContentOrigin:    ContentOriginUserAuthored,
				DocumentKind:     kind,
				CurrentVersionID: "ver-1",
			},
			Usages:     usages(purposes...),
			ChunkCount: 1,
		},
		chunks: []SourceChunk{{
			SourceChunkID: "chunk-1",
			DocumentID:    "doc-1",
			VersionID:     "ver-1",
			Ordinal:       1,
			Content:       "Kafka 消费幂等需要业务唯一键去重。",
		}},
	}
}

func newCandidateFixture(t *testing.T, kind string, purposes []string, proposals []CandidateProposal) (*CandidateService, *fakeCandidateRepository, *fakeCandidateExtractor) {
	t.Helper()
	repository := newFakeCandidateRepository()
	extractor := &fakeCandidateExtractor{proposals: proposals}
	service := NewCandidateService(repository, candidateFixture(kind, purposes), extractor, CandidateLimits{})
	service.now = func() time.Time { return time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC) }
	return service, repository, extractor
}

func knowledgePointProposal() CandidateProposal {
	return CandidateProposal{
		CandidateType: CandidateTypeKnowledgePoint,
		Title:         "Kafka 消费幂等",
		Summary:       "用业务唯一键去重",
		Sources: []CandidateProposalSource{{
			Ref:           "S1",
			EvidenceQuote: "业务唯一键去重",
		}},
	}
}

// 抽取只产生待确认候选，一条正式数据都不写。
func TestExtractOnlyProducesPendingCandidates(t *testing.T) {
	service, repository, _ := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeLearn}, []CandidateProposal{knowledgePointProposal()})

	result, err := service.Extract(context.Background(), "user-1", "doc-1")
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("期望 1 条候选，实际 %d 条", len(result.Candidates))
	}
	candidate := result.Candidates[0]
	if candidate.Status != CandidateStatusPending {
		t.Fatalf("抽取结果必须是待确认，实际 %q", candidate.Status)
	}
	if candidate.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("抽取结果可信级别必须是 unverified，实际 %q", candidate.TrustLevel)
	}
	if candidate.ConfirmedOutcome != "" {
		t.Fatalf("未确认的候选不该有确认结果，实际 %q", candidate.ConfirmedOutcome)
	}
	if len(repository.knowledgePoints) != 0 {
		t.Fatalf("抽取不得创建知识点，实际创建了 %d 个", len(repository.knowledgePoints))
	}
}

// 每条候选必须引用至少一个来源片段，缺引用或引用不存在都整批拒绝。
func TestExtractRequiresValidSources(t *testing.T) {
	cases := []struct {
		name     string
		proposal CandidateProposal
	}{
		{
			name: "没有任何来源引用",
			proposal: CandidateProposal{
				CandidateType: CandidateTypeKnowledgePoint,
				Title:         "Kafka 消费幂等",
			},
		},
		{
			name: "引用了不存在的片段",
			proposal: CandidateProposal{
				CandidateType: CandidateTypeKnowledgePoint,
				Title:         "Kafka 消费幂等",
				Sources:       []CandidateProposalSource{{Ref: "S9"}},
			},
		},
		{
			name: "证据原文不在片段里",
			proposal: CandidateProposal{
				CandidateType: CandidateTypeKnowledgePoint,
				Title:         "Kafka 消费幂等",
				Sources:       []CandidateProposalSource{{Ref: "S1", EvidenceQuote: "模型编造的原文"}},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, repository, _ := newCandidateFixture(t,
				DocumentKindLearningNote, []string{DocumentPurposeLearn}, []CandidateProposal{testCase.proposal})

			if _, err := service.Extract(context.Background(), "user-1", "doc-1"); !errors.Is(err, ErrCandidateInvalidOutput) {
				t.Fatalf("期望 ErrCandidateInvalidOutput，实际 %v", err)
			}
			if len(repository.candidates) != 0 {
				t.Fatalf("非法输出必须整批拒绝，实际落库 %d 条", len(repository.candidates))
			}
		})
	}
}

// 资料类别 × 用途共同决定允许的候选类型，模型越界的提案被丢弃。
func TestAllowedCandidateTypesByKindAndPurpose(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		purposes []string
		expected []string
	}{
		{"学习 Todo 只能提计划任务候选", DocumentKindLearningTodo, []string{DocumentPurposeGeneratePlan}, []string{CandidateTypePlanTask}},
		{"目标 JD 只能提要求候选", DocumentKindTargetJD, []string{DocumentPurposeFactReference}, []string{CandidateTypeJDRequirement}},
		{"项目事实只能提待核实事实候选", DocumentKindProjectFact, []string{DocumentPurposeFactReference}, []string{CandidateTypePersonalFact}},
		{"仅供 AI 检索只产生参考资料候选", DocumentKindLearningNote, []string{DocumentPurposeAIRetrieval}, []string{CandidateTypeReferenceOnly}},
		{"学习用途才产生知识点候选", DocumentKindLearningNote, []string{DocumentPurposeLearn}, []string{CandidateTypeKnowledgePoint}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			allowed := allowedCandidateTypes(testCase.kind, usages(testCase.purposes...))
			if len(allowed) != len(testCase.expected) {
				t.Fatalf("期望 %v，实际 %v", testCase.expected, allowed)
			}
			for index, candidateType := range testCase.expected {
				if allowed[index] != candidateType {
					t.Fatalf("期望 %v，实际 %v", testCase.expected, allowed)
				}
			}
		})
	}
}

// 仅供 AI 检索的资料不会产生知识点候选：模型硬提也会被丢弃。
func TestExtractFiltersDisallowedTypes(t *testing.T) {
	service, repository, extractor := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeAIRetrieval}, []CandidateProposal{knowledgePointProposal()})

	result, err := service.Extract(context.Background(), "user-1", "doc-1")
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if result.Filtered != 1 || len(result.Candidates) != 0 {
		t.Fatalf("越界候选必须被丢弃，实际 filtered=%d saved=%d", result.Filtered, len(result.Candidates))
	}
	if len(repository.candidates) != 0 {
		t.Fatalf("越界候选不得落库")
	}
	if len(extractor.lastInput.AllowedTypes) != 1 || extractor.lastInput.AllowedTypes[0] != CandidateTypeReferenceOnly {
		t.Fatalf("送入模型的允许类型不对: %v", extractor.lastInput.AllowedTypes)
	}
}

// 仅归档资料不参与抽取。
func TestExtractRejectsArchiveOnlyDocument(t *testing.T) {
	service, _, _ := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeLearn, DocumentPurposeArchiveOnly}, nil)

	if _, err := service.Extract(context.Background(), "user-1", "doc-1"); !errors.Is(err, ErrCandidateDocumentArchived) {
		t.Fatalf("期望 ErrCandidateDocumentArchived，实际 %v", err)
	}
}

// 未配置抽取模型时，抽取不可用，但人工确认链路不受影响。
func TestExtractWithoutExtractorUnavailable(t *testing.T) {
	service := NewCandidateService(newFakeCandidateRepository(),
		candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})

	if _, err := service.Extract(context.Background(), "user-1", "doc-1"); !errors.Is(err, ErrCandidateExtractorUnavailable) {
		t.Fatalf("期望 ErrCandidateExtractorUnavailable，实际 %v", err)
	}
}

// 知识点候选确认后进入正式知识库，初始 UI 状态是「暂无证据」。
func TestConfirmKnowledgePointCandidate(t *testing.T) {
	service, repository, _ := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeLearn}, []CandidateProposal{knowledgePointProposal()})

	extracted, err := service.Extract(context.Background(), "user-1", "doc-1")
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	result, err := service.Confirm(context.Background(), ConfirmCandidateRequest{
		UserID:      "user-1",
		CandidateID: extracted.Candidates[0].CandidateID,
	})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if result.KnowledgePoint == nil {
		t.Fatal("确认知识点候选必须产生知识点")
	}
	if result.MasteryUIState != MasteryUIStateNoEvidence {
		t.Fatalf("新知识点 UI 状态必须是暂无证据，实际 %q", result.MasteryUIState)
	}
	if result.KnowledgePoint.MasteryUIState() != MasteryUIStateNoEvidence {
		t.Fatalf("知识点不得带任何掌握等级")
	}
	if result.Candidate.ConfirmedOutcome != CandidateOutcomeKnowledgePointCreated {
		t.Fatalf("期望 knowledge_point_created，实际 %q", result.Candidate.ConfirmedOutcome)
	}
	if len(repository.knowledgePoints) != 1 {
		t.Fatalf("期望创建 1 个知识点，实际 %d 个", len(repository.knowledgePoints))
	}
}

// 关联已有知识点不会重复创建。
func TestConfirmLinksExistingKnowledgePoint(t *testing.T) {
	repository := newFakeCandidateRepository()
	repository.knowledgePoints["kp-existing"] = KnowledgePoint{KnowledgePointID: "kp-existing", UserID: "user-1", Title: "已有知识点"}
	candidate := repository.put(ContentCandidate{
		CandidateID: "cand-1", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "Kafka 消费幂等"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})

	service := NewCandidateService(repository, candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})
	result, err := service.Confirm(context.Background(), ConfirmCandidateRequest{
		UserID: "user-1", CandidateID: candidate.CandidateID, KnowledgePointID: "kp-existing",
	})
	if err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	if len(repository.knowledgePoints) != 1 {
		t.Fatalf("关联已有项不得新建知识点，实际 %d 个", len(repository.knowledgePoints))
	}
	if result.Candidate.Status != CandidateStatusLinked ||
		result.Candidate.ConfirmedOutcome != CandidateOutcomeKnowledgePointLinked {
		t.Fatalf("关联结果不对: status=%q outcome=%q", result.Candidate.Status, result.Candidate.ConfirmedOutcome)
	}
}

// 确认非知识点候选时，每种类型能走多远都有硬边界。
func TestConfirmOutcomeAndTrustByType(t *testing.T) {
	cases := []struct {
		name            string
		candidateType   string
		contentOrigin   string
		expectedOutcome string
		expectedTrust   string
	}{
		{"计划任务候选只到待接入计划", CandidateTypePlanTask, ContentOriginUserAuthored, CandidateOutcomePlanTaskPending, CandidateTrustUserConfirmed},
		{"JD 要求候选只到待接入目标", CandidateTypeJDRequirement, ContentOriginUserAuthored, CandidateOutcomeJDRequirementPending, CandidateTrustUserConfirmed},
		{"项目事实确认后只是待核实事实", CandidateTypePersonalFact, ContentOriginUserAuthored, CandidateOutcomeUnverifiedFact, CandidateTrustUnverified},
		{"仅供检索资料确认后只是参考", CandidateTypeReferenceOnly, ContentOriginUserAuthored, CandidateOutcomeReferenceOnly, CandidateTrustUserConfirmed},
		{"AI 整理内容确认后不提升可信级别", CandidateTypeReferenceOnly, ContentOriginAIGenerated, CandidateOutcomeReferenceOnly, CandidateTrustUnverified},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := newFakeCandidateRepository()
			candidate := repository.put(ContentCandidate{
				CandidateID: "cand-1", UserID: "user-1", CandidateType: testCase.candidateType,
				Payload: CandidatePayload{Title: "候选标题"}, Status: CandidateStatusPending,
				SourceContentOrigin: testCase.contentOrigin, TrustLevel: CandidateTrustUnverified,
			})
			service := NewCandidateService(repository, candidateFixture(DocumentKindOther, []string{DocumentPurposeLearn}), nil, CandidateLimits{})

			result, err := service.Confirm(context.Background(), ConfirmCandidateRequest{UserID: "user-1", CandidateID: candidate.CandidateID})
			if err != nil {
				t.Fatalf("确认失败: %v", err)
			}
			if result.KnowledgePoint != nil {
				t.Fatal("非知识点候选确认后不得创建知识点")
			}
			if result.Candidate.ConfirmedOutcome != testCase.expectedOutcome {
				t.Fatalf("期望结果 %q，实际 %q", testCase.expectedOutcome, result.Candidate.ConfirmedOutcome)
			}
			if result.Candidate.TrustLevel != testCase.expectedTrust {
				t.Fatalf("期望可信级别 %q，实际 %q", testCase.expectedTrust, result.Candidate.TrustLevel)
			}
		})
	}
}

// AI 整理的知识点候选，确认后来源标记与可信级别都不被洗掉。
func TestConfirmKeepsAIGeneratedOriginUnverified(t *testing.T) {
	repository := newFakeCandidateRepository()
	candidate := repository.put(ContentCandidate{
		CandidateID: "cand-1", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "AI 整理的知识点"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginAIGenerated, TrustLevel: CandidateTrustUnverified,
	})
	service := NewCandidateService(repository, candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})

	result, err := service.Confirm(context.Background(), ConfirmCandidateRequest{UserID: "user-1", CandidateID: candidate.CandidateID})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if result.Candidate.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("AI 整理内容确认后可信级别必须仍是 unverified，实际 %q", result.Candidate.TrustLevel)
	}
	if result.Candidate.SourceContentOrigin != ContentOriginAIGenerated {
		t.Fatalf("AI 整理来源标记不得被洗掉，实际 %q", result.Candidate.SourceContentOrigin)
	}
}

// 修改只改正文，改完仍是待确认候选。
func TestModifyKeepsCandidatePending(t *testing.T) {
	repository := newFakeCandidateRepository()
	candidate := repository.put(ContentCandidate{
		CandidateID: "cand-1", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "原标题"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})
	service := NewCandidateService(repository, candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})

	updated, err := service.Modify(context.Background(), "user-1", candidate.CandidateID, CandidatePayload{Title: "  改后的标题  "})
	if err != nil {
		t.Fatalf("修改失败: %v", err)
	}
	if updated.Payload.Title != "改后的标题" {
		t.Fatalf("标题未按预期规范化: %q", updated.Payload.Title)
	}
	if updated.Status != CandidateStatusPending || updated.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("修改不得改变状态或可信级别: status=%q trust=%q", updated.Status, updated.TrustLevel)
	}
	if _, err := service.Modify(context.Background(), "user-1", candidate.CandidateID, CandidatePayload{Title: ""}); err == nil {
		t.Fatal("空标题必须报错")
	}
}

// 合并、归档、拒绝都是可追溯的终态决定，且不提升可信级别。
func TestMergeArchiveReject(t *testing.T) {
	repository := newFakeCandidateRepository()
	newPending := func(id, candidateType string) ContentCandidate {
		return repository.put(ContentCandidate{
			CandidateID: id, UserID: "user-1", CandidateType: candidateType,
			Payload: CandidatePayload{Title: id}, Status: CandidateStatusPending,
			SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
		})
	}
	source := newPending("cand-source", CandidateTypeKnowledgePoint)
	target := newPending("cand-target", CandidateTypeKnowledgePoint)
	other := newPending("cand-other", CandidateTypePlanTask)
	archived := newPending("cand-archive", CandidateTypeReferenceOnly)
	rejected := newPending("cand-reject", CandidateTypeReferenceOnly)

	service := NewCandidateService(repository, candidateFixture(DocumentKindOther, []string{DocumentPurposeLearn}), nil, CandidateLimits{})
	ctx := context.Background()

	if _, err := service.Merge(ctx, "user-1", source.CandidateID, other.CandidateID, ""); err == nil {
		t.Fatal("跨类型合并必须报错")
	}
	if _, err := service.Merge(ctx, "user-1", source.CandidateID, source.CandidateID, ""); err == nil {
		t.Fatal("合并到自身必须报错")
	}

	merged, err := service.Merge(ctx, "user-1", source.CandidateID, target.CandidateID, "重复")
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if merged.Status != CandidateStatusMerged || merged.MergedIntoCandidateID != target.CandidateID {
		t.Fatalf("合并结果不对: status=%q into=%q", merged.Status, merged.MergedIntoCandidateID)
	}
	if merged.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("合并不得提升可信级别，实际 %q", merged.TrustLevel)
	}

	archivedResult, err := service.Archive(ctx, "user-1", archived.CandidateID, "留档")
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if archivedResult.Status != CandidateStatusArchived || archivedResult.ConfirmedOutcome != CandidateOutcomeArchived {
		t.Fatalf("归档结果不对: %+v", archivedResult)
	}

	rejectedResult, err := service.Reject(ctx, "user-1", rejected.CandidateID, "不需要")
	if err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	if rejectedResult.Status != CandidateStatusRejected || rejectedResult.ConfirmedOutcome != CandidateOutcomeRejected {
		t.Fatalf("拒绝结果不对: %+v", rejectedResult)
	}
	if rejectedResult.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("拒绝不得改变可信级别")
	}
}

func TestMergeRejectsTargetResolvedAfterValidation(t *testing.T) {
	repository := newFakeCandidateRepository()
	service := NewCandidateService(repository, &fakeCandidateDocuments{}, nil, DefaultCandidateLimits())
	source := repository.put(ContentCandidate{
		CandidateID: "source", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Status: CandidateStatusPending, TrustLevel: CandidateTrustUnverified,
	})
	target := repository.put(ContentCandidate{
		CandidateID: "target", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Status: CandidateStatusPending, TrustLevel: CandidateTrustUnverified,
	})
	repository.beforeResolve = func(repository *fakeCandidateRepository, _ ResolveCandidateParams) {
		resolvedTarget := repository.candidates[target.CandidateID]
		resolvedTarget.Status = CandidateStatusConfirmed
		repository.candidates[target.CandidateID] = resolvedTarget
	}

	_, err := service.Merge(context.Background(), "user-1", source.CandidateID, target.CandidateID, "")
	if !errors.Is(err, ErrCandidateResolved) {
		t.Fatalf("err = %v, want ErrCandidateResolved", err)
	}
	if got := repository.candidates[source.CandidateID].Status; got != CandidateStatusPending {
		t.Fatalf("source status = %q, want pending", got)
	}
}

// 终态候选不可重复处理。
func TestResolvedCandidateCannotBeProcessedAgain(t *testing.T) {
	repository := newFakeCandidateRepository()
	candidate := repository.put(ContentCandidate{
		CandidateID: "cand-1", UserID: "user-1", CandidateType: CandidateTypeReferenceOnly,
		Payload: CandidatePayload{Title: "已处理"}, Status: CandidateStatusRejected,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})
	service := NewCandidateService(repository, candidateFixture(DocumentKindOther, []string{DocumentPurposeLearn}), nil, CandidateLimits{})
	ctx := context.Background()

	if _, err := service.Confirm(ctx, ConfirmCandidateRequest{UserID: "user-1", CandidateID: candidate.CandidateID}); !errors.Is(err, ErrCandidateResolved) {
		t.Fatalf("重复确认期望 ErrCandidateResolved，实际 %v", err)
	}
	if _, err := service.Archive(ctx, "user-1", candidate.CandidateID, ""); !errors.Is(err, ErrCandidateResolved) {
		t.Fatalf("重复归档期望 ErrCandidateResolved，实际 %v", err)
	}
	if _, err := service.Modify(ctx, "user-1", candidate.CandidateID, CandidatePayload{Title: "改"}); !errors.Is(err, ErrCandidateResolved) {
		t.Fatalf("修改终态候选期望 ErrCandidateResolved，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 故障路径：模型超时、数据库失败
// ---------------------------------------------------------------------------

// LLM 超时或调用失败时，抽取整体失败，不留下任何半成品候选。
func TestExtractPropagatesExtractorFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"模型超时", context.DeadlineExceeded},
		{"模型输出非法", ErrCandidateInvalidOutput},
		{"上游不可用", errors.New("upstream 502")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, repository, extractor := newCandidateFixture(t,
				DocumentKindLearningNote, []string{DocumentPurposeLearn}, nil)
			extractor.err = testCase.err

			_, err := service.Extract(context.Background(), "user-1", "doc-1")
			if !errors.Is(err, testCase.err) {
				t.Fatalf("期望透传 %v，实际 %v", testCase.err, err)
			}
			if len(repository.candidates) != 0 {
				t.Fatalf("抽取失败仍落库 %d 条候选", len(repository.candidates))
			}
		})
	}
}

// 抽取超时可以直接重试：失败不改变任何状态，所以重试是安全的。
func TestExtractRetryAfterTimeoutSucceeds(t *testing.T) {
	service, repository, extractor := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeLearn}, []CandidateProposal{knowledgePointProposal()})
	extractor.err = context.DeadlineExceeded

	if _, err := service.Extract(context.Background(), "user-1", "doc-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望超时错误，实际 %v", err)
	}

	extractor.err = nil
	result, err := service.Extract(context.Background(), "user-1", "doc-1")
	if err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("重试后候选数 = %d, want 1", len(result.Candidates))
	}
	if extractor.calls != 2 {
		t.Fatalf("模型调用次数 = %d, want 2", extractor.calls)
	}
	if len(repository.candidates) != 1 {
		t.Fatalf("重试产生了重复候选: %d 条", len(repository.candidates))
	}
}

// 数据库落库失败时抽取整体失败；恢复后重试仍然只产生待确认候选。
func TestExtractFailsWhenRepositoryFails(t *testing.T) {
	service, repository, _ := newCandidateFixture(t,
		DocumentKindLearningNote, []string{DocumentPurposeLearn}, []CandidateProposal{knowledgePointProposal()})
	repository.saveErr = errors.New("数据库故障")

	if _, err := service.Extract(context.Background(), "user-1", "doc-1"); err == nil {
		t.Fatal("数据库故障时抽取应返回错误")
	}
	if len(repository.candidates) != 0 {
		t.Fatal("数据库故障后留下了候选记录")
	}

	repository.saveErr = nil
	result, err := service.Extract(context.Background(), "user-1", "doc-1")
	if err != nil {
		t.Fatalf("恢复后重试失败: %v", err)
	}
	if result.Candidates[0].Status != CandidateStatusPending {
		t.Fatalf("重试结果状态 = %q, want pending", result.Candidates[0].Status)
	}
}

// 确认时数据库失败：候选必须仍停在待确认，不能出现「知识点建了但候选没落终态」。
func TestConfirmFailsWhenRepositoryFails(t *testing.T) {
	repository := newFakeCandidateRepository()
	repository.resolveErr = errors.New("数据库故障")
	candidate := repository.put(ContentCandidate{
		CandidateID: "cand-1", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "Kafka 消费幂等"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})
	service := NewCandidateService(repository, candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})

	if _, err := service.Confirm(context.Background(), ConfirmCandidateRequest{UserID: "user-1", CandidateID: candidate.CandidateID}); err == nil {
		t.Fatal("数据库故障时确认应返回错误")
	}
	if len(repository.knowledgePoints) != 0 {
		t.Fatal("确认失败却创建了知识点")
	}
	if repository.candidates["cand-1"].Status != CandidateStatusPending {
		t.Fatalf("确认失败后候选状态 = %q, want pending", repository.candidates["cand-1"].Status)
	}
}

// ---------------------------------------------------------------------------
// 用户隔离
// ---------------------------------------------------------------------------

// 跨用户读取、修改、处理候选一律按「不存在」处理，不泄露存在性。
func TestCandidateOperationsAreIsolatedByUser(t *testing.T) {
	repository := newFakeCandidateRepository()
	repository.put(ContentCandidate{
		CandidateID: "cand-owner", UserID: "user-1", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "别人的候选"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})
	repository.knowledgePoints["kp-owner"] = KnowledgePoint{KnowledgePointID: "kp-owner", UserID: "user-1", Title: "别人的知识点"}
	service := NewCandidateService(repository, candidateFixture(DocumentKindLearningNote, []string{DocumentPurposeLearn}), nil, CandidateLimits{})
	ctx := context.Background()

	operations := map[string]func() error{
		"读取": func() error {
			_, err := service.Get(ctx, "user-2", "cand-owner")
			return err
		},
		"修改": func() error {
			_, err := service.Modify(ctx, "user-2", "cand-owner", CandidatePayload{Title: "篡改"})
			return err
		},
		"确认": func() error {
			_, err := service.Confirm(ctx, ConfirmCandidateRequest{UserID: "user-2", CandidateID: "cand-owner"})
			return err
		},
		"合并": func() error {
			_, err := service.Merge(ctx, "user-2", "cand-owner", "cand-other", "")
			return err
		},
		"归档": func() error {
			_, err := service.Archive(ctx, "user-2", "cand-owner", "")
			return err
		},
		"拒绝": func() error {
			_, err := service.Reject(ctx, "user-2", "cand-owner", "")
			return err
		},
	}
	for name, operation := range operations {
		if err := operation(); !errors.Is(err, ErrCandidateNotFound) {
			t.Fatalf("跨用户%s err = %v, want ErrCandidateNotFound", name, err)
		}
	}

	if repository.candidates["cand-owner"].Status != CandidateStatusPending {
		t.Fatal("他人候选被跨用户操作改动了")
	}

	// 列表与知识点列表也必须按 user_id 过滤，不能靠前端筛。
	list, err := service.List(ctx, "user-2", CandidateQuery{})
	if err != nil {
		t.Fatalf("列表出错: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("跨用户列表返回了 %d 条他人候选", len(list))
	}
	points, err := service.ListKnowledgePoints(ctx, "user-2")
	if err != nil {
		t.Fatalf("知识点列表出错: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("跨用户返回了 %d 个他人知识点", len(points))
	}

	// 关联已有知识点同样不能跨用户借用他人知识点。
	own := repository.put(ContentCandidate{
		CandidateID: "cand-self", UserID: "user-2", CandidateType: CandidateTypeKnowledgePoint,
		Payload: CandidatePayload{Title: "自己的候选"}, Status: CandidateStatusPending,
		SourceContentOrigin: ContentOriginUserAuthored, TrustLevel: CandidateTrustUnverified,
	})
	result, err := service.Confirm(ctx, ConfirmCandidateRequest{
		UserID: "user-2", CandidateID: own.CandidateID, KnowledgePointID: "kp-owner",
	})
	if err == nil && result.KnowledgePoint != nil && result.KnowledgePoint.UserID != "user-2" {
		t.Fatal("跨用户关联到了他人的知识点")
	}
}
