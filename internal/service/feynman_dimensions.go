package service

import "strings"

// ---------------------------------------------------------------------------
// 对话式费曼学习 · 诊断维度（纯规则，无 IO）
//
// 维度是题目属性的函数，不是全局配置。固定检查表有两个实打实的损害：
//   - 凑数式缺口：让模型对“offset 是什么”这种定义题也检查“项目映射”，
//     它会为了填满维度编出“你没有结合项目讲”，占掉本该给真问题的位置；
//   - 反馈稀释：每轮 5 条里有 3 条是套模板凑的，用户很快学会跳读整段反馈。
//
// 判定用规则而不是再调一次模型，理由和意图识别一致（见 feynman_dialog_intent.go）：
// 分析本身已经是一次 LLM 调用，再加一次分类等于加一倍延迟和失败面；而规则的误判
// 代价可控——多启用一个维度只是多检查一项，漏启用最多退化成“只查关键点”。
// ---------------------------------------------------------------------------

// 诊断维度取值。
const (
	// FeynmanDimensionKeyPoints 是费曼练习的本体：该讲到的关键点有没有讲到。
	FeynmanDimensionKeyPoints = "key_points"
	// FeynmanDimensionCausalChain 检查因果链是否完整、取舍依据是否讲清。
	FeynmanDimensionCausalChain = "causal_chain"
	// FeynmanDimensionProjectMapping 检查有没有落到自己真实做过的事上、边界是否诚实。
	FeynmanDimensionProjectMapping = "project_mapping"
	// FeynmanDimensionFactBoundary 检查事实断言是否成立、适用边界是否说清。
	FeynmanDimensionFactBoundary = "fact_boundary"
	// FeynmanDimensionExpression 检查表达是否影响理解或面试呈现（受数量与长度双重约束）。
	FeynmanDimensionExpression = "expression"
)

// feynmanCausalQuestionMarkers 标记因果题与权衡题。
var feynmanCausalQuestionMarkers = []string{
	"为什么", "为何", "凭什么", "怎么会", "原因", "动机",
	"权衡", "取舍", "选型", "怎么选", "如何选择", "为什么不用",
	"对比", "相比", "区别", "差异", "优缺点", "优势", "劣势", "好处", "坏处",
	"什么场景", "适合", "代价", "trade-off", "tradeoff",
}

// feynmanProjectQuestionMarkers 标记项目题与经历题。
var feynmanProjectQuestionMarkers = []string{
	"项目", "当时", "线上", "生产", "你们", "我们", "落地", "上线",
	"实际", "真实", "做过", "踩过", "遇到过", "经历", "业务里", "怎么做的",
}

// feynmanFactQuestionMarkers 标记定义题与原理题：这类题天然全是事实断言。
var feynmanFactQuestionMarkers = []string{
	"是什么", "什么是", "定义", "原理", "机制", "流程", "怎么实现", "如何实现",
	"怎么工作", "内部", "底层", "区别", "组成",
}

// feynmanAssertionMarkers 标记回答里的绝对化断言。
//
// 这是 fact_boundary 唯一同时读回答的原因：checklist 原文是“涉及事实断言时才核对
// 事实与适用边界”，而用户是否做出断言，只有读了回答才知道——“讲讲你当时怎么做的”
// 这种题，如果用户张口就是“Kafka 保证 exactly once”，事实核对必须能启用。
var feynmanAssertionMarkers = []string{
	"一定", "必须", "肯定", "绝对", "不可能", "永远", "总是", "所有", "任何",
	"只能", "只要", "保证", "确保", "百分之百", "等价于", "本质上", "就是说",
	"不会丢", "不会重复", "原子",
}

// resolveFeynmanDimensions 按题目（必要时结合回答）决定本轮启用哪些诊断维度。
// 返回顺序固定，便于表驱动测试与 Prompt 稳定。
func resolveFeynmanDimensions(question, answer string) []string {
	// 词表用 Contains 而不是控制词那种整句匹配：两者怕的方向相反。
	// 控制词怕“把一整段回答误吞成跳过”，所以要严；维度判定错了只影响检查范围，宁可宽。
	dimensions := []string{FeynmanDimensionKeyPoints}
	if containsAny(question, feynmanCausalQuestionMarkers) {
		dimensions = append(dimensions, FeynmanDimensionCausalChain)
	}
	if containsAny(question, feynmanProjectQuestionMarkers) {
		dimensions = append(dimensions, FeynmanDimensionProjectMapping)
	}
	if containsAny(question, feynmanFactQuestionMarkers) || containsAny(answer, feynmanAssertionMarkers) {
		dimensions = append(dimensions, FeynmanDimensionFactBoundary)
	}
	dimensions = append(dimensions, FeynmanDimensionExpression)
	return dimensions
}

// feynmanDimensionEnabled 判断一条缺口所属维度是否在本轮启用集合内。
// 集合为空表示调用方没有做维度裁剪（例如老数据回放），此时不做限制。
func feynmanDimensionEnabled(dimensions []string, dimension string) bool {
	if len(dimensions) == 0 {
		return true
	}
	for _, item := range dimensions {
		if item == dimension {
			return true
		}
	}
	return false
}

func containsAny(text string, markers []string) bool {
	if text == "" {
		return false
	}
	lowered := strings.ToLower(text)
	for _, marker := range markers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
