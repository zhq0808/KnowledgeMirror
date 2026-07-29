package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMiMoProviderRequiresAPIKey(t *testing.T) {
	provider := NewMiMoProvider("", "https://example.invalid/v1", "", "", time.Second)

	if _, err := provider.Synthesize(context.Background(), "你好", ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("期望 ErrNotConfigured，实际 %v", err)
	}
}

func TestMiMoProviderRejectsEmptyText(t *testing.T) {
	provider := NewMiMoProvider("test-key", "https://example.invalid/v1", "", "", time.Second)

	if _, err := provider.Synthesize(context.Background(), "   ", ""); err == nil {
		t.Fatal("空文本应当报错")
	}
}

func TestMiMoProviderSynthesize(t *testing.T) {
	wantAudio := []byte("RIFF....fake wav")

	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		encoded := base64.StdEncoding.EncodeToString(wantAudio)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","choices":[{"message":{"audio":{"data":"` + encoded + `"}}}]}`))
	}))
	defer server.Close()

	provider := NewMiMoProvider("test-key", server.URL+"/v1", "", "苏打", 5*time.Second)
	speech, err := provider.Synthesize(context.Background(), "为什么这里要用 Kafka？", "沉稳的技术面试官")
	if err != nil {
		t.Fatalf("Synthesize 失败: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("请求路径 = %q，期望 /v1/chat/completions", gotPath)
	}
	if gotBody["model"] != MiMoDefaultModel {
		t.Fatalf("model = %v", gotBody["model"])
	}
	audioOptions := gotBody["audio"].(map[string]any)
	if audioOptions["format"] != "wav" || audioOptions["voice"] != "苏打" {
		t.Fatalf("audio 参数 = %v", audioOptions)
	}

	// 这是最容易写反、且写反了会被用户当场听出来的地方：
	// 风格指令必须在 user 消息里，待念文本必须在 assistant 消息里。
	// 反过来的话，模型会把「沉稳的技术面试官」这几个字原样念出来。
	messages := gotBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages 数量 = %d，期望 2", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "沉稳的技术面试官" {
		t.Fatalf("首条消息应为承载风格指令的 user 消息，实际 %v", first)
	}
	second := messages[1].(map[string]any)
	if second["role"] != "assistant" || second["content"] != "为什么这里要用 Kafka？" {
		t.Fatalf("次条消息应为承载待念文本的 assistant 消息，实际 %v", second)
	}

	if string(speech.Audio) != string(wantAudio) {
		t.Fatalf("音频解码结果不一致: %q", speech.Audio)
	}
	if speech.MIMEType != "audio/wav" {
		t.Fatalf("MIMEType = %q", speech.MIMEType)
	}
	if speech.Voice != "苏打" || speech.Provider != MiMoProviderName {
		t.Fatalf("Speech 元信息不对: %+v", speech)
	}
	if speech.RequestID != "chatcmpl-2" {
		t.Fatalf("RequestID 应回退到响应体 id，实际 %q", speech.RequestID)
	}
}

func TestMiMoProviderOmitsEmptyStyleHint(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		encoded := base64.StdEncoding.EncodeToString([]byte("wav"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"audio":{"data":"` + encoded + `"}}}]}`))
	}))
	defer server.Close()

	provider := NewMiMoProvider("test-key", server.URL+"/v1", "", "", 5*time.Second)
	if _, err := provider.Synthesize(context.Background(), "念这句", "  "); err != nil {
		t.Fatalf("Synthesize 失败: %v", err)
	}

	// 空的风格指令不能占一条 user 消息：传空串会让模型收到一条无意义的空指令。
	messages := gotBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages 数量 = %d，期望 1", len(messages))
	}
	if messages[0].(map[string]any)["role"] != "assistant" {
		t.Fatalf("唯一一条消息应为 assistant，实际 %v", messages[0])
	}
}

func TestMiMoProviderRejectsMissingAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{}}]}`))
	}))
	defer server.Close()

	provider := NewMiMoProvider("test-key", server.URL+"/v1", "", "", 5*time.Second)
	if _, err := provider.Synthesize(context.Background(), "念这句", ""); err == nil {
		t.Fatal("响应缺少音频时应当报错")
	}
}

func TestMiMoProviderSurfacesErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()

	provider := NewMiMoProvider("test-key", server.URL+"/v1", "", "", 5*time.Second)
	_, err := provider.Synthesize(context.Background(), "念这句", "")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("期望错误里带上状态码，实际 %v", err)
	}
}
