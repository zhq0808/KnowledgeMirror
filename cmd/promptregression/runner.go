package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appconfig "KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/llm"
)

func executeEvaluation(config evaluationConfig, dataset regressionDataset, cases []regressionCase, clients map[string]completionClient, applicationConfig *appconfig.Config) (report, error) {
	result := report{
		SchemaVersion:  reportSchemaVersion,
		GeneratedAt:    time.Now().UTC(),
		DatasetName:    dataset.Name,
		DatasetVersion: dataset.DatasetVersion,
		DatasetPath:    config.DatasetPath,
		SelectedTasks:  taskTypesIn(cases),
		Runs:           make([]promptRun, 0, len(config.Targets)),
		Summary:        reportSummary{Cases: len(cases)},
	}

	var judge completionClient
	if config.Judge.Enabled {
		judgeModel := config.Judge.Model
		if judgeModel == "" {
			judgeModel = applicationConfig.DeepSeek.Model
		}
		judge = llm.NewDeepSeekClient(applicationConfig.DeepSeek.APIKey, applicationConfig.DeepSeek.BaseURL, judgeModel, config.Judge.Temperature, time.Duration(applicationConfig.DeepSeek.TimeoutSeconds)*time.Second)
	}
	for _, target := range config.Targets {
		client, ok := clients[target.Name]
		if !ok {
			return report{}, fmt.Errorf("target %s 缺少模型客户端", target.Name)
		}
		runResult, err := executeTarget(context.Background(), target, dataset.DatasetVersion, cases, client, judge, applicationConfig.Chat.TrustBoundary)
		if err != nil {
			return report{}, err
		}
		result.Runs = append(result.Runs, runResult)
	}

	result.Comparisons, result.Summary.Regressions, result.Summary.Improvements = compareRuns(findRun(result.Runs, "baseline"), findRun(result.Runs, "candidate"))
	return result, nil
}

func executeTarget(ctx context.Context, target targetConfig, datasetVersion string, cases []regressionCase, client, judge completionClient, trustBoundary string) (promptRun, error) {
	startedAt := time.Now()
	runResult := promptRun{
		Name:           target.Name,
		Model:          client.ModelName(),
		Parameters:     target.Parameters,
		PromptVersions: make(map[string]string),
		DatasetVersion: datasetVersion,
		Results:        make([]caseResult, 0, len(cases)),
	}
	prompts := make(map[string]*evaluationPrompt)
	for _, testCase := range cases {
		promptConfig := target.promptFor(testCase.TaskType)
		cacheKey := promptConfig.Path + "\x00" + promptConfig.Version
		prompt, ok := prompts[cacheKey]
		if !ok {
			loaded, err := loadEvaluationPrompt(promptConfig)
			if err != nil {
				return promptRun{}, err
			}
			prompt = loaded
			prompts[cacheKey] = prompt
		}
		runResult.PromptVersions[testCase.TaskType] = prompt.version
		caseResult := executeCase(ctx, testCase, prompt, client, judge, trustBoundary)
		runResult.Results = append(runResult.Results, caseResult)
		runResult.Tokens.add(caseResult.Tokens)
		if caseResult.InvocationError != "" {
			runResult.Errors++
		} else if caseResult.DeterministicScore.Passed {
			runResult.Passed++
		} else {
			runResult.Failed++
		}
	}
	runResult.DurationMillis = time.Since(startedAt).Milliseconds()
	return runResult, nil
}

func executeCase(ctx context.Context, testCase regressionCase, prompt *evaluationPrompt, client, judge completionClient, trustBoundary string) caseResult {
	result := caseResult{ID: testCase.ID, TaskType: testCase.TaskType, Category: testCase.Category, Expected: testCase.Expected, PromptVersion: prompt.version}
	startedAt := time.Now()
	systemMessage, err := prompt.render(testCase, trustBoundary)
	if err != nil {
		result.InvocationError = err.Error()
		result.DeterministicScore = deterministicScore{Passed: false, Failures: []string{err.Error()}}
		result.ManualReview = manualReview{Required: true, Status: "pending"}
		return result
	}
	messages := make([]llm.Message, 0, len(testCase.Context.Messages)+2)
	messages = append(messages, llm.Message{Role: "system", Content: systemMessage})
	messages = append(messages, testCase.Context.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: testCase.Input})
	caseContext, cancel := context.WithTimeout(ctx, client.Timeout())
	completion, err := client.Complete(caseContext, messages)
	cancel()
	result.DurationMillis = time.Since(startedAt).Milliseconds()
	if err != nil {
		result.InvocationError = err.Error()
		result.DeterministicScore = deterministicScore{Passed: false, Failures: []string{"模型调用失败: " + err.Error()}}
		result.ManualReview = manualReview{Required: true, Status: "pending"}
		return result
	}
	result.RawOutput = completion.Content
	result.Tokens = tokenUsage(completion.Usage)
	failures := evaluate(completion.Content, testCase.Assertions)
	result.DeterministicScore = deterministicScore{Passed: len(failures) == 0, Failures: failures}
	result.ManualReview = manualReview{Required: testCase.Assertions.ManualReview || len(failures) > 0, Status: "not_required"}
	if result.ManualReview.Required {
		result.ManualReview.Status = "pending"
	}
	if judge != nil {
		result.LLMJudge = evaluateWithJudge(ctx, judge, testCase, completion.Content)
	}
	return result
}

func evaluateWithJudge(ctx context.Context, judge completionClient, testCase regressionCase, output string) judgeResult {
	result := judgeResult{Enabled: true}
	prompt := fmt.Sprintf("你是辅助评测员，不得改变确定性规则结果。根据期望判断输出质量，只返回 JSON：{\"passed\":bool,\"score\":0到1,\"reason\":string}。\n任务：%s\n期望：%s\n输出：%s", testCase.TaskType, testCase.Expected, output)
	judgeContext, cancel := context.WithTimeout(ctx, judge.Timeout())
	completion, err := judge.Complete(judgeContext, []llm.Message{{Role: "user", Content: prompt}})
	cancel()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.RawOutput = completion.Content
	var parsed struct {
		Passed bool    `json:"passed"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(completion.Content), &parsed); err != nil {
		result.Error = "解析 Judge 输出失败: " + err.Error()
		return result
	}
	result.Passed = &parsed.Passed
	result.Score = parsed.Score
	result.Reason = parsed.Reason
	return result
}

func (usage *tokenUsage) add(other tokenUsage) {
	usage.PromptTokens += other.PromptTokens
	usage.CompletionTokens += other.CompletionTokens
	usage.TotalTokens += other.TotalTokens
}

func findRun(runs []promptRun, name string) promptRun {
	for _, run := range runs {
		if run.Name == name {
			return run
		}
	}
	return promptRun{}
}

func compareRuns(baseline, candidate promptRun) ([]caseComparison, int, int) {
	candidateByID := make(map[string]caseResult, len(candidate.Results))
	for _, result := range candidate.Results {
		candidateByID[result.ID] = result
	}
	comparisons := make([]caseComparison, 0, len(baseline.Results))
	regressions := 0
	improvements := 0
	for _, baselineResult := range baseline.Results {
		candidateResult, ok := candidateByID[baselineResult.ID]
		if !ok {
			candidateResult = caseResult{ID: baselineResult.ID, InvocationError: "candidate 缺少同题结果"}
		}
		baselineStatus := resultStatus(baselineResult)
		candidateStatus := resultStatus(candidateResult)
		comparison := caseComparison{
			ID: baselineResult.ID, TaskType: baselineResult.TaskType, Category: baselineResult.Category,
			BaselineStatus: baselineStatus, CandidateStatus: candidateStatus,
			Regression:        baselineStatus == "passed" && candidateStatus != "passed",
			Improvement:       baselineStatus != "passed" && candidateStatus == "passed",
			BaselineFailures:  baselineResult.DeterministicScore.Failures,
			CandidateFailures: candidateResult.DeterministicScore.Failures,
		}
		if comparison.Regression {
			regressions++
		}
		if comparison.Improvement {
			improvements++
		}
		comparisons = append(comparisons, comparison)
	}
	return comparisons, regressions, improvements
}

func resultStatus(result caseResult) string {
	if result.InvocationError != "" {
		return "error"
	}
	if result.DeterministicScore.Passed {
		return "passed"
	}
	return "failed"
}
