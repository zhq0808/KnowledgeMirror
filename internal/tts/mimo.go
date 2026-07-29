package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MiMoProviderName 是小米 MiMo 语音合成供应商的标识。
const MiMoProviderName = "mimo_tts"

// MiMoDefaultModel 是使用预置音色时的 MiMo 语音合成模型。
// 另有 voicedesign / voiceclone 两个模型，分别用于文本设计音色与音色复刻，
// 它们不支持预置音色，需要额外参数，这里暂不接入。
const MiMoDefaultModel = "mimo-v2.5-tts"

// MiMoDefaultVoice 是缺省音色。中国集群下 mimo_default 等价于“冰糖”。
const MiMoDefaultVoice = "mimo_default"

const maxMiMoTTSResponseBytes = 32 << 20

// ErrNotConfigured 表示未配置 API Key，调用方应把语音合成视为未启用。
var ErrNotConfigured = errors.New("TTS 供应商未配置 API Key")

// MiMoProvider 通过小米 MiMo 开放平台合成语音。
//
// 协议要点（与常见的 /audio/speech 接口不同，所以必须单独实现）：
//   - 走 chat/completions，待合成文本放在 role=assistant 的消息里；
//   - 风格指令放在 role=user 的消息里，模型理解后据此调整语气，但不会念出来；
//   - 非流式响应把音频放在 choices[0].message.audio.data，是 Base64 字符串。
//
// 这里只用非流式 wav：一次费曼提问最多百来字，等全部合成完再播放的延迟可以接受，
// 换来的是前端拿到的就是能直接播的完整文件，不用在浏览器里拼 PCM 分片、补 WAV 头。
//
// 并发安全：底层 http.Client 可被多个 goroutine 共用，自身无可变状态。
type MiMoProvider struct {
	apiKey  string
	baseURL string
	model   string
	voice   string
	http    *http.Client
}

// NewMiMoProvider 构造 MiMo 语音合成客户端。
func NewMiMoProvider(apiKey, baseURL, model, voice string, timeout time.Duration) *MiMoProvider {
	if model == "" {
		model = MiMoDefaultModel
	}
	if voice == "" {
		voice = MiMoDefaultVoice
	}
	return &MiMoProvider{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		voice:   voice,
		http:    &http.Client{Timeout: timeout},
	}
}

func (p *MiMoProvider) Name() string { return MiMoProviderName }

func (p *MiMoProvider) Synthesize(ctx context.Context, text, styleHint string) (Speech, error) {
	if p == nil || p.apiKey == "" {
		return Speech{}, ErrNotConfigured
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Speech{}, errors.New("待合成文本为空")
	}

	messages := make([]mimoTTSMessage, 0, 2)
	if hint := strings.TrimSpace(styleHint); hint != "" {
		// 风格指令必须放在 user 消息里。放进 assistant 会被当成待念文本直接读出来。
		messages = append(messages, mimoTTSMessage{Role: "user", Content: hint})
	}
	messages = append(messages, mimoTTSMessage{Role: "assistant", Content: text})

	payload := mimoTTSRequest{
		Model:    p.model,
		Messages: messages,
		Audio: mimoTTSAudioOptions{
			Format: "wav",
			Voice:  p.voice,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Speech{}, fmt.Errorf("构造语音合成请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Speech{}, fmt.Errorf("构造语音合成请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return Speech{}, fmt.Errorf("调用 TTS 供应商失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMiMoTTSResponseBytes+1))
	if err != nil {
		return Speech{}, fmt.Errorf("读取 TTS 响应失败: %w", err)
	}
	if len(respBody) > maxMiMoTTSResponseBytes {
		return Speech{}, fmt.Errorf("TTS 响应超过 %d 字节上限", maxMiMoTTSResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return Speech{}, fmt.Errorf("TTS 供应商返回错误状态 %d: %s", resp.StatusCode, truncateForError(respBody))
	}

	var parsed struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Audio struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Speech{}, fmt.Errorf("解析 TTS 响应失败: %w", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Audio.Data == "" {
		return Speech{}, errors.New("TTS 供应商未返回音频数据")
	}

	audio, err := base64.StdEncoding.DecodeString(parsed.Choices[0].Message.Audio.Data)
	if err != nil {
		return Speech{}, fmt.Errorf("解码 TTS 音频失败: %w", err)
	}

	requestID := resp.Header.Get("x-request-id")
	if requestID == "" {
		requestID = parsed.ID
	}

	return Speech{
		Audio:     audio,
		MIMEType:  "audio/wav",
		Provider:  MiMoProviderName,
		Model:     p.model,
		Voice:     p.voice,
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

type mimoTTSRequest struct {
	Model    string              `json:"model"`
	Messages []mimoTTSMessage    `json:"messages"`
	Audio    mimoTTSAudioOptions `json:"audio"`
}

type mimoTTSMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type mimoTTSAudioOptions struct {
	Format string `json:"format"`
	Voice  string `json:"voice"`
}
