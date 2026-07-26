package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// OpenAIWhisperProviderName 是 OpenAI 兼容 Whisper 转写供应商的标识。
// “OpenAI 兼容”指很多供应商（含国内中转服务）都实现了同一套
// multipart/form-data `/audio/transcriptions` 协议，因此 BaseURL 可配置为任意兼容端点。
const OpenAIWhisperProviderName = "openai_whisper"

// ErrNotConfigured 表示未配置 API Key，调用方应改用 LocalPlaceholderProvider。
var ErrNotConfigured = errors.New("STT 供应商未配置 API Key")

// OpenAIWhisperProvider 通过 OpenAI 兼容的 `/audio/transcriptions` 接口完成真实语音转文字。
//
// 并发安全：底层 http.Client 可被多个 goroutine 共用，自身无可变状态。
type OpenAIWhisperProvider struct {
	apiKey  string
	baseURL string
	model   string
	timeout time.Duration
	http    *http.Client
}

// NewOpenAIWhisperProvider 构造 Whisper 转写客户端。timeout 为单次请求超时上限。
func NewOpenAIWhisperProvider(apiKey, baseURL, model string, timeout time.Duration) *OpenAIWhisperProvider {
	return &OpenAIWhisperProvider{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		timeout: timeout,
		http:    &http.Client{Timeout: timeout},
	}
}

func (p *OpenAIWhisperProvider) Name() string { return OpenAIWhisperProviderName }

// whisperFilename 只影响供应商侧按扩展名猜测编码，不作为业务文件名使用。
func whisperFilename(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "webm"):
		return "audio.webm"
	case strings.Contains(mimeType, "ogg"):
		return "audio.ogg"
	case strings.Contains(mimeType, "mp4"):
		return "audio.mp4"
	case strings.Contains(mimeType, "wav"):
		return "audio.wav"
	case strings.Contains(mimeType, "mpeg"), strings.Contains(mimeType, "mp3"):
		return "audio.mp3"
	default:
		return "audio.bin"
	}
}

func (p *OpenAIWhisperProvider) Transcribe(ctx context.Context, audio []byte, mimeType string) (Transcript, error) {
	if p == nil || p.apiKey == "" {
		return Transcript{}, ErrNotConfigured
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	filePart, err := writer.CreateFormFile("file", whisperFilename(mimeType))
	if err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}
	if _, err := filePart.Write(audio); err != nil {
		return Transcript{}, fmt.Errorf("写入音频数据失败: %w", err)
	}
	if err := writer.WriteField("model", p.model); err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return Transcript{}, fmt.Errorf("调用 STT 供应商失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Transcript{}, fmt.Errorf("读取 STT 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Transcript{}, fmt.Errorf("STT 供应商返回错误状态 %d: %s", resp.StatusCode, truncateForError(respBody))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Transcript{}, fmt.Errorf("解析 STT 响应失败: %w", err)
	}
	requestID := resp.Header.Get("x-request-id")

	return Transcript{
		Text:      strings.TrimSpace(parsed.Text),
		Provider:  OpenAIWhisperProviderName,
		Model:     p.model,
		RequestID: requestID,
	}, nil
}

// truncateForError 避免把过长的供应商错误响应整体塞进错误信息和日志。
func truncateForError(body []byte) string {
	const maxErrorBodyChars = 300
	text := string(body)
	if len(text) > maxErrorBodyChars {
		return text[:maxErrorBodyChars] + "..."
	}
	return text
}
