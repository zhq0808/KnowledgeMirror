package handler

import (
	"time"

	"KnowledgeMirror/internal/service"
)

// ---------------------------------------------------------------------------
// 通用语音输入接口
//
// 这里只做「把一段录音变成文本」。它不触发费曼分析、不产生掌握状态：
// 转写文本回到前端输入框后，用户按发送走的仍然是 /chat/stream 那一条路，
// 和打字完全一样。这样语音就是输入法，不是第二条业务链路。
// ---------------------------------------------------------------------------

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
