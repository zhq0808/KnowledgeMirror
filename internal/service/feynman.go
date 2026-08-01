package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"KnowledgeMirror/internal/stt"
)

// ---------------------------------------------------------------------------
// 语音费曼练习 v0：录音 -> STT 转写 -> 用户确认。
//
// 边界与候选内容模块一致：STT 原始转写（raw_transcript）永远不可信，
// 只有用户确认/修正后的文本（confirmed_transcript）才能作为下一阶段
// （费曼评估，未在本次范围）的合法输入。确认转写本身不产生任何掌握状态或证据。
// ---------------------------------------------------------------------------

// 音频任务状态。v0 同步调用 STT，写入时已是 transcribed/failed 终态；
// uploaded/transcribing 为未来切换异步 Worker 预留，本次不会落库这两个值。
const (
	FeynmanAudioStatusUploaded     = "uploaded"
	FeynmanAudioStatusTranscribing = "transcribing"
	FeynmanAudioStatusTranscribed  = "transcribed"
	FeynmanAudioStatusFailed       = "failed"
)

// 练习尝试的派生状态（不落单独的 status 列，见 FeynmanAttemptDetail.Status）。
const (
	FeynmanAttemptStatusOpen                = "open"
	FeynmanAttemptStatusTranscribing        = "transcribing"
	FeynmanAttemptStatusTranscribed         = "transcribed"
	FeynmanAttemptStatusFailed              = "failed"
	FeynmanAttemptStatusTranscriptConfirmed = "transcript_confirmed"
)

// Rubric 固定维度：事实正确性、遗漏、因果链、项目映射、事实边界。
const (
	RubricDimensionFactualAccuracy = "factual_accuracy"
	RubricDimensionOmission        = "omission"
	RubricDimensionCausalChain     = "causal_chain"
	RubricDimensionProjectMapping  = "project_mapping"
	RubricDimensionFactBoundary    = "fact_boundary"
)

// FeynmanRubricTemplateV1 是首次访问知识点 Rubric 时自动实例化的默认模板版本号。
const FeynmanRubricTemplateV1 = "feynman-rubric-v1"

const rubricTotalWeight = 100

var requiredRubricDimensions = []string{
	RubricDimensionFactualAccuracy, RubricDimensionOmission, RubricDimensionCausalChain,
	RubricDimensionProjectMapping, RubricDimensionFactBoundary,
}

// allowedFeynmanAudioMimeTypes 是 Push-to-Talk 录音允许的 MIME 白名单。
// 覆盖主流浏览器 MediaRecorder 的常见输出（Chrome/Edge: webm+opus；Safari: mp4）。
var allowedFeynmanAudioMimeTypes = []string{
	"audio/webm", "audio/ogg", "audio/mp4", "audio/mpeg", "audio/mp3", "audio/wav", "audio/x-wav",
}

func isAllowedFeynmanAudioMIME(mimeType string) bool {
	base := mimeType
	if idx := strings.Index(base, ";"); idx >= 0 {
		base = base[:idx]
	}
	base = strings.TrimSpace(strings.ToLower(base))
	return slices.Contains(allowedFeynmanAudioMimeTypes, base)
}

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

var (
	// ErrFeynmanAttemptNotFound 表示练习尝试不存在或不属于当前用户。
	ErrFeynmanAttemptNotFound = errors.New("练习尝试不存在")
	// ErrFeynmanAttemptConfirmed 表示该练习已确认，处于只读状态。
	ErrFeynmanAttemptConfirmed = errors.New("练习已确认转写，不能再修改")
	// ErrFeynmanKnowledgePointNotFound 表示知识点不存在或不属于当前用户。
	ErrFeynmanKnowledgePointNotFound = errors.New("知识点不存在")
	// ErrFeynmanAudioNotReady 表示当前录音尚未转写成功，不能确认。
	ErrFeynmanAudioNotReady = errors.New("当前录音尚未转写成功，无法确认")
	// ErrFeynmanNoActiveAudio 表示该练习尚未上传过任何录音。
	ErrFeynmanNoActiveAudio = errors.New("请先完成一次录音上传")
	// ErrFeynmanIdempotencyMismatch 表示同一幂等键被用于不同的业务请求。
	ErrFeynmanIdempotencyMismatch = errors.New("幂等键已用于另一个练习主题")
)

// FeynmanInputError 是可以安全回显给用户的输入/预算类错误，接口层映射为 400。
type FeynmanInputError struct{ Message string }

func (e *FeynmanInputError) Error() string { return e.Message }

func invalidFeynmanInput(format string, args ...any) error {
	return &FeynmanInputError{Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// 领域模型
// ---------------------------------------------------------------------------

// FeynmanAudioTask 是一次录音上传与 STT 转写结果，写入后不可变。
type FeynmanAudioTask struct {
	AudioTaskID     string
	AttemptID       string
	UserID          string
	AttemptNo       int
	Status          string
	MIMEType        string
	SizeBytes       int64
	DurationMs      *int
	SHA256          []byte
	STTProvider     string
	STTModel        string
	STTRequestID    string
	RawTranscript   string
	TranscriptError string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// FeynmanTranscriptConfirmation 是用户确认/修正后的转写文本，写入后不可变。
type FeynmanTranscriptConfirmation struct {
	ConfirmationID      string
	AttemptID           string
	AudioTaskID         string
	UserID              string
	RawTranscript       string
	ConfirmedTranscript string
	Edited              bool
	ConfirmedBy         string
	ConfirmedAt         time.Time
}

// FeynmanAttempt 是一次语音费曼练习尝试的主记录。
type FeynmanAttempt struct {
	AttemptID         string
	UserID            string
	KnowledgePointID  string
	IdempotencyKey    string
	ActiveAudioTaskID string
	ConfirmationID    string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// FeynmanAttemptDetail 聚合练习尝试、当前音频任务与确认记录，供接口层一次性返回。
type FeynmanAttemptDetail struct {
	Attempt         FeynmanAttempt
	ActiveAudioTask *FeynmanAudioTask
	Confirmation    *FeynmanTranscriptConfirmation
}

// Status 返回派生状态。这是唯一的状态计算入口，避免落库状态与派生状态不一致。
func (d FeynmanAttemptDetail) Status() string {
	if d.Confirmation != nil {
		return FeynmanAttemptStatusTranscriptConfirmed
	}
	if d.ActiveAudioTask == nil {
		return FeynmanAttemptStatusOpen
	}
	switch d.ActiveAudioTask.Status {
	case FeynmanAudioStatusUploaded, FeynmanAudioStatusTranscribing:
		return FeynmanAttemptStatusTranscribing
	case FeynmanAudioStatusTranscribed:
		return FeynmanAttemptStatusTranscribed
	case FeynmanAudioStatusFailed:
		return FeynmanAttemptStatusFailed
	default:
		return FeynmanAttemptStatusOpen
	}
}

// RubricCriterion 是 Rubric 的一个评分维度。
type RubricCriterion struct {
	Dimension   string `json:"dimension"`
	Label       string `json:"label"`
	Weight      int    `json:"weight"`
	Description string `json:"description"`
}

// KnowledgePointRubric 是知识点的一个版本化评估 Rubric。
type KnowledgePointRubric struct {
	RubricID         string
	KnowledgePointID string
	UserID           string
	VersionNo        int
	TemplateVersion  string
	Criteria         []RubricCriterion
	CreatedBy        string
	CreatedAt        time.Time
}

// DefaultRubricCriteria 返回 feynman-rubric-v1 的固定默认维度。
// 这是代码常量，不是模型生成内容，因此知识点首次访问时可以直接实例化，
// 不违反“AI 只能提案、用户确认”的边界——它不产生任何掌握状态或证据。
func DefaultRubricCriteria() []RubricCriterion {
	return []RubricCriterion{
		{Dimension: RubricDimensionFactualAccuracy, Label: "事实正确性", Weight: 30,
			Description: "复述的技术事实、数据、结论是否与来源片段和真实经历一致，有没有编造或颠倒。"},
		{Dimension: RubricDimensionOmission, Label: "遗漏", Weight: 15,
			Description: "相对于该知识点的完整因果链，是否漏掉了关键环节。"},
		{Dimension: RubricDimensionCausalChain, Label: "因果链", Weight: 25,
			Description: "能否讲清“问题 → 原因 → 方案 → 取舍/效果”的完整链路，而不是孤立列点。"},
		{Dimension: RubricDimensionProjectMapping, Label: "项目映射", Weight: 20,
			Description: "是否能把知识点映射到具体项目/代码位置，还是停留在抽象概念层面。"},
		{Dimension: RubricDimensionFactBoundary, Label: "事实边界", Weight: 10,
			Description: "是否清楚区分“真实生产经历 / 个人 Demo 验证 / 仅学习过”，没有夸大或混淆。"},
	}
}

// validateRubricCriteria 校验用户提交的新版本 Rubric：必须恰好覆盖 5 个固定维度，
// 每个维度权重在 1-100 之间且总和为 100，标签与说明非空。
func validateRubricCriteria(criteria []RubricCriterion) error {
	if len(criteria) != len(requiredRubricDimensions) {
		return invalidFeynmanInput("Rubric 必须且只能包含 %d 个固定维度", len(requiredRubricDimensions))
	}
	seen := make(map[string]bool, len(criteria))
	totalWeight := 0
	for _, c := range criteria {
		if !slices.Contains(requiredRubricDimensions, c.Dimension) {
			return invalidFeynmanInput("未知的 Rubric 维度: %s", c.Dimension)
		}
		if seen[c.Dimension] {
			return invalidFeynmanInput("Rubric 维度重复: %s", c.Dimension)
		}
		seen[c.Dimension] = true
		if strings.TrimSpace(c.Label) == "" {
			return invalidFeynmanInput("维度 %s 缺少展示标签", c.Dimension)
		}
		if strings.TrimSpace(c.Description) == "" {
			return invalidFeynmanInput("维度 %s 缺少评分说明", c.Dimension)
		}
		if c.Weight <= 0 || c.Weight > rubricTotalWeight {
			return invalidFeynmanInput("维度 %s 权重必须在 1-100 之间", c.Dimension)
		}
		totalWeight += c.Weight
	}
	if totalWeight != rubricTotalWeight {
		return invalidFeynmanInput("Rubric 权重总和必须为 100，当前为 %d", totalWeight)
	}
	for _, dimension := range requiredRubricDimensions {
		if !seen[dimension] {
			return invalidFeynmanInput("Rubric 缺少必需维度: %s", dimension)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 仓储契约
// ---------------------------------------------------------------------------

// CreateFeynmanAttemptParams 是创建练习尝试的输入。
type CreateFeynmanAttemptParams struct {
	AttemptID        string
	UserID           string
	KnowledgePointID string
	IdempotencyKey   string
}

// ClaimFeynmanAudioTaskParams 是 STT 前原子创建或接管音频任务的输入。
type ClaimFeynmanAudioTaskParams struct {
	AudioTaskID string
	AttemptID   string
	UserID      string
	MIMEType    string
	SizeBytes   int64
	DurationMs  *int
	SHA256      []byte
	AudioData   []byte
	STTProvider string
	StaleBefore time.Time
}

// CompleteFeynmanAudioTaskParams 是 STT 返回后写入终态的输入。
type CompleteFeynmanAudioTaskParams struct {
	AudioTaskID     string
	AttemptID       string
	UserID          string
	Status          string
	STTProvider     string
	STTModel        string
	STTRequestID    string
	RawTranscript   string
	TranscriptError string
}

// ConfirmFeynmanTranscriptParams 是确认转写文本的输入。
type ConfirmFeynmanTranscriptParams struct {
	ConfirmationID      string
	AttemptID           string
	UserID              string
	ConfirmedTranscript string
	ConfirmedBy         string
}

// CreateRubricVersionParams 是创建知识点新 Rubric 版本的输入。
type CreateRubricVersionParams struct {
	RubricID         string
	KnowledgePointID string
	UserID           string
	TemplateVersion  string
	Criteria         []RubricCriterion
	CreatedBy        string
}

// FeynmanRepository 定义语音费曼练习 v0 的持久化契约。
// 所有方法都必须以 user_id 隔离，跨用户读写不可能命中。
type FeynmanRepository interface {
	// FindAttemptByIdempotencyKey 查询是否已存在同一幂等键的练习尝试。
	FindAttemptByIdempotencyKey(ctx context.Context, userID, idempotencyKey string) (FeynmanAttemptDetail, bool, error)
	// CreateAttempt 创建一条新的练习尝试；knowledge_point_id 不属于该用户时返回 ErrFeynmanKnowledgePointNotFound。
	CreateAttempt(ctx context.Context, params CreateFeynmanAttemptParams) (FeynmanAttemptDetail, error)
	// GetAttemptDetail 返回练习尝试及其当前音频任务、确认记录（如有）。
	GetAttemptDetail(ctx context.Context, userID, attemptID string) (FeynmanAttemptDetail, error)
	// ClaimAudioTask 在一个事务内按音频哈希去重、写入 transcribing 任务并激活。
	// claimed=false 表示已有新鲜任务，本次不得重复调用 STT。
	ClaimAudioTask(ctx context.Context, params ClaimFeynmanAudioTaskParams) (detail FeynmanAttemptDetail, claimed bool, err error)
	// CompleteAudioTask 只完成指定任务，不改变 active 指针，避免旧录音晚返回覆盖新录音。
	CompleteAudioTask(ctx context.Context, params CompleteFeynmanAudioTaskParams) (FeynmanAttemptDetail, error)
	// ConfirmTranscript 在一个事务内写入确认记录，并把 attempt.confirmation_id 定住。
	ConfirmTranscript(ctx context.Context, params ConfirmFeynmanTranscriptParams) (FeynmanAttemptDetail, error)

	// GetActiveRubric 返回知识点当前生效的 Rubric 版本；不存在返回 found=false。
	GetActiveRubric(ctx context.Context, userID, knowledgePointID string) (KnowledgePointRubric, bool, error)
	// InitializeRubric 在知识点行锁内仅当尚无当前版本时创建默认 v1，否则返回已有版本。
	InitializeRubric(ctx context.Context, params CreateRubricVersionParams) (KnowledgePointRubric, error)
	// CreateRubricVersion 创建新的 Rubric 版本并把知识点的当前版本指针移过去。
	CreateRubricVersion(ctx context.Context, params CreateRubricVersionParams) (KnowledgePointRubric, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// FeynmanLimits 是 Push-to-Talk 录音的硬上限，全部是防御性预算。
type FeynmanLimits struct {
	MaxAudioBytes      int64
	MaxDurationMS      int
	MaxTranscriptChars int
	TranscribingStale  time.Duration
}

// FeynmanService 实现语音费曼练习 v0：录音上传、STT 转写、转写确认、Rubric 版本化。
type FeynmanService struct {
	repo                FeynmanRepository
	stt                 stt.Provider
	limits              FeynmanLimits
	log                 *slog.Logger
	evaluationRepo      FeynmanEvaluationRepository
	evaluator           FeynmanEvaluationModel
	evaluationRetriever ChatRetriever
}

// NewFeynmanService 构造费曼练习服务。log 为 nil 时使用 slog 默认 logger。
func NewFeynmanService(repo FeynmanRepository, sttProvider stt.Provider, limits FeynmanLimits, log *slog.Logger) *FeynmanService {
	if log == nil {
		log = slog.Default()
	}
	if limits.TranscribingStale <= 0 {
		limits.TranscribingStale = 2 * time.Minute
	}
	return &FeynmanService{repo: repo, stt: sttProvider, limits: limits, log: log}
}

// Limits 返回当前生效的预算配置，供接口层提前校验请求体大小。
func (s *FeynmanService) Limits() FeynmanLimits { return s.limits }

// STTProviderName 返回当前使用的 STT 供应商标识，供启动日志与排查确认实际生效的供应商。
func (s *FeynmanService) STTProviderName() string { return s.stt.Name() }

// CreateAttempt 创建（或复用）一次练习尝试。同一用户同一 idempotency_key 永远返回同一条记录。
func (s *FeynmanService) CreateAttempt(ctx context.Context, userID, knowledgePointID, idempotencyKey string) (FeynmanAttemptDetail, error) {
	userID = strings.TrimSpace(userID)
	knowledgePointID = strings.TrimSpace(knowledgePointID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if userID == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("用户身份缺失")
	}
	if knowledgePointID == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("请先选择本次练习的知识点")
	}
	if idempotencyKey == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("缺少幂等键")
	}
	if utf8.RuneCountInString(idempotencyKey) > 128 {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("幂等键长度超过上限")
	}

	if existing, found, err := s.repo.FindAttemptByIdempotencyKey(ctx, userID, idempotencyKey); err != nil {
		return FeynmanAttemptDetail{}, fmt.Errorf("查询练习幂等记录失败: %w", err)
	} else if found {
		if existing.Attempt.KnowledgePointID != knowledgePointID {
			return FeynmanAttemptDetail{}, ErrFeynmanIdempotencyMismatch
		}
		return existing, nil
	}

	attemptID, err := NewFeynmanAttemptID()
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}

	detail, err := s.repo.CreateAttempt(ctx, CreateFeynmanAttemptParams{
		AttemptID:        attemptID,
		UserID:           userID,
		KnowledgePointID: knowledgePointID,
		IdempotencyKey:   idempotencyKey,
	})
	if errors.Is(err, ErrFeynmanIdempotencyConflict) {
		// 并发重放：另一个请求已经用同一个幂等键落库，本次直接复用其结果。
		existing, found, findErr := s.repo.FindAttemptByIdempotencyKey(ctx, userID, idempotencyKey)
		if findErr != nil {
			return FeynmanAttemptDetail{}, fmt.Errorf("查询练习幂等记录失败: %w", findErr)
		}
		if found {
			if existing.Attempt.KnowledgePointID != knowledgePointID {
				return FeynmanAttemptDetail{}, ErrFeynmanIdempotencyMismatch
			}
			return existing, nil
		}
	}
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}
	return detail, nil
}

// GetAttempt 返回练习尝试详情，供前端刷新页面后恢复到正确步骤。
func (s *FeynmanService) GetAttempt(ctx context.Context, userID, attemptID string) (FeynmanAttemptDetail, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("用户身份缺失")
	}
	return s.repo.GetAttemptDetail(ctx, userID, attemptID)
}

// UploadAudio 上传一段 Push-to-Talk 录音并同步完成 STT 转写。
//
// v0 决策：STT 在本次请求内同步调用，不进异步队列——录音有硬上限（见 FeynmanLimits），
// 单次转写耗时可控。相同字节的重复提交会命中 (attempt_id, sha256) 幂等去重，不重复调用 STT。
func (s *FeynmanService) UploadAudio(ctx context.Context, userID, attemptID string, audio []byte, mimeType string, durationMs *int) (FeynmanAttemptDetail, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("用户身份缺失")
	}
	if len(audio) == 0 {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("音频内容为空")
	}
	if int64(len(audio)) > s.limits.MaxAudioBytes {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("音频大小超过上限（%d 字节）", s.limits.MaxAudioBytes)
	}
	if !isAllowedFeynmanAudioMIME(mimeType) {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("不支持的音频格式: %s", mimeType)
	}
	if durationMs != nil && (*durationMs <= 0 || *durationMs > s.limits.MaxDurationMS) {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("录音时长超过上限（%d 毫秒）", s.limits.MaxDurationMS)
	}

	hash := sha256.Sum256(audio)
	audioTaskID, err := NewFeynmanAudioTaskID()
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}

	claimedDetail, claimed, err := s.repo.ClaimAudioTask(ctx, ClaimFeynmanAudioTaskParams{
		AudioTaskID: audioTaskID,
		AttemptID:   attemptID,
		UserID:      userID,
		MIMEType:    mimeType,
		SizeBytes:   int64(len(audio)),
		DurationMs:  durationMs,
		SHA256:      hash[:],
		AudioData:   audio,
		STTProvider: s.stt.Name(),
		StaleBefore: time.Now().Add(-s.limits.TranscribingStale),
	})
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}
	if !claimed {
		s.log.Info("命中已有录音任务，跳过重复 STT 调用", "attempt_id", attemptID)
		return claimedDetail, nil
	}
	if claimedDetail.ActiveAudioTask != nil {
		audioTaskID = claimedDetail.ActiveAudioTask.AudioTaskID
	}

	params := CompleteFeynmanAudioTaskParams{
		AudioTaskID: audioTaskID,
		AttemptID:   attemptID,
		UserID:      userID,
		STTProvider: s.stt.Name(),
	}

	transcript, sttErr := s.stt.Transcribe(ctx, audio, mimeType)
	if sttErr != nil {
		s.log.Warn("STT 转写失败", "attempt_id", attemptID, "provider", s.stt.Name(), "error", sttErr)
		params.Status = FeynmanAudioStatusFailed
		params.TranscriptError = truncateFeynmanError(sttErr.Error(), 2000)
	} else {
		text := strings.TrimSpace(transcript.Text)
		switch {
		case text == "":
			params.Status = FeynmanAudioStatusFailed
			params.TranscriptError = emptyTranscriptError
		case utf8.RuneCountInString(text) > s.limits.MaxTranscriptChars:
			params.Status = FeynmanAudioStatusFailed
			params.TranscriptError = fmt.Sprintf("STT 转写超过 %d 字上限", s.limits.MaxTranscriptChars)
		default:
			params.Status = FeynmanAudioStatusTranscribed
			params.RawTranscript = text
			params.STTProvider = transcript.Provider
			params.STTModel = transcript.Model
			params.STTRequestID = transcript.RequestID
		}
	}

	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	detail, err := s.repo.CompleteAudioTask(completeCtx, params)
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}
	return detail, nil
}

func truncateFeynmanError(value string, maxChars int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxChars {
		return string(runes)
	}
	return string(runes[:maxChars])
}

// ConfirmTranscript 确认或修正当前有效录音的转写文本。
// 只能对状态为 transcribed 的当前录音确认；一旦确认，该练习永久只读。
func (s *FeynmanService) ConfirmTranscript(ctx context.Context, userID, attemptID, confirmedTranscript string) (FeynmanAttemptDetail, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("用户身份缺失")
	}
	confirmedTranscript = strings.TrimSpace(confirmedTranscript)
	if confirmedTranscript == "" {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("确认文本不能为空")
	}
	if utf8.RuneCountInString(confirmedTranscript) > s.limits.MaxTranscriptChars {
		return FeynmanAttemptDetail{}, invalidFeynmanInput("确认文本长度超过上限（%d 字）", s.limits.MaxTranscriptChars)
	}

	current, err := s.repo.GetAttemptDetail(ctx, userID, attemptID)
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}
	if current.Confirmation != nil {
		return FeynmanAttemptDetail{}, ErrFeynmanAttemptConfirmed
	}
	if current.ActiveAudioTask == nil {
		return FeynmanAttemptDetail{}, ErrFeynmanNoActiveAudio
	}
	if current.ActiveAudioTask.Status != FeynmanAudioStatusTranscribed {
		return FeynmanAttemptDetail{}, ErrFeynmanAudioNotReady
	}

	confirmationID, err := NewFeynmanConfirmationID()
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}

	detail, err := s.repo.ConfirmTranscript(ctx, ConfirmFeynmanTranscriptParams{
		ConfirmationID:      confirmationID,
		AttemptID:           attemptID,
		UserID:              userID,
		ConfirmedTranscript: confirmedTranscript,
		ConfirmedBy:         userID,
	})
	if err != nil {
		return FeynmanAttemptDetail{}, err
	}
	return detail, nil
}

// GetRubric 返回知识点当前生效的 Rubric；不存在任何版本时按固定模板自动创建 v1。
//
// 自动创建不违反“AI 只能提案、用户确认”的边界：模板是代码常量而非模型生成内容，
// 且只定义评估看哪些维度，不产生任何掌握状态或证据。
func (s *FeynmanService) GetRubric(ctx context.Context, userID, knowledgePointID string) (KnowledgePointRubric, error) {
	userID = strings.TrimSpace(userID)
	knowledgePointID = strings.TrimSpace(knowledgePointID)
	if userID == "" {
		return KnowledgePointRubric{}, invalidFeynmanInput("用户身份缺失")
	}
	if knowledgePointID == "" {
		return KnowledgePointRubric{}, invalidFeynmanInput("知识点ID缺失")
	}

	existing, found, err := s.repo.GetActiveRubric(ctx, userID, knowledgePointID)
	if err != nil {
		return KnowledgePointRubric{}, fmt.Errorf("查询知识点 Rubric 失败: %w", err)
	}
	if found {
		return existing, nil
	}

	rubricID, err := NewKnowledgePointRubricID()
	if err != nil {
		return KnowledgePointRubric{}, err
	}
	return s.repo.InitializeRubric(ctx, CreateRubricVersionParams{
		RubricID:         rubricID,
		KnowledgePointID: knowledgePointID,
		UserID:           userID,
		TemplateVersion:  FeynmanRubricTemplateV1,
		Criteria:         DefaultRubricCriteria(),
		CreatedBy:        userID,
	})
}

// CreateRubricVersion 让用户提交一份新的 Rubric 版本，替换当前生效版本；历史版本永久保留。
func (s *FeynmanService) CreateRubricVersion(ctx context.Context, userID, knowledgePointID string, criteria []RubricCriterion) (KnowledgePointRubric, error) {
	userID = strings.TrimSpace(userID)
	knowledgePointID = strings.TrimSpace(knowledgePointID)
	if userID == "" {
		return KnowledgePointRubric{}, invalidFeynmanInput("用户身份缺失")
	}
	if knowledgePointID == "" {
		return KnowledgePointRubric{}, invalidFeynmanInput("知识点ID缺失")
	}
	if err := validateRubricCriteria(criteria); err != nil {
		return KnowledgePointRubric{}, err
	}

	rubricID, err := NewKnowledgePointRubricID()
	if err != nil {
		return KnowledgePointRubric{}, err
	}
	return s.repo.CreateRubricVersion(ctx, CreateRubricVersionParams{
		RubricID:         rubricID,
		KnowledgePointID: knowledgePointID,
		UserID:           userID,
		TemplateVersion:  FeynmanRubricTemplateV1,
		Criteria:         criteria,
		CreatedBy:        userID,
	})
}

// ---------------------------------------------------------------------------
// ID 生成
// ---------------------------------------------------------------------------

// ErrFeynmanIdempotencyConflict 表示并发重放命中了同一个幂等键，调用方应重新查询已有结果。
var ErrFeynmanIdempotencyConflict = errors.New("练习幂等键冲突")

func NewFeynmanAttemptID() (string, error)       { return newUUIDv7("attempt_id") }
func NewFeynmanAudioTaskID() (string, error)     { return newUUIDv7("audio_task_id") }
func NewFeynmanConfirmationID() (string, error)  { return newUUIDv7("confirmation_id") }
func NewKnowledgePointRubricID() (string, error) { return newUUIDv7("rubric_id") }
