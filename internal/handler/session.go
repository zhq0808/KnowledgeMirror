package handler

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

var sessionIDPattern = regexp.MustCompile(`^session_[0-9a-f]{32}$`)

type createSessionReply struct {
	SessionID string `json:"session_id"`
}

// sessionListItemReply 是会话列表接口的单项 DTO；时间统一格式化成 RFC3339 字符串，
// 未定标题时返回空字符串（首版还没做自动命名，前端可以自行回退成"新会话"之类的占位文案）。
type sessionListItemReply struct {
	SessionID     string  `json:"session_id"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	MessageCount  int     `json:"message_count"`
	LastMessageAt *string `json:"last_message_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

func validSessionID(sessionID string) bool {
	return sessionIDPattern.MatchString(sessionID)
}

func (s *Server) createSessionHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	sessionID, err := s.sessions.Create(c.Request.Context(), userID)
	if err != nil {
		s.log.Error("创建会话失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, "创建会话失败")
		return
	}
	ok(c, createSessionReply{SessionID: sessionID})
}

// listSessionsHandler 返回当前认证用户名下未删除的会话列表，按最近活跃时间倒序。
// 只信任认证 context 里的 user_id，绝不接受客户端传参指定要查哪个用户。
func (s *Server) listSessionsHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	items, err := s.sessions.List(c.Request.Context(), userID)
	if err != nil {
		s.log.Error("查询会话列表失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, "查询会话列表失败")
		return
	}

	reply := make([]sessionListItemReply, 0, len(items))
	for _, item := range items {
		entry := sessionListItemReply{
			SessionID:    item.SessionID,
			Title:        item.Title,
			Status:       item.Status,
			MessageCount: item.MessageCount,
			CreatedAt:    item.CreatedAt.Format(time.RFC3339),
		}
		if item.LastMessageAt != nil {
			formatted := item.LastMessageAt.Format(time.RFC3339)
			entry.LastMessageAt = &formatted
		}
		reply = append(reply, entry)
	}
	ok(c, reply)
}

// sessionMessageReply 是会话消息列表接口的单项 DTO。对外只暴露稳定的 UUID message_id，
// 不暴露数据库内部行主键 id，也不暴露 turn lease 状态记录。
type sessionMessageReply struct {
	MessageID string                    `json:"message_id"`
	Role      string                    `json:"role"`
	Content   string                    `json:"content"`
	Seq       int64                     `json:"seq"`
	Retrieval *retrievalSourcesResponse `json:"retrieval,omitempty"`
	CreatedAt string                    `json:"created_at"`
}

// listSessionMessagesHandler 按归属返回指定会话内已完成、未删除的 user/assistant 消息。
// "按归属"意味着：URL 里的 session_id 不可信，必须先用认证 context 里的可信 user_id
// 校验这个会话确实属于当前用户。归档会话仍可读取历史；已删除或不属于当前用户时统一返回 404。
func (s *Server) listSessionMessagesHandler(c *gin.Context) {
	sessionID := c.Param("session_id")
	if !validSessionID(sessionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "会话ID格式错误")
		return
	}
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	if err := s.sessions.RequireOwned(c.Request.Context(), userID, sessionID); err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
			return
		}
		s.log.Error("校验会话归属失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, "会话服务暂时不可用")
		return
	}

	messages, err := s.messages.ListMessages(c.Request.Context(), userID, sessionID)
	if err != nil {
		s.log.Error("查询会话消息失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, "查询会话消息失败")
		return
	}

	reply := make([]sessionMessageReply, 0, len(messages))
	for _, message := range messages {
		reply = append(reply, sessionMessageReply{
			MessageID: message.MessageID,
			Role:      message.Role,
			Content:   message.Content,
			Seq:       message.Seq,
			Retrieval: newPersistedRetrievalSourcesResponse(message.Retrieval),
			CreatedAt: message.CreatedAt.Format(time.RFC3339),
		})
	}
	ok(c, reply)
}
