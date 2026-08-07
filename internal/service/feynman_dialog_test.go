package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeFeynmanPracticeRepo struct {
	state    FeynmanPracticeState
	found    bool
	saves    []FeynmanPracticeState
	loadErr  error
	saveErr  error
	saveFail int // 前 N 次 Save 返回 saveErr
}

func (r *fakeFeynmanPracticeRepo) Load(_ context.Context, userID, sessionID string) (FeynmanPracticeState, bool, error) {
	if r.loadErr != nil {
		return FeynmanPracticeState{}, false, r.loadErr
	}
	if !r.found {
		return FeynmanPracticeState{}, false, nil
	}
	state := r.state
	state.UserID = userID
	state.SessionID = sessionID
	return state, true, nil
}

func (r *fakeFeynmanPracticeRepo) Save(_ context.Context, state FeynmanPracticeState) error {
	if r.saveFail > 0 {
		r.saveFail--
		return r.saveErr
	}
	r.saves = append(r.saves, state)
	r.state = state
	r.found = true
	return nil
}

func (r *fakeFeynmanPracticeRepo) lastSave(t *testing.T) FeynmanPracticeState {
	t.Helper()
	if len(r.saves) == 0 {
		t.Fatal("期望至少保存过一次状态")
	}
	return r.saves[len(r.saves)-1]
}

type fakeCoachRepository struct {
	task       CoachDailyTask
	startErr   error
	startCalls int
	starts     []StartCoachTaskParams
	control    []CoachTaskControlParams
	commits    []CommitCoachAnalysisParams
	prior      map[string]PriorGapEvidence
	practice   *fakeFeynmanPracticeRepo
}

func (r *fakeCoachRepository) EnsureDailyPlan(context.Context, string, time.Time) (CoachDailyPlan, error) {
	return CoachDailyPlan{}, nil
}
func (r *fakeCoachRepository) GetProgress(context.Context, string, time.Time, time.Time) (CoachProgress, error) {
	return CoachProgress{}, nil
}
func (r *fakeCoachRepository) ListGaps(context.Context, string, string, int) ([]FeynmanGap, error) {
	return nil, nil
}
func (r *fakeCoachRepository) GetTask(_ context.Context, userID, taskID string) (CoachDailyTask, error) {
	task := r.task
	task.UserID = userID
	task.CoachTaskID = taskID
	return task, nil
}
func (r *fakeCoachRepository) GetGap(_ context.Context, userID, gapID string) (FeynmanGap, error) {
	return FeynmanGap{GapID: gapID, UserID: userID, GapKey: "key_points-target", DiagnosticDimension: FeynmanDimensionKeyPoints, Title: "target"}, nil
}
func (r *fakeCoachRepository) StartTaskInSession(_ context.Context, p StartCoachTaskParams) (CoachDailyTask, error) {
	r.startCalls++
	r.starts = append(r.starts, p)
	if r.startErr != nil {
		return CoachDailyTask{}, r.startErr
	}
	if r.practice != nil && r.practice.found && r.practice.state.LastAnsweredMessageID == p.UserMessageID && r.practice.state.LastFeedback == p.Reply {
		t := r.task
		t.CoachTaskID, t.UserID, t.SessionID = p.CoachTaskID, p.UserID, p.SessionID
		return t, nil
	}
	t := r.task
	t.CoachTaskID = p.CoachTaskID
	t.UserID = p.UserID
	t.SessionID = p.SessionID
	if r.practice != nil {
		r.practice.state = FeynmanPracticeState{
			UserID: p.UserID, SessionID: p.SessionID, State: FeynmanStateAwaitingAnswer,
			ActiveQuestionText: t.QuestionText, OriginalQuestionText: t.QuestionText,
			QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: p.CoachTaskID,
			LastAnsweredMessageID: p.UserMessageID, LastFeedback: p.Reply, RoundNo: 1,
		}
		r.practice.found = true
	}
	return t, nil
}
func (r *fakeCoachRepository) FetchPriorGapEvidence(context.Context, string, []string) (map[string]PriorGapEvidence, error) {
	return r.prior, nil
}
func (r *fakeCoachRepository) ControlTask(_ context.Context, p CoachTaskControlParams) error {
	r.control = append(r.control, p)
	if r.practice != nil {
		r.practice.state = p.PracticeState
		r.practice.found = true
	}
	return nil
}
func (r *fakeCoachRepository) CommitAnalysis(_ context.Context, p CommitCoachAnalysisParams) (CoachAttempt, error) {
	r.commits = append(r.commits, p)
	return p.Attempt, nil
}

type fakeAnswerAnalyzer struct {
	calls  int
	input  AnswerAnalysisInput
	result AnswerAnalysisResult
	err    error
}

func (a *fakeAnswerAnalyzer) Analyze(_ context.Context, input AnswerAnalysisInput) (AnswerAnalysisResult, error) {
	a.calls++
	a.input = input
	if a.err != nil {
		return AnswerAnalysisResult{}, a.err
	}
	return a.result, nil
}

func (a *fakeAnswerAnalyzer) ModelName() string     { return "fake-model" }
func (a *fakeAnswerAnalyzer) PromptVersion() string { return "fake-v1" }

func newTestDialogService(repo FeynmanPracticeRepository, analyzer FeynmanAnswerAnalyzer) *FeynmanDialogService {
	return NewFeynmanDialogService(repo, analyzer, nil, FeynmanDialogLimits{}, discardLogger())
}

func testStreamRequest(message, messageID string) (ChatStreamRequest, *strings.Builder) {
	var output strings.Builder
	return ChatStreamRequest{
		UserID:        "user-1",
		SessionID:     "session-1",
		TraceID:       "trace-1",
		Message:       message,
		UserMessageID: messageID,
		LocalDate:     time.Now().Format(time.DateOnly),
		OnDelta: func(delta string) error {
			output.WriteString(delta)
			return nil
		},
	}, &output
}

// ---------------------------------------------------------------------------
// 意图识别
// ---------------------------------------------------------------------------

func TestResolveFeynmanIntent(t *testing.T) {
	longAnswerWithControlWord := "我们当时在对账链路里做了幂等，" +
		strings.Repeat("因为下游可能重复投递消息，", 5) +
		"所以那一步其实可以跳过，直接用唯一键去重就行。"

	cases := []struct {
		name      string
		state     string
		message   string
		wantKind  string
		wantTopic string
	}{
		{name: "空闲态普通提问不介入", state: FeynmanStateIdle, message: "Kafka 的 offset 是什么？", wantKind: feynmanIntentNone},
		{name: "空闲态入口文案开始练习", state: FeynmanStateIdle, message: feynmanEntryMessage, wantKind: feynmanIntentStartPractice},
		{name: "空闲态直接指定主题", state: FeynmanStateIdle, message: "我来讲一下为什么当时选 Kafka", wantKind: feynmanIntentStartTopic, wantTopic: "为什么当时选 Kafka"},
		{name: "空闲态只说我来讲当作开始", state: FeynmanStateIdle, message: "我来讲", wantKind: feynmanIntentStartPractice},
		{name: "空闲态控制词不生效", state: FeynmanStateIdle, message: "跳过", wantKind: feynmanIntentNone},
		{name: "等待主题时整句都是主题", state: FeynmanStateAwaitingTopic, message: "分布式事务的补偿设计", wantKind: feynmanIntentStartTopic, wantTopic: "分布式事务的补偿设计"},
		{name: "等待回答时普通内容是回答", state: FeynmanStateAwaitingAnswer, message: "Kafka 的 offset 有两个，一个是写入 offset", wantKind: feynmanIntentAnswer},
		{name: "等待回答时问号内容仍是回答", state: FeynmanStateAwaitingAnswer, message: "是不是因为要保证顺序？我觉得是。", wantKind: feynmanIntentAnswer},
		{name: "等待回答时短控制词生效", state: FeynmanStateAwaitingAnswer, message: "跳过这题", wantKind: feynmanIntentSkip},
		{name: "控制词带句尾标点也生效", state: FeynmanStateAwaitingAnswer, message: " 暂停。 ", wantKind: feynmanIntentPause},
		{name: "长回答里出现跳过不算控制词", state: FeynmanStateAwaitingAnswer, message: longAnswerWithControlWord, wantKind: feynmanIntentAnswer},
		{name: "暂停期间普通聊天不介入", state: FeynmanStatePaused, message: "顺便问一下 Redis 怎么做分布式锁", wantKind: feynmanIntentNone},
		{name: "暂停期间继续恢复", state: FeynmanStatePaused, message: "继续", wantKind: feynmanIntentResume},
		{name: "反馈后换主题", state: FeynmanStateAwaitingFollowUp, message: "我来讲 Outbox 的补偿", wantKind: feynmanIntentStartTopic, wantTopic: "Outbox 的补偿"},
		{name: "反馈后继续讲算回答", state: FeynmanStateAwaitingFollowUp, message: "补充一点，重试是靠定时任务扫的", wantKind: feynmanIntentAnswer},
		{name: "任意状态都能结束", state: FeynmanStateAwaitingAnswer, message: "不练了", wantKind: feynmanIntentStop},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			intent := resolveFeynmanIntent(testCase.state, testCase.message, 0, 0)
			if intent.Kind != testCase.wantKind {
				t.Fatalf("意图错误: 期望 %s, 实际 %s", testCase.wantKind, intent.Kind)
			}
			if testCase.wantTopic != "" && intent.Topic != testCase.wantTopic {
				t.Fatalf("主题错误: 期望 %q, 实际 %q", testCase.wantTopic, intent.Topic)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 状态迁移
// ---------------------------------------------------------------------------

func TestDecideFeynmanTransition(t *testing.T) {
	withQuestion := func(state string) FeynmanPracticeState {
		return FeynmanPracticeState{
			SessionID:          "session-1",
			UserID:             "user-1",
			State:              state,
			ActiveQuestionText: "为什么当时选 Kafka",
			QuestionOrigin:     FeynmanQuestionOriginUserTopic,
			RoundNo:            2,
		}
	}

	cases := []struct {
		name        string
		current     FeynmanPracticeState
		intent      feynmanIntent
		message     string
		wantHandled bool
		wantAnalyze bool
		wantState   string
	}{
		{
			name:        "空闲态点入口进入等待主题",
			current:     FeynmanPracticeState{State: FeynmanStateIdle},
			intent:      feynmanIntent{Kind: feynmanIntentStartPractice},
			wantHandled: true, wantState: FeynmanStateAwaitingTopic,
		},
		{
			name:        "空闲态给主题直接等待回答",
			current:     FeynmanPracticeState{State: FeynmanStateIdle},
			intent:      feynmanIntent{Kind: feynmanIntentStartTopic, Topic: "分布式事务"},
			wantHandled: true, wantState: FeynmanStateAwaitingAnswer,
		},
		{
			name:        "空闲态普通消息不接管",
			current:     FeynmanPracticeState{State: FeynmanStateIdle},
			intent:      feynmanIntent{Kind: feynmanIntentNone},
			wantHandled: false,
		},
		{
			name:        "等待回答时的回答触发分析",
			current:     withQuestion(FeynmanStateAwaitingAnswer),
			intent:      feynmanIntent{Kind: feynmanIntentAnswer},
			wantHandled: true, wantAnalyze: true, wantState: FeynmanStateAnalyzingAnswer,
		},
		{
			name:        "等待回答时暂停保留题目",
			current:     withQuestion(FeynmanStateAwaitingAnswer),
			intent:      feynmanIntent{Kind: feynmanIntentPause},
			wantHandled: true, wantState: FeynmanStatePaused,
		},
		{
			name:        "跳过回到等待主题",
			current:     withQuestion(FeynmanStateAwaitingAnswer),
			intent:      feynmanIntent{Kind: feynmanIntentSkip},
			wantHandled: true, wantState: FeynmanStateAwaitingTopic,
		},
		{
			name:        "暂停后继续回到原题",
			current:     withQuestion(FeynmanStatePaused),
			intent:      feynmanIntent{Kind: feynmanIntentResume},
			wantHandled: true, wantState: FeynmanStateAwaitingAnswer,
		},
		{
			name:        "暂停期间普通消息不接管",
			current:     withQuestion(FeynmanStatePaused),
			intent:      feynmanIntent{Kind: feynmanIntentNone},
			wantHandled: false,
		},
		{
			name:        "结束回到空闲并清空题目",
			current:     withQuestion(FeynmanStateAwaitingFollowUp),
			intent:      feynmanIntent{Kind: feynmanIntentStop},
			wantHandled: true, wantState: FeynmanStateIdle,
		},
		{
			name:        "无题目时的回答退化成主题",
			current:     FeynmanPracticeState{State: FeynmanStateAwaitingAnswer},
			intent:      feynmanIntent{Kind: feynmanIntentAnswer},
			message:     "我想讲讲幂等",
			wantHandled: true, wantState: FeynmanStateAwaitingAnswer,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := decideFeynmanTransition(testCase.current, testCase.intent, testCase.message, 0)
			if decision.Handled != testCase.wantHandled {
				t.Fatalf("接管判定错误: 期望 %v, 实际 %v", testCase.wantHandled, decision.Handled)
			}
			if !decision.Handled {
				return
			}
			if decision.Analyze != testCase.wantAnalyze {
				t.Fatalf("分析判定错误: 期望 %v, 实际 %v", testCase.wantAnalyze, decision.Analyze)
			}
			if decision.Next.State != testCase.wantState {
				t.Fatalf("目标状态错误: 期望 %s, 实际 %s", testCase.wantState, decision.Next.State)
			}
			if !decision.Analyze && decision.Reply == "" {
				t.Fatal("非分析路径必须有固定回复文案")
			}
			if decision.Next.State == FeynmanStateAwaitingAnswer && strings.TrimSpace(decision.Next.ActiveQuestionText) == "" {
				t.Fatal("等待回答状态必须带题目，否则违反数据库约束")
			}
			if decision.Next.State == FeynmanStateIdle && decision.Next.ActiveQuestionText != "" {
				t.Fatal("结束练习必须清空题目")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 服务编排
// ---------------------------------------------------------------------------

func TestFeynmanDialogServiceStartsFromEntryMessage(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{}
	service := newTestDialogService(repo, &fakeAnswerAnalyzer{})
	request, output := testStreamRequest(feynmanEntryMessage, "msg-1")

	handled, content, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled {
		t.Fatal("入口文案必须由练习链路接管")
	}
	if content != output.String() {
		t.Fatalf("返回内容与下发内容不一致: %q vs %q", content, output.String())
	}
	if got := repo.lastSave(t).State; got != FeynmanStateAwaitingTopic {
		t.Fatalf("状态错误: 期望 %s, 实际 %s", FeynmanStateAwaitingTopic, got)
	}
}

func TestFeynmanDialogServiceAnalyzesAnswerAndChainsFollowUp(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:              FeynmanStateAwaitingAnswer,
			ActiveQuestionText: "为什么当时选 Kafka",
			QuestionOrigin:     FeynmanQuestionOriginUserTopic,
			RoundNo:            1,
		},
	}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{
		Covered: []string{"讲清了吞吐诉求"},
		Gaps: []AnswerAnalysisGap{{
			ConceptLabel: "顺序性保证",
			Verdict:      FeynmanGapVerdictOmitted,
			Analysis:     "没有说明分区键怎么选",
		}},
		NextProbe: "同一笔交易的消息怎么保证按顺序被消费？",
	}}
	service := newTestDialogService(repo, analyzer)
	request, output := testStreamRequest("因为当时要扛住高吞吐，所以选了 Kafka。", "msg-2")

	handled, content, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled {
		t.Fatal("等待回答状态下的消息必须被接管")
	}
	if analyzer.calls != 1 {
		t.Fatalf("分析调用次数错误: 期望 1, 实际 %d", analyzer.calls)
	}
	if !strings.Contains(content, "顺序性保证") || !strings.Contains(content, "同一笔交易") {
		t.Fatalf("反馈缺少缺口或追问: %q", content)
	}
	if output.String() != content {
		t.Fatal("下发内容必须与落库内容一致")
	}

	// 中间态必须先落库，避免进程中断后无法判断上一轮死在分析里。
	if len(repo.saves) != 2 || repo.saves[0].State != FeynmanStateAnalyzingAnswer {
		t.Fatalf("期望先保存 analyzing_answer 中间态, 实际: %+v", repo.saves)
	}
	final := repo.lastSave(t)
	if final.State != FeynmanStateAwaitingAnswer {
		t.Fatalf("有追问时应继续等待回答, 实际 %s", final.State)
	}
	if final.ActiveQuestionText != "同一笔交易的消息怎么保证按顺序被消费？" {
		t.Fatalf("追问未串成下一题: %q", final.ActiveQuestionText)
	}
	if final.QuestionOrigin != FeynmanQuestionOriginFollowUp {
		t.Fatalf("题目来源错误: %s", final.QuestionOrigin)
	}
	if final.RoundNo != 2 {
		t.Fatalf("轮次未推进: %d", final.RoundNo)
	}
	if final.LastAnsweredMessageID != "msg-2" || final.LastFeedback != content {
		t.Fatal("必须记录本轮回答与反馈，供重试回放")
	}
}

func TestFeynmanDialogServiceBuildsAnalysisInput(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:              FeynmanStateAwaitingAnswer,
			ActiveQuestionText: "为什么当时选 Kafka",
			QuestionOrigin:     FeynmanQuestionOriginUserTopic,
			RoundNo:            1,
		},
	}
	analyzer := &fakeAnswerAnalyzer{}
	service := newTestDialogService(repo, analyzer)
	answer := "因为当时要横住高吞吐，所以选了 Kafka。"
	request, _ := testStreamRequest(answer, "msg-2")
	request.History = []ConversationMessage{
		{Seq: 1, Role: "user", Content: "我来讲为什么当时选 Kafka"},
		{Seq: 2, Role: "assistant", Content: "明白了，你直接讲\n就行"},
		{Seq: 3, Role: "user", Content: answer},
	}

	if _, _, err := service.Handle(context.Background(), request); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	input := analyzer.input
	if input.Question != "为什么当时选 Kafka" || input.Answer != answer {
		t.Fatalf("分析输入缺少当前问题或有效回答: %+v", input)
	}
	if input.QuestionOrigin != FeynmanQuestionOriginUserTopic || input.RoundNo != 1 {
		t.Fatalf("分析输入缺少题目来源或轮次: %+v", input)
	}
	if len(input.SessionContext) != 2 {
		t.Fatalf("会话上下文应剔除本轮回答后保留 2 条: %+v", input.SessionContext)
	}
	if input.SessionContext[0].Content != "我来讲为什么当时选 Kafka" {
		t.Fatalf("会话上下文必须按时间顺序: %+v", input.SessionContext)
	}
	if strings.Contains(input.SessionContext[1].Content, "\n") {
		t.Fatalf("历史消息必须折行折叠，避免伪造出新的指令行: %q", input.SessionContext[1].Content)
	}
	// “为什么选”是因果题；题目里没有定义类词，回答也没有绝对化断言，事实边界不该启用。
	if !feynmanDimensionEnabled(input.Dimensions, FeynmanDimensionCausalChain) {
		t.Fatalf("因果题必须启用因果链维度: %v", input.Dimensions)
	}
	if feynmanDimensionEnabled(input.Dimensions, FeynmanDimensionFactBoundary) {
		t.Fatalf("本题不应启用事实边界维度: %v", input.Dimensions)
	}
}

func TestFeynmanDialogServiceReplaysFeedbackOnRetry(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:                 FeynmanStateAwaitingAnswer,
			ActiveQuestionText:    "下一题",
			LastAnsweredMessageID: "msg-2",
			LastFeedback:          "上一轮的反馈原文",
			RoundNo:               2,
		},
	}
	analyzer := &fakeAnswerAnalyzer{}
	service := newTestDialogService(repo, analyzer)
	request, output := testStreamRequest("因为当时要扛住高吞吐，所以选了 Kafka。", "msg-2")

	handled, content, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled || content != "上一轮的反馈原文" || output.String() != "上一轮的反馈原文" {
		t.Fatalf("重试必须原样回放上次反馈, 实际 %q", content)
	}
	if analyzer.calls != 0 {
		t.Fatalf("重试不得重复调用模型, 实际调用 %d 次", analyzer.calls)
	}
	if len(repo.saves) != 0 {
		t.Fatal("重试不得重复推进状态")
	}
}

func TestFeynmanDialogServiceKeepsQuestionWhenAnalysisFails(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:              FeynmanStateAwaitingAnswer,
			ActiveQuestionText: "为什么当时选 Kafka",
			RoundNo:            1,
		},
	}
	analyzer := &fakeAnswerAnalyzer{err: errors.New("模型不可用")}
	service := newTestDialogService(repo, analyzer)
	request, output := testStreamRequest("因为要扛吞吐。", "msg-3")

	handled, content, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled || !strings.Contains(content, "题目我给你留着") {
		t.Fatalf("分析失败应给出可继续的提示, 实际 %q", content)
	}
	if output.String() != content {
		t.Fatal("下发内容必须与落库内容一致")
	}
	final := repo.lastSave(t)
	if final.State != FeynmanStateAwaitingAnswer {
		t.Fatalf("分析失败必须回退到等待回答, 实际 %s", final.State)
	}
	if final.ActiveQuestionText != "为什么当时选 Kafka" {
		t.Fatal("分析失败不得丢题")
	}
	if final.LastAnsweredMessageID != "" || final.LastFeedback != "" {
		t.Fatal("分析失败不得留下可回放的反馈")
	}
	if final.RoundNo != 1 {
		t.Fatalf("分析失败不得推进轮次, 实际 %d", final.RoundNo)
	}
}

func TestFeynmanDialogServiceAllowsNormalChatWhilePaused(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:              FeynmanStatePaused,
			ActiveQuestionText: "为什么当时选 Kafka",
		},
	}
	analyzer := &fakeAnswerAnalyzer{}
	service := newTestDialogService(repo, analyzer)

	request, _ := testStreamRequest("顺便问一下 Redis 分布式锁怎么做？", "msg-4")
	handled, _, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if handled {
		t.Fatal("暂停期间的普通消息必须交回自由对话")
	}
	if analyzer.calls != 0 {
		t.Fatal("暂停期间不得触发分析")
	}

	resumeRequest, resumeOutput := testStreamRequest("继续", "msg-5")
	handled, content, err := service.Handle(context.Background(), resumeRequest)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if !handled || !strings.Contains(content, "为什么当时选 Kafka") {
		t.Fatalf("恢复必须回到原题, 实际 %q", content)
	}
	if resumeOutput.String() != content {
		t.Fatal("下发内容必须与落库内容一致")
	}
	if got := repo.lastSave(t).State; got != FeynmanStateAwaitingAnswer {
		t.Fatalf("恢复后状态错误: %s", got)
	}
}

func TestFeynmanDialogServiceRecoversFromInterruptedAnalysis(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{
		found: true,
		state: FeynmanPracticeState{
			State:                 FeynmanStateAnalyzingAnswer,
			ActiveQuestionText:    "为什么当时选 Kafka",
			LastAnsweredMessageID: "msg-6",
		},
	}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{NextProbe: "追问"}}
	service := newTestDialogService(repo, analyzer)
	request, _ := testStreamRequest("我再讲一遍：当时是为了扛吞吐。", "msg-7")

	handled, _, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled {
		t.Fatal("中断恢复后这条消息仍应被当作回答")
	}
	if analyzer.calls != 1 {
		t.Fatalf("恢复后应正常分析一次, 实际 %d", analyzer.calls)
	}
}

func TestFeynmanDialogServiceLaunchesCoachTaskFromStoredQuestion(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{}
	coach := &fakeCoachRepository{task: CoachDailyTask{QuestionText: "服务端处方原题"}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, output := testStreamRequest("客户端展示文案，不能当题目", "msg-launch")
	request.CoachTaskID = "019fd849-c39b-7342-ab30-5b2a7d40a681"
	handled, content, err := service.Handle(context.Background(), request)
	if err != nil || !handled {
		t.Fatalf("launch = (%v, %q, %v)", handled, content, err)
	}
	if analyzer.calls != 0 || len(coach.commits) != 0 {
		t.Fatalf("launch must not analyze: calls=%d commits=%d", analyzer.calls, len(coach.commits))
	}
	if content != "每日教练题：服务端处方原题" {
		t.Fatalf("reply=%q", content)
	}
	if coach.startCalls != 1 || output.String() != content {
		t.Fatalf("start=%d output=%q", coach.startCalls, output.String())
	}
}

func TestFeynmanDialogServiceCoachLaunchPersistsReplayBeforeEmit(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{}
	coach := &fakeCoachRepository{task: CoachDailyTask{QuestionText: "服务端处方原题"}, practice: practice}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{}, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("开始", "019fd849-c39b-7342-ab30-5b2a7d40a688")
	request.CoachTaskID = "019fd849-c39b-7342-ab30-5b2a7d40a681"
	request.OnDelta = func(string) error { return errors.New("client disconnected") }
	if _, _, err := service.Handle(context.Background(), request); err == nil {
		t.Fatal("expected emit failure")
	}
	if len(coach.starts) != 1 || coach.starts[0].UserMessageID != request.UserMessageID || coach.starts[0].Reply != "每日教练题：服务端处方原题" {
		t.Fatalf("start replay fields = %+v", coach.starts)
	}
	request.OnDelta = nil
	handled, reply, err := service.Handle(context.Background(), request)
	if err != nil || !handled || reply != "每日教练题：服务端处方原题" || len(coach.starts) != 1 {
		t.Fatalf("launch replay = (%v,%q,%v), starts=%d", handled, reply, err, len(coach.starts))
	}
}

func TestFeynmanDialogServiceCoachControlPersistsReplayBeforeEmit(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "原题", OriginalQuestionText: "原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1",
	}}
	coach := &fakeCoachRepository{practice: practice}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{}, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("暂停", "019fd849-c39b-7342-ab30-5b2a7d40a689")
	request.CoachTaskID = "task-1"
	request.OnDelta = func(string) error { return errors.New("client disconnected") }
	if _, _, err := service.Handle(context.Background(), request); err == nil {
		t.Fatal("expected emit failure")
	}
	if len(coach.control) != 1 || coach.control[0].UserMessageID != request.UserMessageID ||
		coach.control[0].PracticeState.LastAnsweredMessageID != request.UserMessageID || coach.control[0].Reply == "" {
		t.Fatalf("control replay fields = %+v", coach.control)
	}
	request.OnDelta = nil
	handled, reply, err := service.Handle(context.Background(), request)
	if err != nil || !handled || reply != feynmanCopyPaused || len(coach.control) != 1 {
		t.Fatalf("control replay = (%v,%q,%v), controls=%d", handled, reply, err, len(coach.control))
	}
}

func TestFeynmanDialogServiceCoachCorrectionUsesClientLocalDate(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingRetry, ActiveQuestionText: "原题", OriginalQuestionText: "原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1", RetryRequired: true,
	}}
	coach := &fakeCoachRepository{task: CoachDailyTask{TaskDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)}}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{result: AnswerAnalysisResult{}}, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("完整纠正", "019fd849-c39b-7342-ab30-5b2a7d40a690")
	request.CoachTaskID = "task-1"
	wantDate := time.Now().Format(time.DateOnly)
	request.LocalDate = wantDate
	if _, _, err := service.Handle(context.Background(), request); err != nil {
		t.Fatalf("correction error = %v", err)
	}
	if got := coach.commits[0].CorrectionDate.Format(time.DateOnly); got != wantDate {
		t.Fatalf("correction date = %s, want %s", got, wantDate)
	}
}

func TestFeynmanDialogServiceCoachActiveAnswerRequiresLocalDate(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "原题", OriginalQuestionText: "原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1",
	}}
	coach := &fakeCoachRepository{task: CoachDailyTask{QuestionText: "原题"}}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{result: AnswerAnalysisResult{}}, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("完整回答", "019fd849-c39b-7342-ab30-5b2a7d40a691")
	request.CoachTaskID = "task-1"
	request.LocalDate = ""
	if _, _, err := service.Handle(context.Background(), request); !errors.Is(err, ErrCoachAnalysisInput) {
		t.Fatalf("missing local_date error = %v", err)
	}
}

func TestCoachAnswerLocalDateRejectsFarDate(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.Local)
	for _, value := range []string{"2026-08-05", "2026-08-09", "2026-8-7"} {
		if _, err := coachAnswerLocalDate(value, now); !errors.Is(err, ErrCoachAnalysisInput) {
			t.Fatalf("coachAnswerLocalDate(%q) error = %v", value, err)
		}
	}
}

func TestFeynmanDialogServiceCoachMatchingSecondRequestAnalyzes(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "服务端处方原题", OriginalQuestionText: "服务端处方原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1", RoundNo: 1,
	}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{}}
	coach := &fakeCoachRepository{task: CoachDailyTask{QuestionText: "服务端处方原题"}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("这是第二个请求里的完整回答", "019fd849-c39b-7342-ab30-5b2a7d40a685")
	request.CoachTaskID = "task-1"
	handled, content, err := service.Handle(context.Background(), request)
	if err != nil || !handled || content != "这次回答通过。" {
		t.Fatalf("answer=(%v,%q,%v)", handled, content, err)
	}
	if analyzer.calls != 1 || analyzer.input.Question != "服务端处方原题" || len(coach.commits) != 1 {
		t.Fatalf("analysis=%+v calls=%d commits=%d", analyzer.input, analyzer.calls, len(coach.commits))
	}
}

func TestFeynmanDialogServiceRetestTargetDecisionIsIndependentOfNewGap(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "复测原题", OriginalQuestionText: "复测原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-retest", RoundNo: 1,
	}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{Gaps: []AnswerAnalysisGap{{
		Dimension: FeynmanDimensionKeyPoints, ConceptLabel: "新遗漏", Verdict: FeynmanGapVerdictOmitted, Analysis: "补全新遗漏",
	}}}}
	coach := &fakeCoachRepository{task: CoachDailyTask{
		TaskDate: time.Date(2026, 8, 7, 0, 0, 0, 0, time.Local), SourceGapID: "target-gap", SourceReviewID: "review-2",
	}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("目标已讲清但出现了另一个遗漏", "019fd849-c39b-7342-ab30-5b2a7d40a686")
	request.CoachTaskID = "task-retest"
	if _, _, err := service.Handle(context.Background(), request); err != nil {
		t.Fatalf("retest answer error = %v", err)
	}
	commit := coach.commits[0]
	if commit.ReviewDecision.CurrentReviewStatus != FeynmanGapReviewStatusPassed || !commit.Gaps[0].RequiresCorrection {
		t.Fatalf("commit decision/gaps = %+v / %+v", commit.ReviewDecision, commit.Gaps)
	}
}

func TestFeynmanDialogServiceRetestTargetRecurrenceFailsReview(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "复测原题", OriginalQuestionText: "复测原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-retest", RoundNo: 1,
	}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{Gaps: []AnswerAnalysisGap{{
		Dimension: FeynmanDimensionKeyPoints, ConceptLabel: "target", Verdict: FeynmanGapVerdictOmitted, UserQuote: "仍遗漏目标", Analysis: "目标仍遗漏",
	}}}}
	coach := &fakeCoachRepository{task: CoachDailyTask{
		TaskDate: time.Date(2026, 8, 7, 0, 0, 0, 0, time.Local), SourceGapID: "target-gap", SourceReviewID: "review-1",
	}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("仍遗漏目标", "019fd849-c39b-7342-ab30-5b2a7d40a687")
	request.CoachTaskID = "task-retest"
	if _, _, err := service.Handle(context.Background(), request); err != nil {
		t.Fatalf("retest answer error = %v", err)
	}
	commit := coach.commits[0]
	if commit.ReviewDecision.CurrentReviewStatus != FeynmanGapReviewStatusFailed || !commit.ReviewDecision.TargetRecurred || commit.Gaps[0].ForceCanonicalGapID != "target-gap" {
		t.Fatalf("commit decision/gaps = %+v / %+v", commit.ReviewDecision, commit.Gaps)
	}
}

func TestFeynmanDialogServiceCoachRequiresMatchingTaskID(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{
		State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "原题", OriginalQuestionText: "原题",
		QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1",
	}}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{}, nil, FeynmanDialogLimits{}, discardLogger(), &fakeCoachRepository{})
	request, _ := testStreamRequest("回答", "msg-missing")
	if _, _, err := service.Handle(context.Background(), request); !errors.Is(err, ErrCoachTaskIDRequired) {
		t.Fatalf("missing id error=%v", err)
	}
	request.CoachTaskID = "task-2"
	if _, _, err := service.Handle(context.Background(), request); !errors.Is(err, ErrCoachTaskMismatch) {
		t.Fatalf("mismatch error=%v", err)
	}
}

func TestCoachWeaknessClassifiers(t *testing.T) {
	cases := []struct {
		name  string
		gap   AnswerAnalysisGap
		prior PriorGapEvidence
		want  string
	}{
		{"expression", AnswerAnalysisGap{Dimension: FeynmanDimensionExpression}, PriorGapEvidence{}, CoachGapTypeExpression},
		{"project", AnswerAnalysisGap{Dimension: FeynmanDimensionProjectMapping}, PriorGapEvidence{}, CoachGapTypeProjectEvidence},
		{"recall", AnswerAnalysisGap{Dimension: FeynmanDimensionKeyPoints}, PriorGapEvidence{GapID: "old", Status: FeynmanGapStatusResolved}, CoachGapTypeRecall},
		{"knowledge", AnswerAnalysisGap{Dimension: FeynmanDimensionCausalChain}, PriorGapEvidence{}, CoachGapTypeKnowledge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCoachWeakness(tc.gap, tc.prior); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestFeynmanDialogServiceCoachStrictRetryPersistsAllGapsAndOriginalQuestion(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{State: FeynmanStateAwaitingAnswer, ActiveQuestionText: "原题", OriginalQuestionText: "原题", QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1", RoundNo: 1}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{Covered: []string{"不要展示"}, NextProbe: "不要采用模型追问", Gaps: []AnswerAnalysisGap{
		{Dimension: FeynmanDimensionExpression, ConceptLabel: "表达", Verdict: FeynmanGapVerdictUncertain, Analysis: "先说结论"},
		{Dimension: FeynmanDimensionKeyPoints, ConceptLabel: "关键遗漏", Verdict: FeynmanGapVerdictOmitted, Analysis: "补充关键点"},
		{Dimension: FeynmanDimensionFactBoundary, ConceptLabel: "事实错误", Verdict: FeynmanGapVerdictIncorrect, UserQuote: "保证不丢", Analysis: "只能降低概率"},
	}}}
	coach := &fakeCoachRepository{prior: map[string]PriorGapEvidence{}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("它保证不丢", "019fd849-c39b-7342-ab30-5b2a7d40a682")
	request.CoachTaskID = "task-1"
	handled, content, err := service.Handle(context.Background(), request)
	if err != nil || !handled {
		t.Fatalf("answer = (%v,%q,%v)", handled, content, err)
	}
	if len(coach.commits) != 1 || len(coach.commits[0].Gaps) != 3 {
		t.Fatalf("commits=%+v", coach.commits)
	}
	commit := coach.commits[0]
	if commit.Attempt.Outcome != CoachAttemptOutcomeRetryRequired || commit.PracticeState.State != FeynmanStateAwaitingRetry || !commit.PracticeState.RetryRequired || commit.PracticeState.ActiveQuestionText != "原题" {
		t.Fatalf("commit=%+v", commit)
	}
	if strings.Contains(content, "不要展示") || strings.Contains(content, "不要采用模型追问") || !strings.Contains(content, "保证不丢") || !strings.Contains(content, "现在重新完整回答原题：原题") {
		t.Fatalf("feedback=%q", content)
	}
	focus := 0
	for _, g := range commit.Gaps {
		if g.IsFocus {
			focus++
			if g.Title != "事实错误" {
				t.Fatalf("focus=%+v", g)
			}
		}
	}
	if focus != 1 {
		t.Fatalf("focus count=%d", focus)
	}
}

func TestFeynmanDialogServiceCoachRepeatedFailureThenPass(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{State: FeynmanStateAwaitingRetry, ActiveQuestionText: "原题", OriginalQuestionText: "原题", QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1", RetryRequired: true, RoundNo: 2}}
	analyzer := &fakeAnswerAnalyzer{result: AnswerAnalysisResult{Gaps: []AnswerAnalysisGap{{Dimension: FeynmanDimensionKeyPoints, ConceptLabel: "遗漏", Verdict: FeynmanGapVerdictOmitted, Analysis: "补上关键点"}}}}
	coach := &fakeCoachRepository{prior: map[string]PriorGapEvidence{}}
	service := NewFeynmanDialogService(practice, analyzer, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("第二次仍没讲全", "019fd849-c39b-7342-ab30-5b2a7d40a683")
	request.CoachTaskID = "task-1"
	_, content, err := service.Handle(context.Background(), request)
	if err != nil || !strings.Contains(content, "原题") {
		t.Fatalf("first=%q %v", content, err)
	}
	practice.state = coach.commits[0].PracticeState
	analyzer.result = AnswerAnalysisResult{}
	request, _ = testStreamRequest("第三次完整讲清", "019fd849-c39b-7342-ab30-5b2a7d40a684")
	request.CoachTaskID = "task-1"
	_, content, err = service.Handle(context.Background(), request)
	if err != nil || content != "这次回答通过。" {
		t.Fatalf("pass=%q %v", content, err)
	}
	last := coach.commits[len(coach.commits)-1]
	if last.Attempt.Outcome != CoachAttemptOutcomePassed || last.PracticeState.State != FeynmanStateIdle || last.PracticeState.CoachTaskID != "" {
		t.Fatalf("last=%+v", last)
	}
}

func TestFeynmanDialogServiceCoachPauseResumeAndSkipAreAtomic(t *testing.T) {
	practice := &fakeFeynmanPracticeRepo{found: true, state: FeynmanPracticeState{State: FeynmanStateAwaitingRetry, ActiveQuestionText: "原题", OriginalQuestionText: "原题", QuestionOrigin: FeynmanQuestionOriginCoachTask, CoachTaskID: "task-1", RetryRequired: true}}
	coach := &fakeCoachRepository{}
	service := NewFeynmanDialogService(practice, &fakeAnswerAnalyzer{}, nil, FeynmanDialogLimits{}, discardLogger(), coach)
	request, _ := testStreamRequest("暂停", "msg-pause")
	request.CoachTaskID = "task-1"
	_, _, err := service.Handle(context.Background(), request)
	if err != nil || len(coach.control) != 1 || coach.control[0].Action != "pause" || !coach.control[0].PracticeState.RetryRequired {
		t.Fatalf("pause=%+v %v", coach.control, err)
	}
	practice.state = coach.control[0].PracticeState
	request, _ = testStreamRequest("继续", "msg-resume")
	request.CoachTaskID = "task-1"
	_, content, err := service.Handle(context.Background(), request)
	if err != nil || coach.control[1].PracticeState.State != FeynmanStateAwaitingRetry || !strings.Contains(content, "原题") {
		t.Fatalf("resume=%+v %q %v", coach.control, content, err)
	}
	practice.state = coach.control[1].PracticeState
	request, _ = testStreamRequest("跳过这题", "msg-skip")
	request.CoachTaskID = "task-1"
	_, _, err = service.Handle(context.Background(), request)
	if err != nil || coach.control[2].Action != "skip" || coach.control[2].PracticeState.State != FeynmanStateIdle {
		t.Fatalf("skip=%+v %v", coach.control, err)
	}
}

func TestFeynmanDialogServiceFallsBackWhenRepositoryFails(t *testing.T) {
	repo := &fakeFeynmanPracticeRepo{loadErr: errors.New("数据库不可用")}
	analyzer := &fakeAnswerAnalyzer{}
	service := newTestDialogService(repo, analyzer)
	request, _ := testStreamRequest(feynmanEntryMessage, "msg-8")

	handled, _, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("状态读取失败不应向上抛错: %v", err)
	}
	if handled {
		t.Fatal("状态不可用时必须降级为普通对话，不能把用户困住")
	}
}
