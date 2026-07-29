package service

import (
	"errors"
	"testing"
)

func validFeynmanEvaluationPayload() FeynmanEvaluationPayload {
	return FeynmanEvaluationPayload{
		Summary: "能够说明核心链路，但事实边界仍需补充。",
		Dimensions: []FeynmanDimensionEvaluation{
			{Dimension: RubricDimensionFactualAccuracy, Score: 80, Feedback: "核心事实正确", OutputQuotes: []string{"消息发送成功后更新状态"}, SourceRefs: []string{"S1"}},
			{Dimension: RubricDimensionOmission, Score: 70, Feedback: "遗漏失败恢复", OutputQuotes: []string{}, SourceRefs: []string{"S1"}},
			{Dimension: RubricDimensionCausalChain, Score: 75, Feedback: "因果关系基本完整", OutputQuotes: []string{"先写本地消息"}, SourceRefs: []string{"S1"}},
			{Dimension: RubricDimensionProjectMapping, Score: 65, Feedback: "提到了项目位置", OutputQuotes: []string{"我在 demo 中验证过"}, SourceRefs: []string{}},
			{Dimension: RubricDimensionFactBoundary, Score: 90, Feedback: "边界表达清楚", OutputQuotes: []string{"不是生产实现"}, SourceRefs: []string{}},
		},
		EvidenceCandidate: FeynmanEvidenceCandidate{
			Claim:         "能解释 Outbox 的基本提交链路并区分 demo 与生产经验",
			Rationale:     "输出包含顺序、结果和事实边界",
			EvidenceScope: FeynmanEvidenceScopeLearning,
			OutputQuotes:  []string{"先写本地消息", "不是生产实现"},
			SourceRefs:    []string{"S1"},
		},
	}
}

func TestParseFeynmanEvaluationPayloadStrictJSON(t *testing.T) {
	valid := `{
		"summary":"ok",
		"insufficient_sources":true,
		"dimensions":[],
		"evidence_candidate":{"claim":"c","rationale":"r","evidence_scope":"learning","output_quotes":[],"source_refs":[]}
	}`

	if _, err := parseFeynmanEvaluationPayload([]byte("```json\n" + valid + "\n```")); err != nil {
		t.Fatalf("parse fenced valid JSON: %v", err)
	}
	if _, err := parseFeynmanEvaluationPayload([]byte(`{"summary":"ok","unexpected":true}`)); !errors.Is(err, ErrFeynmanEvaluationInvalid) {
		t.Fatalf("unknown field error = %v, want ErrFeynmanEvaluationInvalid", err)
	}
	if _, err := parseFeynmanEvaluationPayload([]byte(valid + ` {}`)); !errors.Is(err, ErrFeynmanEvaluationInvalid) {
		t.Fatalf("trailing JSON error = %v, want ErrFeynmanEvaluationInvalid", err)
	}
}

func TestValidateFeynmanEvaluationPayloadAcceptsAuditableProposal(t *testing.T) {
	transcript := "我在 demo 中验证过：先写本地消息，消息发送成功后更新状态；这不是生产实现。"
	sources := []FeynmanSourceSnapshot{{Ref: "S1", SourceChunkID: "chunk-1"}}

	if err := validateFeynmanEvaluationPayload(validFeynmanEvaluationPayload(), transcript, sources); err != nil {
		t.Fatalf("validate valid payload: %v", err)
	}
}

func TestValidateFeynmanEvaluationPayloadRejectsUnverifiableClaims(t *testing.T) {
	transcript := "我在 demo 中验证过：先写本地消息，消息发送成功后更新状态；这不是生产实现。"
	sources := []FeynmanSourceSnapshot{{Ref: "S1", SourceChunkID: "chunk-1"}}

	tests := []struct {
		name   string
		mutate func(*FeynmanEvaluationPayload)
		source []FeynmanSourceSnapshot
	}{
		{
			name: "quote not in confirmed transcript",
			mutate: func(payload *FeynmanEvaluationPayload) {
				payload.EvidenceCandidate.OutputQuotes = []string{"生产环境已经上线"}
			},
			source: sources,
		},
		{
			name: "unknown source reference",
			mutate: func(payload *FeynmanEvaluationPayload) {
				payload.Dimensions[0].SourceRefs = []string{"S99"}
			},
			source: sources,
		},
		{
			name: "missing insufficient sources marker",
			mutate: func(payload *FeynmanEvaluationPayload) {
				payload.InsufficientSources = false
				for index := range payload.Dimensions {
					payload.Dimensions[index].SourceRefs = nil
				}
				payload.EvidenceCandidate.SourceRefs = nil
			},
			source: nil,
		},
		{
			name: "duplicate rubric dimension",
			mutate: func(payload *FeynmanEvaluationPayload) {
				payload.Dimensions[1].Dimension = RubricDimensionFactualAccuracy
			},
			source: sources,
		},
		{
			name: "candidate without output quote",
			mutate: func(payload *FeynmanEvaluationPayload) {
				payload.EvidenceCandidate.OutputQuotes = nil
			},
			source: sources,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := validFeynmanEvaluationPayload()
			test.mutate(&payload)
			if err := validateFeynmanEvaluationPayload(payload, transcript, test.source); !errors.Is(err, ErrFeynmanEvaluationInvalid) {
				t.Fatalf("validation error = %v, want ErrFeynmanEvaluationInvalid", err)
			}
		})
	}
}
