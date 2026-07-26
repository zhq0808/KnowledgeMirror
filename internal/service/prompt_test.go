package service

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInterviewChatPromptEnforcesEvidenceBasedTraining(t *testing.T) {
	prompt, err := LoadChatPrompt(
		filepath.Join("..", "..", "prompts", "interview_chat_v1.tmpl"),
		"interview-chat-v1",
		"不得夸大用户经历或掌握状态",
	)
	if err != nil {
		t.Fatalf("LoadChatPrompt() error = %v", err)
	}

	rendered, err := prompt.Render("目标岗位：Go 后端；已确认生产经历：Kafka 消费者开发", "")
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, required := range []string{
		"主动回忆",
		"输入不等于掌握",
		"费曼学习",
		"知识点回顾",
		"模拟面试",
		"JD 分析",
		"真实生产实践、个人 Demo、独立练习和概念学习",
		"AI 结论先作为候选",
		"尚未保存",
		"不得夸大用户经历或掌握状态",
		"600 个汉字以内",
		"interview-chat-v1",
		"Kafka 消费者开发",
	} {
		if !strings.Contains(rendered, required) {
			t.Errorf("rendered prompt missing %q", required)
		}
	}
}
