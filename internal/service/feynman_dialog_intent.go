package service

import (
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 对话式费曼学习 · 意图识别（纯规则，无 IO）
//
// 为什么 v0 不调模型做意图分类：
//   - `idle` 态每条普通消息都多一次分类调用，等于给全站聊天加一次延迟和费用；
//   - 模型误判会直接吃掉用户的正常提问（把普通问题当成练习开始），代价比漏识别大；
//   - 入口只有两种（点“费曼学习”发固定文案、显式说“我来讲 X”），规则足够覆盖。
//
// 后续要加模型兜底时 seam 已经留好：resolveFeynmanIntent 返回 feynmanIntentNone 时
// 再挂一层分类器即可，状态机本身不用改。
// ---------------------------------------------------------------------------

// 会话级费曼练习状态。queue_paused 在 v0 没有题目队列，语义是“整段练习暂停”，
// 保留这个取值是为了后续接入文档供题队列时不再改枚举和历史数据。
const (
	FeynmanStateIdle             = "idle"
	FeynmanStateAwaitingTopic    = "awaiting_topic"
	FeynmanStateAwaitingAnswer   = "awaiting_answer"
	FeynmanStateAnalyzingAnswer  = "analyzing_answer"
	FeynmanStateAwaitingFollowUp = "awaiting_follow_up"
	FeynmanStateAwaitingRetry    = "awaiting_retry"
	FeynmanStatePaused           = "queue_paused"
)

// 题目来源：用户自述主题、AI 上一轮追问或每日教练处方。
const (
	FeynmanQuestionOriginUserTopic  = "user_topic"
	FeynmanQuestionOriginFollowUp   = "ai_follow_up"
	FeynmanQuestionOriginCoachTask  = "coach_task"
	feynmanEntryMessage             = "我想开始费曼学习练习"
	defaultMaxControlPhraseRunes    = 16
	defaultMaxFeynmanTopicRunes     = 120
	defaultMaxFeynmanProbeRunes     = 120
	defaultMaxFeynmanGaps           = 5
	defaultMaxFeynmanQuestionRunes  = 200
	defaultMaxFeynmanAnswerRunes    = 6000
	defaultMaxFeynmanGapTextRunes   = 400
	defaultMaxFeynmanCoveredEntries = 3
	// 一次反馈里除“最关键错误”之外最多展示几条（完整结果仍保留，只是不入本轮回复）。
	defaultMaxFeynmanSecondaryGaps = 3
	// 进入分析的最近对话轮数与单条长度上限。
	defaultMaxFeynmanContextTurns     = 6
	defaultMaxFeynmanContextTurnRunes = 300
	// 表达维度的数量闸与长度闸：只允许一句话指出，不展开成写作建议。
	defaultMaxFeynmanExpressionGaps  = 1
	defaultMaxFeynmanExpressionRunes = 100
)

// 意图取值。
const (
	feynmanIntentNone          = "none" // 不归费曼管，调用方继续走自由对话
	feynmanIntentStartPractice = "start_practice"
	feynmanIntentStartTopic    = "start_topic"
	feynmanIntentAnswer        = "answer"
	feynmanIntentPause         = "pause"
	feynmanIntentResume        = "resume"
	feynmanIntentSkip          = "skip"
	feynmanIntentRetry         = "retry"
	feynmanIntentStop          = "stop"
)

type feynmanIntent struct {
	Kind string
	// Topic 只在 start_topic / awaiting_topic 场景有值。
	Topic string
}

// feynmanControlPhrases 是整句控制词表：必须整条消息（去掉首尾空白与结尾标点后）等于表中某一项。
var feynmanControlPhrases = map[string]string{
	"暂停":    feynmanIntentPause,
	"先暂停":   feynmanIntentPause,
	"暂停一下":  feynmanIntentPause,
	"先停一下":  feynmanIntentPause,
	"先不练了":  feynmanIntentPause,
	"等下再练":  feynmanIntentPause,
	"继续":    feynmanIntentResume,
	"继续吧":   feynmanIntentResume,
	"继续练习":  feynmanIntentResume,
	"我们继续":  feynmanIntentResume,
	"接着来":   feynmanIntentResume,
	"下一题":   feynmanIntentResume,
	"跳过":    feynmanIntentSkip,
	"跳过吧":   feynmanIntentSkip,
	"跳过这题":  feynmanIntentSkip,
	"换一题":   feynmanIntentSkip,
	"换个题":   feynmanIntentSkip,
	"这题不会":  feynmanIntentSkip,
	"重来":    feynmanIntentRetry,
	"重讲":    feynmanIntentRetry,
	"重新讲":   feynmanIntentRetry,
	"重新讲一次": feynmanIntentRetry,
	"再讲一遍":  feynmanIntentRetry,
	"我重讲":   feynmanIntentRetry,
	"不练了":   feynmanIntentStop,
	"结束练习":  feynmanIntentStop,
	"退出练习":  feynmanIntentStop,
	"停止练习":  feynmanIntentStop,
	"结束费曼":  feynmanIntentStop,
	"不做费曼了": feynmanIntentStop,
}

// feynmanStartPhrases 是 idle 态的开始练习整句触发词。
var feynmanStartPhrases = map[string]struct{}{
	feynmanEntryMessage: {},
	"费曼学习":              {},
	"费曼学习法":             {},
	"开始费曼":              {},
	"开始费曼学习":            {},
	"开始费曼练习":            {},
	"开始练习":              {},
	"我要练习讲":             {},
}

// feynmanTopicPrefixes 是“用户直接指定一个主题”的前缀。命中后剩余部分即主题。
var feynmanTopicPrefixes = []string{
	"我来讲讲", "我来讲", "我来回答", "我来说说", "我来说", "我讲一下", "我说一下",
	"我想讲讲", "我想讲", "我试着讲", "我来复述", "考考我", "考我", "问问我", "问我",
}

// feynmanTopicNoise 是主题前缀之后需要继续剥掉的连接词与标点。
var feynmanTopicNoise = []string{"一下", "一讲", "关于", "：", ":", "，", ",", "、", "。", " ", "　"}

// feynmanTrailingPunctuation 在做整句匹配前先去掉句尾语气标点，
// 让“暂停。”“继续～”这类输入同样能命中控制词。
const feynmanTrailingPunctuation = "。．.!！?？~～、,，;；:： 　\t"

// resolveFeynmanIntent 按当前状态解释一条用户消息。
//
// 核心规则（对齐需求校准 §3.2）：awaiting_answer 下除控制词外的一切内容都是回答，
// 哪怕它看起来像一个提问——用户完全可能在自问自答式讲解。
func resolveFeynmanIntent(state, message string, maxControlPhraseRunes, maxTopicRunes int) feynmanIntent {
	message = strings.TrimSpace(message)
	if message == "" {
		return feynmanIntent{Kind: feynmanIntentNone}
	}
	if maxControlPhraseRunes <= 0 {
		maxControlPhraseRunes = defaultMaxControlPhraseRunes
	}
	if maxTopicRunes <= 0 {
		maxTopicRunes = defaultMaxFeynmanTopicRunes
	}

	if state != FeynmanStateIdle {
		if kind, ok := matchFeynmanControlPhrase(message, maxControlPhraseRunes); ok {
			return feynmanIntent{Kind: kind}
		}
	}

	switch state {
	case FeynmanStateIdle:
		return resolveFeynmanStartIntent(message, maxTopicRunes)
	case FeynmanStateAwaitingTopic:
		// 这里等的是主题，不是回答：整条消息就是用户要讲的题目。
		if topic := extractFeynmanTopic(message, maxTopicRunes); topic != "" {
			return feynmanIntent{Kind: feynmanIntentStartTopic, Topic: topic}
		}
		return feynmanIntent{Kind: feynmanIntentNone}
	case FeynmanStateAwaitingAnswer, FeynmanStateAwaitingRetry, FeynmanStateAnalyzingAnswer:
		return feynmanIntent{Kind: feynmanIntentAnswer}
	case FeynmanStateAwaitingFollowUp:
		// 已经给过反馈：用户如果显式换主题就换，否则当作对上一题的补充讲解。
		if start := resolveFeynmanStartIntent(message, maxTopicRunes); start.Kind == feynmanIntentStartTopic {
			return start
		}
		return feynmanIntent{Kind: feynmanIntentAnswer}
	case FeynmanStatePaused:
		// 暂停期间必须能正常聊天：除控制词外一律不介入。
		return feynmanIntent{Kind: feynmanIntentNone}
	default:
		return feynmanIntent{Kind: feynmanIntentNone}
	}
}

// matchFeynmanControlPhrase 只在“整条消息就是一句控制词”时命中。
//
// 长度闸门是关键防误伤规则：用户的真实回答里完全可能出现“……这一步可以跳过……”，
// 如果做子串匹配，会把一整段回答误吞成一次“跳过”指令。
func matchFeynmanControlPhrase(message string, maxControlPhraseRunes int) (string, bool) {
	normalized := strings.Trim(message, feynmanTrailingPunctuation)
	normalized = strings.TrimSpace(normalized)
	if normalized == "" || utf8.RuneCountInString(normalized) > maxControlPhraseRunes {
		return "", false
	}
	if kind, ok := feynmanControlPhrases[normalized]; ok {
		return kind, true
	}
	return "", false
}

// resolveFeynmanStartIntent 处理 idle 态：只认显式的开始说法，其余一律不介入。
func resolveFeynmanStartIntent(message string, maxTopicRunes int) feynmanIntent {
	normalized := strings.Trim(strings.TrimSpace(message), feynmanTrailingPunctuation)
	if _, ok := feynmanStartPhrases[normalized]; ok {
		return feynmanIntent{Kind: feynmanIntentStartPractice}
	}
	for _, prefix := range feynmanTopicPrefixes {
		if !strings.HasPrefix(normalized, prefix) {
			continue
		}
		rest := strings.TrimPrefix(normalized, prefix)
		if topic := extractFeynmanTopic(rest, maxTopicRunes); topic != "" {
			return feynmanIntent{Kind: feynmanIntentStartTopic, Topic: topic}
		}
		// 说了“我来讲”却没给主题：当成开始练习，由 AI 追问主题。
		return feynmanIntent{Kind: feynmanIntentStartPractice}
	}
	return feynmanIntent{Kind: feynmanIntentNone}
}

// extractFeynmanTopic 清洗并截断主题文本。
// 折叠换行是安全要求：主题会被回显进后续 Prompt，多行文本可以伪造出“系统指令行”。
func extractFeynmanTopic(raw string, maxTopicRunes int) string {
	topic := strings.TrimSpace(memoryValueNeutralizer.Replace(raw))
	for {
		trimmed := topic
		for _, noise := range feynmanTopicNoise {
			trimmed = strings.TrimPrefix(trimmed, noise)
		}
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == topic {
			break
		}
		topic = trimmed
	}
	return truncateRunes(topic, maxTopicRunes)
}
