package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"KnowledgeMirror/internal/llm"
)

// ---------------------------------------------------------------------------
// 对话式费曼学习 · 回答分析
//
// 与 feynman_evaluation.go 里那套“正式评估”的分工：
//   - 正式评估：对已人工确认的转写打 Rubric 分，产出待确认的学习证据，走审阅闭环；
//   - 这里的分析：对话中一轮回答的即时反馈，不打分、不落证据、不改知识状态，
//     只回答“哪里没讲到”并给出下一个追问。
//
// 之所以不复用评估器：Rubric 五维打分对一次口头讲解来说太重（延迟高、反馈冗长），
// 而且会把“评分表”这个被明确砍掉的心智负担又带回对话里。
// ---------------------------------------------------------------------------

// ErrFeynmanAnalysisInvalid 表示模型输出不符合约定结构。
var ErrFeynmanAnalysisInvalid = errors.New("费曼回答分析输出非法")

// 缺口判定取值。
const (
	FeynmanGapVerdictOmitted   = "omitted"
	FeynmanGapVerdictIncorrect = "incorrect"
	FeynmanGapVerdictUncertain = "uncertain"
)

// AnswerAnalysisTurn 是进入分析的一条历史对话。
type AnswerAnalysisTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnswerAnalysisInput 是一次分析的全部输入。
//
// SessionContext 的作用是让分析认得出“这是第几次讲同一个点”：第 3 轮追问时用户说
// “就是我刚才说的那个补偿”，没有上下文的分析器会判成“讲得含糊”，有上下文才知道
// 他上一轮已经展开过。Dimensions 则决定这一题该查哪几项，见 feynman_dimensions.go。
type AnswerAnalysisInput struct {
	Question         string               `json:"question"`
	QuestionOrigin   string               `json:"question_origin,omitempty"`
	RoundNo          int                  `json:"round_no,omitempty"`
	Answer           string               `json:"answer"`
	SessionContext   []AnswerAnalysisTurn `json:"session_context,omitempty"`
	Dimensions       []string             `json:"dimensions"`
	RetrievedContext string               `json:"retrieved_context"`
}

// AnswerAnalysisGap 是一条具体缺口。UserQuote 必须逐字来自用户回答，
// 这是“可核对”的底线：用户能立刻回到自己那句话上，而不是被模型的转述说服。
type AnswerAnalysisGap struct {
	Dimension    string `json:"dimension"`
	ConceptLabel string `json:"concept_label"`
	Verdict      string `json:"verdict"`
	UserQuote    string `json:"user_quote"`
	Analysis     string `json:"analysis"`
}

// AnswerAnalysisResult 是一次分析的结构化输出。
//
// 这里没有、也不可能有综合分数：解析用 DisallowUnknownFields，模型自作主张多吐一个
// "score" 不是被忽略，而是整次分析判为非法输出。分数在结构上就进不来。
type AnswerAnalysisResult struct {
	Covered             []string            `json:"covered"`
	InsufficientSources bool                `json:"insufficient_sources"`
	Gaps                []AnswerAnalysisGap `json:"gaps"`
	NextProbe           string              `json:"next_probe"`
}

// WeakPointCandidates 返回本次分析里可以生成或更新薄弱点的条目。
//
// covered 永远不在其中：讲对了不产生薄弱点，这是需求基线 §2 的硬边界。
// 把这条边界收敛成一个方法，是为了让后续落库逻辑只有一处入口，
// 而不是每个调用方各自遍历 Gaps 时再各自判断一遍。
func (r AnswerAnalysisResult) WeakPointCandidates() []AnswerAnalysisGap {
	return r.Gaps
}

// FeynmanAnswerAnalyzer 是对话链路需要的最小分析能力。
type FeynmanAnswerAnalyzer interface {
	Analyze(ctx context.Context, input AnswerAnalysisInput) (AnswerAnalysisResult, error)
	ModelName() string
	PromptVersion() string
}

// LLMFeynmanAnswerAnalyzer 用大模型实现回答分析。
type LLMFeynmanAnswerAnalyzer struct {
	model         feynmanCompletionModel
	systemPrompt  string
	modelName     string
	promptVersion string
}

// LoadLLMFeynmanAnswerAnalyzer 在启动期加载并渲染 Prompt：
// Prompt 缺失或语法错误必须在启动时暴露，而不是等到用户讲完一段话才失败。
func LoadLLMFeynmanAnswerAnalyzer(path, version, modelName string, model feynmanCompletionModel) (*LLMFeynmanAnswerAnalyzer, error) {
	if model == nil {
		return nil, errors.New("费曼回答分析模型不能为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取费曼回答分析 Prompt 失败: %w", err)
	}
	parsed, err := template.New("feynman_analyzer").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("解析费曼回答分析 Prompt 失败: %w", err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct{ Version string }{Version: version}); err != nil {
		return nil, fmt.Errorf("渲染费曼回答分析 Prompt 失败: %w", err)
	}
	return &LLMFeynmanAnswerAnalyzer{
		model:         model,
		systemPrompt:  rendered.String(),
		modelName:     modelName,
		promptVersion: version,
	}, nil
}

func (a *LLMFeynmanAnswerAnalyzer) ModelName() string     { return a.modelName }
func (a *LLMFeynmanAnswerAnalyzer) PromptVersion() string { return a.promptVersion }

func (a *LLMFeynmanAnswerAnalyzer) Analyze(ctx context.Context, input AnswerAnalysisInput) (AnswerAnalysisResult, error) {
	// 用 JSON 承载用户内容，而不是拼进自然语言模板：
	// 结构化边界让“回答正文”始终是一个字段值，不会被读成新的指令行。
	rawInput, err := json.Marshal(input)
	if err != nil {
		return AnswerAnalysisResult{}, err
	}
	messages := []llm.Message{
		{Role: "system", Content: a.systemPrompt},
		{Role: "user", Content: string(rawInput)},
	}
	completion, err := a.model.Complete(ctx, messages)
	if err != nil {
		return AnswerAnalysisResult{}, err
	}
	result, err := parseAnswerAnalysisResult([]byte(completion.Content))
	if err == nil || !errors.Is(err, ErrFeynmanAnalysisInvalid) {
		return result, err
	}

	// 模型偶尔会在合法 JSON 外补一句解释或自作主张加字段。保留严格解析边界，
	// 但只对这种可修复的格式漂移重试一次；上游错误和超时不重试，避免放大故障。
	repairMessages := append(messages,
		llm.Message{Role: "assistant", Content: completion.Content},
		llm.Message{Role: "user", Content: "上一条输出不符合约定的 JSON 结构。请严格按系统消息中的输出格式重新输出，只返回一个 JSON 对象，不要添加未知字段、代码块或说明。"},
	)
	repaired, repairErr := a.model.Complete(ctx, repairMessages)
	if repairErr != nil {
		return AnswerAnalysisResult{}, repairErr
	}
	return parseAnswerAnalysisResult([]byte(repaired.Content))
}

func parseAnswerAnalysisResult(raw []byte) (AnswerAnalysisResult, error) {
	trimmed := stripJSONCodeFence(bytes.TrimSpace(raw))
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var result AnswerAnalysisResult
	if err := decoder.Decode(&result); err != nil {
		return AnswerAnalysisResult{}, fmt.Errorf("%w: %v", ErrFeynmanAnalysisInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return AnswerAnalysisResult{}, fmt.Errorf("%w: JSON 存在尾随内容", ErrFeynmanAnalysisInvalid)
	}
	return result, nil
}

// sanitizeAnswerAnalysis 对模型输出做一次强约束收敛。
//
// 这里丢弃而不是报错，是因为一次分析只要还剩下可核对的内容就仍然有价值；
// 但两条底线不让步：引语必须逐字出自用户回答，追问必须是单条且长度受控。
//
// dimensions 是本轮启用的诊断维度；模型返回未启用维度的缺口一律丢弃——
// “这道题不检查项目映射”是产品决定，保留就等于规则形同虚设。
func sanitizeAnswerAnalysis(result AnswerAnalysisResult, answer string, dimensions []string, limits FeynmanDialogLimits) AnswerAnalysisResult {
	limits = limits.withDefaults()

	covered := make([]string, 0, len(result.Covered))
	for _, item := range result.Covered {
		item = strings.TrimSpace(memoryValueNeutralizer.Replace(item))
		if item == "" {
			continue
		}
		covered = append(covered, truncateRunes(item, defaultMaxFeynmanGapTextRunes))
		if len(covered) >= defaultMaxFeynmanCoveredEntries {
			break
		}
	}

	gaps := make([]AnswerAnalysisGap, 0, len(result.Gaps))
	expressionKept := 0
	for _, gap := range result.Gaps {
		switch gap.Verdict {
		case FeynmanGapVerdictOmitted, FeynmanGapVerdictIncorrect, FeynmanGapVerdictUncertain:
		default:
			continue
		}
		// 维度为空按“关键点”兜底，兼容不带 dimension 的历史输出，不因此丢内容。
		dimension := strings.TrimSpace(gap.Dimension)
		if dimension == "" {
			dimension = FeynmanDimensionKeyPoints
		}
		if !feynmanDimensionEnabled(dimensions, dimension) {
			continue
		}
		// 表达维度太好写了：任何一段口语讲解都能挑出“结构可以更清晰”。
		// 数量闸放在服务层，不指望模型自觉。
		if dimension == FeynmanDimensionExpression {
			if expressionKept >= defaultMaxFeynmanExpressionGaps {
				continue
			}
			expressionKept++
		}
		verdict := gap.Verdict
		quote := strings.TrimSpace(gap.UserQuote)
		// 引语核对不放松：一个字对不上就清空，用户必须能回到自己那句话上，
		// 而不是被模型的转述说服。
		if quote != "" && !strings.Contains(answer, quote) {
			quote = ""
		}
		// 判错必须举得出原句。举不出通常不是幻觉，而是口语转写里的断句/语气词让摘抄不精确；
		// 直接丢掉等于静默扔掉一条真问题，所以降级为 uncertain：不冤枉用户，也把线索留给下一轮。
		if verdict == FeynmanGapVerdictIncorrect && quote == "" {
			verdict = FeynmanGapVerdictUncertain
		}
		label := strings.TrimSpace(memoryValueNeutralizer.Replace(gap.ConceptLabel))
		analysis := strings.TrimSpace(memoryValueNeutralizer.Replace(gap.Analysis))
		if label == "" && analysis == "" {
			continue
		}
		analysisLimit := defaultMaxFeynmanGapTextRunes
		if dimension == FeynmanDimensionExpression {
			// 长度闸：表达建议物理上写不成长篇写作指导。
			analysisLimit = defaultMaxFeynmanExpressionRunes
		}
		gaps = append(gaps, AnswerAnalysisGap{
			Dimension:    dimension,
			ConceptLabel: truncateRunes(label, defaultMaxFeynmanTopicRunes),
			Verdict:      verdict,
			UserQuote:    truncateRunes(quote, defaultMaxFeynmanGapTextRunes),
			Analysis:     truncateRunes(analysis, analysisLimit),
		})
		if len(gaps) >= limits.MaxGaps {
			break
		}
	}

	probe := strings.TrimSpace(memoryValueNeutralizer.Replace(result.NextProbe))

	return AnswerAnalysisResult{
		Covered:             covered,
		InsufficientSources: result.InsufficientSources,
		Gaps:                gaps,
		NextProbe:           truncateRunes(probe, limits.MaxProbeRunes),
	}
}

var feynmanGapVerdictLabels = map[string]string{
	FeynmanGapVerdictOmitted:   "没讲到",
	FeynmanGapVerdictIncorrect: "说反了",
	FeynmanGapVerdictUncertain: "讲得含糊",
}

// selectFeynmanFeedbackGaps 把完整分析收敛成一次对话里能扫完的反馈：
// 一条最关键错误 + 至多 maxSecondary 条重要遗漏/不确定项。
//
// 之所以要和 sanitizeAnswerAnalysis 分开：薄弱点账本不能漏（漏了就永远不会被复习到），
// 而一轮对话反馈必须短到能读完。这是两条互相拉扯的要求，只能用两份数据满足。
func selectFeynmanFeedbackGaps(gaps []AnswerAnalysisGap, maxSecondary int) (*AnswerAnalysisGap, []AnswerAnalysisGap, int) {
	if maxSecondary <= 0 {
		maxSecondary = defaultMaxFeynmanSecondaryGaps
	}
	var keyError *AnswerAnalysisGap
	secondary := make([]AnswerAnalysisGap, 0, maxSecondary)
	omitted := 0
	for index := range gaps {
		gap := gaps[index]
		// 模型已按重要程度排序，所以第一条 incorrect 就是这一轮最该先纠的错。
		if keyError == nil && gap.Verdict == FeynmanGapVerdictIncorrect {
			keyError = &gap
			continue
		}
		if len(secondary) < maxSecondary {
			secondary = append(secondary, gap)
			continue
		}
		omitted++
	}
	return keyError, secondary, omitted
}

// renderFeynmanFeedback 把结构化分析渲染成一段对话回复。
// 排版固定在服务端，不交给模型：反馈格式稳定，用户才能一眼扫到“哪里没讲到”。
// 四个区块顺序固定：讲对的部分 → 最关键错误 → 重要遗漏/不确定项 → 下一道针对性问题。
func renderFeynmanFeedback(result AnswerAnalysisResult, limits FeynmanDialogLimits) string {
	limits = limits.withDefaults()
	keyError, secondary, omitted := selectFeynmanFeedbackGaps(result.Gaps, limits.MaxSecondaryGaps)

	var builder strings.Builder
	if len(result.Covered) > 0 {
		builder.WriteString("讲对的部分：\n")
		for _, item := range result.Covered {
			builder.WriteString("- " + item + "\n")
		}
		builder.WriteString("\n")
	}
	if keyError == nil && len(secondary) == 0 {
		builder.WriteString("这一轮我没挑出明显的漏洞。\n")
	}
	if keyError != nil {
		builder.WriteString(fmt.Sprintf("最关键的问题：【%s】%s\n", feynmanGapVerdictLabels[keyError.Verdict], feynmanGapLabel(*keyError)))
		if keyError.UserQuote != "" {
			builder.WriteString("  你的原话：“" + keyError.UserQuote + "”\n")
		}
		if keyError.Analysis != "" {
			builder.WriteString("  " + keyError.Analysis + "\n")
		}
		if len(secondary) > 0 {
			builder.WriteString("\n")
		}
	}
	if len(secondary) > 0 {
		builder.WriteString("还要补这几点：\n")
		for index, gap := range secondary {
			builder.WriteString(fmt.Sprintf("%d. 【%s】%s\n", index+1, feynmanGapVerdictLabels[gap.Verdict], feynmanGapLabel(gap)))
			if gap.UserQuote != "" {
				builder.WriteString("   你的原话：“" + gap.UserQuote + "”\n")
			}
			if gap.Analysis != "" {
				builder.WriteString("   " + gap.Analysis + "\n")
			}
		}
	}
	// 没展示的条目要留一行说明：静默截断会让用户以为这一轮就这些问题。
	if omitted > 0 {
		builder.WriteString(fmt.Sprintf("\n（另有 %d 条次要问题这轮先不展开。）\n", omitted))
	}
	if result.InsufficientSources {
		builder.WriteString("\n（本轮没有可用资料支撑，以上判断基于常识，仅供参考。）\n")
	}
	if result.NextProbe != "" {
		builder.WriteString("\n接着回答这个：" + result.NextProbe)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func feynmanGapLabel(gap AnswerAnalysisGap) string {
	if gap.ConceptLabel != "" {
		return gap.ConceptLabel
	}
	return "这一点"
}
