package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// MiMoASRProviderName 是小米 MiMo 语音识别供应商的标识。
const MiMoASRProviderName = "mimo_asr"

// MiMoASRDefaultModel 是当前 MiMo 唯一支持的语音识别模型。
const MiMoASRDefaultModel = "mimo-v2.5-asr"

const maxMiMoASRResponseBytes = 1 << 20

// maxMiMoASRAudioBytes 是 MiMo 侧 Base64 字符串 10 MB 上限反推出的原始音频上限。
// Base64 会把体积放大到 4/3，这里留出请求头与 JSON 包装的余量，取 7 MB。
const maxMiMoASRAudioBytes = 7 << 20

// ErrUnsupportedAudioFormat 表示音频格式不被当前供应商接受。
//
// 单独定义而不是混进通用错误：这是唯一一类“重试多少次都不会成功、必须换录音格式”的失败，
// 上层需要据此给出“请改用 wav/mp3 录音”的明确提示，而不是笼统的“转写失败，请重试”。
var ErrUnsupportedAudioFormat = errors.New("STT 供应商不接受该音频格式")

// MiMoASRProvider 通过小米 MiMo 开放平台的 ASR 接口完成语音转文字。
//
// 与 OpenAI Whisper 的差异（也是必须单独实现一个 Provider 的原因）：
//   - 走的是 chat/completions 协议而不是 multipart 的 /audio/transcriptions；
//   - 音频以 Base64 Data URL 内联在请求体里，且只接受 wav 和 mp3；
//   - 响应里没有 avg_logprob 之类的置信度信息，因此 Confidence 恒为 nil。
//
// 并发安全：底层 http.Client 可被多个 goroutine 共用，自身无可变状态。
type MiMoASRProvider struct {
	apiKey   string
	baseURL  string
	model    string
	language string
	http     *http.Client
}

// NewMiMoASRProvider 构造 MiMo 语音识别客户端。
// language 取值 auto / zh / en，留空时按 auto 处理；明确语种能提升识别准确率。
func NewMiMoASRProvider(apiKey, baseURL, model, language string, timeout time.Duration) *MiMoASRProvider {
	if model == "" {
		model = MiMoASRDefaultModel
	}
	if language == "" {
		language = "auto"
	}
	return &MiMoASRProvider{
		apiKey:   apiKey,
		baseURL:  strings.TrimRight(baseURL, "/"),
		model:    model,
		language: language,
		http:     &http.Client{Timeout: timeout},
	}
}

func (p *MiMoASRProvider) Name() string { return MiMoASRProviderName }

// mimoAudioMIME 把客户端上报的 MIME 归一成 MiMo 接受的取值。
// 第二个返回值为 false 表示该格式无法送给 MiMo，调用方必须转码而不是硬发。
func mimoAudioMIME(mimeType string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(mimeType))
	// 去掉 "audio/wav; codecs=1" 这类参数，只保留主类型。
	if idx := strings.Index(normalized, ";"); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	switch normalized {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "audio/wav", true
	case "audio/mpeg", "audio/mp3":
		return "audio/mpeg", true
	default:
		return "", false
	}
}

func (p *MiMoASRProvider) Transcribe(ctx context.Context, audio []byte, mimeType string) (Transcript, error) {
	if p == nil || p.apiKey == "" {
		return Transcript{}, ErrNotConfigured
	}
	if len(audio) == 0 {
		return Transcript{}, errors.New("音频数据为空")
	}
	if len(audio) > maxMiMoASRAudioBytes {
		return Transcript{}, fmt.Errorf("音频超过 MiMo ASR %d 字节上限", maxMiMoASRAudioBytes)
	}
	normalizedMIME, ok := mimoAudioMIME(mimeType)
	if !ok {
		return Transcript{}, fmt.Errorf("%w: %q（MiMo ASR 仅支持 wav 与 mp3）", ErrUnsupportedAudioFormat, mimeType)
	}
	if normalizedMIME == "audio/wav" && isDigitalSilencePCM16WAV(audio) {
		return Transcript{}, nil
	}

	dataURL := "data:" + normalizedMIME + ";base64," + base64.StdEncoding.EncodeToString(audio)
	payload := mimoASRRequest{
		Model: p.model,
		Messages: []mimoASRMessage{{
			Role: "user",
			Content: []mimoASRContent{{
				Type:       "input_audio",
				InputAudio: mimoASRInputAudio{Data: dataURL},
			}},
		}},
		ASROptions: mimoASROptions{Language: p.language},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Transcript{}, fmt.Errorf("构造转写请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return Transcript{}, fmt.Errorf("调用 STT 供应商失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMiMoASRResponseBytes+1))
	if err != nil {
		return Transcript{}, fmt.Errorf("读取 STT 响应失败: %w", err)
	}
	if len(respBody) > maxMiMoASRResponseBytes {
		return Transcript{}, fmt.Errorf("STT 响应超过 %d 字节上限", maxMiMoASRResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return Transcript{}, fmt.Errorf("STT 供应商返回错误状态 %d: %s", resp.StatusCode, truncateForError(respBody))
	}

	var parsed struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Transcript{}, fmt.Errorf("解析 STT 响应失败: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Transcript{}, errors.New("STT 供应商未返回转写结果")
	}

	requestID := resp.Header.Get("x-request-id")
	if requestID == "" {
		requestID = parsed.ID
	}

	return Transcript{
		Text:      normalizeMiMoASRText(parsed.Choices[0].Message.Content),
		Provider:  MiMoASRProviderName,
		Model:     p.model,
		RequestID: requestID,
		// MiMo ASR 不返回任何逐段置信度，这里只能如实留空。
		// 按 Transcript.Confidence 的约定，nil 会被上层当作“没有把握”，
		// 也就是每次都要用户确认一遍——对面试练习场景来说，宁可多确认也不能塞错话。
		Confidence: nil,
	}, nil
}

func normalizeMiMoASRText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, trimmed)
	markers := [...]string{"<think>", "think>", "</think>", "<chinese>", "chinese>", "</chinese>"}
	for compact != "" {
		matched := false
		for _, marker := range markers {
			if strings.HasPrefix(compact, marker) {
				compact = strings.TrimPrefix(compact, marker)
				matched = true
				break
			}
		}
		if !matched {
			return trimmed
		}
	}

	return ""
}

func isDigitalSilencePCM16WAV(audio []byte) bool {
	if len(audio) < 12 || string(audio[0:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		return false
	}

	pcm16 := false
	var samples []byte
	for offset := 12; offset+8 <= len(audio); {
		chunkSize := int(binary.LittleEndian.Uint32(audio[offset+4 : offset+8]))
		dataStart := offset + 8
		if chunkSize < 0 || dataStart > len(audio) || chunkSize > len(audio)-dataStart {
			return false
		}
		chunk := audio[dataStart : dataStart+chunkSize]
		switch string(audio[offset : offset+4]) {
		case "fmt ":
			if len(chunk) >= 16 {
				pcm16 = binary.LittleEndian.Uint16(chunk[0:2]) == 1 &&
					binary.LittleEndian.Uint16(chunk[14:16]) == 16
			}
		case "data":
			samples = chunk
		}
		offset = dataStart + chunkSize + chunkSize%2
	}
	if !pcm16 || len(samples) < 2 || len(samples)%2 != 0 {
		return false
	}
	for offset := 0; offset < len(samples); offset += 2 {
		if binary.LittleEndian.Uint16(samples[offset:offset+2]) != 0 {
			return false
		}
	}
	return true
}

type mimoASRRequest struct {
	Model      string           `json:"model"`
	Messages   []mimoASRMessage `json:"messages"`
	ASROptions mimoASROptions   `json:"asr_options"`
}

type mimoASRMessage struct {
	Role    string           `json:"role"`
	Content []mimoASRContent `json:"content"`
}

type mimoASRContent struct {
	Type       string            `json:"type"`
	InputAudio mimoASRInputAudio `json:"input_audio"`
}

type mimoASRInputAudio struct {
	Data string `json:"data"`
}

type mimoASROptions struct {
	Language string `json:"language"`
}
