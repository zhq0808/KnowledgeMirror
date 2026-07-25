package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 候选内容是「资料」与「正式知识库」之间唯一的缓冲层。
//
// 这一层存在的唯一理由：AI 读得懂资料，但读不懂用户的真实事实边界。
// 因此 AI 只能提出候选，正式知识点、计划任务、可信事实、生产证据和掌握状态
// 全部只能由用户在这里逐条确认后才可能产生——而掌握状态在本阶段一条都不产生。
// ---------------------------------------------------------------------------

// 候选类型。它决定确认后允许流向哪条链路，不决定可信级别。
const (
	CandidateTypeKnowledgePoint = "knowledge_point"
	CandidateTypePlanTask       = "plan_task"
	CandidateTypeJDRequirement  = "jd_requirement"
	CandidateTypePersonalFact   = "personal_fact"
	CandidateTypeReferenceOnly  = "reference_only"
)

// 候选状态。pending 之外全是终态，处理后不可再改。
const (
	CandidateStatusPending   = "pending"
	CandidateStatusConfirmed = "confirmed"
	CandidateStatusLinked    = "linked"
	CandidateStatusMerged    = "merged"
	CandidateStatusArchived  = "archived"
	CandidateStatusRejected  = "rejected"
)

// 确认结果：用户处理一条候选后，系统实际产生了什么。
//
// 注意 plan_task 与 jd_requirement 只到「待接入」：确认动作本身不写计划、不改目标，
// 真正的写入发生在计划模块与目标模块，仍需用户在那里再做一次决定。
const (
	CandidateOutcomeKnowledgePointCreated = "knowledge_point_created"
	CandidateOutcomeKnowledgePointLinked  = "knowledge_point_linked"
	CandidateOutcomePlanTaskPending       = "plan_task_pending_intake"
	CandidateOutcomeJDRequirementPending  = "jd_requirement_pending_intake"
	CandidateOutcomeUnverifiedFact        = "unverified_fact"
	CandidateOutcomeReferenceOnly         = "reference_only"
	CandidateOutcomeMerged                = "merged"
	CandidateOutcomeArchived              = "archived"
	CandidateOutcomeRejected              = "rejected"
)

// 候选可信级别。候选层最高只能到 user_confirmed：
// trusted 属于知识点与证据层，production 属于证据来源维度，都不由候选确认产生。
const (
	CandidateTrustUnverified    = "unverified"
	CandidateTrustUserConfirmed = "user_confirmed"
)

// MasteryUIStateNoEvidence 是知识点进入正式知识库后的 UI 空状态。
// 它不是掌握等级，只表示「已在追踪，但还没有任何证据」。
const MasteryUIStateNoEvidence = "no_evidence"

var validCandidateTypes = []string{
	CandidateTypeKnowledgePoint, CandidateTypePlanTask, CandidateTypeJDRequirement,
	CandidateTypePersonalFact, CandidateTypeReferenceOnly,
}

// CandidateTypes 供接口层枚举与校验复用。
func CandidateTypes() []string { return slices.Clone(validCandidateTypes) }

// kindCandidateTypes 限制「什么类别的资料能提出什么候选」。
// 这是确定性规则，不交给模型判断：学习 Todo 只能提计划任务候选，
// 目标 JD 只能提要求候选，项目事实只能提待核实事实候选。
var kindCandidateTypes = map[string][]string{
	DocumentKindLearningNote:      {CandidateTypeKnowledgePoint, CandidateTypeReferenceOnly},
	DocumentKindLearningTodo:      {CandidateTypePlanTask, CandidateTypeReferenceOnly},
	DocumentKindTechnicalMaterial: {CandidateTypeKnowledgePoint, CandidateTypeReferenceOnly},
	DocumentKindTargetJD:          {CandidateTypeJDRequirement, CandidateTypeReferenceOnly},
	DocumentKindProjectFact:       {CandidateTypePersonalFact, CandidateTypeReferenceOnly},
	DocumentKindInterviewReview: {
		CandidateTypeKnowledgePoint, CandidateTypePlanTask,
		CandidateTypePersonalFact, CandidateTypeReferenceOnly,
	},
	DocumentKindOther: validCandidateTypes,
}

// purposeCandidateTypes 限制「什么用途的资料能提出什么候选」。
// 关键一条：只勾了「供 AI 检索」的资料只会产生 reference_only 候选，
// 不必也不应该为它创建正式知识点——能被检索到不等于用户要学它。
var purposeCandidateTypes = map[string][]string{
	DocumentPurposeLearn:         {CandidateTypeKnowledgePoint},
	DocumentPurposeGeneratePlan:  {CandidateTypePlanTask},
	DocumentPurposeFactReference: {CandidateTypePersonalFact, CandidateTypeJDRequirement},
	DocumentPurposeAIRetrieval:   {CandidateTypeReferenceOnly},
}

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

var (
	// ErrCandidateNotFound 表示候选不存在或不属于当前用户；对外一律 404。
	ErrCandidateNotFound = errors.New("候选内容不存在")
	// ErrCandidateResolved 表示候选已被处理过，终态不可重复处理。
	ErrCandidateResolved = errors.New("候选内容已处理，不能重复处理")
	// ErrCandidateDocumentArchived 表示资料被标记为「仅归档」，不参与候选抽取。
	ErrCandidateDocumentArchived = errors.New("仅归档资料不参与候选抽取")
	// ErrCandidateNoAllowedType 表示当前资料的类别与用途组合不允许产生任何候选。
	ErrCandidateNoAllowedType = errors.New("当前资料的类别与用途不产生任何候选")
	// ErrCandidateExtractorUnavailable 表示未配置候选抽取模型。
	ErrCandidateExtractorUnavailable = errors.New("候选抽取未启用")
	// ErrCandidateInvalidOutput 表示模型输出不满足确定性校验，整批拒绝。
	ErrCandidateInvalidOutput = errors.New("候选抽取输出非法")
)

// CandidateInputError 是可以安全回显给用户的输入类错误，接口层映射为 400。
type CandidateInputError struct{ Message string }

func (e *CandidateInputError) Error() string { return e.Message }

func invalidCandidateInput(format string, args ...any) error {
	return &CandidateInputError{Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// 领域模型
// ---------------------------------------------------------------------------

// CandidatePayload 是候选的可编辑正文。用户修改的就是它，AI 产出的也只是它。
type CandidatePayload struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Reason 是 AI 提出该候选的理由，供用户判断，不作为事实。
	Reason string `json:"reason,omitempty"`
}

// CandidateSource 是候选引用的一个来源片段。EvidenceQuote 必须是片段原文的子串。
type CandidateSource struct {
	SourceChunkID string
	SourceOrder   int16
	EvidenceQuote string
}

// ContentCandidate 是一条待用户处理（或已处理）的候选内容。
type ContentCandidate struct {
	CandidateID string
	UserID      string
	DocumentID  string
	VersionID   string

	CandidateType string
	Payload       CandidatePayload
	Status        string
	// SourceContentOrigin 是抽取时资料来源的快照。
	// AI 整理的资料会一直带着 ai_generated，确认也不会洗掉这个标记。
	SourceContentOrigin string
	TrustLevel          string

	TargetKnowledgePointID string
	MergedIntoCandidateID  string
	ConfirmedOutcome       string
	DecisionNote           string

	ExtractorModel   string
	ExtractorVersion string
	Sources          []CandidateSource

	ConfirmedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// KnowledgePoint 是用户确认后正式追踪的能力单元。
// 它本身不含掌握等级：新建时没有任何证据，UI 显示「暂无证据」。
type KnowledgePoint struct {
	KnowledgePointID string
	UserID           string
	Title            string
	Description      string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// MasteryUIState 返回知识点在 UI 上的掌握展示状态。
// 本阶段没有任何掌握证据表，所以恒为「暂无证据」——这正是设计要的结果。
func (k KnowledgePoint) MasteryUIState() string { return MasteryUIStateNoEvidence }

// ---------------------------------------------------------------------------
// 仓储契约
// ---------------------------------------------------------------------------

// SaveCandidatesParams 是一次抽取的落库输入。
type SaveCandidatesParams struct {
	UserID           string
	DocumentID       string
	VersionID        string
	ContentOrigin    string
	ExtractorModel   string
	ExtractorVersion string
	Candidates       []NewCandidate
}

// NewCandidate 是一条准备落库的候选：正文 + 至少一个来源引用 + 去重哈希。
type NewCandidate struct {
	CandidateType string
	Payload       CandidatePayload
	Sources       []CandidateSource
	DedupeHash    []byte
}

// CandidateQuery 是候选列表过滤条件；空值表示不过滤。
type CandidateQuery struct {
	DocumentID     string
	Status         string
	CandidateTypes []string
	Limit          int
}

// ResolveCandidateParams 是一次终态处理：确认、合并、归档或拒绝。
type ResolveCandidateParams struct {
	UserID                string
	CandidateID           string
	Status                string
	Outcome               string
	TrustLevel            string
	MergedIntoCandidateID string
	DecisionNote          string
	Payload               *CandidatePayload
	ResolvedAt            time.Time
}

// ConfirmKnowledgePointParams 是知识点候选的确认：新建或关联已有知识点。
// KnowledgePointID 为空表示新建；非空表示关联已有项，不重复创建。
type ConfirmKnowledgePointParams struct {
	UserID           string
	CandidateID      string
	KnowledgePointID string
	Title            string
	Description      string
	TrustLevel       string
	DecisionNote     string
	Payload          CandidatePayload
	ResolvedAt       time.Time
}

// CandidateRepository 是候选确认所需的最小持久化能力。所有方法都必须按 user_id 隔离。
type CandidateRepository interface {
	// SaveCandidates 在一个事务里写入候选与来源引用；
	// 与已有待确认候选重复的条目被跳过，返回真正新增的候选。
	SaveCandidates(ctx context.Context, params SaveCandidatesParams) ([]ContentCandidate, error)
	GetCandidate(ctx context.Context, userID, candidateID string) (ContentCandidate, error)
	ListCandidates(ctx context.Context, userID string, query CandidateQuery) ([]ContentCandidate, error)
	// UpdateCandidatePayload 只允许改待确认候选的正文，不改类型与血缘。
	UpdateCandidatePayload(ctx context.Context, userID, candidateID string, payload CandidatePayload, dedupeHash []byte) (ContentCandidate, error)
	// ResolveCandidate 把待确认候选推进到终态；已处理的候选返回 ErrCandidateResolved。
	ResolveCandidate(ctx context.Context, params ResolveCandidateParams) (ContentCandidate, error)
	// ConfirmKnowledgePointCandidate 在一个事务里创建或关联知识点、写入知识点与来源片段的关联，并把候选推进到终态。
	ConfirmKnowledgePointCandidate(ctx context.Context, params ConfirmKnowledgePointParams) (ContentCandidate, KnowledgePoint, error)
	ListKnowledgePoints(ctx context.Context, userID string, limit int) ([]KnowledgePoint, error)
}

// CandidateDocumentReader 是候选抽取对资料模块的最小依赖，由 *DocumentService 满足。
type CandidateDocumentReader interface {
	Get(ctx context.Context, userID, documentID string) (DocumentDetail, error)
	ListSourceChunks(ctx context.Context, userID, documentID, versionID string) ([]SourceChunk, error)
}

// CandidateExtractor 把来源片段转换成候选提案。它只提案，不落库、不确认。
type CandidateExtractor interface {
	Extract(ctx context.Context, input CandidateExtractionInput) ([]CandidateProposal, error)
}

// CandidateExtractionInput 是交给模型的输入。
// 真实 user_id / document_id / source_chunk_id 都不进入模型，模型只看到临时引用 S1、S2……
type CandidateExtractionInput struct {
	DocumentTitle string
	DocumentKind  string
	ContentOrigin string
	AllowedTypes  []string
	MaxCandidates int
	Chunks        []CandidateChunkRef
}

// CandidateChunkRef 是一个来源片段的临时引用。
type CandidateChunkRef struct {
	Ref         string
	HeadingPath []string
	Content     string
}

// CandidateProposal 是模型提出的一条候选，引用仍是临时的 S 编号。
type CandidateProposal struct {
	CandidateType string
	Title         string
	Summary       string
	Reason        string
	Sources       []CandidateProposalSource
}

// CandidateProposalSource 是提案引用的临时片段编号与原文证据。
type CandidateProposalSource struct {
	Ref           string
	EvidenceQuote string
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

// CandidateLimits 是候选抽取的硬预算：一份异常资料不能变成成百上千条待确认项。
type CandidateLimits struct {
	MaxCandidates          int
	MaxTitleChars          int
	MaxSummaryChars        int
	MaxNoteChars           int
	MaxSourcesPerCandidate int
	MaxChunksPerRequest    int
	MaxChunkChars          int
}

func DefaultCandidateLimits() CandidateLimits {
	return CandidateLimits{
		MaxCandidates:          20,
		MaxTitleChars:          120,
		MaxSummaryChars:        800,
		MaxNoteChars:           500,
		MaxSourcesPerCandidate: 5,
		MaxChunksPerRequest:    40,
		MaxChunkChars:          2000,
	}
}

// defaultCandidateListLimit 是候选列表默认条数上限，首版不做游标分页。
const defaultCandidateListLimit = 200

// CandidateService 编排候选抽取与用户确认。
//
// 它守住四条边界：
//  1. 抽取结果只能是 pending，任何链路都不会因为抽取而写入正式数据；
//  2. 每条候选必须引用至少一个来源片段，且证据必须是原文子串；
//  3. 候选类型由资料类别与用途确定性推导，模型无权越界；
//  4. 确认知识点候选只创建/关联知识点，不创建任何掌握状态或掌握证据。
type CandidateService struct {
	repository CandidateRepository
	documents  CandidateDocumentReader
	extractor  CandidateExtractor
	limits     CandidateLimits
	now        func() time.Time
}

// NewCandidateService 构造候选服务。extractor 为空时抽取接口返回未启用错误，
// 但确认、修改、合并、归档、拒绝等人工操作仍然可用。
func NewCandidateService(
	repository CandidateRepository,
	documents CandidateDocumentReader,
	extractor CandidateExtractor,
	limits CandidateLimits,
) *CandidateService {
	defaults := DefaultCandidateLimits()
	if limits.MaxCandidates <= 0 {
		limits.MaxCandidates = defaults.MaxCandidates
	}
	if limits.MaxTitleChars <= 0 {
		limits.MaxTitleChars = defaults.MaxTitleChars
	}
	if limits.MaxSummaryChars <= 0 {
		limits.MaxSummaryChars = defaults.MaxSummaryChars
	}
	if limits.MaxNoteChars <= 0 {
		limits.MaxNoteChars = defaults.MaxNoteChars
	}
	if limits.MaxSourcesPerCandidate <= 0 {
		limits.MaxSourcesPerCandidate = defaults.MaxSourcesPerCandidate
	}
	if limits.MaxChunksPerRequest <= 0 {
		limits.MaxChunksPerRequest = defaults.MaxChunksPerRequest
	}
	if limits.MaxChunkChars <= 0 {
		limits.MaxChunkChars = defaults.MaxChunkChars
	}
	return &CandidateService{
		repository: repository,
		documents:  documents,
		extractor:  extractor,
		limits:     limits,
		now:        time.Now,
	}
}

// Limits 暴露生效中的预算，供接口层提示。
func (s *CandidateService) Limits() CandidateLimits { return s.limits }

// ExtractResult 汇报一次抽取：新增了什么、丢弃了什么、跳过了什么。
// Filtered 与 Duplicated 不是错误：模型越界提案被丢弃、重复提案被跳过都是预期行为。
type ExtractResult struct {
	Candidates   []ContentCandidate
	AllowedTypes []string
	Proposed     int
	Filtered     int
	Duplicated   int
}

// Extract 从资料当前版本抽取候选内容。
//
// 结果只能是候选：这个方法没有任何一条路径会写入知识点、计划、可信事实或掌握状态。
func (s *CandidateService) Extract(ctx context.Context, userID, documentID string) (ExtractResult, error) {
	if userID == "" {
		return ExtractResult{}, errors.New("缺少用户身份")
	}
	if s.extractor == nil {
		return ExtractResult{}, ErrCandidateExtractorUnavailable
	}

	detail, err := s.documents.Get(ctx, userID, documentID)
	if err != nil {
		return ExtractResult{}, err
	}
	if detail.ChunkCount == 0 {
		return ExtractResult{}, invalidCandidateInput("资料尚未解析成功，无法抽取候选")
	}
	if purposeEnabled(detail.Usages, DocumentPurposeArchiveOnly) {
		return ExtractResult{}, ErrCandidateDocumentArchived
	}

	allowedTypes := allowedCandidateTypes(detail.Document.DocumentKind, detail.Usages)
	if len(allowedTypes) == 0 {
		return ExtractResult{}, ErrCandidateNoAllowedType
	}

	chunks, err := s.documents.ListSourceChunks(ctx, userID, documentID, detail.Document.CurrentVersionID)
	if err != nil {
		return ExtractResult{}, err
	}
	if len(chunks) == 0 {
		return ExtractResult{}, invalidCandidateInput("资料没有可引用的来源片段")
	}
	if len(chunks) > s.limits.MaxChunksPerRequest {
		chunks = chunks[:s.limits.MaxChunksPerRequest]
	}

	refs := make([]CandidateChunkRef, 0, len(chunks))
	byRef := make(map[string]SourceChunk, len(chunks))
	for index, chunk := range chunks {
		ref := fmt.Sprintf("S%d", index+1)
		refs = append(refs, CandidateChunkRef{
			Ref:         ref,
			HeadingPath: chunk.HeadingPath,
			Content:     truncateRunes(chunk.Content, s.limits.MaxChunkChars),
		})
		byRef[ref] = chunk
	}

	proposals, err := s.extractor.Extract(ctx, CandidateExtractionInput{
		DocumentTitle: detail.Document.Title,
		DocumentKind:  detail.Document.DocumentKind,
		ContentOrigin: detail.Document.ContentOrigin,
		AllowedTypes:  allowedTypes,
		MaxCandidates: s.limits.MaxCandidates,
		Chunks:        refs,
	})
	if err != nil {
		return ExtractResult{}, err
	}
	if len(proposals) > s.limits.MaxCandidates {
		return ExtractResult{}, fmt.Errorf("%w: 候选数 %d 超过单次上限 %d",
			ErrCandidateInvalidOutput, len(proposals), s.limits.MaxCandidates)
	}

	result := ExtractResult{AllowedTypes: allowedTypes, Proposed: len(proposals)}
	pending := make([]NewCandidate, 0, len(proposals))
	for index, proposal := range proposals {
		// 越界类型只丢弃不报错：模型可能不听话，但丢弃是无副作用的安全动作。
		if !slices.Contains(allowedTypes, proposal.CandidateType) {
			result.Filtered++
			continue
		}
		candidate, err := s.buildCandidate(index, proposal, byRef)
		if err != nil {
			return ExtractResult{}, err
		}
		pending = append(pending, candidate)
	}
	if len(pending) == 0 {
		return result, nil
	}

	saved, err := s.repository.SaveCandidates(ctx, SaveCandidatesParams{
		UserID:     userID,
		DocumentID: documentID,
		VersionID:  detail.Document.CurrentVersionID,
		// 来源快照跟着候选走：AI 整理的资料，其候选永远带着 ai_generated。
		ContentOrigin:    detail.Document.ContentOrigin,
		ExtractorModel:   extractorModelName(s.extractor),
		ExtractorVersion: extractorVersionName(s.extractor),
		Candidates:       pending,
	})
	if err != nil {
		return ExtractResult{}, err
	}
	result.Candidates = saved
	result.Duplicated = len(pending) - len(saved)
	return result, nil
}

// buildCandidate 做全部确定性校验：临时引用换成真实片段 ID，原文证据必须对得上。
// 引用不存在或缺引用属于结构性违规，整批拒绝，不做「尽力而为」的部分保存。
func (s *CandidateService) buildCandidate(index int, proposal CandidateProposal, byRef map[string]SourceChunk) (NewCandidate, error) {
	title := strings.TrimSpace(proposal.Title)
	if title == "" {
		return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选缺少标题", ErrCandidateInvalidOutput, index+1)
	}
	if utf8.RuneCountInString(title) > s.limits.MaxTitleChars {
		return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选标题超过 %d 个字符",
			ErrCandidateInvalidOutput, index+1, s.limits.MaxTitleChars)
	}
	summary := truncateRunes(strings.TrimSpace(proposal.Summary), s.limits.MaxSummaryChars)
	reason := truncateRunes(strings.TrimSpace(proposal.Reason), s.limits.MaxSummaryChars)

	if len(proposal.Sources) == 0 {
		return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选没有引用任何来源片段",
			ErrCandidateInvalidOutput, index+1)
	}
	if len(proposal.Sources) > s.limits.MaxSourcesPerCandidate {
		return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选引用的来源片段超过 %d 个",
			ErrCandidateInvalidOutput, index+1, s.limits.MaxSourcesPerCandidate)
	}

	sources := make([]CandidateSource, 0, len(proposal.Sources))
	seen := make(map[string]struct{}, len(proposal.Sources))
	for _, source := range proposal.Sources {
		chunk, found := byRef[strings.TrimSpace(source.Ref)]
		if !found {
			return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选引用了不存在的来源片段 %q",
				ErrCandidateInvalidOutput, index+1, source.Ref)
		}
		if _, duplicated := seen[chunk.SourceChunkID]; duplicated {
			continue
		}
		seen[chunk.SourceChunkID] = struct{}{}

		quote := strings.TrimSpace(source.EvidenceQuote)
		// 证据必须真的出现在原文里，否则就是模型编的。
		if quote != "" && !strings.Contains(chunk.Content, quote) {
			return NewCandidate{}, fmt.Errorf("%w: 第 %d 条候选的证据原文不在来源片段中",
				ErrCandidateInvalidOutput, index+1)
		}
		sources = append(sources, CandidateSource{
			SourceChunkID: chunk.SourceChunkID,
			SourceOrder:   int16(len(sources) + 1),
			EvidenceQuote: truncateRunes(quote, s.limits.MaxSummaryChars),
		})
	}

	payload := CandidatePayload{Title: title, Summary: summary, Reason: reason}
	return NewCandidate{
		CandidateType: proposal.CandidateType,
		Payload:       payload,
		Sources:       sources,
		DedupeHash:    candidateDedupeHash(proposal.CandidateType, payload.Title, sources),
	}, nil
}

// List 返回当前用户的候选列表。
func (s *CandidateService) List(ctx context.Context, userID string, query CandidateQuery) ([]ContentCandidate, error) {
	if userID == "" {
		return nil, errors.New("缺少用户身份")
	}
	if query.Status != "" && !slices.Contains(candidateStatuses(), query.Status) {
		return nil, invalidCandidateInput("不支持的候选状态: %s", query.Status)
	}
	for _, candidateType := range query.CandidateTypes {
		if !slices.Contains(validCandidateTypes, candidateType) {
			return nil, invalidCandidateInput("不支持的候选类型: %s", candidateType)
		}
	}
	if query.Limit <= 0 || query.Limit > defaultCandidateListLimit {
		query.Limit = defaultCandidateListLimit
	}
	return s.repository.ListCandidates(ctx, userID, query)
}

// Get 返回单条候选。
func (s *CandidateService) Get(ctx context.Context, userID, candidateID string) (ContentCandidate, error) {
	return s.repository.GetCandidate(ctx, userID, candidateID)
}

// Modify 修改待确认候选的正文。修改后仍然是候选，不产生任何正式数据。
func (s *CandidateService) Modify(ctx context.Context, userID, candidateID string, payload CandidatePayload) (ContentCandidate, error) {
	candidate, err := s.repository.GetCandidate(ctx, userID, candidateID)
	if err != nil {
		return ContentCandidate{}, err
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, ErrCandidateResolved
	}
	normalized, err := s.normalizePayload(payload)
	if err != nil {
		return ContentCandidate{}, err
	}
	dedupeHash := candidateDedupeHash(candidate.CandidateType, normalized.Title, candidate.Sources)
	return s.repository.UpdateCandidatePayload(ctx, userID, candidateID, normalized, dedupeHash)
}

// ConfirmCandidateRequest 是一次确认。
// KnowledgePointID 非空表示「关联已有知识点」而不是新建；只有知识点候选允许携带它。
type ConfirmCandidateRequest struct {
	UserID           string
	CandidateID      string
	KnowledgePointID string
	Payload          *CandidatePayload
	DecisionNote     string
}

// ConfirmCandidateResult 是确认结果。
// KnowledgePoint 只有知识点候选才有；MasteryUIState 恒为「暂无证据」。
type ConfirmCandidateResult struct {
	Candidate      ContentCandidate
	KnowledgePoint *KnowledgePoint
	MasteryUIState string
}

// Confirm 确认一条候选。
//
// 每种类型确认后允许走多远，是产品边界不是实现细节：
//   - knowledge_point：进入正式知识库，初始 UI 状态「暂无证据」，不写掌握等级；
//   - plan_task：只标记为待接入计划，不写计划、不排期；
//   - jd_requirement：只标记为待接入目标，不改目标、不改计划；
//   - personal_fact：只成为待核实事实，不成为可信事实，更不是生产证据；
//   - reference_only：只作为检索参考，不创建正式知识点。
func (s *CandidateService) Confirm(ctx context.Context, request ConfirmCandidateRequest) (ConfirmCandidateResult, error) {
	candidate, err := s.repository.GetCandidate(ctx, request.UserID, request.CandidateID)
	if err != nil {
		return ConfirmCandidateResult{}, err
	}
	if candidate.Status != CandidateStatusPending {
		return ConfirmCandidateResult{}, ErrCandidateResolved
	}

	payload := candidate.Payload
	if request.Payload != nil {
		payload, err = s.normalizePayload(*request.Payload)
		if err != nil {
			return ConfirmCandidateResult{}, err
		}
	}
	note, err := s.normalizeNote(request.DecisionNote)
	if err != nil {
		return ConfirmCandidateResult{}, err
	}

	if request.KnowledgePointID != "" && candidate.CandidateType != CandidateTypeKnowledgePoint {
		return ConfirmCandidateResult{}, invalidCandidateInput("只有知识点候选可以关联已有知识点")
	}

	trustLevel := confirmedTrustLevel(candidate)
	if candidate.CandidateType == CandidateTypeKnowledgePoint {
		updated, knowledgePoint, err := s.repository.ConfirmKnowledgePointCandidate(ctx, ConfirmKnowledgePointParams{
			UserID:           request.UserID,
			CandidateID:      request.CandidateID,
			KnowledgePointID: request.KnowledgePointID,
			Title:            payload.Title,
			Description:      payload.Summary,
			TrustLevel:       trustLevel,
			DecisionNote:     note,
			Payload:          payload,
			ResolvedAt:       s.now(),
		})
		if err != nil {
			return ConfirmCandidateResult{}, err
		}
		return ConfirmCandidateResult{
			Candidate:      updated,
			KnowledgePoint: &knowledgePoint,
			// 新知识点没有任何证据，UI 必须显示空状态而不是某个掌握等级。
			MasteryUIState: MasteryUIStateNoEvidence,
		}, nil
	}

	updated, err := s.repository.ResolveCandidate(ctx, ResolveCandidateParams{
		UserID:       request.UserID,
		CandidateID:  request.CandidateID,
		Status:       CandidateStatusConfirmed,
		Outcome:      confirmOutcome(candidate.CandidateType),
		TrustLevel:   trustLevel,
		DecisionNote: note,
		Payload:      &payload,
		ResolvedAt:   s.now(),
	})
	if err != nil {
		return ConfirmCandidateResult{}, err
	}
	return ConfirmCandidateResult{Candidate: updated}, nil
}

// Merge 把一条候选合并进另一条同类型的待确认候选，被合并方进入终态。
func (s *CandidateService) Merge(ctx context.Context, userID, candidateID, intoCandidateID, note string) (ContentCandidate, error) {
	if intoCandidateID == "" {
		return ContentCandidate{}, invalidCandidateInput("请提供合并目标候选")
	}
	if intoCandidateID == candidateID {
		return ContentCandidate{}, invalidCandidateInput("不能把候选合并到自身")
	}
	candidate, err := s.repository.GetCandidate(ctx, userID, candidateID)
	if err != nil {
		return ContentCandidate{}, err
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, ErrCandidateResolved
	}
	target, err := s.repository.GetCandidate(ctx, userID, intoCandidateID)
	if err != nil {
		return ContentCandidate{}, err
	}
	if target.CandidateType != candidate.CandidateType {
		return ContentCandidate{}, invalidCandidateInput("只能合并同类型的候选")
	}
	if target.Status != CandidateStatusPending {
		return ContentCandidate{}, invalidCandidateInput("合并目标必须仍处于待确认状态")
	}
	normalizedNote, err := s.normalizeNote(note)
	if err != nil {
		return ContentCandidate{}, err
	}
	return s.repository.ResolveCandidate(ctx, ResolveCandidateParams{
		UserID:                userID,
		CandidateID:           candidateID,
		Status:                CandidateStatusMerged,
		Outcome:               CandidateOutcomeMerged,
		TrustLevel:            candidate.TrustLevel,
		MergedIntoCandidateID: intoCandidateID,
		DecisionNote:          normalizedNote,
		ResolvedAt:            s.now(),
	})
}

// Archive 仅归档一条候选：留档可追溯，但不进入任何链路。
func (s *CandidateService) Archive(ctx context.Context, userID, candidateID, note string) (ContentCandidate, error) {
	return s.resolveSimple(ctx, userID, candidateID, CandidateStatusArchived, CandidateOutcomeArchived, note)
}

// Reject 拒绝一条候选。拒绝同样是可追溯的决定，不是删除。
func (s *CandidateService) Reject(ctx context.Context, userID, candidateID, note string) (ContentCandidate, error) {
	return s.resolveSimple(ctx, userID, candidateID, CandidateStatusRejected, CandidateOutcomeRejected, note)
}

func (s *CandidateService) resolveSimple(ctx context.Context, userID, candidateID, status, outcome, note string) (ContentCandidate, error) {
	candidate, err := s.repository.GetCandidate(ctx, userID, candidateID)
	if err != nil {
		return ContentCandidate{}, err
	}
	if candidate.Status != CandidateStatusPending {
		return ContentCandidate{}, ErrCandidateResolved
	}
	normalizedNote, err := s.normalizeNote(note)
	if err != nil {
		return ContentCandidate{}, err
	}
	return s.repository.ResolveCandidate(ctx, ResolveCandidateParams{
		UserID:      userID,
		CandidateID: candidateID,
		Status:      status,
		Outcome:     outcome,
		// 归档和拒绝都不提升可信级别。
		TrustLevel:   candidate.TrustLevel,
		DecisionNote: normalizedNote,
		ResolvedAt:   s.now(),
	})
}

// ListKnowledgePoints 返回已进入正式知识库的知识点，供「关联已有项」选择。
func (s *CandidateService) ListKnowledgePoints(ctx context.Context, userID string) ([]KnowledgePoint, error) {
	if userID == "" {
		return nil, errors.New("缺少用户身份")
	}
	return s.repository.ListKnowledgePoints(ctx, userID, defaultCandidateListLimit)
}

// ---------------------------------------------------------------------------
// 内部规则
// ---------------------------------------------------------------------------

// allowedCandidateTypes = 类别允许的类型 ∩ 已确认用途允许的类型。
// 两个维度都必须点头，模型才有资格提这类候选。
func allowedCandidateTypes(kind string, usages []DocumentUsage) []string {
	byKind, found := kindCandidateTypes[kind]
	if !found {
		byKind = kindCandidateTypes[DocumentKindOther]
	}
	byPurpose := make([]string, 0, len(validCandidateTypes))
	for _, usage := range usages {
		if !usage.Enabled {
			continue
		}
		for _, candidateType := range purposeCandidateTypes[usage.Purpose] {
			if !slices.Contains(byPurpose, candidateType) {
				byPurpose = append(byPurpose, candidateType)
			}
		}
	}

	allowed := make([]string, 0, len(validCandidateTypes))
	for _, candidateType := range validCandidateTypes {
		if slices.Contains(byKind, candidateType) && slices.Contains(byPurpose, candidateType) {
			allowed = append(allowed, candidateType)
		}
	}
	return allowed
}

// confirmOutcome 把候选类型映射成「确认后到底产生了什么」。
func confirmOutcome(candidateType string) string {
	switch candidateType {
	case CandidateTypePlanTask:
		return CandidateOutcomePlanTaskPending
	case CandidateTypeJDRequirement:
		return CandidateOutcomeJDRequirementPending
	case CandidateTypePersonalFact:
		return CandidateOutcomeUnverifiedFact
	default:
		return CandidateOutcomeReferenceOnly
	}
}

// confirmedTrustLevel 决定确认后的可信级别。
// 两种情况永远停在 unverified：AI 整理的内容、以及项目事实候选。
// 前者是因为作者不是用户本人，后者是因为「用户说的」和「已核实的」不是一回事。
func confirmedTrustLevel(candidate ContentCandidate) string {
	if candidate.SourceContentOrigin == ContentOriginAIGenerated {
		return CandidateTrustUnverified
	}
	if candidate.CandidateType == CandidateTypePersonalFact {
		return CandidateTrustUnverified
	}
	return CandidateTrustUserConfirmed
}

func candidateStatuses() []string {
	return []string{
		CandidateStatusPending, CandidateStatusConfirmed, CandidateStatusLinked,
		CandidateStatusMerged, CandidateStatusArchived, CandidateStatusRejected,
	}
}

func (s *CandidateService) normalizePayload(payload CandidatePayload) (CandidatePayload, error) {
	title := strings.TrimSpace(payload.Title)
	if title == "" {
		return CandidatePayload{}, invalidCandidateInput("候选标题不能为空")
	}
	if utf8.RuneCountInString(title) > s.limits.MaxTitleChars {
		return CandidatePayload{}, invalidCandidateInput("候选标题不能超过 %d 个字符", s.limits.MaxTitleChars)
	}
	summary := strings.TrimSpace(payload.Summary)
	if utf8.RuneCountInString(summary) > s.limits.MaxSummaryChars {
		return CandidatePayload{}, invalidCandidateInput("候选说明不能超过 %d 个字符", s.limits.MaxSummaryChars)
	}
	return CandidatePayload{
		Title:   title,
		Summary: summary,
		Reason:  strings.TrimSpace(payload.Reason),
	}, nil
}

func (s *CandidateService) normalizeNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > s.limits.MaxNoteChars {
		return "", invalidCandidateInput("处理说明不能超过 %d 个字符", s.limits.MaxNoteChars)
	}
	return note, nil
}

// candidateDedupeHash 用「类型 + 标题 + 来源片段集合」标识一条候选，
// 让重复抽取同一版本不会堆出一模一样的待确认项。
func candidateDedupeHash(candidateType, title string, sources []CandidateSource) []byte {
	chunkIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		chunkIDs = append(chunkIDs, source.SourceChunkID)
	}
	sort.Strings(chunkIDs)
	digest := sha256.Sum256([]byte(strings.Join(
		append([]string{candidateType, strings.ToLower(title)}, chunkIDs...), "\x00")))
	return digest[:]
}

// truncateRunes 按字符（不是字节）截断，避免把多字节汉字切坏。
func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	return string([]rune(value)[:maxRunes])
}

// NewCandidateID 生成候选内容 ID。
func NewCandidateID() (string, error) { return newUUIDv7("candidate_id") }

// NewKnowledgePointID 生成知识点 ID。
func NewKnowledgePointID() (string, error) { return newUUIDv7("knowledge_point_id") }

// candidateExtractorMetadata 让服务层记录实际使用的模型与 Prompt 版本，
// 便于日后定位「某批候选是哪个版本产生的」。未实现该接口时留空。
type candidateExtractorMetadata interface {
	ModelName() string
	PromptVersion() string
}

func extractorModelName(extractor CandidateExtractor) string {
	if metadata, ok := extractor.(candidateExtractorMetadata); ok {
		return metadata.ModelName()
	}
	return ""
}

func extractorVersionName(extractor CandidateExtractor) string {
	if metadata, ok := extractor.(candidateExtractorMetadata); ok {
		return metadata.PromptVersion()
	}
	return ""
}
