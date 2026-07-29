package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// ---------------------------------------------------------------------------
// 通用语音输入接口
//
// 这里只做「把一段录音变成文本」。它不触发费曼分析、不产生掌握状态：
// 转写文本回到前端输入框后，用户按发送走的仍然是 /chat/stream 那一条路，
// 和打字完全一样。这样语音就是输入法，不是第二条业务链路。
// ---------------------------------------------------------------------------

const voiceCaptureFormField = "audio"

// voiceCapturePath 是录音上传接口的完整路径；
// 全局请求体上限中间件要按它放行（见 routes），否则外层小上限会截断音频。
const voiceCapturePath = "/api/v1/voice/captures"

// voiceCaptureReply 是一次录音转写的对外表示。
//
// 只回 raw_transcript 一份文本：用户改成什么样，是他在输入框里的事，
// 后端不猜、不预填「修正建议」，避免把 AI 的猜测混进用户的表达里。
type voiceCaptureReply struct {
	CaptureID          string               `json:"capture_id"`
	SessionID          string               `json:"session_id"`
	Status             string               `json:"status"`
	Transcript         string               `json:"transcript"`
	Confidence         *float64             `json:"confidence"`
	AmbiguousTerms     []voiceAmbiguousTerm `json:"ambiguous_terms"`
	NeedsConfirmation  bool                 `json:"needs_confirmation"`
	ConfirmationReason string               `json:"confirmation_reason"`
	TranscriptError    string               `json:"transcript_error,omitempty"`
	DurationMs         *int                 `json:"duration_ms,omitempty"`
	CreatedAt          string               `json:"created_at"`
}

type voiceAmbiguousTerm struct {
	Term  string `json:"term"`
	Heard string `json:"heard"`
}

func toVoiceCaptureReply(capture service.VoiceCapture) voiceCaptureReply {
	terms := make([]voiceAmbiguousTerm, 0, len(capture.AmbiguousTerms))
	for _, term := range capture.AmbiguousTerms {
		terms = append(terms, voiceAmbiguousTerm{Term: term.Term, Heard: term.Heard})
	}
	return voiceCaptureReply{
		CaptureID:          capture.CaptureID,
		SessionID:          capture.SessionID,
		Status:             capture.Status,
		Transcript:         capture.RawTranscript,
		Confidence:         capture.Confidence,
		AmbiguousTerms:     terms,
		NeedsConfirmation:  capture.NeedsConfirmation,
		ConfirmationReason: capture.ConfirmationReason,
		TranscriptError:    capture.TranscriptError,
		DurationMs:         capture.DurationMs,
		CreatedAt:          capture.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) failVoiceError(c *gin.Context, action string, err error) {
	var inputError *service.VoiceInputError
	switch {
	case errors.As(err, &inputError):
		fail(c, http.StatusBadRequest, CodeBadRequest, inputError.Message)
	case errors.Is(err, service.ErrSessionNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
	case errors.Is(err, service.ErrVoiceCaptureNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "语音记录不存在")
	default:
		s.log.Error(action, "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, action)
	}
}

// createVoiceCaptureHandler 上传一段 Push-to-Talk 录音并同步返回转写结果。
//
// 转写失败也返回 200 + status=failed：录音这件事确实发生了，
// 前端据此提示「没听清，请改用打字」，而不是弹一个让用户以为系统坏了的错误。
func (s *Server) createVoiceCaptureHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	sessionID := strings.TrimSpace(c.PostForm("session_id"))
	if sessionID == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "缺少 session_id")
		return
	}

	maxAudioBytes := s.voice.Limits().MaxAudioBytes
	fileHeader, err := c.FormFile(voiceCaptureFormField)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "请求体大小超过上限")
			return
		}
		fail(c, http.StatusBadRequest, CodeBadRequest, "请通过 audio 字段上传录音文件")
		return
	}
	if fileHeader.Size > maxAudioBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "音频大小超过上限")
		return
	}

	opened, err := fileHeader.Open()
	if err != nil {
		s.failVoiceError(c, "读取录音文件失败", err)
		return
	}
	defer func() { _ = opened.Close() }()

	// 再套一层上限：Size 来自客户端声明，不能作为唯一防线。
	content, err := io.ReadAll(io.LimitReader(opened, maxAudioBytes+1))
	if err != nil {
		s.failVoiceError(c, "读取录音文件失败", err)
		return
	}
	if int64(len(content)) > maxAudioBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "音频大小超过上限")
		return
	}

	var durationMs *int
	if raw := strings.TrimSpace(c.PostForm("duration_ms")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			fail(c, http.StatusBadRequest, CodeBadRequest, "duration_ms 必须是整数")
			return
		}
		durationMs = &parsed
	}

	capture, err := s.voice.Capture(c.Request.Context(), userID, sessionID, content,
		fileHeader.Header.Get("Content-Type"), durationMs)
	if err != nil {
		s.failVoiceError(c, "语音转写失败", err)
		return
	}
	ok(c, toVoiceCaptureReply(capture))
}

// getVoiceCaptureHandler 读取一条录音记录，供前端刷新后恢复还没发出去的转写。
func (s *Server) getVoiceCaptureHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	captureID := c.Param("capture_id")
	if !uuidPattern.MatchString(captureID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "语音记录ID格式错误")
		return
	}
	sessionID := strings.TrimSpace(c.Query("session_id"))
	if sessionID == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "缺少 session_id")
		return
	}
	capture, err := s.voice.Get(c.Request.Context(), userID, sessionID, captureID)
	if err != nil {
		s.failVoiceError(c, "查询语音记录失败", err)
		return
	}
	ok(c, toVoiceCaptureReply(capture))
}
