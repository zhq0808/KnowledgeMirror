package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// OpenAIWhisperProviderName 是 OpenAI 兼容 Whisper 转写供应商的标识。
// “OpenAI 兼容”指很多供应商（含国内中转服务）都实现了同一套
// multipart/form-data `/audio/transcriptions` 协议，因此 BaseURL 可配置为任意兼容端点。
const OpenAIWhisperProviderName = "openai_whisper"

const maxWhisperResponseBytes = 1 << 20

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
	// verbose_json 多返回 segments（含 avg_logprob），是算置信度的唯一依据。
	// 很多 OpenAI 兼容中转端点会忽略这个参数只回 {"text":...}，
	// 那种情况下解析仍然成立，只是拿不到置信度（返回 nil）。
	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxWhisperResponseBytes+1))
	if err != nil {
		return Transcript{}, fmt.Errorf("读取 STT 响应失败: %w", err)
	}
	if len(respBody) > maxWhisperResponseBytes {
		return Transcript{}, fmt.Errorf("STT 响应超过 %d 字节上限", maxWhisperResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return Transcript{}, fmt.Errorf("STT 供应商返回错误状态 %d: %s", resp.StatusCode, truncateForError(respBody))
	}

	var parsed struct {
		Text     string `json:"text"`
		Segments []struct {
			Start      float64 `json:"start"`
			End        float64 `json:"end"`
			AvgLogprob float64 `json:"avg_logprob"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Transcript{}, fmt.Errorf("解析 STT 响应失败: %w", err)
	}
	requestID := resp.Header.Get("x-request-id")

	segments := make([]Segment, 0, len(parsed.Segments))
	for _, s := range parsed.Segments {
		segments = append(segments, Segment{Start: s.Start, End: s.End, AvgLogprob: s.AvgLogprob})
	}

	return Transcript{
		Text:       strings.TrimSpace(parsed.Text),
		Provider:   OpenAIWhisperProviderName,
		Model:      p.model,
		RequestID:  requestID,
		Confidence: SegmentConfidence(segments),
	}, nil
}

// Segment 是 Whisper verbose_json 返回的一段转写区间。
type Segment struct {
	Start      float64
	End        float64
	AvgLogprob float64 // 该区间内 token 对数概率均值，≤ 0，越接近 0 越确定
}

// SegmentConfidence 把 Whisper 的 avg_logprob 折算成 0-1 的整体置信度。
//
// 按区间时长加权而不是简单平均：一段长讲解里夹杂的“嗯”“那个”会被切成很多
// 极短且低分的片段，简单平均会被它们拉得很难看，导致明明听清楚了却反复要求用户确认。
// 无 segments（供应商不支持 verbose_json）时返回 nil，由业务层按“没有把握”处理。
func SegmentConfidence(segments []Segment) *float64 {
	if len(segments) == 0 {
		return nil
	}
	var weightedSum, totalWeight float64
	for _, segment := range segments {
		weight := segment.End - segment.Start
		if weight <= 0 {
			weight = 1 // 时间戳异常时退化为等权，不丢弃这段
		}
		weightedSum += segment.AvgLogprob * weight
		totalWeight += weight
	}
	if totalWeight <= 0 {
		return nil
	}
	confidence := math.Exp(weightedSum / totalWeight)
	if math.IsNaN(confidence) {
		return nil
	}
	confidence = math.Max(0, math.Min(1, confidence))
	return &confidence
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
