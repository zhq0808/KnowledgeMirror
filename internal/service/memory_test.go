package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"KnowledgeMirror/internal/service"
)

// fakeMemoryRepository 记录服务层解析后的落库请求，用于在无数据库的情况下断言确定性校验与引用解析。
type fakeMemoryRepository struct {
	listResult   []service.Memory
	listErr      error
	listMaxCount int
	applied      *service.ApplyExtractionRequest
	applyErr     error
}

func (f *fakeMemoryRepository) ListCurrentMemories(_ context.Context, _, _ string, maxCount int) ([]service.Memory, error) {
	f.listMaxCount = maxCount
	return f.listResult, f.listErr
}

func (f *fakeMemoryRepository) ApplyExtraction(_ context.Context, request service.ApplyExtractionRequest) (service.ApplyExtractionResult, error) {
	f.applied = &request
	if f.applyErr != nil {
		return service.ApplyExtractionResult{}, f.applyErr
	}
	return service.ApplyExtractionResult{ToResultSeq: request.ToResultSeq}, nil
}

func confidencePtr(value float64) *float64 { return &value }

func baseApplyInput() service.ApplyExtractionInput {
	return service.ApplyExtractionInput{
		UserID:    "usr_memory_unit",
		SessionID: "session_unit",
		ExistingMemories: []service.ExistingMemoryRef{
			{Ref: "M1", Memory: service.Memory{MemoryID: "mem-1", UserID: "usr_memory_unit", MemoryType: "preference", MemoryValue: "用户不吃辣", Version: 2}},
		},
		BatchMessages: []service.BatchMessageRef{
			{Ref: "N1", MessageID: "msg-user-1", Role: "user", Content: "我现在可以吃一点微辣了", Seq: 5},
			{Ref: "N2", MessageID: "msg-assistant-1", Role: "assistant", Content: "知道了", Seq: 6},
		},
		ExtractorModel:   "deepseek-chat",
		ExtractorVersion: "memory-extractor-v1",
		FromResultSeq:    4,
		ToResultSeq:      6,
		LeaseToken:       "lease-token-1",
	}
}

func TestMemoryServiceApplyExtractionResolvesRefsAndFiltersLowSignalOperations(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})

	input := baseApplyInput()
	input.Operations = []service.LLMMemoryOperation{
		{
			Action:       service.MemoryActionUpdate,
			TargetRef:    "M1",
			Sources:      []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "可以吃一点微辣"}},
			MemoryType:   "preference",
			MemoryValue:  "用户可以接受少量微辣",
			Explicitness: "explicit",
			Confidence:   confidencePtr(0.93),
		},
		{
			Action:       service.MemoryActionAdd,
			Sources:      []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
			MemoryType:   "preference",
			MemoryValue:  "低置信度应被过滤",
			Explicitness: "explicit",
			Confidence:   confidencePtr(0.3),
		},
		{
			Action:       service.MemoryActionAdd,
			Sources:      []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
			MemoryType:   "preference",
			MemoryValue:  "非显式应被过滤",
			Explicitness: "implicit",
			Confidence:   confidencePtr(0.99),
		},
	}

	if _, err := svc.ApplyExtraction(context.Background(), input); err != nil {
		t.Fatalf("apply extraction: %v", err)
	}
	if repo.applied == nil {
		t.Fatal("expected repository ApplyExtraction to be called")
	}
	if got := len(repo.applied.Operations); got != 1 {
		t.Fatalf("resolved operations = %d, want 1 (low confidence + implicit filtered)", got)
	}
	op := repo.applied.Operations[0]
	if op.Action != service.MemoryActionUpdate || op.MemoryID != "mem-1" || op.ExpectedVersion != 2 {
		t.Fatalf("resolved op = %+v, want UPDATE mem-1 version 2", op)
	}
	if len(op.Sources) != 1 || op.Sources[0].MessageID != "msg-user-1" || op.Sources[0].SourceOrder != 1 {
		t.Fatalf("resolved sources = %+v, want single msg-user-1 order 1", op.Sources)
	}
	if op.LatestSourceMessageID != "msg-user-1" {
		t.Fatalf("latest source = %q, want msg-user-1", op.LatestSourceMessageID)
	}
	if repo.applied.AgentID != service.MemoryAgentID || repo.applied.LeaseToken != "lease-token-1" {
		t.Fatalf("apply request = %+v, want agent %s lease-token-1", repo.applied, service.MemoryAgentID)
	}
}

func TestMemoryServiceApplyExtractionPicksLatestSourceBySeq(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})

	input := baseApplyInput()
	input.BatchMessages = []service.BatchMessageRef{
		{Ref: "N1", MessageID: "msg-early", Role: "user", Content: "较早的话 微辣", Seq: 3},
		{Ref: "N2", MessageID: "msg-late", Role: "user", Content: "较晚的话 甜食", Seq: 9},
	}
	input.Operations = []service.LLMMemoryOperation{
		{
			Action:       service.MemoryActionAdd,
			Sources:      []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}, {Ref: "N2", EvidenceQuote: "甜食"}},
			MemoryType:   "preference",
			MemoryValue:  "用户口味有变化",
			Explicitness: "explicit",
			Confidence:   confidencePtr(0.8),
		},
	}

	if _, err := svc.ApplyExtraction(context.Background(), input); err != nil {
		t.Fatalf("apply extraction: %v", err)
	}
	op := repo.applied.Operations[0]
	if op.LatestSourceMessageID != "msg-late" {
		t.Fatalf("latest source = %q, want msg-late (max seq)", op.LatestSourceMessageID)
	}
	if len(op.Sources) != 2 || op.Sources[1].SourceOrder != 2 {
		t.Fatalf("resolved sources = %+v, want two ordered sources", op.Sources)
	}
}

func TestMemoryServiceApplyExtractionRejectsInvalidReferences(t *testing.T) {
	tests := []struct {
		name      string
		operation service.LLMMemoryOperation
		wantErr   error
	}{
		{
			name: "未知目标引用",
			operation: service.LLMMemoryOperation{
				Action: service.MemoryActionUpdate, TargetRef: "M9",
				Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
				MemoryType: "preference", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryInvalidReference,
		},
		{
			name: "未知来源引用",
			operation: service.LLMMemoryOperation{
				Action:     service.MemoryActionAdd,
				Sources:    []service.LLMMemorySource{{Ref: "N9", EvidenceQuote: "微辣"}},
				MemoryType: "preference", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryInvalidReference,
		},
		{
			name: "来源不是 user 消息",
			operation: service.LLMMemoryOperation{
				Action:     service.MemoryActionAdd,
				Sources:    []service.LLMMemorySource{{Ref: "N2", EvidenceQuote: "知道了"}},
				MemoryType: "preference", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemorySourceForbidden,
		},
		{
			name: "原文对不上",
			operation: service.LLMMemoryOperation{
				Action:     service.MemoryActionAdd,
				Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "我根本没说过这句话"}},
				MemoryType: "preference", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryEvidenceMismatch,
		},
		{
			name: "ADD 不允许 target_ref",
			operation: service.LLMMemoryOperation{
				Action: service.MemoryActionAdd, TargetRef: "M1",
				Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
				MemoryType: "preference", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryInvalidOperation,
		},
		{
			name: "非法 memory_type",
			operation: service.LLMMemoryOperation{
				Action:     service.MemoryActionAdd,
				Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
				MemoryType: "diet", MemoryValue: "值", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryInvalidOperation,
		},
		{
			name: "空 memory_value",
			operation: service.LLMMemoryOperation{
				Action:     service.MemoryActionAdd,
				Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
				MemoryType: "preference", MemoryValue: "   ", Explicitness: "explicit", Confidence: confidencePtr(0.9),
			},
			wantErr: service.ErrMemoryInvalidOperation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeMemoryRepository{}
			svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})
			input := baseApplyInput()
			input.Operations = []service.LLMMemoryOperation{tt.operation}
			if _, err := svc.ApplyExtraction(context.Background(), input); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if repo.applied != nil {
				t.Fatal("expected no repository call when validation fails (整批拒绝)")
			}
		})
	}
}

func TestMemoryServiceApplyExtractionRejectsDuplicateTarget(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})
	input := baseApplyInput()
	input.Operations = []service.LLMMemoryOperation{
		{
			Action: service.MemoryActionUpdate, TargetRef: "M1",
			Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
			MemoryType: "preference", MemoryValue: "第一次改", Explicitness: "explicit", Confidence: confidencePtr(0.9),
		},
		{
			Action: service.MemoryActionDelete, TargetRef: "M1",
			Sources:    []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}},
			MemoryType: "preference", Explicitness: "explicit", Confidence: confidencePtr(0.9),
		},
	}
	if _, err := svc.ApplyExtraction(context.Background(), input); !errors.Is(err, service.ErrMemoryInvalidOperation) {
		t.Fatalf("err = %v, want ErrMemoryInvalidOperation", err)
	}
}

func TestMemoryServiceApplyExtractionRejectsOversizedBatch(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{MaxOperations: 1})
	input := baseApplyInput()
	input.Operations = []service.LLMMemoryOperation{
		{Action: service.MemoryActionAdd, Sources: []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}}, MemoryType: "preference", MemoryValue: "a", Explicitness: "explicit", Confidence: confidencePtr(0.9)},
		{Action: service.MemoryActionAdd, Sources: []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}}, MemoryType: "preference", MemoryValue: "b", Explicitness: "explicit", Confidence: confidencePtr(0.9)},
	}
	if _, err := svc.ApplyExtraction(context.Background(), input); !errors.Is(err, service.ErrMemoryInvalidOperation) {
		t.Fatalf("err = %v, want ErrMemoryInvalidOperation", err)
	}
}

func TestMemoryServiceApplyExtractionAdvancesCursorWhenAllFiltered(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})
	input := baseApplyInput()
	input.Operations = []service.LLMMemoryOperation{
		{Action: service.MemoryActionAdd, Sources: []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: "微辣"}}, MemoryType: "preference", MemoryValue: "被过滤", Explicitness: "implicit", Confidence: confidencePtr(0.99)},
	}
	if _, err := svc.ApplyExtraction(context.Background(), input); err != nil {
		t.Fatalf("apply extraction: %v", err)
	}
	if repo.applied == nil || len(repo.applied.Operations) != 0 {
		t.Fatal("expected repository call with zero operations to still advance cursor")
	}
	if repo.applied.ToResultSeq != input.ToResultSeq {
		t.Fatalf("to result seq = %d, want %d", repo.applied.ToResultSeq, input.ToResultSeq)
	}
}

func TestParseExtractionOperationsRejectsForgedIDFields(t *testing.T) {
	valid := []byte(`{"operations":[{"action":"ADD","target_ref":"","sources":[{"ref":"N1","evidence_quote":"微辣"}],"memory_type":"preference","memory_value":"值","explicitness":"explicit","confidence":0.9}]}`)
	operations, err := service.ParseExtractionOperations(valid)
	if err != nil {
		t.Fatalf("parse valid: %v", err)
	}
	if len(operations) != 1 || operations[0].Action != service.MemoryActionAdd {
		t.Fatalf("operations = %+v, want single ADD", operations)
	}

	forged := []byte(`{"operations":[{"action":"UPDATE","memory_id":"mem-1","user_id":"attacker","version":9,"sources":[],"memory_type":"preference","memory_value":"值"}]}`)
	if _, err := service.ParseExtractionOperations(forged); !errors.Is(err, service.ErrMemoryInvalidOperation) {
		t.Fatalf("forged parse err = %v, want ErrMemoryInvalidOperation", err)
	}
}

func TestParseExtractionOperationsRejectsMissingRequiredFieldsAndTrailingJSON(t *testing.T) {
	tests := []string{
		`{}`,
		`{"operations":null}`,
		`{"operations":[{"action":"ADD"}]}`,
		`{"operations":[]} {"operations":[]}`,
	}
	for _, raw := range tests {
		if _, err := service.ParseExtractionOperations([]byte(raw)); !errors.Is(err, service.ErrMemoryInvalidOperation) {
			t.Fatalf("raw=%s err=%v, want ErrMemoryInvalidOperation", raw, err)
		}
	}
}

func TestMemoryServiceRejectsMalformedOperationBeforeFiltering(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})
	input := baseApplyInput()
	input.Operations = []service.LLMMemoryOperation{{Action: "ADDD"}}

	if _, err := svc.ApplyExtraction(context.Background(), input); !errors.Is(err, service.ErrMemoryInvalidOperation) {
		t.Fatalf("err=%v, want ErrMemoryInvalidOperation", err)
	}
	if repo.applied != nil {
		t.Fatal("malformed operation must not advance the cursor")
	}
}

func TestMemoryServiceRejectsOversizedEvidenceQuote(t *testing.T) {
	repo := &fakeMemoryRepository{}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})
	input := baseApplyInput()
	quote := strings.Repeat("微", 1001)
	input.BatchMessages[0].Content = quote
	input.Operations = []service.LLMMemoryOperation{{
		Action: service.MemoryActionAdd, TargetRef: "", MemoryType: "preference", MemoryValue: "值",
		Explicitness: "explicit", Confidence: confidencePtr(0.9),
		Sources: []service.LLMMemorySource{{Ref: "N1", EvidenceQuote: quote}},
	}}

	if _, err := svc.ApplyExtraction(context.Background(), input); !errors.Is(err, service.ErrMemoryInvalidOperation) {
		t.Fatalf("err=%v, want ErrMemoryInvalidOperation", err)
	}
}

func TestMemoryServiceListCurrentMemoriesAppliesCharBudget(t *testing.T) {
	repo := &fakeMemoryRepository{
		listResult: []service.Memory{
			{MemoryID: "m1", MemoryValue: "0123456789"},
			{MemoryID: "m2", MemoryValue: "0123456789"},
			{MemoryID: "m3", MemoryValue: "0123456789"},
		},
	}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})

	memories, err := svc.ListCurrentMemories(context.Background(), "usr", service.MemoryBudget{MaxCount: 50, MaxChars: 25})
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("kept %d memories, want 2 within 25 chars", len(memories))
	}
	if repo.listMaxCount != 50 {
		t.Fatalf("max count passed = %d, want 50", repo.listMaxCount)
	}
}

func TestMemoryServiceListCurrentMemoriesKeepsAtLeastOneOverBudget(t *testing.T) {
	repo := &fakeMemoryRepository{
		listResult: []service.Memory{{MemoryID: "m1", MemoryValue: "01234567890123456789"}},
	}
	svc := service.NewMemoryService(repo, service.MemoryExtractionLimits{})

	memories, err := svc.ListCurrentMemories(context.Background(), "usr", service.MemoryBudget{MaxChars: 5})
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("kept %d memories, want first kept even if over budget", len(memories))
	}
}
