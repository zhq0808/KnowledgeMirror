package service

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// MarkdownParserVersion 标识切分算法版本。算法变更必须提升版本号，
// 否则无法区分“同一份原文在不同解析器下得到的来源片段”。
const MarkdownParserVersion = "markdown-ast-v1"

// MarkdownChunk 是一份 Markdown 版本中的一个可追溯来源片段。
// StartOffset/EndOffset 是相对该版本原文的字符（rune）偏移，左闭右开，
// 与 PostgreSQL char_length 语义一致，保证片段能精确回到原文位置。
type MarkdownChunk struct {
	Ordinal     int
	HeadingPath []string
	Content     string
	StartOffset int64
	EndOffset   int64
}

// MarkdownParseLimits 是解析预算。任何一项超限都在落库之前失败，
// 避免把一份异常文件变成成千上万条来源片段或超长 Prompt 上下文。
type MarkdownParseLimits struct {
	MaxRawChars   int // 原文字符总数上限
	MaxHeadings   int // 顶层标题数量上限
	MaxChunks     int // 来源片段数量上限
	MaxChunkChars int // 单个来源片段字符数上限
}

// DefaultMarkdownParseLimits 是 Markdown v0 的默认预算。
func DefaultMarkdownParseLimits() MarkdownParseLimits {
	return MarkdownParseLimits{
		MaxRawChars:   400_000,
		MaxHeadings:   800,
		MaxChunks:     1_000,
		MaxChunkChars: 20_000,
	}
}

// headingBoundary 是一个顶层标题在原文中的位置与层级。
type headingBoundary struct {
	level     int
	title     string
	lineStart int // 标题所在行的起始字节偏移
}

// ParseMarkdown 用 Markdown AST 读取标题层级，再按“标题分节”切出来源片段。
//
// 为什么不用字符串切割：`#` 可能出现在代码块、行内代码或 YAML front matter 中，
// 按行前缀匹配会把它们误判成标题。这里只承认 AST 判定的顶层 Heading 节点，
// 嵌套在引用块、列表中的标题不作为分节边界，从而保证片段边界稳定可复现。
func ParseMarkdown(raw string, limits MarkdownParseLimits) ([]MarkdownChunk, error) {
	if !utf8.ValidString(raw) {
		return nil, fmt.Errorf("原文不是合法 UTF-8")
	}
	if limits.MaxRawChars > 0 {
		if total := utf8.RuneCountInString(raw); total > limits.MaxRawChars {
			return nil, fmt.Errorf("原文长度 %d 字符超过解析预算 %d", total, limits.MaxRawChars)
		}
	}

	source := []byte(raw)
	document := goldmark.New().Parser().Parse(text.NewReader(source))

	boundaries := make([]headingBoundary, 0, 16)
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		heading, isHeading := node.(*ast.Heading)
		if !isHeading {
			continue
		}
		lines := heading.Lines()
		if lines.Len() == 0 {
			// 空标题（形如单独一个 `#`）无法定位文本行，不作为分节边界，
			// 其内容归入上一节，避免产生 start_offset 不可信的片段。
			continue
		}
		if limits.MaxHeadings > 0 && len(boundaries) >= limits.MaxHeadings {
			return nil, fmt.Errorf("标题数量超过上限 %d", limits.MaxHeadings)
		}
		boundaries = append(boundaries, headingBoundary{
			level:     heading.Level,
			title:     headingTitle(heading, source),
			lineStart: lineStart(source, lines.At(0).Start),
		})
	}

	sectionStarts := make([]int, 0, len(boundaries)+1)
	sectionStarts = append(sectionStarts, 0) // 第一个标题之前的前言
	for _, boundary := range boundaries {
		sectionStarts = append(sectionStarts, boundary.lineStart)
	}

	converter := newRuneOffsetConverter(raw)
	headingPath := make([]string, 0, 8)
	chunks := make([]MarkdownChunk, 0, len(sectionStarts))

	for index, start := range sectionStarts {
		end := len(source)
		if index+1 < len(sectionStarts) {
			end = sectionStarts[index+1]
		}
		if index > 0 {
			boundary := boundaries[index-1]
			headingPath = appendHeadingPath(headingPath, boundary)
		}
		if start >= end {
			continue
		}

		trimmedStart, trimmedEnd := trimSpaceRange(raw, start, end)
		if trimmedStart >= trimmedEnd {
			continue // 空白小节不产生来源片段
		}
		content := raw[trimmedStart:trimmedEnd]
		if limits.MaxChunkChars > 0 {
			if length := utf8.RuneCountInString(content); length > limits.MaxChunkChars {
				return nil, fmt.Errorf("来源片段 %q 长度 %d 字符超过上限 %d，请拆分小节后重试",
					headingPathLabel(headingPath), length, limits.MaxChunkChars)
			}
		}
		if limits.MaxChunks > 0 && len(chunks) >= limits.MaxChunks {
			return nil, fmt.Errorf("来源片段数量超过上限 %d", limits.MaxChunks)
		}

		chunks = append(chunks, MarkdownChunk{
			Ordinal:     len(chunks) + 1,
			HeadingPath: append([]string(nil), headingPath...),
			Content:     content,
			StartOffset: converter.at(trimmedStart),
			EndOffset:   converter.at(trimmedEnd),
		})
	}

	if len(chunks) == 0 {
		return nil, fmt.Errorf("原文没有可解析的正文内容")
	}
	return chunks, nil
}

// appendHeadingPath 按标题层级维护路径栈：同级或更高层级的标题会截断旧路径。
func appendHeadingPath(path []string, boundary headingBoundary) []string {
	depth := boundary.level - 1
	if depth > len(path) {
		depth = len(path)
	}
	if depth < 0 {
		depth = 0
	}
	path = append(path[:depth], boundary.title)
	return path
}

func headingPathLabel(path []string) string {
	if len(path) == 0 {
		return "(前言)"
	}
	return strings.Join(path, " / ")
}

// headingTitle 从 AST 行片段还原标题文本，不依赖已废弃的 Node.Text。
func headingTitle(heading *ast.Heading, source []byte) string {
	var builder strings.Builder
	lines := heading.Lines()
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		builder.Write(segment.Value(source))
	}
	title := strings.TrimSpace(builder.String())
	if title == "" {
		return "(无标题)"
	}
	return title
}

// lineStart 从 pos 向前回溯到所在行的起始字节偏移。
func lineStart(source []byte, pos int) int {
	if pos > len(source) {
		pos = len(source)
	}
	for pos > 0 && source[pos-1] != '\n' {
		pos--
	}
	return pos
}

// trimSpaceRange 在 [start,end) 内去掉首尾空白，返回仍然指向原文的字节区间，
// 从而保证 content 与 start_offset/end_offset 完全对应。
func trimSpaceRange(raw string, start, end int) (int, int) {
	trimmed := strings.TrimRight(raw[start:end], " \t\r\n")
	end = start + len(trimmed)
	trimmed = strings.TrimLeft(raw[start:end], " \t\r\n")
	start = end - len(trimmed)
	return start, end
}

// runeOffsetConverter 把递增的字节偏移换算成字符偏移。
// 片段区间天然有序，所以只需线性扫描一遍原文。
type runeOffsetConverter struct {
	raw       string
	lastByte  int
	lastRunes int64
}

func newRuneOffsetConverter(raw string) *runeOffsetConverter {
	return &runeOffsetConverter{raw: raw}
}

func (c *runeOffsetConverter) at(byteOffset int) int64 {
	if byteOffset < c.lastByte {
		// 防御性回退：理论上不会发生，退化为整段重算而不是返回错误偏移。
		c.lastByte, c.lastRunes = 0, 0
	}
	c.lastRunes += int64(utf8.RuneCountInString(c.raw[c.lastByte:byteOffset]))
	c.lastByte = byteOffset
	return c.lastRunes
}
