package service

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"
)

const noConfirmedUserFacts = "暂无已确认用户事实。不得根据对话自行补全或推断。"

// ChatPromptData 是面试训练 Prompt 允许注入的变量。
type ChatPromptData struct {
	Version         string
	TrustBoundary   string
	UserFactSummary string
	// RetrievedContext 是已授权资料的受控数据块。
	// 它永远由服务端渲染（命中/未命中/未启用/检索故障都有固定文案），
	// 不允许为空串——空串会让模型自行脑补“我应该有资料”。
	RetrievedContext string
}

// ChatPrompt 是启动时完成解析、请求时只负责渲染的版本化 Prompt。
type ChatPrompt struct {
	version       string
	trustBoundary string
	template      *template.Template
}

// LoadChatPrompt 在启动时读取并校验模板，避免把缺失或损坏的 Prompt 留到请求阶段才暴露。
func LoadChatPrompt(path, version, trustBoundary string) (*ChatPrompt, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("prompt version 不能为空")
	}
	if strings.TrimSpace(trustBoundary) == "" {
		return nil, fmt.Errorf("prompt trust boundary 不能为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 chat prompt 模板失败: %w", err)
	}
	prompt, err := ParseChatPrompt(string(raw), version, trustBoundary)
	if err != nil {
		return nil, err
	}
	if err := validateRequiredPromptVariables(string(raw)); err != nil {
		return nil, err
	}
	return prompt, nil
}

func validateRequiredPromptVariables(templateText string) error {
	for _, variable := range []string{".Version", ".TrustBoundary", ".UserFactSummary", ".RetrievedContext"} {
		if !strings.Contains(templateText, "{{"+variable+"}}") {
			return fmt.Errorf("chat prompt 模板缺少必需变量 {{%s}}", variable)
		}
	}
	return nil
}

// ParseChatPrompt 解析模板内容，供测试和内嵌模板场景复用同一套校验。
func ParseChatPrompt(templateText, version, trustBoundary string) (*ChatPrompt, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("prompt version 不能为空")
	}
	if strings.TrimSpace(trustBoundary) == "" {
		return nil, fmt.Errorf("prompt trust boundary 不能为空")
	}
	parsed, err := template.New("interview_chat").Option("missingkey=error").Parse(templateText)
	if err != nil {
		return nil, fmt.Errorf("解析 chat prompt 模板失败: %w", err)
	}
	prompt := &ChatPrompt{
		version:       strings.TrimSpace(version),
		trustBoundary: strings.TrimSpace(trustBoundary),
		template:      parsed,
	}
	if _, err := prompt.Render("", ""); err != nil {
		return nil, fmt.Errorf("校验 chat prompt 模板失败: %w", err)
	}
	return prompt, nil
}

func (p *ChatPrompt) Version() string {
	return p.version
}

// Render 只接收服务端确认过的用户事实摘要与已授权资料数据块；空值使用明确占位，避免模型自行猜测。
func (p *ChatPrompt) Render(userFactSummary, retrievedContext string) (string, error) {
	if p == nil || p.template == nil {
		return "", fmt.Errorf("chat prompt 未初始化")
	}
	userFactSummary = strings.TrimSpace(userFactSummary)
	if userFactSummary == "" {
		userFactSummary = noConfirmedUserFacts
	}
	retrievedContext = strings.TrimSpace(retrievedContext)
	if retrievedContext == "" {
		retrievedContext = retrievalDisabledNotice
	}
	var rendered bytes.Buffer
	if err := p.template.Execute(&rendered, ChatPromptData{
		Version:          p.version,
		TrustBoundary:    p.trustBoundary,
		UserFactSummary:  userFactSummary,
		RetrievedContext: retrievedContext,
	}); err != nil {
		return "", fmt.Errorf("渲染 chat prompt 模板失败: %w", err)
	}
	return rendered.String(), nil
}
