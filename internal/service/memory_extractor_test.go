package service

import (
	"context"
	"strings"
	"testing"

	"KnowledgeMirror/internal/llm"
)

type memoryExtractionModelStub struct {
	messages []llm.Message
	output   string
	err      error
}

func (s *memoryExtractionModelStub) Stream(_ context.Context, messages []llm.Message, onDelta func(string) error) error {
	s.messages = messages
	if s.err != nil {
		return s.err
	}
	return onDelta(s.output)
}

func TestLLMMemoryExtractorUsesTemporaryRefsAndParsesOperations(t *testing.T) {
	model := &memoryExtractionModelStub{output: `{"operations":[{"action":"ADD","target_ref":"","sources":[{"ref":"N1","evidence_quote":"目标是 Go 后端"}],"memory_type":"goal","memory_value":"用户的目标岗位是 Go 后端","explicitness":"explicit","confidence":0.9}]}`}
	extractor, err := LoadLLMMemoryExtractor("../../prompts/memory_extractor_v1.tmpl", "memory-extractor-v1", model)
	if err != nil {
		t.Fatalf("load extractor: %v", err)
	}

	operations, err := extractor.Extract(context.Background(), ExtractionInput{
		ExistingMemories: []ExistingMemoryRef{{Ref: "M1", Memory: Memory{MemoryID: "secret-memory-id", MemoryType: "context", MemoryValue: "用户写过 Kafka 业务消费代码"}}},
		BatchMessages:    []BatchMessageRef{{Ref: "N1", MessageID: "secret-message-id", Role: "user", Content: "我的目标是 Go 后端"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(operations) != 1 || operations[0].Action != MemoryActionAdd {
		t.Fatalf("operations = %+v, want one ADD", operations)
	}
	if len(model.messages) != 2 {
		t.Fatalf("messages = %d, want system + user", len(model.messages))
	}
	input := model.messages[1].Content
	if strings.Contains(input, "secret-memory-id") || strings.Contains(input, "secret-message-id") {
		t.Fatalf("model input leaked real IDs: %s", input)
	}
	for _, ref := range []string{"M1", "N1", "用户写过 Kafka 业务消费代码", "我的目标是 Go 后端"} {
		if !strings.Contains(input, ref) {
			t.Errorf("model input missing %q: %s", ref, input)
		}
	}
}

func TestLoadLLMMemoryExtractorRejectsTemplateWithoutVersion(t *testing.T) {
	if _, err := LoadLLMMemoryExtractor("../../prompts/interview_chat_v1.tmpl", "memory-extractor-v1", &memoryExtractionModelStub{}); err == nil {
		t.Fatal("expected incompatible prompt template to be rejected")
	}
}
