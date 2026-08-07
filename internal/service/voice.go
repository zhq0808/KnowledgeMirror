package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"KnowledgeMirror/internal/stt"
)

// ---------------------------------------------------------------------------
// 实时语音输入持久化：固化实时 ASR 的最终文本，并关联最终聊天消息。
//
// 这里刻意只做「把声音变成文本」这一件事。转写完成后，这段文本走的是和打字
// 完全相同的一条链路（/chat/stream -> 费曼对话服务），所以本文件里不存在任何
// 费曼分析逻辑，也不产生任何掌握状态或掌握证据 —— 说了话不等于讲对了。
//
// 这套记录绑在聊天会话上，是实时语音输入的审计记录，不是独立练习任务。
// ---------------------------------------------------------------------------

// 实时 ASR 在会话结束时固化 transcribed/failed 终态；transcribing 仅是短暂落库过程态。
const (
	VoiceCaptureStatusUploaded     = "uploaded"
	VoiceCaptureStatusTranscribing = "transcribing"
	VoiceCaptureStatusTranscribed  = "transcribed"
	VoiceCaptureStatusFailed       = "failed"
)

// 要求用户先确认再发送的原因。空字符串表示本次可以直接自动发送。
// 顺序即优先级：转写失败 > 拿不到置信度 > 置信度偏低 > 术语疑似听错。
const (
	VoiceConfirmReasonTranscribeFailed  = "transcribe_failed"
	VoiceConfirmReasonMissingConfidence = "missing_confidence"
	VoiceConfirmReasonLowConfidence     = "low_confidence"
	VoiceConfirmReasonAmbiguousTerms    = "ambiguous_terms"
)

const emptyTranscriptError = "没有听到可转写内容，请重新录音"

// ErrVoiceCaptureNotFound 表示录音记录不存在或不属于当前用户/会话。
var ErrVoiceCaptureNotFound = errors.New("语音记录不存在")

// VoiceInputError 是可以安全回显给用户的输入/预算类错误，接口层映射为 400。
type VoiceInputError struct{ Message string }

func (e *VoiceInputError) Error() string { return e.Message }

func invalidVoiceInput(format string, args ...any) error {
	return &VoiceInputError{Message: fmt.Sprintf(format, args...)}
}

// VoiceCapture 是一次录音及其转写结果。
//
// RawTranscript 是不可信输入：它没有被任何人确认过，不得直接当作用户的正式表达，
// 更不能作为掌握证据。真正被发送出去的文本是聊天消息本身，通过 MessageID 关联。
type VoiceCapture struct {
	CaptureID          string
	UserID             string
	SessionID          string
	Status             string
	MIMEType           string
	SizeBytes          int64
	DurationMs         *int
	STTProvider        string
	STTModel           string
	STTRequestID       string
	RawTranscript      string
	Confidence         *float64
	AmbiguousTerms     []AmbiguousTerm
	NeedsConfirmation  bool
	ConfirmationReason string
	TranscriptError    string
	MessageID          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ClaimVoiceCaptureParams 是抢占一次转写任务所需的全部参数。
type ClaimVoiceCaptureParams struct {
	CaptureID   string
	UserID      string
	SessionID   string
	MIMEType    string
	SizeBytes   int64
	DurationMs  *int
	SHA256      []byte
	AudioData   []byte
	STTProvider string
	// StaleBefore 之前仍停在 transcribing 的记录视为上次调用崩溃，允许本次接管重试。
	StaleBefore time.Time
}

// CompleteVoiceCaptureParams 写入一次转写的终态结果。
type CompleteVoiceCaptureParams struct {
	CaptureID          string
	UserID             string
	Status             string
	STTProvider        string
	STTModel           string
	STTRequestID       string
	RawTranscript      string
	Confidence         *float64
	AmbiguousTerms     []AmbiguousTerm
	NeedsConfirmation  bool
	ConfirmationReason string
	TranscriptError    string
}

// FinalizeRealtimeInput 是实时 STT 已结束后一次性固化所需的数据。
// TranscriptError 非空表示上游失败、用户取消或浏览器断连等失败终态。
type FinalizeRealtimeInput struct {
	UserID          string
	SessionID       string
	WAV             []byte
	Transcript      string
	Provider        string
	Model           string
	TaskID          string
	DurationMs      *int
	TranscriptError string
}

// VoiceCaptureRepository 是语音录音的持久化边界。
type VoiceCaptureRepository interface {
	// Claim 抢占一次转写。相同用户+会话+字节的重复提交命中去重记录并返回 claimed=false，
	// 由调用方直接复用已有结果，不重复调用 STT（不重复计费）。
	Claim(ctx context.Context, params ClaimVoiceCaptureParams) (VoiceCapture, bool, error)
	// Complete 写入终态，仅当记录仍处于 transcribing 时生效。
	Complete(ctx context.Context, params CompleteVoiceCaptureParams) (VoiceCapture, error)
	// Get 按 (user_id, session_id, capture_id) 读取一条录音记录。
	Get(ctx context.Context, userID, sessionID, captureID string) (VoiceCapture, error)
	// BindMessage 把转写记录一次性绑定到它最终发出的那条消息上；已绑定过则返回错误。
	BindMessage(ctx context.Context, userID, sessionID, captureID, messageID string) error
}

// VoiceLimits 是语音输入的硬上限与判定阈值，全部是防御性预算。
type VoiceLimits struct {
	MaxAudioBytes      int64
	MaxDurationMS      int
	MaxTranscriptChars int
	// MinConfidence 是可以自动发送的最低整体置信度；低于它就先让用户看一眼。
	MinConfidence     float64
	MaxAmbiguousTerms int
	TranscribingStale time.Duration
}

// VoiceCaptureService 固化实时 ASR 结果并维护语音记录与消息的关联。
type VoiceCaptureService struct {
	repo     VoiceCaptureRepository
	stt      stt.Provider
	glossary *TermGlossary
	limits   VoiceLimits
	log      *slog.Logger
}

// NewVoiceCaptureService 构造语音结果持久化服务。sttProvider 为 nil 时只支持实时结果固化，
// glossary 为 nil 时关闭术语歧义检测。
func NewVoiceCaptureService(repo VoiceCaptureRepository, sttProvider stt.Provider, glossary *TermGlossary, limits VoiceLimits, log *slog.Logger) *VoiceCaptureService {
	if log == nil {
		log = slog.Default()
	}
	if limits.TranscribingStale <= 0 {
		limits.TranscribingStale = 2 * time.Minute
	}
	if limits.MaxAmbiguousTerms <= 0 {
		limits.MaxAmbiguousTerms = 5
	}
	return &VoiceCaptureService{repo: repo, stt: sttProvider, glossary: glossary, limits: limits, log: log}
}

// Limits 返回当前生效的预算配置，供接口层提前校验请求体大小。
func (s *VoiceCaptureService) Limits() VoiceLimits { return s.limits }

// UploadEnabled 表示当前服务是否配置了可执行录音上传转写的供应商。
func (s *VoiceCaptureService) UploadEnabled() bool { return s != nil && s.stt != nil }

// STTProviderName 返回录音上传转写使用的供应商标识。
func (s *VoiceCaptureService) STTProviderName() string {
	if !s.UploadEnabled() {
		return ""
	}
	return s.stt.Name()
}

// Capture 接收一段完整录音并同步完成转写。
// 转写失败也会固化为 failed 记录；只有输入或存储错误才直接返回 error。
func (s *VoiceCaptureService) Capture(ctx context.Context, userID, sessionID string, audio []byte, mimeType string, durationMs *int) (VoiceCapture, error) {
	if !s.UploadEnabled() {
		return VoiceCapture{}, invalidVoiceInput("录音上传转写服务未配置")
	}
	userID, sessionID, err := s.validateInput(userID, sessionID, audio, mimeType, durationMs)
	if err != nil {
		return VoiceCapture{}, err
	}
	claimed, won, err := s.claim(ctx, userID, sessionID, audio, mimeType, durationMs, s.stt.Name())
	if err != nil {
		return VoiceCapture{}, err
	}
	if !won {
		s.log.Info("命中已有语音记录，跳过重复 STT 调用",
			"session_id", sessionID, "capture_id", claimed.CaptureID)
		return claimed, nil
	}

	params := CompleteVoiceCaptureParams{
		CaptureID:   claimed.CaptureID,
		UserID:      userID,
		STTProvider: s.stt.Name(),
	}
	transcript, sttErr := s.stt.Transcribe(ctx, audio, mimeType)
	switch {
	case sttErr != nil:
		s.log.Warn("语音转写失败", "session_id", sessionID, "capture_id", claimed.CaptureID,
			"provider", s.stt.Name(), "error", sttErr)
		params.Status = VoiceCaptureStatusFailed
		params.TranscriptError = truncateFeynmanError(sttErr.Error(), 2000)
	default:
		text := strings.TrimSpace(transcript.Text)
		switch {
		case text == "":
			params.Status = VoiceCaptureStatusFailed
			params.TranscriptError = emptyTranscriptError
		case utf8.RuneCountInString(text) > s.limits.MaxTranscriptChars:
			params.Status = VoiceCaptureStatusFailed
			params.TranscriptError = fmt.Sprintf("STT 转写超过 %d 字上限", s.limits.MaxTranscriptChars)
		default:
			params.Status = VoiceCaptureStatusTranscribed
			params.RawTranscript = text
			params.STTProvider = strings.TrimSpace(transcript.Provider)
			if params.STTProvider == "" {
				params.STTProvider = s.stt.Name()
			}
			params.STTModel = strings.TrimSpace(transcript.Model)
			params.STTRequestID = strings.TrimSpace(transcript.RequestID)
			params.Confidence = transcript.Confidence
			params.AmbiguousTerms = s.glossary.AmbiguousTerms(text, s.limits.MaxAmbiguousTerms)
		}
	}
	params.NeedsConfirmation, params.ConfirmationReason = s.judgeConfirmation(params)
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.repo.Complete(completeCtx, params)
}

// FinalizeRealtime 将实时供应商已经生成的最终文本和完整 WAV 一次性固化。
// 它不会再次调用文件式 ASR，避免同一段音频产生第二份文本和重复费用。
func (s *VoiceCaptureService) FinalizeRealtime(ctx context.Context, input FinalizeRealtimeInput) (VoiceCapture, error) {
	const mimeType = "audio/wav"
	userID, sessionID, err := s.validateInput(input.UserID, input.SessionID, input.WAV, mimeType, input.DurationMs)
	if err != nil {
		return VoiceCapture{}, err
	}
	provider := strings.TrimSpace(input.Provider)
	if provider == "" {
		return VoiceCapture{}, invalidVoiceInput("缺少实时转写供应商")
	}

	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	claimed, won, err := s.claim(finalizeCtx, userID, sessionID, input.WAV, mimeType, input.DurationMs, provider)
	if err != nil {
		return VoiceCapture{}, err
	}
	if !won {
		s.log.Info("命中已有实时语音记录，跳过重复固化",
			"session_id", sessionID, "capture_id", claimed.CaptureID)
		return claimed, nil
	}

	params := CompleteVoiceCaptureParams{
		CaptureID:    claimed.CaptureID,
		UserID:       userID,
		STTProvider:  provider,
		STTModel:     strings.TrimSpace(input.Model),
		STTRequestID: strings.TrimSpace(input.TaskID),
	}
	text := strings.TrimSpace(input.Transcript)
	transcriptError := strings.TrimSpace(input.TranscriptError)
	switch {
	case transcriptError != "":
		params.Status = VoiceCaptureStatusFailed
		params.TranscriptError = truncateFeynmanError(transcriptError, 2000)
	case text == "":
		params.Status = VoiceCaptureStatusFailed
		params.TranscriptError = emptyTranscriptError
	case utf8.RuneCountInString(text) > s.limits.MaxTranscriptChars:
		params.Status = VoiceCaptureStatusFailed
		params.TranscriptError = fmt.Sprintf("STT 转写超过 %d 字上限", s.limits.MaxTranscriptChars)
	default:
		params.Status = VoiceCaptureStatusTranscribed
		params.RawTranscript = text
		params.AmbiguousTerms = s.glossary.AmbiguousTerms(text, s.limits.MaxAmbiguousTerms)
	}
	params.NeedsConfirmation, params.ConfirmationReason = s.judgeConfirmation(params)
	return s.repo.Complete(finalizeCtx, params)
}

func (s *VoiceCaptureService) validateInput(userID, sessionID string, audio []byte, mimeType string, durationMs *int) (string, string, error) {
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	if userID == "" {
		return "", "", invalidVoiceInput("用户身份缺失")
	}
	if sessionID == "" {
		return "", "", invalidVoiceInput("缺少会话 ID")
	}
	if len(audio) == 0 {
		return "", "", invalidVoiceInput("音频内容为空")
	}
	if int64(len(audio)) > s.limits.MaxAudioBytes {
		return "", "", invalidVoiceInput("音频大小超过上限（%d 字节）", s.limits.MaxAudioBytes)
	}
	if !isAllowedFeynmanAudioMIME(mimeType) {
		return "", "", invalidVoiceInput("不支持的音频格式: %s", mimeType)
	}
	if durationMs != nil && (*durationMs <= 0 || *durationMs > s.limits.MaxDurationMS) {
		return "", "", invalidVoiceInput("录音时长超过上限（%d 毫秒）", s.limits.MaxDurationMS)
	}
	return userID, sessionID, nil
}

func (s *VoiceCaptureService) claim(ctx context.Context, userID, sessionID string, audio []byte, mimeType string, durationMs *int, provider string) (VoiceCapture, bool, error) {
	hash := sha256.Sum256(audio)
	captureID, err := NewVoiceCaptureID()
	if err != nil {
		return VoiceCapture{}, false, err
	}
	return s.repo.Claim(ctx, ClaimVoiceCaptureParams{
		CaptureID:   captureID,
		UserID:      userID,
		SessionID:   sessionID,
		MIMEType:    mimeType,
		SizeBytes:   int64(len(audio)),
		DurationMs:  durationMs,
		SHA256:      hash[:],
		AudioData:   audio,
		STTProvider: provider,
		StaleBefore: time.Now().Add(-s.limits.TranscribingStale),
	})
}

// judgeConfirmation 裁决本次是否需要用户确认后再发送，并给出唯一原因。
//
// 这是「STT 完成后默认继续分析，仅在置信度低/术语有歧义时暂停」的落地点：
// 默认值是自动发送，只有下面这几条明确证据才会拦一次。
// 拿不到置信度（供应商不支持 verbose_json）按需要确认处理 —— 不确定时偏保守，
// 让用户多按一次回车，好过把听错的话当成他的正式回答喂进分析链路。
func (s *VoiceCaptureService) judgeConfirmation(params CompleteVoiceCaptureParams) (bool, string) {
	switch {
	case params.Status != VoiceCaptureStatusTranscribed:
		return true, VoiceConfirmReasonTranscribeFailed
	case params.Confidence == nil:
		return true, VoiceConfirmReasonMissingConfidence
	case *params.Confidence < s.limits.MinConfidence:
		return true, VoiceConfirmReasonLowConfidence
	case len(params.AmbiguousTerms) > 0:
		return true, VoiceConfirmReasonAmbiguousTerms
	default:
		return false, ""
	}
}

// Get 读取一条录音记录，供前端刷新后恢复未发送的转写。
func (s *VoiceCaptureService) Get(ctx context.Context, userID, sessionID, captureID string) (VoiceCapture, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return VoiceCapture{}, invalidVoiceInput("用户身份缺失")
	}
	return s.repo.Get(ctx, userID, sessionID, captureID)
}

// BindMessage 把一段转写和它最终发出的那条消息绑起来。
//
// 这条关联就是「保留原始转写与更正版本」的全部实现：原始转写锁在 voice_captures 里，
// 用户改过的版本是消息内容本身，两份文字都在，谁也没覆盖谁。
func (s *VoiceCaptureService) BindMessage(ctx context.Context, userID, sessionID, captureID, messageID string) error {
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	captureID = strings.TrimSpace(captureID)
	messageID = strings.TrimSpace(messageID)
	if userID == "" || sessionID == "" || captureID == "" || messageID == "" {
		return invalidVoiceInput("绑定语音记录的参数不完整")
	}
	return s.repo.BindMessage(ctx, userID, sessionID, captureID, messageID)
}

// NewVoiceCaptureID 生成语音记录主键（UUIDv7，按时间有序）。
func NewVoiceCaptureID() (string, error) { return newUUIDv7("capture_id") }
