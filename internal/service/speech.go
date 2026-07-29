package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"healthAgent/internal/tts"
)

// SpeechLimits 是语音合成的防御性预算。
type SpeechLimits struct {
	// MaxTextRunes 是单次合成文本的长度上限。
	// 超长时直接拒绝而不是截断：念一半的问题比不念更糟——用户会以为自己听漏了。
	MaxTextRunes int
	// StyleHint 是缺省的念稿风格指令，描述“怎么念”，不会被念出来。
	StyleHint string
}

func (l SpeechLimits) withDefaults() SpeechLimits {
	if l.MaxTextRunes <= 0 {
		l.MaxTextRunes = 600
	}
	return l
}

// ErrSpeechTextEmpty 表示待合成文本为空。
var ErrSpeechTextEmpty = errors.New("待合成文本为空")

// ErrSpeechTextTooLong 表示待合成文本超过长度上限。
var ErrSpeechTextTooLong = errors.New("待合成文本超过长度上限")

// SpeechService 把文本合成成语音，供前端播放费曼提问与追问。
//
// 这条链路刻意做得很薄：不落库、不改任何会话状态、不产生掌握状态。
// 语音只是同一段文字的另一种呈现方式，合成失败时前端退回纯文字即可，
// 绝不能因为念不出来就挡住用户继续练习。
type SpeechService struct {
	provider tts.Provider
	limits   SpeechLimits
	log      *slog.Logger
}

// NewSpeechService 构造语音合成服务。provider 为 nil 时调用方不应注册该服务。
func NewSpeechService(provider tts.Provider, limits SpeechLimits, log *slog.Logger) *SpeechService {
	return &SpeechService{
		provider: provider,
		limits:   limits.withDefaults(),
		log:      log,
	}
}

// ProviderName 返回当前 TTS 供应商标识。
func (s *SpeechService) ProviderName() string {
	if s == nil || s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

// Limits 返回生效中的合成预算。
func (s *SpeechService) Limits() SpeechLimits { return s.limits }

// Synthesize 把 text 合成成音频。styleHint 为空时使用配置里的缺省风格。
func (s *SpeechService) Synthesize(ctx context.Context, text, styleHint string) (tts.Speech, error) {
	if s == nil || s.provider == nil {
		return tts.Speech{}, tts.ErrNotConfigured
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return tts.Speech{}, ErrSpeechTextEmpty
	}
	if len([]rune(text)) > s.limits.MaxTextRunes {
		return tts.Speech{}, ErrSpeechTextTooLong
	}
	if strings.TrimSpace(styleHint) == "" {
		styleHint = s.limits.StyleHint
	}

	speech, err := s.provider.Synthesize(ctx, text, styleHint)
	if err != nil {
		s.log.Warn("语音合成失败", "provider", s.provider.Name(), "error", err)
		return tts.Speech{}, err
	}
	return speech, nil
}
