package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
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
