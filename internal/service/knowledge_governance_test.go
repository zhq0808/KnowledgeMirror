package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 治理集成测试：把真实的 DocumentService 与 CandidateService 串起来跑完整链路
//（上传 → 解析 → 确认用途 → 抽取候选 → 处理候选），
// 断言的不是某个函数的返回值，而是整条链路**没有**产生掌握状态。
//
// 这里刻意不使用假 DocumentService：只有真解析器 + 真规则串在一起，
// 才能发现「某一层单独看没问题、连起来却越权」的问题。
// ---------------------------------------------------------------------------

// masteryProbeRepository 包住候选仓储，记录链路上出现过的一切「像掌握状态」的写入。
// 本阶段仓储契约里根本没有掌握相关方法，所以任何一次记录都意味着契约被偷偷扩大了。
type masteryProbeRepository struct {
	*fakeCandidateRepository
	trustLevelsWritten []string
	outcomesWritten    []string
}

func (r *masteryProbeRepository) ResolveCandidate(ctx context.Context, params ResolveCandidateParams) (ContentCandidate, error) {
	r.trustLevelsWritten = append(r.trustLevelsWritten, params.TrustLevel)
	r.outcomesWritten = append(r.outcomesWritten, params.Outcome)
	return r.fakeCandidateRepository.ResolveCandidate(ctx, params)
}

func (r *masteryProbeRepository) ConfirmKnowledgePointCandidate(ctx context.Context, params ConfirmKnowledgePointParams) (ContentCandidate, KnowledgePoint, error) {
	r.trustLevelsWritten = append(r.trustLevelsWritten, params.TrustLevel)
	return r.fakeCandidateRepository.ConfirmKnowledgePointCandidate(ctx, params)
}

// governanceRig 是一套跑完整链路所需的最小装配。
type governanceRig struct {
	documents          *DocumentService
	documentRepository *fakeDocumentRepository
	candidates         *CandidateService
	probe              *masteryProbeRepository
	extractor          *fakeCandidateExtractor
}

func newGovernanceRig(t *testing.T, proposals []CandidateProposal) *governanceRig {
	t.Helper()
	documentService, documentRepository := newDocumentServiceForTest(t)
	probe := &masteryProbeRepository{fakeCandidateRepository: newFakeCandidateRepository()}
	extractor := &fakeCandidateExtractor{proposals: proposals}
	return &governanceRig{
		documents:          documentService,
		documentRepository: documentRepository,
		candidates:         NewCandidateService(probe, documentService, extractor, CandidateLimits{}),
		probe:              probe,
		extractor:          extractor,
	}
}

// uploadAndConfirm 走完「上传 → 解析 → 确认来源与用途」，返回资料 ID。
func (r *governanceRig) uploadAndConfirm(t *testing.T, kind string, purposes []string) string {
	t.Helper()
	ctx := context.Background()
	result := mustUpload(t, r.documents, uploadRequest(governanceMarkdown, ""))
	documentID := result.Detail.Document.DocumentID

	if _, err := r.documents.UpdateMetadata(ctx, UpdateMetadataRequest{
		UserID:        "user-1",
		DocumentID:    documentID,
		ContentOrigin: &[]string{ContentOriginUserAuthored}[0],
		DocumentKind:  &kind,
	}); err != nil {
		t.Fatalf("确认来源与类别失败: %v", err)
	}
	if _, err := r.documents.ConfirmUsages(ctx, "user-1", documentID, purposes); err != nil {
		t.Fatalf("确认用途失败: %v", err)
	}
	return documentID
}

// governanceMarkdown 的正文必须能被下面提案里的 evidence_quote 命中，否则抽取会整批拒绝。
const governanceMarkdown = "# Kafka 消费\n\nKafka 消费幂等需要业务唯一键去重。\n\n## 计划\n\n本周补齐 Kafka 消费幂等实验。\n"

// 完整链路：上传、解析、确认用途、抽取、确认候选，全程不产生任何掌握状态。
func TestUploadParseAndCandidateConfirmProduceNoMasteryState(t *testing.T) {
	rig := newGovernanceRig(t, []CandidateProposal{{
		CandidateType: CandidateTypeKnowledgePoint,
		Title:         "Kafka 消费幂等",
		Summary:       "用业务唯一键去重",
		Sources:       []CandidateProposalSource{{Ref: "S1", EvidenceQuote: "业务唯一键去重"}},
	}})
	ctx := context.Background()

	documentID := rig.uploadAndConfirm(t, DocumentKindLearningNote, []string{DocumentPurposeLearn})

	// 1. 上传与解析阶段：资料只是内容线索，片段既不可信也不可检索。
	chunks, err := rig.documents.ListSourceChunks(ctx, "user-1", documentID, "")
	if err != nil {
		t.Fatalf("查询来源片段失败: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("没有解析出来源片段")
	}
	for _, chunk := range chunks {
		if chunk.TrustLevel != SourceChunkTrustUnverified {
			t.Fatalf("解析提升了片段可信级别: %q", chunk.TrustLevel)
		}
	}

	// 2. 抽取阶段：只产生待确认候选。
	extracted, err := rig.candidates.Extract(ctx, "user-1", documentID)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if len(extracted.Candidates) != 1 {
		t.Fatalf("候选数 = %d, want 1", len(extracted.Candidates))
	}
	if extracted.Candidates[0].Status != CandidateStatusPending {
		t.Fatalf("抽取结果状态 = %q, want pending", extracted.Candidates[0].Status)
	}
	if len(rig.probe.knowledgePoints) != 0 {
		t.Fatal("抽取阶段就创建了知识点")
	}

	// 3. 确认阶段：知识点进入正式知识库，但 UI 状态只能是「暂无证据」。
	confirmed, err := rig.candidates.Confirm(ctx, ConfirmCandidateRequest{
		UserID: "user-1", CandidateID: extracted.Candidates[0].CandidateID,
	})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if confirmed.MasteryUIState != MasteryUIStateNoEvidence {
		t.Fatalf("确认后 UI 状态 = %q, want %q", confirmed.MasteryUIState, MasteryUIStateNoEvidence)
	}
	if confirmed.KnowledgePoint.MasteryUIState() != MasteryUIStateNoEvidence {
		t.Fatal("知识点带上了掌握等级")
	}

	// 4. 全链路写入过的可信级别里，绝不能出现掌握等级或证据来源。
	forbidden := []string{"exposed", "explainable", "implementable", "verified", "trusted", "production"}
	for _, written := range rig.probe.trustLevelsWritten {
		for _, word := range forbidden {
			if strings.EqualFold(written, word) {
				t.Fatalf("链路写入了掌握等级或越权可信级别: %q", written)
			}
		}
	}

	// 5. 知识点列表里也不该出现任何掌握字段的替代品。
	points, err := rig.candidates.ListKnowledgePoints(ctx, "user-1")
	if err != nil {
		t.Fatalf("查询知识点失败: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("知识点数 = %d, want 1", len(points))
	}
	if points[0].MasteryUIState() != MasteryUIStateNoEvidence {
		t.Fatalf("知识点 UI 状态 = %q, want %q", points[0].MasteryUIState(), MasteryUIStateNoEvidence)
	}
}

// 学习 Todo 的完整链路：确认后只到「待接入计划」，不写计划，也不产生掌握状态。
func TestLearningTodoConfirmStopsBeforePlan(t *testing.T) {
	rig := newGovernanceRig(t, []CandidateProposal{{
		CandidateType: CandidateTypePlanTask,
		Title:         "补齐 Kafka 消费幂等实验",
		Sources:       []CandidateProposalSource{{Ref: "S2", EvidenceQuote: "本周补齐 Kafka 消费幂等实验"}},
	}})
	ctx := context.Background()

	documentID := rig.uploadAndConfirm(t, DocumentKindLearningTodo, []string{DocumentPurposeGeneratePlan})
	extracted, err := rig.candidates.Extract(ctx, "user-1", documentID)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if len(extracted.Candidates) != 1 {
		t.Fatalf("候选数 = %d, want 1", len(extracted.Candidates))
	}

	confirmed, err := rig.candidates.Confirm(ctx, ConfirmCandidateRequest{
		UserID: "user-1", CandidateID: extracted.Candidates[0].CandidateID,
	})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if confirmed.KnowledgePoint != nil {
		t.Fatal("计划任务候选确认后创建了知识点")
	}
	if confirmed.Candidate.ConfirmedOutcome != CandidateOutcomePlanTaskPending {
		t.Fatalf("确认结果 = %q, want %q", confirmed.Candidate.ConfirmedOutcome, CandidateOutcomePlanTaskPending)
	}
	if len(rig.probe.knowledgePoints) != 0 {
		t.Fatal("链路上产生了知识点")
	}
}

// 项目事实的完整链路：确认后只是待核实事实，不是可信事实，更不是生产证据。
func TestProjectFactConfirmStaysUnverified(t *testing.T) {
	rig := newGovernanceRig(t, []CandidateProposal{{
		CandidateType: CandidateTypePersonalFact,
		Title:         "在借贷链路里做过 Kafka 消费幂等",
		Sources:       []CandidateProposalSource{{Ref: "S1", EvidenceQuote: "业务唯一键去重"}},
	}})
	ctx := context.Background()

	documentID := rig.uploadAndConfirm(t, DocumentKindProjectFact, []string{DocumentPurposeFactReference})
	extracted, err := rig.candidates.Extract(ctx, "user-1", documentID)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	confirmed, err := rig.candidates.Confirm(ctx, ConfirmCandidateRequest{
		UserID: "user-1", CandidateID: extracted.Candidates[0].CandidateID,
	})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if confirmed.Candidate.ConfirmedOutcome != CandidateOutcomeUnverifiedFact {
		t.Fatalf("确认结果 = %q, want %q", confirmed.Candidate.ConfirmedOutcome, CandidateOutcomeUnverifiedFact)
	}
	if confirmed.Candidate.TrustLevel != CandidateTrustUnverified {
		t.Fatalf("可信级别 = %q, want %q", confirmed.Candidate.TrustLevel, CandidateTrustUnverified)
	}
}

// 仅供 AI 检索的资料：链路跑完也不会产生正式知识点。
func TestAIRetrievalOnlyDocumentCreatesNoKnowledgePoint(t *testing.T) {
	rig := newGovernanceRig(t, []CandidateProposal{{
		CandidateType: CandidateTypeKnowledgePoint,
		Title:         "Kafka 消费幂等",
		Sources:       []CandidateProposalSource{{Ref: "S1", EvidenceQuote: "业务唯一键去重"}},
	}})
	ctx := context.Background()

	documentID := rig.uploadAndConfirm(t, DocumentKindLearningNote, []string{DocumentPurposeAIRetrieval})
	extracted, err := rig.candidates.Extract(ctx, "user-1", documentID)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if extracted.Filtered != 1 || len(extracted.Candidates) != 0 {
		t.Fatalf("越界候选未被丢弃: filtered=%d saved=%d", extracted.Filtered, len(extracted.Candidates))
	}
	if len(rig.probe.knowledgePoints) != 0 {
		t.Fatal("仅供检索的资料产生了知识点")
	}
}

// 仅归档资料：整条链路在抽取入口就被拦住。
func TestArchiveOnlyDocumentNeverEntersCandidatePipeline(t *testing.T) {
	rig := newGovernanceRig(t, nil)
	ctx := context.Background()

	documentID := rig.uploadAndConfirm(t, DocumentKindLearningNote, []string{DocumentPurposeArchiveOnly})

	if _, err := rig.candidates.Extract(ctx, "user-1", documentID); !errors.Is(err, ErrCandidateDocumentArchived) {
		t.Fatalf("err = %v, want ErrCandidateDocumentArchived", err)
	}
	if len(rig.probe.candidates) != 0 {
		t.Fatal("仅归档资料产生了候选")
	}
}

// 解析失败后重试成功，链路仍然从零开始：不因为「重试过」就跳过任何确认。
func TestRetryParseStillRequiresFullConfirmationChain(t *testing.T) {
	rig := newGovernanceRig(t, []CandidateProposal{{
		CandidateType: CandidateTypeKnowledgePoint,
		Title:         "Kafka 消费幂等",
		Sources:       []CandidateProposalSource{{Ref: "S1", EvidenceQuote: "业务唯一键去重"}},
	}})
	ctx := context.Background()

	rig.documentRepository.saveChunksErr = errors.New("数据库故障")
	if _, err := rig.documents.Upload(ctx, uploadRequest(governanceMarkdown, "")); err == nil {
		t.Fatal("落库失败时 Upload 应返回错误")
	}
	documentID := rig.documentRepository.markedFailed[0]

	// 解析失败的资料没有片段，抽取必须被拒绝，而不是抽出空候选。
	if _, err := rig.candidates.Extract(ctx, "user-1", documentID); err == nil {
		t.Fatal("解析失败的资料仍然允许抽取")
	}

	rig.documentRepository.saveChunksErr = nil
	detail, err := rig.documents.RetryParse(ctx, "user-1", documentID)
	if err != nil {
		t.Fatalf("重试解析失败: %v", err)
	}
	// 重试成功后仍是「待确认」，用途为空，所以抽取依然无类型可提。
	if detail.Document.Status != DocumentStatusPendingConfirmation {
		t.Fatalf("重试后状态 = %q, want %q", detail.Document.Status, DocumentStatusPendingConfirmation)
	}
	if _, err := rig.candidates.Extract(ctx, "user-1", documentID); !errors.Is(err, ErrCandidateNoAllowedType) {
		t.Fatalf("err = %v, want ErrCandidateNoAllowedType", err)
	}
}
