package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"healthAgent/internal/llm"
)

type fakeChatModel struct {
	messages []llm.Message
}

type fakeChatMemoryReader struct {
	userID   string
	budget   MemoryBudget
	memories []Memory
	err      error
}

type fakeChatRetriever struct {
	result RetrievalResult
	err    error
}

func (r *fakeChatRetriever) Retrieve(_ context.Context, _ RetrievalQuery) (RetrievalResult, error) {
	return r.result, r.err
}

func (r *fakeChatMemoryReader) ListCurrentMemories(_ context.Context, userID string, budget MemoryBudget) ([]Memory, error) {
	r.userID = userID
	r.budget = budget
	return r.memories, r.err
}

func testChatPrompt(t *testing.T) *ChatPrompt {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.tmpl")
	templateText := `版本={{.Version}}
可信边界={{.TrustBoundary}}
事实={{.UserFactSummary}}
资料={{.RetrievedContext}}
你是面试训练伙伴。可信边界 > 已确认用户事实 > 当前问题 > 最近会话历史。`
	if err := os.WriteFile(path, []byte(templateText), 0o600); err != nil {
		t.Fatalf("write prompt template: %v", err)
	}
	prompt, err := LoadChatPrompt(path, "test-v2", "禁止夸大用户经历或掌握状态")
	if err != nil {
		t.Fatalf("LoadChatPrompt() error = %v", err)
	}
	return prompt
}

func (f *fakeChatModel) Timeout() time.Duration {
	return time.Second
}

func (f *fakeChatModel) ModelName() string { return "fake-model" }

func (f *fakeChatModel) Stream(_ context.Context, messages []llm.Message, onDelta func(string) error) error {
	f.messages = messages
	return onDelta("reply")
}

func TestChatServiceStreamBuildsMessages(t *testing.T) {
	model := &fakeChatModel{}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, DefaultMaxReplyChars)
	var reply string
	history := []ConversationMessage{
		{Seq: 1, Role: "user", Content: "my name is Alice"},
		{Seq: 2, Role: "assistant", Content: "nice to meet you"},
		{Seq: 3, Role: "user", Content: "what is my name?"},
	}

	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID:  "usr-alice",
		History: history,
		OnDelta: func(delta string) error {
			reply += delta
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if reply != "reply" {
		t.Fatalf("reply = %q, want reply", reply)
	}
	if content != "reply" {
		t.Fatalf("content = %q, want reply", content)
	}
	if len(model.messages) != 4 {
		t.Fatalf("message count = %d, want 4", len(model.messages))
	}
	if model.messages[0].Role != "system" {
		t.Fatalf("system message = %#v", model.messages[0])
	}
	for _, required := range []string{"test-v2", "禁止夸大用户经历或掌握状态", noConfirmedUserFacts, "面试训练伙伴"} {
		if !strings.Contains(model.messages[0].Content, required) {
			t.Fatalf("system prompt missing %q: %q", required, model.messages[0].Content)
		}
	}
	if strings.Contains(model.messages[0].Content, "Intent Categories") || strings.Contains(model.messages[0].Content, "Strict JSON") {
		t.Fatalf("chat system prompt contains intent-classifier instructions: %q", model.messages[0].Content)
	}
	for index, message := range history {
		want := llm.Message{Role: message.Role, Content: message.Content}
		if model.messages[index+1] != want {
			t.Fatalf("model message %d = %#v, want %#v", index+1, model.messages[index+1], want)
		}
	}
}

func TestChatServiceStreamInjectsCurrentUserMemoriesAcrossSessions(t *testing.T) {
	model := &fakeChatModel{}
	memoryReader := &fakeChatMemoryReader{memories: []Memory{
		{MemoryType: "preference", MemoryValue: "用户不爱吃辣"},
	}}
	budget := MemoryBudget{MaxCount: 20, MaxChars: 2000}
	chatService := NewChatService(model, testChatPrompt(t), memoryReader, budget, DefaultMaxReplyChars)

	_, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID: "usr-owner",
		History: []ConversationMessage{
			{Seq: 1, Role: "user", Content: "我喜欢吃辣吗？"},
		},
		OnDelta: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if memoryReader.userID != "usr-owner" || memoryReader.budget.MaxCount != budget.MaxCount {
		t.Fatalf("memory lookup user=%q budget=%+v, want usr-owner and MaxCount=%d", memoryReader.userID, memoryReader.budget, budget.MaxCount)
	}
	if len(model.messages) == 0 || !strings.Contains(model.messages[0].Content, "- [preference] 用户不爱吃辣") {
		t.Fatalf("system prompt did not include recalled preference: %+v", model.messages)
	}
}

func TestChatServiceStreamDegradesRetrievalFailureAndKeepsAuditRequestID(t *testing.T) {
	model := &fakeChatModel{}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, DefaultMaxReplyChars).
		WithRetrieval(&fakeChatRetriever{
			result: RetrievalResult{
				RequestID:    "00000000-0000-4000-8000-000000000099",
				Status:       RetrievalStatusFailed,
				ContextBlock: retrievalFailedNotice,
			},
			err: errors.New("database unavailable"),
		})
	var callbackResult RetrievalResult

	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID:  "usr-owner",
		History: []ConversationMessage{{Seq: 1, Role: "user", Content: "outbox"}},
		OnRetrieval: func(result RetrievalResult) error {
			callbackResult = result
			return nil
		},
		OnDelta: func(string) error { return nil },
	})
	if err != nil || content != "reply" {
		t.Fatalf("Stream() content=%q err=%v, want degraded reply/nil", content, err)
	}
	if callbackResult.RequestID != "00000000-0000-4000-8000-000000000099" || callbackResult.Status != RetrievalStatusFailed {
		t.Fatalf("callback result = %+v, want failed result with original audit id", callbackResult)
	}
	if len(model.messages) == 0 || !strings.Contains(model.messages[0].Content, "本轮资料检索未能执行") {
		t.Fatalf("system prompt did not contain explicit retrieval failure notice: %+v", model.messages)
	}
}

// sequencedChatModel 模拟底层模型逐段回调 onDelta ，只要 onDelta 返回 error 就立即停止读取并把该 error 传回去，
// 这与 llm.DeepSeekClient.Stream 的真实行为保持一致。
type sequencedChatModel struct {
	deltas    []string
	streamErr error // 所有 delta 都正常发完后再返回的错误，模拟真实的上游调用失败
}

func (m *sequencedChatModel) Timeout() time.Duration { return time.Second }
func (m *sequencedChatModel) ModelName() string      { return "sequenced-model" }

func (m *sequencedChatModel) Stream(_ context.Context, _ []llm.Message, onDelta func(string) error) error {
	for _, delta := range m.deltas {
		if err := onDelta(delta); err != nil {
			return err
		}
	}
	return m.streamErr
}

func TestChatServiceStreamTruncatesLongReplyWithoutTreatingItAsFailure(t *testing.T) {
	model := &sequencedChatModel{deltas: []string{"12345", "67890", "abcde"}}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, 5) // 上限 5 个字符，第一段刚好用完

	var forwarded []string
	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID: "usr-test",
		OnDelta: func(delta string) error {
			forwarded = append(forwarded, delta)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil：截断不算调用失败，客户端已经看到的内容不该被当成一次错误", err)
	}
	if !strings.HasPrefix(content, "12345") {
		t.Fatalf("content = %q, want to start with the first chunk that filled the cap", content)
	}
	if !strings.Contains(content, "已截断") {
		t.Fatalf("content = %q, want a truncation notice appended", content)
	}
	if len(forwarded) != 2 || forwarded[0] != "12345" || forwarded[1] != truncationNotice {
		t.Fatalf("forwarded = %v, want exactly [12345, truncationNotice]；超出上限后的分片不应再转发给客户端", forwarded)
	}
}

func TestChatServiceStreamCountsUnicodeCharactersAndTrimsOversizedDelta(t *testing.T) {
	model := &sequencedChatModel{deltas: []string{"你好世界"}}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, 2)

	var forwarded []string
	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID: "usr-test",
		OnDelta: func(delta string) error {
			forwarded = append(forwarded, delta)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if !strings.HasPrefix(content, "你好") || strings.Contains(content, "世界") {
		t.Fatalf("content = %q, want exactly two Chinese characters before truncation notice", content)
	}
	if len(forwarded) != 2 || forwarded[0] != "你好" || forwarded[1] != truncationNotice {
		t.Fatalf("forwarded = %q, want trimmed delta followed by truncation notice", forwarded)
	}
}

func TestChatServiceStreamPropagatesRealModelErrorWithPartialContent(t *testing.T) {
	wantErr := errors.New("upstream failed")
	model := &sequencedChatModel{deltas: []string{"partial"}, streamErr: wantErr}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, DefaultMaxReplyChars)

	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID:  "usr-test",
		OnDelta: func(string) error { return nil },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Stream() error = %v, want %v", err, wantErr)
	}
	if content != "partial" {
		t.Fatalf("content = %q, want partial content preserved even when the model call ultimately fails", content)
	}
}

func TestNewChatServiceAppliesDefaultMaxReplyCharsWhenNotPositive(t *testing.T) {
	model := &sequencedChatModel{deltas: []string{strings.Repeat("a", DefaultMaxReplyChars)}}
	chatService := NewChatService(model, testChatPrompt(t), nil, MemoryBudget{}, 0)

	content, err := chatService.Stream(context.Background(), ChatStreamRequest{
		UserID:  "usr-test",
		OnDelta: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if strings.Contains(content, "已截断") {
		t.Fatalf("content 被截断，want maxReplyChars <= 0 时回退为 DefaultMaxReplyChars 且刚好不截断: %q", content)
	}
}
