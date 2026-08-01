package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"KnowledgeMirror/internal/llm"
)

type scriptedFeynmanCompletionModel struct {
	contents []string
	calls    [][]llm.Message
}

func (m *scriptedFeynmanCompletionModel) Complete(_ context.Context, messages []llm.Message) (llm.Completion, error) {
	m.calls = append(m.calls, messages)
	content := m.contents[len(m.calls)-1]
	return llm.Completion{Content: content}, nil
}

func TestParseAnswerAnalysisResultRejectsMalformedOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "未知字段", raw: `{"covered":[],"gaps":[],"next_probe":"","score":90}`},
		{name: "尾随内容", raw: `{"covered":[],"gaps":[],"next_probe":""}{"covered":[]}`},
		{name: "非 JSON", raw: `这次讲得不错，继续加油`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseAnswerAnalysisResult([]byte(testCase.raw)); !errors.Is(err, ErrFeynmanAnalysisInvalid) {
				t.Fatalf("期望非法输出错误, 实际 %v", err)
			}
		})
	}
}

func TestParseAnswerAnalysisResultAcceptsCodeFence(t *testing.T) {
	raw := "```json\n{\"covered\":[\"吞吐\"],\"insufficient_sources\":true,\"gaps\":[],\"next_probe\":\"再讲讲顺序性\"}\n```"
	result, err := parseAnswerAnalysisResult([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !result.InsufficientSources || result.NextProbe != "再讲讲顺序性" {
		t.Fatalf("解析结果不符: %+v", result)
	}
}

func TestFeynmanAnswerAnalyzerRepairsInvalidStructuredOutputOnce(t *testing.T) {
	model := &scriptedFeynmanCompletionModel{contents: []string{
		`{"covered":[],"gaps":[],"next_probe":"","score":90}`,
		`{"covered":["说明了幂等目的"],"insufficient_sources":true,"gaps":[],"next_probe":"再讲讲幂等键怎么选"}`,
	}}
	analyzer := &LLMFeynmanAnswerAnalyzer{model: model, systemPrompt: "只返回 JSON"}

	result, err := analyzer.Analyze(context.Background(), AnswerAnalysisInput{Answer: "用业务键去重"})
	if err != nil {
		t.Fatalf("修复重试后仍然失败: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("非法结构应只触发一次修复重试，实际调用 %d 次", len(model.calls))
	}
	if len(model.calls[1]) != 4 || model.calls[1][2].Role != "assistant" {
		t.Fatalf("修复请求必须携带上一条模型输出: %+v", model.calls[1])
	}
	if result.NextProbe != "再讲讲幂等键怎么选" {
		t.Fatalf("修复结果不符: %+v", result)
	}
}

func TestSanitizeAnswerAnalysisDropsUnverifiableQuotes(t *testing.T) {
	answer := "我们当时用 Kafka 是因为要扛住峰值吞吐。"
	result := sanitizeAnswerAnalysis(AnswerAnalysisResult{
		Covered: []string{" 讲清了吞吐 ", "", "第二点", "第三点", "第四点会被丢弃"},
		Gaps: []AnswerAnalysisGap{
			{ConceptLabel: "顺序性", Verdict: FeynmanGapVerdictIncorrect, UserQuote: "我说了顺序不重要", Analysis: "引语对不上原文"},
			{ConceptLabel: "分区键", Verdict: "made_up", Analysis: "非法判定"},
			{ConceptLabel: "峰值吞吐", Verdict: FeynmanGapVerdictUncertain, UserQuote: "要扛住峰值吞吐", Analysis: "没说明峰值是多少"},
			{ConceptLabel: "重试", Verdict: FeynmanGapVerdictOmitted, UserQuote: "模型编的原文", Analysis: "完全没提重试"},
		},
		NextProbe: "峰值\n到底是多少？",
	}, answer, nil, FeynmanDialogLimits{})

	if len(result.Covered) != defaultMaxFeynmanCoveredEntries {
		t.Fatalf("covered 未按上限收敛: %+v", result.Covered)
	}
	if result.Covered[0] != "讲清了吞吐" {
		t.Fatalf("covered 未去除首尾空白: %q", result.Covered[0])
	}
	if len(result.Gaps) != 3 {
		t.Fatalf("期望保留 3 条缺口, 实际 %+v", result.Gaps)
	}
	// 引语对不上原文的 incorrect 必须降级为 uncertain，而不是被静默丢掉：
	// 举不出原句通常是口语转写导致摘抄不精确，丢掉等于扔掉一条真问题。
	if result.Gaps[0].Verdict != FeynmanGapVerdictUncertain || result.Gaps[0].UserQuote != "" {
		t.Fatalf("举不出原话的判错必须降级为 uncertain 并清空引语: %+v", result.Gaps[0])
	}
	if result.Gaps[0].Dimension != FeynmanDimensionKeyPoints {
		t.Fatalf("缺省维度应兜底为关键点: %q", result.Gaps[0].Dimension)
	}
	if result.Gaps[2].UserQuote != "" {
		t.Fatal("omitted 缺口的捏造引语必须清空而不是保留")
	}
	if strings.Contains(result.NextProbe, "\n") {
		t.Fatal("追问必须折叠成单行，避免伪造出新的指令行")
	}
}

func TestSanitizeAnswerAnalysisDropsDisabledDimensions(t *testing.T) {
	answer := "我们当时用 Kafka 是因为要扛住峰值吞吐。"
	dimensions := []string{FeynmanDimensionKeyPoints, FeynmanDimensionCausalChain, FeynmanDimensionExpression}
	result := sanitizeAnswerAnalysis(AnswerAnalysisResult{
		Gaps: []AnswerAnalysisGap{
			{Dimension: FeynmanDimensionProjectMapping, ConceptLabel: "没结合项目", Verdict: FeynmanGapVerdictOmitted, Analysis: "凑数条目"},
			{Dimension: FeynmanDimensionCausalChain, ConceptLabel: "为什么不是 RocketMQ", Verdict: FeynmanGapVerdictOmitted, Analysis: "没讲备选方案"},
		},
	}, answer, dimensions, FeynmanDialogLimits{})

	if len(result.Gaps) != 1 || result.Gaps[0].Dimension != FeynmanDimensionCausalChain {
		t.Fatalf("未启用维度的缺口必须丢弃: %+v", result.Gaps)
	}
	// covered 在类型上就变不成一条薄弱点，薄弱点候选只能是剩下三类。
	for _, candidate := range result.WeakPointCandidates() {
		switch candidate.Verdict {
		case FeynmanGapVerdictOmitted, FeynmanGapVerdictIncorrect, FeynmanGapVerdictUncertain:
		default:
			t.Fatalf("薄弱点候选出现非法判定: %+v", candidate)
		}
	}
}

func TestSanitizeAnswerAnalysisLimitsExpressionGaps(t *testing.T) {
	answer := "呃，就是那个，我们当时就是用了 Kafka。"
	longAdvice := strings.Repeat("建议先总后分再展开细节。", 30)
	result := sanitizeAnswerAnalysis(AnswerAnalysisResult{
		Gaps: []AnswerAnalysisGap{
			{Dimension: FeynmanDimensionExpression, ConceptLabel: "结论埋在最后", Verdict: FeynmanGapVerdictUncertain, Analysis: longAdvice},
			{Dimension: FeynmanDimensionExpression, ConceptLabel: "口头禅多", Verdict: FeynmanGapVerdictUncertain, Analysis: "语气词偏多"},
		},
	}, answer, []string{FeynmanDimensionExpression}, FeynmanDialogLimits{})

	if len(result.Gaps) != defaultMaxFeynmanExpressionGaps {
		t.Fatalf("表达类缺口最多保留 %d 条, 实际 %+v", defaultMaxFeynmanExpressionGaps, result.Gaps)
	}
	if utf8.RuneCountInString(result.Gaps[0].Analysis) > defaultMaxFeynmanExpressionRunes {
		t.Fatalf("表达类建议必须截断到 %d 字: %q", defaultMaxFeynmanExpressionRunes, result.Gaps[0].Analysis)
	}
}

func TestSelectFeynmanFeedbackGaps(t *testing.T) {
	gaps := []AnswerAnalysisGap{
		{ConceptLabel: "遗漏一", Verdict: FeynmanGapVerdictOmitted},
		{ConceptLabel: "关键错误", Verdict: FeynmanGapVerdictIncorrect, UserQuote: "原话"},
		{ConceptLabel: "遗漏二", Verdict: FeynmanGapVerdictOmitted},
		{ConceptLabel: "含糊一", Verdict: FeynmanGapVerdictUncertain},
		{ConceptLabel: "含糊二", Verdict: FeynmanGapVerdictUncertain},
	}
	keyError, secondary, omitted := selectFeynmanFeedbackGaps(gaps, defaultMaxFeynmanSecondaryGaps)
	if keyError == nil || keyError.ConceptLabel != "关键错误" {
		t.Fatalf("最关键错误应取第一条 incorrect: %+v", keyError)
	}
	if len(secondary) != defaultMaxFeynmanSecondaryGaps || omitted != 1 {
		t.Fatalf("次要条目应截断到 %d 条并报告 1 条未展示, 实际 %d/%d", defaultMaxFeynmanSecondaryGaps, len(secondary), omitted)
	}

	keyError, secondary, omitted = selectFeynmanFeedbackGaps(gaps[:1], defaultMaxFeynmanSecondaryGaps)
	if keyError != nil || len(secondary) != 1 || omitted != 0 {
		t.Fatalf("没有 incorrect 时不应有最关键错误: %+v %+v %d", keyError, secondary, omitted)
	}
}

func TestRenderFeynmanFeedbackKeepsStableLayout(t *testing.T) {
	feedback := renderFeynmanFeedback(AnswerAnalysisResult{
		Covered:             []string{"讲清了吞吐诉求"},
		InsufficientSources: true,
		Gaps: []AnswerAnalysisGap{
			{
				Dimension:    FeynmanDimensionFactBoundary,
				ConceptLabel: "outbox 的原子性边界",
				Verdict:      FeynmanGapVerdictIncorrect,
				UserQuote:    "发消息和写库是原子的",
				Analysis:     "能原子提交的是业务数据与 outbox 记录",
			},
			{
				Dimension:    FeynmanDimensionKeyPoints,
				ConceptLabel: "顺序性保证",
				Verdict:      FeynmanGapVerdictOmitted,
				Analysis:     "没有说明分区键怎么选",
			},
		},
		NextProbe: "同一笔交易怎么保证顺序？",
	}, FeynmanDialogLimits{})

	fragments := []string{
		"讲对的部分：",
		"最关键的问题：【说反了】outbox 的原子性边界",
		"你的原话：",
		"还要补这几点：",
		"1. 【没讲到】顺序性保证",
		"没有可用资料支撑",
		"接着回答这个：同一笔交易怎么保证顺序？",
	}
	previous := -1
	for _, fragment := range fragments {
		index := strings.Index(feedback, fragment)
		if index < 0 {
			t.Fatalf("反馈缺少片段 %q:\n%s", fragment, feedback)
		}
		if index < previous {
			t.Fatalf("反馈区块顺序不符，%q 出现得太早:\n%s", fragment, feedback)
		}
		previous = index
	}
}

func TestRenderFeynmanFeedbackWithoutGaps(t *testing.T) {
	feedback := renderFeynmanFeedback(AnswerAnalysisResult{NextProbe: "再讲讲失败重试"}, FeynmanDialogLimits{})
	if !strings.Contains(feedback, "没挑出明显的漏洞") {
		t.Fatalf("无缺口时应给出明确结论:\n%s", feedback)
	}
}

func TestResolveFeynmanDimensions(t *testing.T) {
	cases := []struct {
		name     string
		question string
		answer   string
		want     []string
		absent   []string
	}{
		{
			name:     "因果题只开因果链",
			question: "为什么 Kafka 的吞吐能压过 RabbitMQ？",
			answer:   "因为它是顺序写磁盘加零拷贝。",
			want:     []string{FeynmanDimensionCausalChain},
			absent:   []string{FeynmanDimensionProjectMapping, FeynmanDimensionFactBoundary},
		},
		{
			name:     "定义题开事实边界，不开项目映射",
			question: "Kafka 的 offset 是什么？",
			answer:   "就是消息在分区里的位置。",
			want:     []string{FeynmanDimensionFactBoundary},
			absent:   []string{FeynmanDimensionProjectMapping, FeynmanDimensionCausalChain},
		},
		{
			name:     "项目题开项目映射",
			question: "讲讲你们线上是怎么做对账的",
			answer:   "我们用定时任务扫差异。",
			want:     []string{FeynmanDimensionProjectMapping},
			absent:   []string{FeynmanDimensionFactBoundary},
		},
		{
			name:     "非定义题里出现绝对化断言也要开事实边界",
			question: "讲讲你们线上是怎么做对账的",
			answer:   "Kafka 保证消息不会丢，所以不用对账。",
			want:     []string{FeynmanDimensionProjectMapping, FeynmanDimensionFactBoundary},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dimensions := resolveFeynmanDimensions(testCase.question, testCase.answer)
			// 关键点与表达维度恒在：前者是费曼练习的本体，后者受数量和长度双闸约束。
			for _, always := range []string{FeynmanDimensionKeyPoints, FeynmanDimensionExpression} {
				if !feynmanDimensionEnabled(dimensions, always) {
					t.Fatalf("%s 必须始终启用: %v", always, dimensions)
				}
			}
			for _, expected := range testCase.want {
				if !feynmanDimensionEnabled(dimensions, expected) {
					t.Fatalf("期望启用 %s: %v", expected, dimensions)
				}
			}
			for _, unexpected := range testCase.absent {
				if feynmanDimensionEnabled(dimensions, unexpected) {
					t.Fatalf("不应启用 %s: %v", unexpected, dimensions)
				}
			}
		})
	}
}
