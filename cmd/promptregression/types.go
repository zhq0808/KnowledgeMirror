package main

import (
	"context"
	"time"

	"KnowledgeMirror/internal/llm"
)

const (
	datasetSchemaVersion = "evaluation-dataset/v1"
	reportSchemaVersion  = "evaluation-report/v1"
)

type completionClient interface {
	Complete(ctx context.Context, messages []llm.Message) (llm.Completion, error)
	Timeout() time.Duration
	ModelName() string
}

type assertions struct {
	MustContainAny  [][]string `json:"must_contain_any,omitempty"`
	MustContainAll  []string   `json:"must_contain_all,omitempty"`
	MustNotContain  []string   `json:"must_not_contain,omitempty"`
	MustAskQuestion bool       `json:"must_ask_question,omitempty"`
	ValidJSON       bool       `json:"valid_json,omitempty"`
	MaxRunes        int        `json:"max_runes,omitempty"`
	ManualReview    bool       `json:"manual_review,omitempty"`
}

type caseContext struct {
	Messages        []llm.Message     `json:"messages,omitempty"`
	Variables       map[string]string `json:"variables,omitempty"`
	SourceChunks    []string          `json:"source_chunks,omitempty"`
	UserFactSummary string            `json:"user_fact_summary,omitempty"`
}

type regressionCase struct {
	ID         string      `json:"id"`
	TaskType   string      `json:"task_type"`
	Category   string      `json:"category"`
	Input      string      `json:"input"`
	Context    caseContext `json:"context"`
	Expected   string      `json:"expected"`
	Assertions assertions  `json:"assertions"`
}

type regressionDataset struct {
	SchemaVersion  string           `json:"schema_version"`
	DatasetVersion string           `json:"dataset_version"`
	Name           string           `json:"name"`
	Cases          []regressionCase `json:"cases"`
}

type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type deterministicScore struct {
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
}

type manualReview struct {
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Notes    string `json:"notes,omitempty"`
}

type judgeResult struct {
	Enabled   bool    `json:"enabled"`
	Passed    *bool   `json:"passed,omitempty"`
	Score     float64 `json:"score,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	RawOutput string  `json:"raw_output,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type caseResult struct {
	ID                 string             `json:"id"`
	TaskType           string             `json:"task_type"`
	Category           string             `json:"category"`
	Expected           string             `json:"expected"`
	PromptVersion      string             `json:"prompt_version"`
	DurationMillis     int64              `json:"duration_ms"`
	Tokens             tokenUsage         `json:"tokens"`
	RawOutput          string             `json:"raw_output"`
	InvocationError    string             `json:"invocation_error,omitempty"`
	DeterministicScore deterministicScore `json:"deterministic_score"`
	ManualReview       manualReview       `json:"manual_review"`
	LLMJudge           judgeResult        `json:"llm_judge"`
}

type promptRun struct {
	Name           string            `json:"name"`
	Model          string            `json:"model"`
	Parameters     map[string]any    `json:"parameters"`
	PromptVersions map[string]string `json:"prompt_versions"`
	DatasetVersion string            `json:"dataset_version"`
	DurationMillis int64             `json:"duration_ms"`
	Tokens         tokenUsage        `json:"tokens"`
	Passed         int               `json:"passed"`
	Failed         int               `json:"failed"`
	Errors         int               `json:"errors"`
	Results        []caseResult      `json:"results"`
}

type caseComparison struct {
	ID                string   `json:"id"`
	TaskType          string   `json:"task_type"`
	Category          string   `json:"category"`
	BaselineStatus    string   `json:"baseline_status"`
	CandidateStatus   string   `json:"candidate_status"`
	Regression        bool     `json:"regression"`
	Improvement       bool     `json:"improvement"`
	BaselineFailures  []string `json:"baseline_failures,omitempty"`
	CandidateFailures []string `json:"candidate_failures,omitempty"`
}

type reportSummary struct {
	Cases        int `json:"cases"`
	Regressions  int `json:"regressions"`
	Improvements int `json:"improvements"`
}

type report struct {
	SchemaVersion  string           `json:"schema_version"`
	GeneratedAt    time.Time        `json:"generated_at"`
	DatasetName    string           `json:"dataset_name"`
	DatasetVersion string           `json:"dataset_version"`
	DatasetPath    string           `json:"dataset_path"`
	SelectedTasks  []string         `json:"selected_task_types"`
	Runs           []promptRun      `json:"runs"`
	Comparisons    []caseComparison `json:"comparisons"`
	Summary        reportSummary    `json:"summary"`
}
