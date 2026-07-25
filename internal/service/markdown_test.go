package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseMarkdownBuildsHeadingPathAndOffsets(t *testing.T) {
	raw := "前言正文。\n\n# 一级\n\n正文 A\n\n## 二级\n\n正文 B\n\n# 另一个一级\n\n正文 C\n"

	chunks, err := ParseMarkdown(raw, DefaultMarkdownParseLimits())
	if err != nil {
		t.Fatalf("ParseMarkdown 出错: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("片段数 = %d, want 4", len(chunks))
	}

	wantPaths := [][]string{
		{},
		{"一级"},
		{"一级", "二级"},
		{"另一个一级"},
	}
	for index, chunk := range chunks {
		if chunk.Ordinal != index+1 {
			t.Fatalf("chunks[%d].Ordinal = %d, want %d", index, chunk.Ordinal, index+1)
		}
		if strings.Join(chunk.HeadingPath, "/") != strings.Join(wantPaths[index], "/") {
			t.Fatalf("chunks[%d].HeadingPath = %v, want %v", index, chunk.HeadingPath, wantPaths[index])
		}
	}

	// 来源片段必须能按字符偏移回到原文，这是后续引用与评估的前提。
	runes := []rune(raw)
	for index, chunk := range chunks {
		got := string(runes[chunk.StartOffset:chunk.EndOffset])
		if got != chunk.Content {
			t.Fatalf("chunks[%d] 偏移取回 %q, want %q", index, got, chunk.Content)
		}
	}
}

func TestParseMarkdownIgnoresHeadingLikeLinesInCodeBlock(t *testing.T) {
	raw := "# 标题\n\n```sh\n# 这是注释不是标题\necho hi\n```\n\n正文\n"

	chunks, err := ParseMarkdown(raw, DefaultMarkdownParseLimits())
	if err != nil {
		t.Fatalf("ParseMarkdown 出错: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("片段数 = %d, want 1（代码块内的 # 不是标题）", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "echo hi") {
		t.Fatalf("片段丢失了代码块正文: %q", chunks[0].Content)
	}
}

func TestParseMarkdownOffsetsUseCharactersNotBytes(t *testing.T) {
	raw := "中文前言\n\n# 标题\n\n正文\n"

	chunks, err := ParseMarkdown(raw, DefaultMarkdownParseLimits())
	if err != nil {
		t.Fatalf("ParseMarkdown 出错: %v", err)
	}
	last := chunks[len(chunks)-1]
	if last.EndOffset > int64(utf8.RuneCountInString(raw)) {
		t.Fatalf("EndOffset = %d 超过原文字符数 %d，偏移仍然是字节口径",
			last.EndOffset, utf8.RuneCountInString(raw))
	}
}

func TestParseMarkdownEnforcesBudgets(t *testing.T) {
	t.Run("标题数量", func(t *testing.T) {
		raw := strings.Repeat("# 标题\n\n正文\n\n", 5)
		if _, err := ParseMarkdown(raw, MarkdownParseLimits{MaxHeadings: 2, MaxChunks: 100, MaxChunkChars: 1000}); err == nil {
			t.Fatal("标题数量超限时应当失败")
		}
	})
	t.Run("片段长度", func(t *testing.T) {
		raw := "# 标题\n\n" + strings.Repeat("正", 50)
		if _, err := ParseMarkdown(raw, MarkdownParseLimits{MaxHeadings: 10, MaxChunks: 100, MaxChunkChars: 10}); err == nil {
			t.Fatal("片段过长时应当失败")
		}
	})
	t.Run("原文长度", func(t *testing.T) {
		raw := strings.Repeat("正文\n", 100)
		if _, err := ParseMarkdown(raw, MarkdownParseLimits{MaxRawChars: 10}); err == nil {
			t.Fatal("原文超过解析预算时应当失败")
		}
	})
	t.Run("空正文", func(t *testing.T) {
		if _, err := ParseMarkdown("   \n\n  \n", DefaultMarkdownParseLimits()); err == nil {
			t.Fatal("空白原文应当失败")
		}
	})
}
