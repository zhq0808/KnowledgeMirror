package service

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// 术语歧义检测（v0 启发式）
//
// 目的只有一个：把「我知道你可能被听错的高频术语」挑出来，让用户在把这段话当作
// 正式回答发出去之前扫一眼。它不判断对错，也不修改转写文本。
//
// 为什么不是纯编辑距离：中文同音误转写（“幂等” → “读百”）两个词一个字都不重合，
// 编辑距离和随便两个词没有区别，检测不出来。所以主力手段是可配置的词表 +
// 每个术语已知的常见误转写；编辑距离只用于拉丁术语（RocketMQ / Kafka 这类）。
// ---------------------------------------------------------------------------

// AmbiguousTerm 是一次疑似误转写：Term 是词表里的标准术语，Heard 是转写里实际出现的可疑写法。
type AmbiguousTerm struct {
	Term  string `json:"term"`
	Heard string `json:"heard"`
}

type glossaryConfusable struct {
	raw        string
	normalized string
}

type glossaryEntry struct {
	term        string
	normalized  string
	latin       bool // 归一化后只含 ASCII 字母数字，才允许走编辑距离
	confusables []glossaryConfusable
}

// TermGlossary 是术语词表。零值不可用，请用 NewTermGlossary 或 LoadTermGlossary 构造。
// 构造后只读，可被多个请求并发使用。
type TermGlossary struct {
	entries []glossaryEntry
}

// NewTermGlossary 从若干行词表定义构造术语表。每行格式：
//
//	术语|常见误转写1,常见误转写2
//
// `|` 及之后可省略；`#` 开头和空行会被忽略。
func NewTermGlossary(lines []string) *TermGlossary {
	glossary := &TermGlossary{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		termPart, confusablePart, _ := strings.Cut(line, "|")
		term := strings.TrimSpace(termPart)
		normalized := normalizeGlossaryText(term)
		if term == "" || normalized == "" {
			continue
		}
		entry := glossaryEntry{term: term, normalized: normalized, latin: isLatinToken(normalized)}
		for _, raw := range strings.Split(confusablePart, ",") {
			raw = strings.TrimSpace(raw)
			normalizedConfusable := normalizeGlossaryText(raw)
			if raw == "" || normalizedConfusable == "" {
				continue
			}
			entry.confusables = append(entry.confusables, glossaryConfusable{raw: raw, normalized: normalizedConfusable})
		}
		glossary.entries = append(glossary.entries, entry)
	}
	return glossary
}

// LoadTermGlossary 从文件加载术语表。文件不存在或读取失败时返回错误，
// 由调用方决定是降级（关闭歧义检测）还是启动失败。
func LoadTermGlossary(path string) (*TermGlossary, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开术语词表失败: %w", err)
	}
	defer func() { _ = file.Close() }()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取术语词表失败: %w", err)
	}
	return NewTermGlossary(lines), nil
}

// Size 返回词表里的术语条数，供启动日志确认词表确实生效。
func (g *TermGlossary) Size() int {
	if g == nil {
		return 0
	}
	return len(g.entries)
}

// AmbiguousTerms 返回文本里疑似被听错的术语，最多 max 条。
//
// 对每个术语的判定顺序：
//  1. 文本里出现术语本身 → 讲对了，不算歧义；
//  2. 文本里出现该术语已知的常见误转写 → 记一条；
//  3. 拉丁术语再退一步做逐 token 编辑距离，命中相近但不相同的写法 → 记一条。
func (g *TermGlossary) AmbiguousTerms(text string, max int) []AmbiguousTerm {
	if g == nil || len(g.entries) == 0 || max <= 0 {
		return nil
	}
	normalized := normalizeGlossaryText(text)
	if normalized == "" {
		return nil
	}
	tokens := latinTokens(text)

	var found []AmbiguousTerm
	for _, entry := range g.entries {
		if strings.Contains(normalized, entry.normalized) {
			continue
		}
		heard := ""
		for _, confusable := range entry.confusables {
			if strings.Contains(normalized, confusable.normalized) {
				heard = confusable.raw
				break
			}
		}
		if heard == "" && entry.latin {
			heard = closestLatinToken(tokens, entry.normalized)
		}
		if heard == "" {
			continue
		}
		found = append(found, AmbiguousTerm{Term: entry.term, Heard: heard})
		if len(found) >= max {
			break
		}
	}
	return found
}

// normalizeGlossaryText 统一比较口径：转小写、丢掉空白与标点。
// 这样 “Rocket MQ”“rocket-mq”“RocketMQ” 会被当成同一个写法，不会被误判为歧义。
func normalizeGlossaryText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(text) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// latinTokens 从原文里切出拉丁字母/数字组成的词，供编辑距离比对。
// 只对这些 token 做模糊匹配，避免在整段文本上滑窗带来的无谓开销。
func latinTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func isLatinToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// closestLatinToken 返回与术语足够接近但不相同的 token。
// 阈值随术语长度放宽（max(1, len/4)）：短术语容不下多少差异，长术语允许一两个字母的转写偏差。
func closestLatinToken(tokens []string, term string) string {
	const minFuzzyLen = 4
	if len(term) < minFuzzyLen {
		return ""
	}
	threshold := len(term) / 4
	if threshold < 1 {
		threshold = 1
	}
	for _, token := range tokens {
		if token == term {
			return ""
		}
		if abs(len(token)-len(term)) > threshold {
			continue
		}
		if distance := damerauLevenshtein(token, term); distance > 0 && distance <= threshold {
			return token
		}
	}
	return ""
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// damerauLevenshtein 是带相邻换位的编辑距离（OSA 变体）。
//
// 之所以不用普通编辑距离：转写里最常见的错法就是相邻字母调个个儿（kafka -> kafak），
// 普通编辑距离把它算成 2，会被短术语的阈值直接挡掉，等于白算。
// 输入都是短 token（术语级别），不需要更复杂的优化。
func damerauLevenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	beforePrevious := make([]int, len(br)+1)
	previous := make([]int, len(br)+1)
	current := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		current[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, min(current[j-1]+1, previous[j-1]+cost))
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				current[j] = min(current[j], beforePrevious[j-2]+1)
			}
		}
		beforePrevious, previous, current = previous, current, beforePrevious
	}
	return previous[len(br)]
}
