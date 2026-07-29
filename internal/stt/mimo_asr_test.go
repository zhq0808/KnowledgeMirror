package stt

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

func TestMiMoAudioMIME(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantMIME string
		wantOK   bool
	}{
		{"wav", "audio/wav", "audio/wav", true},
		{"wav 带参数", "audio/wav; codecs=1", "audio/wav", true},
		{"wav 变体", "AUDIO/X-WAV", "audio/wav", true},
		{"mp3", "audio/mp3", "audio/mpeg", true},
		{"mpeg", "audio/mpeg", "audio/mpeg", true},
		// 浏览器 MediaRecorder 的默认格式，MiMo 不接受，必须在录音端就转成 wav。
		{"webm 被拒", "audio/webm;codecs=opus", "", false},
		{"ogg 被拒", "audio/ogg;codecs=opus", "", false},
		{"空值被拒", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMIME, gotOK := mimoAudioMIME(tc.input)
			if gotOK != tc.wantOK || gotMIME != tc.wantMIME {
				t.Fatalf("mimoAudioMIME(%q) = (%q, %v)，期望 (%q, %v)",
					tc.input, gotMIME, gotOK, tc.wantMIME, tc.wantOK)
			}
		})
	}
}

func TestMiMoASRProviderRejectsUnsupportedFormat(t *testing.T) {
	provider := NewMiMoASRProvider("test-key", "https://example.invalid/v1", "", "zh", time.Second)

	_, err := provider.Transcribe(context.Background(), []byte("fake audio"), "audio/webm;codecs=opus")
	if !errors.Is(err, ErrUnsupportedAudioFormat) {
		t.Fatalf("期望 ErrUnsupportedAudioFormat，实际 %v", err)
	}
}

func TestMiMoASRProviderRequiresAPIKey(t *testing.T) {
	provider := NewMiMoASRProvider("", "https://example.invalid/v1", "", "zh", time.Second)

	if _, err := provider.Transcribe(context.Background(), []byte("x"), "audio/wav"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("期望 ErrNotConfigured，实际 %v", err)
	}
}

func TestMiMoASRProviderTranscribe(t *testing.T) {
	audio := []byte("pretend this is a wav file")

	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("x-request-id", "req-123")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"  Kafka 的 offset 是消费位点。  "}}]}`))
	}))
	defer server.Close()

	provider := NewMiMoASRProvider("test-key", server.URL+"/v1", "", "zh", 5*time.Second)
	transcript, err := provider.Transcribe(context.Background(), audio, "audio/wav")
	if err != nil {
		t.Fatalf("Transcribe 失败: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("请求路径 = %q，期望 /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotBody["model"] != MiMoASRDefaultModel {
		t.Fatalf("model = %v，期望 %s", gotBody["model"], MiMoASRDefaultModel)
	}

	// asr_options 必须是顶层字段而不是嵌在 messages 里，否则语种指定会被静默忽略。
	options, ok := gotBody["asr_options"].(map[string]any)
	if !ok || options["language"] != "zh" {
		t.Fatalf("asr_options = %v，期望 {language: zh}", gotBody["asr_options"])
	}

	// 音频必须以 Data URL 形式内联在 user 消息的 input_audio 里。
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %v，期望 1 条", gotBody["messages"])
	}
	message := messages[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("role = %v，期望 user", message["role"])
	}
	parts := message["content"].([]any)
	part := parts[0].(map[string]any)
	if part["type"] != "input_audio" {
		t.Fatalf("content type = %v，期望 input_audio", part["type"])
	}
	dataURL := part["input_audio"].(map[string]any)["data"].(string)
	const wantPrefix = "data:audio/wav;base64,"
	if !strings.HasPrefix(dataURL, wantPrefix) {
		t.Fatalf("data URL 前缀不对: %q", dataURL[:min(len(dataURL), 40)])
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, wantPrefix))
	if decodeErr != nil || string(decoded) != string(audio) {
		t.Fatalf("音频往返后不一致: err=%v got=%q", decodeErr, decoded)
	}

	if transcript.Text != "Kafka 的 offset 是消费位点。" {
		t.Fatalf("转写文本未去除首尾空白: %q", transcript.Text)
	}
	if transcript.Provider != MiMoASRProviderName {
		t.Fatalf("Provider = %q", transcript.Provider)
	}
	if transcript.RequestID != "req-123" {
		t.Fatalf("RequestID = %q", transcript.RequestID)
	}
	// MiMo 不返回置信度。留 nil 才能让上层继续要求用户确认，绝不能伪造成满分。
	if transcript.Confidence != nil {
		t.Fatalf("Confidence 应为 nil，实际 %v", *transcript.Confidence)
	}
}

func TestMiMoASRProviderSurfacesErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer server.Close()

	provider := NewMiMoASRProvider("bad-key", server.URL+"/v1", "", "zh", 5*time.Second)
	_, err := provider.Transcribe(context.Background(), []byte("x"), "audio/wav")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("期望错误里带上状态码，实际 %v", err)
	}
}

func TestMiMoASRProviderRejectsEmptyChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer server.Close()

	provider := NewMiMoASRProvider("test-key", server.URL+"/v1", "", "zh", 5*time.Second)
	if _, err := provider.Transcribe(context.Background(), []byte("x"), "audio/wav"); err == nil {
		t.Fatal("空 choices 应当报错，而不是返回空转写")
	}
}
