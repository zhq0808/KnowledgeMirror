package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 每日教练任务类型。
const (
	CoachTaskTypeFeynmanNew   = "feynman_new"
	CoachTaskTypeFeynmanRetry = "feynman_retry"
)

// 每日计划角色：每天第一条是必做，其余最多两条选做。
const (
	CoachPlanRoleRequired = "required"
	CoachPlanRoleOptional = "optional"
)

// 每日教练任务状态。
const (
	CoachTaskStatusPending       = "pending"
	CoachTaskStatusInProgress    = "in_progress"
	CoachTaskStatusAwaitingRetry = "awaiting_retry"
	CoachTaskStatusCompleted     = "completed"
	CoachTaskStatusSkipped       = "skipped"
)

// 教练分析结果。
const (
	CoachAttemptOutcomePassed        = "passed"
	CoachAttemptOutcomeRetryRequired = "retry_required"
)

// 薄弱点归类、学习诊断及聚合状态。
const (
	CoachGapClassificationNew       = "new"
	CoachGapClassificationRecurrent = "recurrent"
	CoachGapTypeKnowledge           = "knowledge_gap"
	CoachGapTypeRecall              = "recall_failure"
	CoachGapTypeExpression          = "expression_structure"
	CoachGapTypeProjectEvidence     = "missing_project_evidence"
	FeynmanGapStatusOpen            = "open"
	FeynmanGapStatusResolved        = "resolved"
)

// 复习排程状态。
const (
	FeynmanGapReviewStatusScheduled = "scheduled"
	FeynmanGapReviewStatusPassed    = "passed"
	FeynmanGapReviewStatusFailed    = "failed"
	FeynmanGapReviewStatusMissed    = "missed"
	FeynmanGapReviewStatusCancelled = "cancelled"
)

var (
	ErrCoachTaskNotFound      = errors.New("教练任务不存在")
	ErrCoachTaskNotStartable  = errors.New("教练任务当前不可开始")
	ErrCoachTaskIDRequired    = errors.New("当前教练练习必须携带教练任务ID")
	ErrCoachTaskMismatch      = errors.New("教练任务ID与当前练习不匹配")
	ErrCoachUnavailable       = errors.New("教练练习暂时不可用")
	ErrCoachAttemptConflict   = errors.New("该回答已提交过教练分析")
	ErrCoachAnalysisInput     = errors.New("教练分析持久化输入无效")
	ErrCoachCorrectionPending = errors.New("复测失败后必须先完成即时纠正")
	ErrCoachQueryInput        = errors.New("教练查询参数无效")
)

var validCoachGapTypes = map[string]struct{}{
	CoachGapTypeKnowledge:       {},
	CoachGapTypeRecall:          {},
	CoachGapTypeExpression:      {},
	CoachGapTypeProjectEvidence: {},
}

var validCoachDiagnosticDimensions = map[string]struct{}{
	FeynmanDimensionKeyPoints:      {},
	FeynmanDimensionCausalChain:    {},
	FeynmanDimensionProjectMapping: {},
	FeynmanDimensionFactBoundary:   {},
	FeynmanDimensionExpression:     {},
}

// CoachDailyTask 是一天内给用户开出的单个教练处方任务。
type CoachDailyTask struct {
	CoachTaskID      string
	UserID           string
	TaskDate         time.Time
	TaskType         string
	PlanRole         string
	Status           string
	SourceKey        string
	QuestionText     string
	KnowledgePointID string
	SourceGapID      string
	SourceReviewID   string
	Priority         int
	SessionID        string
	StartedAt        *time.Time
	CompletedAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CoachAttempt 是教练任务的一次作答分析快照。AnswerMessageID 指向真实聊天消息，
// AnalysisJSON 只允许保存清洗后的结构化判断，不复制回答正文。
type CoachAttempt struct {
	CoachAttemptID       string
	CoachTaskID          string
	UserID               string
	SessionID            string
	AnswerMessageID      string
	OriginalQuestionText string
	AnalysisJSON         json.RawMessage
	Outcome              string
	PromptVersion        string
	ModelName            string
	CreatedAt            time.Time
}

// CoachGapEvidence 是一次分析识别出的薄弱点输入。GapID 只作为本次提交内关联
// review 的临时引用；仓储必须在事务内根据 (user_id, gap_key) 派生 canonical gap_id
// 和 new/recurrent 分类，不能信任调用方提供的 GapID 或 Classification。
type CoachGapEvidence struct {
	AttemptGapID        string
	ForceCanonicalGapID string
	GapID               string
	GapKey              string
	GapType             string
	DiagnosticDimension string
	Classification      string
	Title               string
	Description         string
	Severity            int
	IsFocus             bool
	RequiresCorrection  bool
	EvidenceJSON        json.RawMessage
}

// FeynmanGap 是跨教练尝试聚合后的薄弱点当前投影。
type FeynmanGap struct {
	GapID               string
	UserID              string
	KnowledgePointID    string
	GapKey              string
	GapType             string
	DiagnosticDimension string
	Title               string
	Description         string
	Status              string
	EvidenceCount       int
	FirstSeenAt         time.Time
	LastSeenAt          time.Time
	NextReviewAt        *time.Time
	NextReviewDate      *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// FeynmanGapReview 是后续调度器可领取的复习钩子。
type FeynmanGapReview struct {
	GapReviewID        string
	ReviewCycleID      string
	GapID              string
	SourceAttemptID    string
	UserID             string
	Stage              int
	ScheduledDate      time.Time
	ScheduledFor       time.Time
	Status             string
	CompletedAttemptID string
	CompletedAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// PriorGapEvidence 是判断“新薄弱点/重复薄弱点”所需的最小历史证据。
type PriorGapEvidence struct {
	GapID            string
	GapKey           string
	GapType          string
	Status           string
	EvidenceCount    int
	LastSeenAt       time.Time
	LastAttemptID    string
	LastSeverity     int
	SuccessfulOutput bool
}

// NormalizeCoachGapKey 生成稳定的薄弱点业务键。模型输出不能直接当唯一键，
// 调用方应给出低熵、可解释的 key，本函数只负责格式归一化与边界校验。
func NormalizeCoachGapKey(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "", fmt.Errorf("%w: 薄弱点键为空", ErrCoachAnalysisInput)
	}
	if utf8.RuneCountInString(value) > 160 {
		return "", fmt.Errorf("%w: 薄弱点键超过 160 字", ErrCoachAnalysisInput)
	}
	return value, nil
}

// ClassifyCoachGap 根据批量预取的历史证据返回新发/复发分类及应复用的 gap_id。
// 没有历史证据时 gapID 为空，由调用方生成新 ID；复发时必须复用既有 ID。
func ClassifyCoachGap(gapKey string, prior map[string]PriorGapEvidence) (classification, gapID string, err error) {
	key, err := NormalizeCoachGapKey(gapKey)
	if err != nil {
		return "", "", err
	}
	if evidence, found := prior[key]; found {
		if strings.TrimSpace(evidence.GapID) == "" {
			return "", "", fmt.Errorf("%w: 历史薄弱点缺少 gap_id", ErrCoachAnalysisInput)
		}
		return CoachGapClassificationRecurrent, evidence.GapID, nil
	}
	return CoachGapClassificationNew, "", nil
}

// ValidateCoachAnalysisCommit 在开启数据库事务前校验并归一化跨行不变量。
func ValidateCoachAnalysisCommit(params *CommitCoachAnalysisParams) error {
	if params == nil {
		return fmt.Errorf("%w: 提交参数为空", ErrCoachAnalysisInput)
	}
	if strings.TrimSpace(params.Attempt.CoachAttemptID) == "" ||
		strings.TrimSpace(params.Attempt.CoachTaskID) == "" ||
		strings.TrimSpace(params.Attempt.UserID) == "" ||
		strings.TrimSpace(params.Attempt.SessionID) == "" ||
		strings.TrimSpace(params.Attempt.AnswerMessageID) == "" {
		return fmt.Errorf("%w: 尝试身份字段不完整", ErrCoachAnalysisInput)
	}
	if strings.TrimSpace(params.Attempt.OriginalQuestionText) == "" {
		return fmt.Errorf("%w: 原题快照为空", ErrCoachAnalysisInput)
	}
	if params.Attempt.Outcome != CoachAttemptOutcomePassed && params.Attempt.Outcome != CoachAttemptOutcomeRetryRequired {
		return fmt.Errorf("%w: 未知分析结果 %q", ErrCoachAnalysisInput, params.Attempt.Outcome)
	}
	if strings.TrimSpace(params.Attempt.PromptVersion) == "" || strings.TrimSpace(params.Attempt.ModelName) == "" {
		return fmt.Errorf("%w: Prompt 或模型版本为空", ErrCoachAnalysisInput)
	}
	if !validJSONObject(params.Attempt.AnalysisJSON) {
		return fmt.Errorf("%w: analysis JSON 必须是对象", ErrCoachAnalysisInput)
	}
	if containsCoachAnswerCopy(params.Attempt.AnalysisJSON) {
		return fmt.Errorf("%w: analysis JSON 不能复制回答正文", ErrCoachAnalysisInput)
	}
	focusCount := 0
	seenKeys := make(map[string]struct{}, len(params.Gaps))
	requestGapRefs := make(map[string]struct{}, len(params.Gaps))
	for i := range params.Gaps {
		gap := &params.Gaps[i]
		key, err := NormalizeCoachGapKey(gap.GapKey)
		if err != nil {
			return err
		}
		gap.GapKey = key
		if _, exists := seenKeys[key]; exists {
			return fmt.Errorf("%w: 同一次分析出现重复薄弱点 %q", ErrCoachAnalysisInput, key)
		}
		seenKeys[key] = struct{}{}
		if _, ok := validCoachGapTypes[gap.GapType]; !ok {
			return fmt.Errorf("%w: 未知薄弱点类型 %q", ErrCoachAnalysisInput, gap.GapType)
		}
		if _, ok := validCoachDiagnosticDimensions[gap.DiagnosticDimension]; !ok {
			return fmt.Errorf("%w: 未知诊断维度 %q", ErrCoachAnalysisInput, gap.DiagnosticDimension)
		}
		// GapID 是本次请求内让 review 指向 gap 的临时引用；canonical gap_id 和
		// classification 由仓储在事务内派生，故这里不接受调用方分类结论。
		if strings.TrimSpace(gap.AttemptGapID) == "" || strings.TrimSpace(gap.GapID) == "" || strings.TrimSpace(gap.Title) == "" {
			return fmt.Errorf("%w: 薄弱点临时身份或标题为空", ErrCoachAnalysisInput)
		}
		if _, exists := requestGapRefs[gap.GapID]; exists {
			return fmt.Errorf("%w: 同一次分析出现重复薄弱点临时引用 %q", ErrCoachAnalysisInput, gap.GapID)
		}
		requestGapRefs[gap.GapID] = struct{}{}
		if gap.Severity < 1 || gap.Severity > 5 {
			return fmt.Errorf("%w: 薄弱点严重度必须在 1-5 之间", ErrCoachAnalysisInput)
		}
		if !validJSONObject(gap.EvidenceJSON) {
			return fmt.Errorf("%w: evidence JSON 必须是对象", ErrCoachAnalysisInput)
		}
		if gap.IsFocus {
			focusCount++
		}
	}
	if focusCount > 1 {
		return fmt.Errorf("%w: 一次分析最多一个 focus", ErrCoachAnalysisInput)
	}
	if params.Attempt.Outcome == CoachAttemptOutcomeRetryRequired && focusCount != 1 {
		return fmt.Errorf("%w: 要求重答时必须且只能有一个 focus", ErrCoachAnalysisInput)
	}
	if params.CorrectionDate.IsZero() {
		return fmt.Errorf("%w: 缺少任务本地日期", ErrCoachAnalysisInput)
	}
	if params.ReviewDecision.IsRetest {
		if params.ReviewDecision.CurrentReviewStatus != FeynmanGapReviewStatusPassed &&
			params.ReviewDecision.CurrentReviewStatus != FeynmanGapReviewStatusFailed {
			return fmt.Errorf("%w: 复测结果必须是 passed 或 failed", ErrCoachAnalysisInput)
		}
		if params.ReviewDecision.TargetRecurred != (params.ReviewDecision.CurrentReviewStatus == FeynmanGapReviewStatusFailed) {
			return fmt.Errorf("%w: 复测目标复发与结果不一致", ErrCoachAnalysisInput)
		}
		if params.ReviewDecision.TargetRecurred {
			if params.Attempt.Outcome != CoachAttemptOutcomeRetryRequired {
				return fmt.Errorf("%w: 目标复发必须要求即时重答", ErrCoachAnalysisInput)
			}
			foundCorrection := false
			for _, gap := range params.Gaps {
				if gap.ForceCanonicalGapID != "" && gap.RequiresCorrection {
					foundCorrection = true
					break
				}
			}
			if !foundCorrection {
				return fmt.Errorf("%w: 目标复发必须持久化待纠正目标", ErrCoachAnalysisInput)
			}
		}
	}
	if params.Attempt.Outcome == CoachAttemptOutcomePassed &&
		(params.PracticeState.RetryRequired || params.PracticeState.State != FeynmanStateIdle || params.PracticeState.CoachTaskID != "") {
		return fmt.Errorf("%w: 已通过的尝试必须清空教练练习状态", ErrCoachAnalysisInput)
	}
	if params.Attempt.Outcome == CoachAttemptOutcomeRetryRequired &&
		(params.PracticeState.State != FeynmanStateAwaitingRetry || !params.PracticeState.RetryRequired) {
		return fmt.Errorf("%w: 要求重答时练习状态必须为 awaiting_retry", ErrCoachAnalysisInput)
	}
	return nil
}

func validJSONObject(value json.RawMessage) bool {
	if len(value) == 0 || !json.Valid(value) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}

// containsCoachAnswerCopy 拒绝常见的回答正文键。结构化证据可以保存片段位置、维度和
// 归类依据，但完整答案只能通过 answer_message_id 读取。
func containsCoachAnswerCopy(value json.RawMessage) bool {
	var root any
	if json.Unmarshal(value, &root) != nil {
		return false
	}
	forbidden := map[string]struct{}{
		"answer": {}, "answer_text": {}, "answertext": {}, "original_answer": {},
		"originalanswer": {}, "raw_answer": {}, "rawanswer": {}, "response_text": {},
		"responsetext": {}, "transcript": {}, "confirmed_transcript": {},
	}
	var walk func(any) bool
	walk = func(node any) bool {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.TrimSpace(key))
				if _, denied := forbidden[normalized]; denied {
					return true
				}
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

var fixedCoachReviewOffsets = [...]int{1, 3, 7}

// FixedCoachReviewDates 以实际纠正通过的用户本地 DATE 为锚点生成固定 +1/+3/+7 到期日。
// 先重建本地午夜再 AddDate，避免夏令时切换导致 24h 加法跨错自然日。
func FixedCoachReviewDates(taskDate time.Time) [3]time.Time {
	anchor := localDate(taskDate)
	return [3]time.Time{
		anchor.AddDate(0, 0, fixedCoachReviewOffsets[0]),
		anchor.AddDate(0, 0, fixedCoachReviewOffsets[1]),
		anchor.AddDate(0, 0, fixedCoachReviewOffsets[2]),
	}
}

// CoachReviewDecision 是一次作答对当前复测的独立判定。
type CoachReviewDecision struct {
	IsRetest            bool
	TargetRecurred      bool
	CurrentReviewStatus string
}

// DecideCoachReviewLifecycle 只根据任务来源、目标是否复发以及当前阶段作决定；
// 新出现的其它 gap 不影响目标复测自身的 passed/failed 结论。
func DecideCoachReviewLifecycle(task CoachDailyTask, gaps []CoachGapEvidence) CoachReviewDecision {
	decision := CoachReviewDecision{IsRetest: task.SourceReviewID != "" && task.SourceGapID != ""}
	if !decision.IsRetest {
		return decision
	}
	for _, gap := range gaps {
		if gap.ForceCanonicalGapID == task.SourceGapID {
			decision.TargetRecurred = true
			decision.CurrentReviewStatus = FeynmanGapReviewStatusFailed
			return decision
		}
	}
	decision.CurrentReviewStatus = FeynmanGapReviewStatusPassed
	return decision
}

// StartCoachTaskParams 是把一条待处理任务原子绑定到会话的输入。
type StartCoachTaskParams struct {
	UserID        string
	CoachTaskID   string
	SessionID     string
	UserMessageID string
	Reply         string
	StartedAt     time.Time
}

// CoachTaskControlParams 原子处理 pause/resume/skip/stop，ExpectedTaskID 防止串题。
type CoachTaskControlParams struct {
	UserID        string
	SessionID     string
	CoachTaskID   string
	Action        string
	UserMessageID string
	Reply         string
	ControlledAt  time.Time
	PracticeState FeynmanPracticeState
}

// CommitCoachAnalysisParams 是分析结果事务提交单元。Repository 必须在一个事务内写入
// attempt、全部 gap/attempt-gap、review，并推进 task/practice 投影。
type CommitCoachAnalysisParams struct {
	Attempt        CoachAttempt
	Gaps           []CoachGapEvidence
	ReviewDecision CoachReviewDecision
	PracticeState  FeynmanPracticeState
	CompletedAt    time.Time
	CorrectionDate time.Time
}

// CoachPlanRepository 是每日计划和只读查询所需的最小契约。
type CoachPlanRepository interface {
	EnsureDailyPlan(ctx context.Context, userID string, date time.Time) (CoachDailyPlan, error)
	GetProgress(ctx context.Context, userID string, from, to time.Time) (CoachProgress, error)
	ListGaps(ctx context.Context, userID, status string, limit int) ([]FeynmanGap, error)
}

// CoachRepository 定义完整数据基础层，不包含 HTTP、模型调用或定时调度。
type CoachRepository interface {
	CoachPlanRepository
	GetTask(ctx context.Context, userID, coachTaskID string) (CoachDailyTask, error)
	GetGap(ctx context.Context, userID, gapID string) (FeynmanGap, error)
	StartTaskInSession(ctx context.Context, params StartCoachTaskParams) (CoachDailyTask, error)
	FetchPriorGapEvidence(ctx context.Context, userID string, gapKeys []string) (map[string]PriorGapEvidence, error)
	ControlTask(ctx context.Context, params CoachTaskControlParams) error
	CommitAnalysis(ctx context.Context, params CommitCoachAnalysisParams) (CoachAttempt, error)
}

func NewCoachTaskID() (string, error)        { return newUUIDv7("coach_task_id") }
func NewCoachAttemptID() (string, error)     { return newUUIDv7("coach_attempt_id") }
func NewFeynmanGapID() (string, error)       { return newUUIDv7("gap_id") }
func NewCoachAttemptGapID() (string, error)  { return newUUIDv7("attempt_gap_id") }
func NewFeynmanGapReviewID() (string, error) { return newUUIDv7("gap_review_id") }
func NewFeynmanGapReviewCycleID() (string, error) {
	return newUUIDv7("review_cycle_id")
}
