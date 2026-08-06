package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"KnowledgeMirror/internal/llm"
)

func TestLoadDatasetHasVersionedMultiTaskCases(t *testing.T) {
	dataset, err := loadDataset(filepath.Join("..", "..", "prompts", "regression", "cases.json"))
	if err != nil {
		t.Fatalf("loadDataset() error = %v", err)
	}
	if dataset.SchemaVersion != datasetSchemaVersion || dataset.DatasetVersion == "" {
		t.Fatalf("dataset versions = %q/%q", dataset.SchemaVersion, dataset.DatasetVersion)
	}
	if len(dataset.Cases) < 30 {
		t.Fatalf("case count = %d, want at least 30", len(dataset.Cases))
	}
	wantTaskTypes := map[string]bool{
		"document_classification": false,
		"candidate_extraction":    false,
		"learning_chat":           false,
		"feynman_evaluation":      false,
		"planning_review":         false,
		"failure_response":        false,
	}
	for _, testCase := range dataset.Cases {
		if _, ok := wantTaskTypes[testCase.TaskType]; ok {
			wantTaskTypes[testCase.TaskType] = true
		}
	}
	for taskType, found := range wantTaskTypes {
		if !found {
			t.Errorf("dataset missing task type %q", taskType)
		}
	}
}

func TestRegressionCasesRepresentProductPositioning(t *testing.T) {
	dataset, err := loadDataset(filepath.Join("..", "..", "prompts", "regression", "cases.json"))
	if err != nil {
		t.Fatalf("loadDataset() error = %v", err)
	}

	var corpus strings.Builder
	for _, testCase := range dataset.Cases {
		corpus.WriteString(testCase.Input)
		corpus.WriteString(testCase.Expected)
		corpus.WriteString(testCase.Context.UserFactSummary)
		for _, message := range testCase.Context.Messages {
			corpus.WriteString(message.Content)
		}
	}
	for _, productAnchor := range []string{"费曼", "主动输出", "生产", "Demo", "模拟面试", "JD", "证据"} {
		if !strings.Contains(corpus.String(), productAnchor) {
			t.Errorf("regression cases missing product anchor %q", productAnchor)
		}
	}
}

func TestEvaluateReportsAllFailedExpectations(t *testing.T) {
	failures := evaluate("可以直接加倍", assertions{
		MustContainAny:  [][]string{{"不要自行", "不能自行"}},
		MustNotContain:  []string{"直接加倍"},
		MustAskQuestion: true,
		MaxRunes:        4,
	})
	if len(failures) != 4 {
		t.Fatalf("failures = %v, want 4 independent failures", failures)
	}
}

func TestEvaluateAcceptsExplicitInformationRequestAsQuestion(t *testing.T) {
	failures := evaluate("请告诉我这段项目经历最值得追问哪一点。", assertions{MustAskQuestion: true})
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want explicit information request to count as a question", failures)
	}
}

func TestLoadDatasetRejectsDuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duplicate.json")
	raw := `{"schema_version":"evaluation-dataset/v1","dataset_version":"v1","name":"test","cases":[{"id":"same","task_type":"chat","category":"a","input":"a","expected":"a","assertions":{}},{"id":"same","task_type":"chat","category":"b","input":"b","expected":"b","assertions":{}}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDataset(path); err == nil || !strings.Contains(err.Error(), "id 重复") {
		t.Fatalf("loadDataset() error = %v, want duplicate ID error", err)
	}
}

func TestFilterCasesSelectsConfiguredTaskTypes(t *testing.T) {
	cases := []regressionCase{{ID: "a", TaskType: "chat"}, {ID: "b", TaskType: "extract"}, {ID: "c", TaskType: "chat"}}
	filtered, err := filterCases(cases, []string{"extract"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != "b" {
		t.Fatalf("filtered = %+v, want case b", filtered)
	}
	if _, err := filterCases(cases, []string{"missing"}); err == nil {
		t.Fatal("filterCases() expected unknown task type error")
	}
}

func TestCheckedInEvaluationConfigIsRunnable(t *testing.T) {
	config, err := loadEvaluationConfig(filepath.Join("..", "..", "prompts", "regression", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	if len(config.Targets) != 2 || config.Targets[0].Name != "baseline" || config.Targets[1].Name != "candidate" {
		t.Fatalf("targets = %+v", config.Targets)
	}
	temperature, err := parameterFloat(config.Targets[0].Parameters, "temperature", 1)
	if err != nil || temperature != 0 {
		t.Fatalf("temperature = %v, error = %v", temperature, err)
	}
}

func TestRegressionCasesCoverProductBoundariesAndFailures(t *testing.T) {
	dataset, err := loadDataset(filepath.Join("..", "..", "prompts", "regression", "cases.json"))
	if err != nil {
		t.Fatalf("loadDataset() error = %v", err)
	}
	var corpus strings.Builder
	for _, testCase := range dataset.Cases {
		corpus.WriteString(testCase.Category)
		corpus.WriteString(testCase.Input)
		corpus.WriteString(testCase.Expected)
	}
	for _, anchor := range []string{"资料来源", "知识点候选", "JD 要求", "个人事实", "来源片段", "供 AI 检索", "掌握状态", "生产事实", "费曼", "每日复盘", "提示词", "不可信资料", "AI 幻觉", "过期资料", "解析失败", "LLM 超时", "工具失败", "STT", "数据库失败"} {
		if !strings.Contains(corpus.String(), anchor) {
			t.Errorf("regression cases missing anchor %q", anchor)
		}
	}
}

func TestEvaluateAcceptsDeterministicRules(t *testing.T) {
	output := `{"status":"candidate","question":"请确认？"}`
	failures := evaluate(output, assertions{
		MustContainAny:  [][]string{{"candidate", "pending"}},
		MustContainAll:  []string{"status"},
		MustNotContain:  []string{"verified"},
		MustAskQuestion: true,
		ValidJSON:       true,
		MaxRunes:        100,
	})
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
}

func TestExecuteTargetRecordsUnifiedRunMetadata(t *testing.T) {
	client := &fakeCompletionClient{
		model:      "model-v1",
		completion: llm.Completion{Content: "candidate output", Usage: llm.TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}},
	}
	target := targetConfig{
		Name: "candidate", Parameters: map[string]any{"temperature": float64(0)},
		Prompts: map[string]promptConfig{"default": {Path: filepath.Join("..", "..", "prompts", "regression", "knowledge_tasks_v1.tmpl"), Version: "prompt-v1"}},
	}
	run, err := executeTarget(context.Background(), target, "dataset-v1", []regressionCase{{
		ID: "case-1", TaskType: "learning_chat", Category: "test", Input: "input", Expected: "expected",
		Assertions: assertions{MustContainAll: []string{"candidate"}},
	}}, client, nil, "trust")
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "model-v1" || run.DatasetVersion != "dataset-v1" || run.PromptVersions["learning_chat"] != "prompt-v1" {
		t.Fatalf("run metadata = %+v", run)
	}
	if run.Tokens.TotalTokens != 13 || run.Results[0].RawOutput != "candidate output" || run.Results[0].DurationMillis < 0 {
		t.Fatalf("result metadata = %+v", run.Results[0])
	}
}

func TestExecuteTargetKeepsInvocationErrorsPerCase(t *testing.T) {
	client := &fakeCompletionClient{model: "broken", err: errors.New("timeout")}
	target := targetConfig{Name: "candidate", Prompts: map[string]promptConfig{"default": {
		Path: filepath.Join("..", "..", "prompts", "regression", "knowledge_tasks_v1.tmpl"), Version: "prompt-v1",
	}}}
	run, err := executeTarget(context.Background(), target, "dataset-v1", []regressionCase{{ID: "case-1", TaskType: "failure_response", Category: "test", Input: "input", Expected: "expected"}}, client, nil, "trust")
	if err != nil {
		t.Fatal(err)
	}
	if run.Errors != 1 || !strings.Contains(run.Results[0].InvocationError, "timeout") || !run.Results[0].ManualReview.Required {
		t.Fatalf("run = %+v, want one reviewable invocation error", run)
	}
}

func TestCompareRunsLocatesRegressionsAndImprovements(t *testing.T) {
	baseline := promptRun{Results: []caseResult{
		{ID: "regressed", TaskType: "chat", Category: "a", DeterministicScore: deterministicScore{Passed: true}},
		{ID: "improved", TaskType: "chat", Category: "b", DeterministicScore: deterministicScore{Passed: false, Failures: []string{"old"}}},
	}}
	candidate := promptRun{Results: []caseResult{
		{ID: "regressed", DeterministicScore: deterministicScore{Passed: false, Failures: []string{"new"}}},
		{ID: "improved", DeterministicScore: deterministicScore{Passed: true}},
	}}
	comparisons, regressions, improvements := compareRuns(baseline, candidate)
	if regressions != 1 || improvements != 1 || !comparisons[0].Regression || !comparisons[1].Improvement {
		t.Fatalf("comparisons = %+v, regressions=%d improvements=%d", comparisons, regressions, improvements)
	}
}

func TestWriteReportsCreatesJSONAndReadableMarkdown(t *testing.T) {
	tempDir := t.TempDir()
	config := reportConfig{JSONPath: filepath.Join(tempDir, "report.json"), MarkdownPath: filepath.Join(tempDir, "report.md")}
	result := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC),
		DatasetName: "test", DatasetVersion: "v1", Summary: reportSummary{Cases: 1, Regressions: 1},
		Runs: []promptRun{
			{
				Name: "baseline", Model: "model", Passed: 1,
				Results: []caseResult{
					{ID: "case-1", TaskType: "chat", PromptVersion: "p1", RawOutput: "raw", DeterministicScore: deterministicScore{Passed: true}},
				},
			},
		},
		Comparisons: []caseComparison{{ID: "case-1", TaskType: "chat", BaselineStatus: "passed", CandidateStatus: "failed", Regression: true}},
	}
	if err := writeReports(result, config); err != nil {
		t.Fatal(err)
	}
	jsonRaw, err := os.ReadFile(config.JSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded report
	if err := json.Unmarshal(jsonRaw, &decoded); err != nil {
		t.Fatalf("JSON report is invalid: %v", err)
	}
	markdown, err := os.ReadFile(config.MarkdownPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Prompt Evaluation Report", "## Comparison", "REGRESSION", "case-1", "raw"} {
		if !strings.Contains(string(markdown), want) {
			t.Errorf("Markdown report missing %q", want)
		}
	}
}

type fakeCompletionClient struct {
	model      string
	completion llm.Completion
	err        error
}

func (client *fakeCompletionClient) Complete(context.Context, []llm.Message) (llm.Completion, error) {
	return client.completion, client.err
}

func (client *fakeCompletionClient) Timeout() time.Duration { return time.Second }

func (client *fakeCompletionClient) ModelName() string { return client.model }
