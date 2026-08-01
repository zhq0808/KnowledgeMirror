package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/tts"
)

// speechRequest 是语音合成请求体。
type speechRequest struct {
	// Text 是要念出来的文本。由前端把已经渲染出来的费曼提问原样回传，
	// 保证「听到的」和「看到的」永远是同一句话。
	Text string `json:"text"`
	// StyleHint 可选，覆盖服务端配置的缺省念稿风格。
	StyleHint string `json:"style_hint"`
}

// createSpeechHandler 把一段文本合成成音频并直接返回音频字节。
//
// 返回二进制而不是 Base64 JSON：前端拿到后可以直接丢给 <audio>，
// 少一次内存里的编解码，长问题也不会因为 Base64 膨胀 33% 而变卡。
func (s *Server) createSpeechHandler(c *gin.Context) {
	if _, authenticated := UserIDFromContext(c.Request.Context()); !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	var req speechRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}

	speech, err := s.speech.Synthesize(c.Request.Context(), req.Text, req.StyleHint)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSpeechTextEmpty):
			fail(c, http.StatusBadRequest, CodeBadRequest, "没有可朗读的内容")
		case errors.Is(err, service.ErrSpeechTextTooLong):
			fail(c, http.StatusBadRequest, CodeBadRequest, "这段文字太长，超出单次朗读上限")
		case errors.Is(err, tts.ErrNotConfigured):
			fail(c, http.StatusServiceUnavailable, CodeInternal, "语音合成未配置")
		default:
			// 念不出来不影响练习本身，前端收到失败后退回纯文字即可。
			fail(c, http.StatusBadGateway, CodeInternal, "语音合成失败，请看文字版")
		}
		return
	}

	// 不缓存：同一段文字可能配不同风格，且这里没有稳定的缓存键。
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, speech.MIMEType, speech.Audio)
}
