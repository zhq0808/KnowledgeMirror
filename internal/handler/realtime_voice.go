package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"healthAgent/internal/service"
)

const (
	realtimeVoicePath        = "/api/v1/voice/realtime"
	realtimeControlFrameSize = int64(4 << 10)
	realtimePingInterval     = 15 * time.Second
	realtimePingTimeout      = 5 * time.Second
)

type realtimeVoiceClientControl struct {
	Type string `json:"type"`
}

type realtimeVoiceServerEvent struct {
	Type        service.RealtimeVoiceEventType `json:"type"`
	Seq         int64                          `json:"seq,omitempty"`
	StreamID    string                         `json:"stream_id,omitempty"`
	SampleRate  int                            `json:"sample_rate,omitempty"`
	SentenceID  int                            `json:"sentence_id,omitempty"`
	Text        string                         `json:"text,omitempty"`
	SentenceEnd bool                           `json:"sentence_end,omitempty"`
	BeginTimeMS int64                          `json:"begin_time_ms,omitempty"`
	EndTimeMS   *int64                         `json:"end_time_ms,omitempty"`
	Capture     *voiceCaptureReply             `json:"capture,omitempty"`
	Code        string                         `json:"code,omitempty"`
	Message     string                         `json:"message,omitempty"`
	Retryable   bool                           `json:"retryable,omitempty"`
}

type realtimeVoiceWebSocketStream struct {
	conn          *websocket.Conn
	maxFrameBytes int64
	writeMu       sync.Mutex
	stateMu       sync.Mutex
	ready         bool
	terminal      bool
}

func (s *Server) realtimeVoiceHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	sessionID := strings.TrimSpace(c.Query("session_id"))
	if !validSessionID(sessionID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "session_id 格式错误")
		return
	}
	if err := s.sessions.RequireOwnedActive(c.Request.Context(), userID, sessionID); err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			fail(c, http.StatusNotFound, CodeNotFound, "会话不存在")
			return
		}
		s.log.Error("校验实时语音会话归属失败", "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, "校验会话失败")
		return
	}
	if !sameWebSocketOrigin(c.Request) {
		fail(c, http.StatusForbidden, CodeForbidden, "WebSocket Origin 不允许")
		return
	}

	reservation, err := s.realtimeVoice.Reserve(userID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrRealtimeVoiceUserLimit):
			fail(c, http.StatusTooManyRequests, CodeConflict, "同一用户已有实时语音流")
		case errors.Is(err, service.ErrRealtimeVoiceInstanceLimit):
			fail(c, http.StatusTooManyRequests, CodeConflict, "实时语音服务并发已满")
		default:
			fail(c, http.StatusInternalServerError, CodeInternal, "实时语音服务暂时不可用")
		}
		return
	}
	defer reservation.Release()

	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	limits := s.realtimeVoice.Limits()
	readLimit := realtimeControlFrameSize
	if limits.MaxFrameBytes > readLimit {
		readLimit = limits.MaxFrameBytes
	}
	conn.SetReadLimit(readLimit)
	stream := &realtimeVoiceWebSocketStream{conn: conn, maxFrameBytes: limits.MaxFrameBytes}

	runCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	go stream.keepAlive(runCtx)

	streamID := uuid.NewString()
	_, runErr := s.realtimeVoice.RunReserved(runCtx, reservation, sessionID, streamID, stream)
	if runErr != nil && stream.canWriteTerminalError() {
		_ = stream.Write(context.Background(), service.RealtimeVoiceEvent{
			Type: service.RealtimeVoiceEventError, Code: "upstream_unavailable",
			Message: "实时语音服务暂时不可用", Retryable: true,
		})
	}
}

func sameWebSocketOrigin(request *http.Request) bool {
	rawOrigin := strings.TrimSpace(request.Header.Get("Origin"))
	if rawOrigin == "" {
		return false
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded != "" {
		scheme = strings.ToLower(forwarded)
	}
	return strings.EqualFold(origin.Scheme, scheme) && strings.EqualFold(origin.Host, request.Host)
}

func (s *realtimeVoiceWebSocketStream) Read(ctx context.Context) (service.RealtimeVoiceInput, error) {
	messageType, payload, err := s.conn.Read(ctx)
	if err != nil {
		return service.RealtimeVoiceInput{}, err
	}
	switch messageType {
	case websocket.MessageBinary:
		if len(payload) == 0 || len(payload)%2 != 0 || int64(len(payload)) > s.maxFrameBytes {
			return service.RealtimeVoiceInput{}, &service.VoiceInputError{Message: "PCM 音频帧必须是上限内的非空偶数字节"}
		}
		return service.RealtimeVoiceInput{Type: service.RealtimeVoiceInputAudio, PCM: payload}, nil
	case websocket.MessageText:
		if int64(len(payload)) > realtimeControlFrameSize {
			return service.RealtimeVoiceInput{}, &service.VoiceInputError{Message: "实时语音控制帧大小超过上限"}
		}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		var control realtimeVoiceClientControl
		if err := decoder.Decode(&control); err != nil || decoder.Decode(&struct{}{}) != io.EOF || control.Type != "stop" {
			return service.RealtimeVoiceInput{}, &service.VoiceInputError{Message: "实时语音只接受 stop 控制帧"}
		}
		return service.RealtimeVoiceInput{Type: service.RealtimeVoiceInputStop}, nil
	default:
		return service.RealtimeVoiceInput{}, &service.VoiceInputError{Message: "不支持的 WebSocket 消息类型"}
	}
}

func (s *realtimeVoiceWebSocketStream) Write(ctx context.Context, event service.RealtimeVoiceEvent) error {
	reply := realtimeVoiceServerEvent{
		Type: event.Type, Seq: event.Seq, StreamID: event.StreamID, SampleRate: event.SampleRate,
		SentenceID: event.SentenceID, Text: event.Text, SentenceEnd: event.SentenceEnd,
		BeginTimeMS: event.BeginTimeMS, EndTimeMS: event.EndTimeMS,
		Code: event.Code, Message: event.Message, Retryable: event.Retryable,
	}
	if event.Capture != nil {
		capture := toVoiceCaptureReply(*event.Capture)
		reply.Capture = &capture
	}
	payload, err := json.Marshal(reply)
	if err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return err
	}
	s.stateMu.Lock()
	if event.Type == service.RealtimeVoiceEventReady {
		s.ready = true
	}
	if event.Type == service.RealtimeVoiceEventCompleted || event.Type == service.RealtimeVoiceEventError {
		s.terminal = true
	}
	s.stateMu.Unlock()
	return nil
}

func (s *realtimeVoiceWebSocketStream) canWriteTerminalError() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return !s.terminal
}

func (s *realtimeVoiceWebSocketStream) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(realtimePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, realtimePingTimeout)
			s.writeMu.Lock()
			err := s.conn.Ping(pingCtx)
			s.writeMu.Unlock()
			cancel()
			if err != nil {
				_ = s.conn.Close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}
