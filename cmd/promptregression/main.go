package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appconfig "KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/llm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	applicationConfigPath := flag.String("config", "config.yaml", "application config path")
	evaluationConfigPath := flag.String("eval-config", "prompts/regression/config.yaml", "evaluation config path")
	datasetOverride := flag.String("dataset", "", "override dataset path")
	taskTypesOverride := flag.String("task-types", "", "override task types, comma-separated")
	jsonOutputOverride := flag.String("out-json", "", "override JSON report path")
	markdownOutputOverride := flag.String("out-md", "", "override Markdown report path")
	flag.Parse()

	applicationConfig, err := appconfig.Load(*applicationConfigPath)
	if err != nil {
		return err
	}
	if applicationConfig.DeepSeek.APIKey == "" {
		return errors.New("缺少 DEEPSEEK_API_KEY，未运行评测；不会生成伪造结果")
	}
	evaluationConfig, err := loadEvaluationConfig(*evaluationConfigPath)
	if err != nil {
		return err
	}
	applyOverrides(&evaluationConfig, *datasetOverride, *taskTypesOverride, *jsonOutputOverride, *markdownOutputOverride)
	if err := evaluationConfig.validate(); err != nil {
		return err
	}
	dataset, err := loadDataset(evaluationConfig.DatasetPath)
	if err != nil {
		return err
	}
	selectedCases, err := filterCases(dataset.Cases, evaluationConfig.TaskTypes)
	if err != nil {
		return err
	}
	clients := make(map[string]completionClient, len(evaluationConfig.Targets))
	for _, target := range evaluationConfig.Targets {
		model := target.Model
		if model == "" {
			model = applicationConfig.DeepSeek.Model
		}
		temperature, err := parameterFloat(target.Parameters, "temperature", applicationConfig.DeepSeek.Temperature)
		if err != nil {
			return fmt.Errorf("target %s: %w", target.Name, err)
		}
		clients[target.Name] = llm.NewDeepSeekClient(applicationConfig.DeepSeek.APIKey, applicationConfig.DeepSeek.BaseURL, model, temperature, time.Duration(applicationConfig.DeepSeek.TimeoutSeconds)*time.Second)
	}

	report, err := executeEvaluation(evaluationConfig, dataset, selectedCases, clients, applicationConfig)
	if err != nil {
		return err
	}
	if err := writeReports(report, evaluationConfig.Reports); err != nil {
		return err
	}
	for _, runResult := range report.Runs {
		fmt.Printf("%s (%s): %d passed, %d failed, %d errors\n", runResult.Name, runResult.Model, runResult.Passed, runResult.Failed, runResult.Errors)
	}
	fmt.Printf("comparisons: %d regressions, %d improvements\n", report.Summary.Regressions, report.Summary.Improvements)
	fmt.Printf("json report: %s\nmarkdown report: %s\n", evaluationConfig.Reports.JSONPath, evaluationConfig.Reports.MarkdownPath)
	return nil
}

func applyOverrides(config *evaluationConfig, dataset, taskTypes, jsonOutput, markdownOutput string) {
	if dataset != "" {
		config.DatasetPath = dataset
	}
	if taskTypes != "" {
		config.TaskTypes = strings.Split(taskTypes, ",")
	}
	if jsonOutput != "" {
		config.Reports.JSONPath = jsonOutput
	}
	if markdownOutput != "" {
		config.Reports.MarkdownPath = markdownOutput
	}
}
