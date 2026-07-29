package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// feynmanPracticeStateResponse 是下发给前端的最小练习状态。
//
// 只暴露“当前状态 + 当前题目 + 第几轮”这三样：状态条要显示的就这些，
// 反馈正文本身已经是对话里的一条 assistant 消息，不需要再复制一份。
type feynmanPracticeStateResponse struct {
	State    string `json:"state"`
	Question string `json:"question"`
	RoundNo  int    `json:"round_no"`
}

func newFeynmanPracticeStateResponse(state service.FeynmanPracticeState) feynmanPracticeStateResponse {
	return feynmanPracticeStateResponse{
		State:    state.State,
		Question: state.ActiveQuestionText,
		RoundNo:  state.RoundNo,
	}
}

// getFeynmanPracticeStateHandler 返回指定会话当前的费曼练习状态。
// 刷新页面或切回旧会话时，前端靠它把状态条恢复出来，而不是假设练习已结束。
func (s *Server) getFeynmanPracticeStateHandler(c *gin.Context) {
	sessionID := strings.TrimSpace(c.Query("session_id"))
	if !validSessionID(sessionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "会话ID格式错误")
		return
	}
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	if err := s.sessions.RequireOwnedActive(c.Request.Context(), userID, sessionID); err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
			return
		}
		s.log.Error("校验会话归属失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", sessionID,
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "会话服务暂时不可用")
		return
	}

	state, err := s.practice.State(c.Request.Context(), userID, sessionID)
	if err != nil {
		s.log.Error("读取费曼练习状态失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", sessionID,
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "练习状态暂时不可用")
		return
	}
	ok(c, newFeynmanPracticeStateResponse(state))
}
