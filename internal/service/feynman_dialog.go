package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 对话式费曼学习 · 会话状态机
//
// 费曼学习不是一种 UI 模式，而是普通对话里的一种持续状态：同一个聊天框、同一个
// /chat/stream 接口，由服务端根据会话状态决定这一条消息是普通聊天还是一次练习回答。
//
// 并发安全性直接复用已有的 turn 租约（agent_turn_lease 的 uk_turn_lease_active_session
// 保证同一 session 同时只有一个 active turn），因此这里不需要再引入任何锁：
// 状态的读-改-写始终发生在一个已经被串行化的 turn 内部。
// ---------------------------------------------------------------------------

// 固定文案：全部由服务层控制，绝不交给模型生成，保证控制类回复稳定可预期。
const (
	feynmanCopyAskTopic       = "好，我们开始。你想先讲哪个主题？说一句就行，比如“我来讲为什么当时选 Kafka”。"
	feynmanCopyTopicAccepted  = "明白了，这次讲：%s\n\n你直接讲就行，讲完我来指出哪里没讲到、哪里说反了。"
	feynmanCopyPaused         = "好，先停在这里。想继续时说一句“继续”，这题我给你留着。"
	feynmanCopyResumed        = "继续。上一题是：%s"
	feynmanCopySkipped        = "这题跳过。下一个你想讲什么？"
	feynmanCopyRetry          = "好，重新讲一次：%s"
	feynmanCopyStopped        = "费曼练习结束，我们回到普通对话。"
	feynmanCopyStoppedIdle    = "好，这次先不练了。想练的时候再点一下“费曼学习”。"
	feynmanCopyAnalysisFailed = "这次分析没跑通（%s）。题目我给你留着，直接再讲一次就行。"
)

// FeynmanPracticeState 是一个会话当前所处的练习状态（当前投影，不是历史账本）。
type FeynmanPracticeState struct {
	SessionID             string
	UserID                string
	State                 string
	ActiveQuestionText    string
	QuestionOrigin        string
	LastAnsweredMessageID string
	LastFeedback          string
	RoundNo               int
	UpdatedAt             time.Time
}

// Active 表示当前会话是否处于练习中，供接口层决定要不要下发状态条。
func (s FeynmanPracticeState) Active() bool {
	return s.State != "" && s.State != FeynmanStateIdle
}

// FeynmanPracticeRepository 持久化会话级练习状态。
type FeynmanPracticeRepository interface {
	Load(ctx context.Context, userID, sessionID string) (FeynmanPracticeState, bool, error)
	Save(ctx context.Context, state FeynmanPracticeState) error
}

// FeynmanDialogLimits 是防御性预算：调大只影响一次反馈的长度，不会绕过任何边界。
type FeynmanDialogLimits struct {
	MaxControlPhraseRunes int
	MaxTopicRunes         int
	MaxProbeRunes         int
	MaxGaps               int
	MaxSecondaryGaps      int
	MaxContextTurns       int
	MaxAnswerRunes        int
}

func (l FeynmanDialogLimits) withDefaults() FeynmanDialogLimits {
	if l.MaxControlPhraseRunes <= 0 {
		l.MaxControlPhraseRunes = defaultMaxControlPhraseRunes
	}
	if l.MaxTopicRunes <= 0 {
		l.MaxTopicRunes = defaultMaxFeynmanTopicRunes
	}
	if l.MaxProbeRunes <= 0 {
		l.MaxProbeRunes = defaultMaxFeynmanProbeRunes
	}
	if l.MaxGaps <= 0 {
		l.MaxGaps = defaultMaxFeynmanGaps
	}
	if l.MaxSecondaryGaps <= 0 {
		l.MaxSecondaryGaps = defaultMaxFeynmanSecondaryGaps
	}
	if l.MaxContextTurns <= 0 {
		l.MaxContextTurns = defaultMaxFeynmanContextTurns
	}
	if l.MaxAnswerRunes <= 0 {
		l.MaxAnswerRunes = defaultMaxFeynmanAnswerRunes
	}
	return l
}

// FeynmanDialogService 编排“意图识别 → 状态迁移 → 分析 → 追问”这一条链路。
type FeynmanDialogService struct {
	repo      FeynmanPracticeRepository
	analyzer  FeynmanAnswerAnalyzer
	retriever ChatRetriever
	limits    FeynmanDialogLimits
	log       *slog.Logger
}

// NewFeynmanDialogService 构造对话式费曼服务。retriever 可为 nil（此时分析不带资料，
// 但上下文里会有明确的“未启用检索”占位，模型不会因此编造引用）。
func NewFeynmanDialogService(repo FeynmanPracticeRepository, analyzer FeynmanAnswerAnalyzer, retriever ChatRetriever, limits FeynmanDialogLimits, log *slog.Logger) *FeynmanDialogService {
	return &FeynmanDialogService{
		repo:      repo,
		analyzer:  analyzer,
		retriever: retriever,
		limits:    limits.withDefaults(),
		log:       log,
	}
}

// State 返回会话当前练习状态，供接口层做状态条展示；无记录时返回 idle。
func (s *FeynmanDialogService) State(ctx context.Context, userID, sessionID string) (FeynmanPracticeState, error) {
	state, found, err := s.repo.Load(ctx, userID, sessionID)
	if err != nil {
		return FeynmanPracticeState{}, err
	}
	if !found {
		return FeynmanPracticeState{SessionID: sessionID, UserID: userID, State: FeynmanStateIdle}, nil
	}
	return state, nil
}

// Handle 实现 ChatPracticeRouter：返回 handled=false 表示这条消息不归费曼管，
// 调用方继续走自由对话。任何内部故障都降级为“不介入”，绝不把用户困在状态机里。
func (s *FeynmanDialogService) Handle(ctx context.Context, request ChatStreamRequest) (bool, string, error) {
	message := strings.TrimSpace(request.Message)
	if message == "" || s.repo == nil {
		return false, "", nil
	}

	state, found, err := s.repo.Load(ctx, request.UserID, request.SessionID)
	if err != nil {
		s.log.Error("读取费曼练习状态失败，本轮降级为普通对话",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		return false, "", nil
	}
	if !found {
		state = FeynmanPracticeState{SessionID: request.SessionID, UserID: request.UserID, State: FeynmanStateIdle}
	}
	state.SessionID = request.SessionID
	state.UserID = request.UserID

	// 崩溃恢复：turn 租约保证同一会话不会并发处理两条消息，所以只要在
	// analyzing_answer 状态下又收到新消息，一定是上一轮进程/请求中断了。
	if state.State == FeynmanStateAnalyzingAnswer {
		s.log.Warn("费曼分析中途中断，自动恢复为等待回答",
			"trace_id", request.TraceID, "session_id", request.SessionID)
		if strings.TrimSpace(state.ActiveQuestionText) == "" {
			state.State = FeynmanStateAwaitingTopic
		} else {
			state.State = FeynmanStateAwaitingAnswer
		}
	}

	// 结果恢复协议：同一条用户消息重试（assistant 落库失败后前端复用 client_message_id）
	// 时直接回放上次反馈，不重复调用模型，也不重复推进状态。
	if request.UserMessageID != "" && request.UserMessageID == state.LastAnsweredMessageID &&
		strings.TrimSpace(state.LastFeedback) != "" {
		return true, state.LastFeedback, s.emit(request, state.LastFeedback)
	}

	intent := resolveFeynmanIntent(state.State, message, s.limits.MaxControlPhraseRunes, s.limits.MaxTopicRunes)
	decision := decideFeynmanTransition(state, intent, message, s.limits.MaxTopicRunes)
	if !decision.Handled {
		return false, "", nil
	}
	if decision.Analyze {
		return s.analyze(ctx, request, decision.Next, message)
	}
	if err := s.repo.Save(ctx, decision.Next); err != nil {
		s.log.Error("保存费曼练习状态失败，本轮降级为普通对话",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		return false, "", nil
	}
	return true, decision.Reply, s.emit(request, decision.Reply)
}

// analyze 执行一次回答分析，并把“下一题”写回状态，形成连续追问。
func (s *FeynmanDialogService) analyze(ctx context.Context, request ChatStreamRequest, state FeynmanPracticeState, answer string) (bool, string, error) {
	if s.analyzer == nil {
		return false, "", nil
	}
	question := state.ActiveQuestionText

	// 先把 analyzing_answer 落库再调用模型：这样即使进程中断，下一条消息也能看到
	// “上一轮死在分析里”，而不是把中断当成什么都没发生。
	state.State = FeynmanStateAnalyzingAnswer
	state.LastAnsweredMessageID = request.UserMessageID
	state.LastFeedback = ""
	if err := s.repo.Save(ctx, state); err != nil {
		s.log.Error("保存费曼分析中状态失败，本轮降级为普通对话",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		return false, "", nil
	}

	answer = truncateRunes(answer, s.limits.MaxAnswerRunes)
	dimensions := resolveFeynmanDimensions(question, answer)
	result, err := s.analyzer.Analyze(ctx, AnswerAnalysisInput{
		Question:         question,
		QuestionOrigin:   state.QuestionOrigin,
		RoundNo:          state.RoundNo,
		Answer:           answer,
		SessionContext:   buildFeynmanSessionContext(request.History, request.Message, s.limits.MaxContextTurns),
		Dimensions:       dimensions,
		RetrievedContext: s.retrieveContext(ctx, request, question, answer),
	})
	if err != nil {
		s.log.Error("费曼回答分析失败",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		s.rollbackAnalysis(ctx, request, state)
		reply := fmt.Sprintf(feynmanCopyAnalysisFailed, classifyFeynmanAnalysisError(err))
		return true, reply, s.emit(request, reply)
	}

	result = sanitizeAnswerAnalysis(result, answer, dimensions, s.limits)
	feedback := renderFeynmanFeedback(result, s.limits)

	next := state
	next.RoundNo++
	next.LastFeedback = feedback
	if probe := result.NextProbe; probe != "" {
		next.State = FeynmanStateAwaitingAnswer
		next.ActiveQuestionText = probe
		next.QuestionOrigin = FeynmanQuestionOriginFollowUp
	} else {
		next.State = FeynmanStateAwaitingFollowUp
	}
	// 先落状态再输出：反馈是一次性整段下发，若先输出后落库失败，用户会看到追问但
	// 状态没变，下一条消息会被当成上一题的回答。
	if err := s.repo.Save(ctx, next); err != nil {
		s.log.Error("保存费曼分析结果状态失败，本轮降级为普通对话",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		return false, "", nil
	}
	return true, feedback, s.emit(request, feedback)
}

// rollbackAnalysis 把状态退回“等待这道题的回答”，题目保留，方便用户直接重讲。
func (s *FeynmanDialogService) rollbackAnalysis(ctx context.Context, request ChatStreamRequest, state FeynmanPracticeState) {
	rollback := state
	rollback.State = FeynmanStateAwaitingAnswer
	rollback.LastAnsweredMessageID = ""
	rollback.LastFeedback = ""
	// 分析失败常常伴随 ctx 超时/取消，回滚必须用不受其影响的上下文，否则状态会卡在 analyzing。
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.repo.Save(persistCtx, rollback); err != nil {
		s.log.Error("回滚费曼分析状态失败",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
	}
}

// retrieveContext 复用已有知识库检索；失败或未启用都返回固定占位文案，
// 绝不静默——否则模型会照常编造引用。
func (s *FeynmanDialogService) retrieveContext(ctx context.Context, request ChatStreamRequest, question, answer string) string {
	if s.retriever == nil {
		return retrievalDisabledNotice
	}
	query := truncateRunes(strings.TrimSpace(question+"\n"+answer), 400)
	result, err := s.retriever.Retrieve(ctx, RetrievalQuery{
		UserID:    request.UserID,
		SessionID: request.SessionID,
		TraceID:   request.TraceID,
		Query:     query,
		Purpose:   DocumentPurposeAIRetrieval,
	})
	if err != nil {
		s.log.Warn("费曼分析检索失败，降级为无资料分析",
			"trace_id", request.TraceID, "session_id", request.SessionID, "error", err)
		if request.OnRetrieval != nil {
			result.Status = RetrievalStatusFailed
			if callbackErr := request.OnRetrieval(result); callbackErr != nil {
				s.log.Warn("下发费曼分析引用来源失败",
					"trace_id", request.TraceID, "session_id", request.SessionID, "error", callbackErr)
			}
		}
		if strings.TrimSpace(result.ContextBlock) != "" {
			return result.ContextBlock
		}
		return retrievalFailedNotice
	}
	if request.OnRetrieval != nil {
		if callbackErr := request.OnRetrieval(result); callbackErr != nil {
			s.log.Warn("下发费曼分析引用来源失败",
				"trace_id", request.TraceID, "session_id", request.SessionID, "error", callbackErr)
		}
	}
	return result.ContextBlock
}

func (s *FeynmanDialogService) emit(request ChatStreamRequest, content string) error {
	if request.OnDelta == nil || content == "" {
		return nil
	}
	return request.OnDelta(content)
}

// buildFeynmanSessionContext 取最近若干轮对话作为分析上下文。
//
// 它解决的是一个具体误判：第 3 轮追问时用户说“就是我刚才说的那个补偿”，
// 没有上下文的分析器只能判成“讲得含糊”，有上下文才知道他上一轮已经展开过。
func buildFeynmanSessionContext(history []ConversationMessage, currentMessage string, maxTurns int) []AnswerAnalysisTurn {
	if maxTurns <= 0 {
		maxTurns = defaultMaxFeynmanContextTurns
	}
	current := strings.TrimSpace(currentMessage)
	turns := make([]AnswerAnalysisTurn, 0, maxTurns)
	for index := len(history) - 1; index >= 0 && len(turns) < maxTurns; index-- {
		entry := history[index]
		// History 末尾就是本轮这条回答，它已经作为 answer 单独传入，重复一次只是白占预算。
		if len(turns) == 0 && entry.Role == "user" && strings.TrimSpace(entry.Content) == current {
			continue
		}
		// 折行折叠：历史消息若保留换行，一条消息就能在 Prompt 里伪造出新的“系统指令行”。
		content := strings.TrimSpace(memoryValueNeutralizer.Replace(entry.Content))
		if content == "" {
			continue
		}
		turns = append(turns, AnswerAnalysisTurn{
			Role:    entry.Role,
			Content: truncateRunes(content, defaultMaxFeynmanContextTurnRunes),
		})
	}
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
	return turns
}

func classifyFeynmanAnalysisError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "分析超时"
	case errors.Is(err, ErrFeynmanAnalysisInvalid):
		return "模型输出异常"
	default:
		return "服务暂时不可用"
	}
}

// ---------------------------------------------------------------------------
// 状态迁移：这里是唯一权威定义，任何非法组合一律降级为“不介入”，
// 让用户随时都能正常聊天，而不是被状态机困住。
// ---------------------------------------------------------------------------

type feynmanDecision struct {
	Handled bool
	Analyze bool
	Reply   string
	Next    FeynmanPracticeState
}

func decideFeynmanTransition(current FeynmanPracticeState, intent feynmanIntent, message string, maxTopicRunes int) feynmanDecision {
	notHandled := feynmanDecision{}
	hasQuestion := strings.TrimSpace(current.ActiveQuestionText) != ""

	// 结束在任何状态下都必须能用。
	if intent.Kind == feynmanIntentStop {
		if current.State == FeynmanStateIdle {
			return notHandled
		}
		return feynmanDecision{Handled: true, Reply: feynmanCopyStopped, Next: idleFeynmanState(current)}
	}

	switch current.State {
	case FeynmanStateIdle:
		switch intent.Kind {
		case feynmanIntentStartPractice:
			next := idleFeynmanState(current)
			next.State = FeynmanStateAwaitingTopic
			return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
		case feynmanIntentStartTopic:
			return acceptFeynmanTopic(current, intent.Topic)
		}
		return notHandled

	case FeynmanStateAwaitingTopic:
		switch intent.Kind {
		case feynmanIntentStartTopic:
			return acceptFeynmanTopic(current, intent.Topic)
		case feynmanIntentPause:
			// 还没有题目，暂停没有可保留的上下文，等价于先不练。
			return feynmanDecision{Handled: true, Reply: feynmanCopyStoppedIdle, Next: idleFeynmanState(current)}
		case feynmanIntentResume, feynmanIntentSkip, feynmanIntentRetry:
			next := idleFeynmanState(current)
			next.State = FeynmanStateAwaitingTopic
			return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
		}
		return notHandled

	case FeynmanStateAwaitingAnswer, FeynmanStateAnalyzingAnswer:
		switch intent.Kind {
		case feynmanIntentAnswer:
			if !hasQuestion {
				return acceptFeynmanTopic(current, extractFeynmanTopic(message, maxTopicRunes))
			}
			next := current
			next.State = FeynmanStateAnalyzingAnswer
			return feynmanDecision{Handled: true, Analyze: true, Next: next}
		case feynmanIntentPause:
			next := current
			next.State = FeynmanStatePaused
			return feynmanDecision{Handled: true, Reply: feynmanCopyPaused, Next: next}
		case feynmanIntentSkip:
			next := idleFeynmanState(current)
			next.State = FeynmanStateAwaitingTopic
			return feynmanDecision{Handled: true, Reply: feynmanCopySkipped, Next: next}
		case feynmanIntentRetry, feynmanIntentResume:
			if !hasQuestion {
				next := idleFeynmanState(current)
				next.State = FeynmanStateAwaitingTopic
				return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
			}
			next := current
			next.State = FeynmanStateAwaitingAnswer
			return feynmanDecision{Handled: true, Reply: fmt.Sprintf(feynmanCopyRetry, current.ActiveQuestionText), Next: next}
		}
		return notHandled

	case FeynmanStateAwaitingFollowUp:
		switch intent.Kind {
		case feynmanIntentStartTopic:
			return acceptFeynmanTopic(current, intent.Topic)
		case feynmanIntentAnswer:
			if !hasQuestion {
				return acceptFeynmanTopic(current, extractFeynmanTopic(message, maxTopicRunes))
			}
			next := current
			next.State = FeynmanStateAnalyzingAnswer
			return feynmanDecision{Handled: true, Analyze: true, Next: next}
		case feynmanIntentResume, feynmanIntentSkip:
			next := idleFeynmanState(current)
			next.State = FeynmanStateAwaitingTopic
			return feynmanDecision{Handled: true, Reply: feynmanCopySkipped, Next: next}
		case feynmanIntentRetry:
			if !hasQuestion {
				next := idleFeynmanState(current)
				next.State = FeynmanStateAwaitingTopic
				return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
			}
			next := current
			next.State = FeynmanStateAwaitingAnswer
			return feynmanDecision{Handled: true, Reply: fmt.Sprintf(feynmanCopyRetry, current.ActiveQuestionText), Next: next}
		case feynmanIntentPause:
			next := current
			next.State = FeynmanStatePaused
			return feynmanDecision{Handled: true, Reply: feynmanCopyPaused, Next: next}
		}
		return notHandled

	case FeynmanStatePaused:
		switch intent.Kind {
		case feynmanIntentResume, feynmanIntentRetry:
			if !hasQuestion {
				next := idleFeynmanState(current)
				next.State = FeynmanStateAwaitingTopic
				return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
			}
			next := current
			next.State = FeynmanStateAwaitingAnswer
			return feynmanDecision{Handled: true, Reply: fmt.Sprintf(feynmanCopyResumed, current.ActiveQuestionText), Next: next}
		case feynmanIntentPause:
			return feynmanDecision{Handled: true, Reply: feynmanCopyPaused, Next: current}
		}
		return notHandled
	}
	return notHandled
}

// acceptFeynmanTopic 接受一个用户自述主题；主题为空说明没解析出内容，退回“请给主题”。
func acceptFeynmanTopic(current FeynmanPracticeState, topic string) feynmanDecision {
	topic = strings.TrimSpace(topic)
	next := idleFeynmanState(current)
	if topic == "" {
		next.State = FeynmanStateAwaitingTopic
		return feynmanDecision{Handled: true, Reply: feynmanCopyAskTopic, Next: next}
	}
	next.State = FeynmanStateAwaitingAnswer
	next.ActiveQuestionText = topic
	next.QuestionOrigin = FeynmanQuestionOriginUserTopic
	return feynmanDecision{Handled: true, Reply: fmt.Sprintf(feynmanCopyTopicAccepted, topic), Next: next}
}

// idleFeynmanState 清掉与上一题相关的全部字段，只保留归属信息。
func idleFeynmanState(current FeynmanPracticeState) FeynmanPracticeState {
	return FeynmanPracticeState{
		SessionID: current.SessionID,
		UserID:    current.UserID,
		State:     FeynmanStateIdle,
		RoundNo:   current.RoundNo,
	}
}
