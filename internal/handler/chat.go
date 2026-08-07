package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/llm"
	"KnowledgeMirror/internal/service"
)

// chatRequest 是对话接口的请求体。
type chatRequest struct {
	SessionID       string `json:"session_id"`
	ClientMessageID string `json:"client_message_id"`
	Message         string `json:"message"`
	CoachTaskID     string `json:"coach_task_id"`
	LocalDate       string `json:"local_date"`
	// VoiceCaptureID 可选：这条消息是哪段录音转写来的。
	// 它只用于把原始转写和用户实际发出的文本关联起来，
	// 不影响任何对话逻辑：语音和打字到了这里已经是同一件东西。
	VoiceCaptureID string `json:"voice_capture_id"`
}

var clientMessageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

const recentHistoryLimit = 20

// setupSSE 检查连接是否支持流式输出，并在确定这个请求接下来一定会以 SSE 回应时才设定头。
// 返回的 writeSSE 写一帧并立即 flush；写失败（客户端断开）返回 error 以便上游停止。
// 调用时机很重要：一旦设完头，就不能再回头用 fail() 返 JSON 错误了（Content-Type 已经是
// text/event-stream），只能用 writeSSE("error", ...) 收尾。
func setupSSE(c *gin.Context) (func(event, data string) error, bool) {
	// http.Flusher：每写一帧就 flush 到网络，否则会被缓冲着一次性发，流式就失意了。
	// gin.ResponseWriter 本身实现了 http.Flusher，这里断言一下更显意图。
	flusher, canFlush := c.Writer.(http.Flusher)
	if !canFlush {
		return nil, false
	}

	// SSE 响应头（必须在写任何 body 前设好）。
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // 关闭 Nginx 缓冲，保证逐帧下发

	writeSSE := func(event, data string) error {
		var b strings.Builder
		if event != "" {
			b.WriteString("event: ")
			b.WriteString(event)
			b.WriteByte('\n')
		}
		b.WriteString("data: ")
		b.WriteString(data)
		b.WriteString("\n\n")
		if _, err := io.WriteString(c.Writer, b.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	return writeSSE, true
}

// chatStreamHandler 处理一条用户消息，以 SSE 流式把模型回复逐段推给前端。
// Content-Type: text/event-stream，每收到一段增量就 `data: {json}\n\n` 刷一帧，
// 结束发一帧 `event: done`。客户端断开时：ctx 取消 + Write 报错双重兑底停止。
func (s *Server) chatStreamHandler(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ClientMessageID = strings.ToLower(strings.TrimSpace(req.ClientMessageID))
	if !validSessionID(req.SessionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "会话ID格式错误")
		return
	}
	if !clientMessageIDPattern.MatchString(req.ClientMessageID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "客户端消息ID格式错误")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "消息不能为空")
		return
	}
	req.VoiceCaptureID = strings.TrimSpace(req.VoiceCaptureID)
	if req.VoiceCaptureID != "" && !uuidPattern.MatchString(req.VoiceCaptureID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "语音记录ID格式错误")
		return
	}
	req.CoachTaskID = strings.TrimSpace(req.CoachTaskID)
	if req.CoachTaskID != "" && !uuidPattern.MatchString(req.CoachTaskID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "教练任务ID格式错误")
		return
	}
	req.LocalDate = strings.TrimSpace(req.LocalDate)
	if req.LocalDate != "" {
		now := time.Now()
		parsed, err := time.ParseInLocation(time.DateOnly, req.LocalDate, now.Location())
		if err != nil || parsed.Format(time.DateOnly) != req.LocalDate {
			fail(c, http.StatusBadRequest, CodeBadRequest, "本地日期必须是 YYYY-MM-DD")
			return
		}
		serverDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if parsed.Before(serverDate.AddDate(0, 0, -1)) || parsed.After(serverDate.AddDate(0, 0, 1)) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "本地日期只能是服务端当前日期前后一天")
			return
		}
	}
	userID, ok := UserIDFromContext(c.Request.Context())
	if !ok {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	if err := s.sessions.RequireOwnedActive(c.Request.Context(), userID, req.SessionID); err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
			return
		}
		s.log.Error("校验会话归属失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", req.SessionID,
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "会话服务暂时不可用")
		return
	}

	// 在一个短事务中幂等写入 user 消息并裁决 turn。冲突请求会整体回滚，不会留下孤立 user 消息。
	//   - active 且未过期 -> 返回处理中，不重新调用 LLM。
	//   - active 已过期 -> 递增 attempt_no 后恢复执行。
	//   - failed -> 原地重开为 active，当作一次合法重试（同样重新跑 LLM）。
	//   - completed -> 不获取租约，下面直接回放当年落库的 assistant 回复，不重调 LLM。
	leaseResult, err := s.turnLeases.Acquire(c.Request.Context(), service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       req.SessionID,
		ClientMessageID: req.ClientMessageID,
		Content:         strings.TrimSpace(req.Message),
	})
	if errors.Is(err, service.ErrClientMessageConflict) {
		fail(c, http.StatusConflict, CodeConflict, "客户端消息ID已用于其他内容")
		return
	}
	if errors.Is(err, service.ErrSessionNotFound) {
		fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
		return
	}
	if errors.Is(err, service.ErrTurnInProgress) {
		fail(c, http.StatusConflict, CodeConflict, "该消息正在处理中，请稍后重试")
		return
	}
	if errors.Is(err, service.ErrTurnLeaseConflict) {
		fail(c, http.StatusConflict, CodeConflict, "该会话正在处理上一条消息，请稍后重试")
		return
	}
	if err != nil {
		s.log.Error("获取 turn 租约失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", req.SessionID,
			"client_message_id", req.ClientMessageID,
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "对话服务暂时不可用")
		return
	}
	if !leaseResult.Acquired {
		if leaseResult.Lease.Status != service.TurnLeaseCompleted || leaseResult.Lease.ResultMessageID == "" {
			s.log.Error("completed turn 缺少结果消息",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"client_message_id", req.ClientMessageID,
			)
			fail(c, http.StatusInternalServerError, CodeInternal, "对话服务暂时不可用")
			return
		}
		s.replayCompletedTurn(c, userID, req.SessionID, leaseResult.Lease.ResultMessageID)
		return
	}

	// 语音来源绑定：拿到真实落库的 message_id 后，把“原始转写”和“用户真正发出的文本”
	// 钩到一起，两份都在，谁也没覆盖谁。
	// 失败只记 ERROR 不中断本轮对话：这只是一条溯源元数据，
	// 为了它把用户已经说出口的话扔掉是本末倒置。
	if req.VoiceCaptureID != "" && s.voice != nil {
		if bindErr := s.voice.BindMessage(c.Request.Context(), userID, req.SessionID,
			req.VoiceCaptureID, leaseResult.UserMessage.MessageID); bindErr != nil {
			s.log.Error("绑定语音转写失败",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"capture_id", req.VoiceCaptureID,
				"message_id", leaseResult.UserMessage.MessageID,
				"error", bindErr,
			)
		}
	}

	turnCompleted := false
	defer func() {
		if turnCompleted {
			return
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if releaseErr := s.turnLeases.Release(releaseCtx, service.ReleaseTurnLeaseRequest{
			UserID:          userID,
			SessionID:       req.SessionID,
			ClientMessageID: req.ClientMessageID,
			AttemptNo:       leaseResult.Lease.AttemptNo,
			Status:          service.TurnLeaseFailed,
		}); releaseErr != nil {
			s.log.Error("释放 turn 租约失败",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"message_id", leaseResult.UserMessage.MessageID,
				"error", releaseErr,
			)
		}
	}()

	history, err := s.messages.LoadRecent(c.Request.Context(), userID, req.SessionID, recentHistoryLimit)
	if err != nil {
		s.log.Error("读取对话历史失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", req.SessionID,
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "读取对话历史失败")
		return
	}

	// http.Flusher 检查 + SSE 头都抽到 setupSSE 里了，回放分支和正常分支共用。
	writeSSE, canFlush := setupSSE(c)
	if !canFlush {
		fail(c, http.StatusInternalServerError, CodeInternal, "服务器不支持流式输出")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), s.chat.Timeout())
	defer cancel()

	// 把一段增量包成 JSON 再下发，避免文本里的换行破坏 SSE 帧结构。
	// 这里只做协议转换，不做任何累积/截断判断——那些属于业务规则，由 ChatService.Stream 统一处理。
	sendDelta := func(delta string) error {
		payload, _ := json.Marshal(map[string]string{"delta": delta})
		return writeSSE("", string(payload))
	}

	var retrievalRequestID string

	// persistAndFinish 是"正常回复"和"降级兜底回复"共享的收尾规则：先落库，落库成功才发 `done`；
	// 落库失败发 `error`，绝不能让客户端已经看到的内容和数据库历史不一致。
	// 返回值表示这一轮 turn 是否真正走到了成功终态（用于决定 leaseStatus）。
	persistAndFinish := func(content string) bool {
		if _, err := s.turnLeases.Complete(c.Request.Context(), service.CompleteTurnRequest{
			UserID:             userID,
			SessionID:          req.SessionID,
			ClientMessageID:    req.ClientMessageID,
			AttemptNo:          leaseResult.Lease.AttemptNo,
			UserMessageID:      leaseResult.UserMessage.MessageID,
			Content:            content,
			PromptVersion:      s.chat.PromptVersion(),
			ModelName:          s.chat.ModelName(),
			RetrievalRequestID: retrievalRequestID,
		}); err != nil {
			s.log.Error("assistant 消息落库失败",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"message_id", leaseResult.UserMessage.MessageID,
				"error", err,
			)
			_ = writeSSE("error", `{"message":"回复保存失败，请稍后重试"}`)
			return false
		}
		turnCompleted = true
		// turn 已成功提交（user+assistant 落库、lease=completed）。向异步记忆抽取管道非阻塞投递，
		// 队列满时管道内部丢弃并记指标，补扫兜底；这里绝不因记忆抽取阻塞或影响聊天回复。
		if s.memory != nil {
			s.memory.Notify(req.SessionID)
		}
		if err := writeSSE("done", s.doneFramePayload(c, userID, req.SessionID)); err != nil {
			s.log.Warn("turn 已完成但 done 事件发送失败",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"message_id", leaseResult.UserMessage.MessageID,
				"error", err,
			)
		}
		return true
	}

	// 引用来源必须在首个内容增量之前下发，前端才能先渲染引用卡片再逐字上屏。
	sendSources := func(result service.RetrievalResult) error {
		retrievalRequestID = result.RequestID
		payload, err := json.Marshal(newRetrievalSourcesResponse(result))
		if err != nil {
			// 序列化失败不影响回复本身，只是这一轮没有引用卡片。
			s.log.Warn("序列化检索来源失败",
				"trace_id", TraceIDFromContext(c.Request.Context()),
				"session_id", req.SessionID,
				"error", err,
			)
			return nil
		}
		return writeSSE("sources", string(payload))
	}

	assistantContent, err := s.chat.Stream(ctx, service.ChatStreamRequest{
		UserID:        userID,
		SessionID:     req.SessionID,
		TraceID:       TraceIDFromContext(c.Request.Context()),
		History:       history,
		Message:       strings.TrimSpace(req.Message),
		UserMessageID: leaseResult.UserMessage.MessageID,
		CoachTaskID:   req.CoachTaskID,
		LocalDate:     req.LocalDate,
		OnRetrieval:   sendSources,
		OnDelta:       sendDelta,
	})
	if err != nil {
		// 未配置 Key：不算服务故障，当作一段普通回复流出去，方便本地先跑通链路。
		// 兜底回复也要走跟正常回复一样的落库规则，否则刷新后历史里会缺这一轮 assistant 回复。
		if errors.Is(err, llm.ErrNotConfigured) {
			fallback := "（未配置大模型 API Key，请在 .env 填入 DEEPSEEK_API_KEY 后重试）"
			_ = sendDelta(fallback)
			persistAndFinish(fallback)
			return
		}
		s.log.Error("流式对话调用失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", req.SessionID,
			"message_id", leaseResult.UserMessage.MessageID,
			"error", err,
		)
		// 已经开始流，Coach 业务错误使用稳定 SSE code；HTTP 状态已无法改变。
		_ = writeSSE("error", coachSSEErrorPayload(err))
		return
	}

	persistAndFinish(assistantContent)
}

func coachSSEErrorPayload(err error) string {
	code, message := CodeInternal, "对话服务暂时不可用"
	switch {
	case errors.Is(err, service.ErrCoachTaskNotFound):
		code, message = CodeCoachTaskNotFound, "教练任务不存在"
	case errors.Is(err, service.ErrCoachTaskNotStartable):
		code, message = CodeCoachTaskNotStartable, "教练任务当前不可开始"
	case errors.Is(err, service.ErrCoachTaskIDRequired):
		code, message = CodeCoachTaskIDRequired, "当前教练练习必须携带教练任务ID"
	case errors.Is(err, service.ErrCoachTaskMismatch):
		code, message = CodeCoachTaskMismatch, "教练任务ID与当前练习不匹配"
	case errors.Is(err, service.ErrCoachAttemptConflict):
		code, message = CodeCoachAttemptConflict, "该回答已提交过教练分析"
	case errors.Is(err, service.ErrCoachCorrectionPending):
		code, message = CodeCoachCorrectionPending, "复测失败后必须先完成即时纠正"
	case errors.Is(err, service.ErrCoachAnalysisInput):
		code, message = CodeCoachAnalysisInput, "教练分析输入无效"
	case errors.Is(err, service.ErrCoachUnavailable):
		code, message = CodeCoachUnavailable, "教练练习暂时不可用"
	}
	payload, marshalErr := json.Marshal(map[string]any{"code": code, "message": message})
	if marshalErr != nil {
		return `{"code":50000,"message":"对话服务暂时不可用"}`
	}
	return string(payload)
}

// replayCompletedTurn 处理"结果恢复协议"里的回放场景：同一条用户消息对应的 turn 之前已经
// completed，说明上一次已经成功拿到过 assistant 回复并落库，这里只需要把它原样发给客户端，
// 绝不重新调用 LLM（否则同一条消息可能得到两份不一样的回复，还白花一次调用成本）。
//
// userMessageSeq 是这条用户消息自己的 seq；由于一个 Session 同一时刻最多一个进行中的 turn
// （turn 租约保证），它的回复必然是 seq 恰好比它大 1 的那条 assistant 消息。
func (s *Server) replayCompletedTurn(c *gin.Context, userID, sessionID string, resultMessageID string) {
	reply, found, err := s.messages.FindReplyForTurn(c.Request.Context(), userID, sessionID, resultMessageID)
	if err != nil {
		s.log.Error("查询待回放的 assistant 回复失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "对话服务暂时不可用")
		return
	}
	if !found {
		// 数据不一致：租约说这一轮已经 completed，却查不到对应的 assistant 回复。
		// 按服务不可用处理，不能凭空编一个回复，也不能重新调用 LLM 掩盖这个异常。
		s.log.Error("turn 租约状态为 completed，但找不到对应的 assistant 回复",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", sessionID,
			"message_id", resultMessageID,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "对话服务暂时不可用")
		return
	}

	writeSSE, canFlush := setupSSE(c)
	if !canFlush {
		fail(c, http.StatusInternalServerError, CodeInternal, "服务器不支持流式输出")
		return
	}
	if sources := newPersistedRetrievalSourcesResponse(reply.Retrieval); sources != nil {
		payload, marshalErr := json.Marshal(sources)
		if marshalErr != nil {
			s.log.Warn("序列化回放检索来源失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", marshalErr)
		} else if err := writeSSE("sources", string(payload)); err != nil {
			return
		}
	}
	payload, _ := json.Marshal(map[string]string{"delta": reply.Content})
	if err := writeSSE("", string(payload)); err != nil {
		return // 客户端已断开，没必要再发 done。
	}
	_ = writeSSE("done", s.doneFramePayload(c, userID, sessionID))
}

// doneFramePayload 组装 done 帧内容。
//
// 费曼练习状态挂在 done 帧上，而不是单独开一次轮询：这一轮消息很可能刚刚改变了练习状态
// （比如从“等回答”推进到下一个追问），前端必须在同一次交互里拿到最新状态条。
// 任何读取失败都退回空对象——状态条缺一次不影响对话本身。
func (s *Server) doneFramePayload(c *gin.Context, userID, sessionID string) string {
	if s.practice == nil {
		return "{}"
	}
	state, err := s.practice.State(c.Request.Context(), userID, sessionID)
	if err != nil {
		s.log.Warn("读取费曼练习状态失败，本轮不下发状态条",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"session_id", sessionID,
			"error", err,
		)
		return "{}"
	}
	payload, err := json.Marshal(map[string]any{"feynman": newFeynmanPracticeStateResponse(state)})
	if err != nil {
		return "{}"
	}
	return string(payload)
}
