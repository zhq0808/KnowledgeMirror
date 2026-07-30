package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"healthAgent/internal/config"
	"healthAgent/internal/service"
	"healthAgent/internal/stt"
)

const realtimeTestCookieName = "interview_guest_realtime_test"

type handlerRealtimeProvider struct{}

func (*handlerRealtimeProvider) Name() string  { return "fake_realtime" }
func (*handlerRealtimeProvider) Model() string { return "fake-model" }
func (*handlerRealtimeProvider) NewSession() stt.RealtimeSession {
	return &handlerRealtimeSession{
		taskID: uuid.NewString(), events: make(chan stt.TranscriptEvent, 8),
	}
}

type handlerRealtimeSession struct {
	taskID string
	events chan stt.TranscriptEvent
	once   sync.Once
}

func (s *handlerRealtimeSession) Start(context.Context) error { return nil }
func (s *handlerRealtimeSession) TaskID() string              { return s.taskID }
func (s *handlerRealtimeSession) Events() <-chan stt.TranscriptEvent {
	return s.events
}
func (s *handlerRealtimeSession) SendAudio(context.Context, []byte) error {
	s.once.Do(func() {
		s.events <- stt.TranscriptEvent{
			Type: stt.TranscriptEventResult, Text: "Kafka 的幂等", BeginTimeMS: 0,
		}
	})
	return nil
}
func (s *handlerRealtimeSession) Finish(context.Context) error {
	s.events <- stt.TranscriptEvent{
		Type: stt.TranscriptEventResult, Text: "Kafka 的幂等需要覆盖生产和消费两端。",
		SentenceEnd: true, BeginTimeMS: 0,
	}
	s.events <- stt.TranscriptEvent{Type: stt.TranscriptEventFinished}
	return nil
}
func (*handlerRealtimeSession) Close() error { return nil }

type handlerRealtimeFinalizer struct {
	inputs chan service.FinalizeRealtimeInput
}

func (f *handlerRealtimeFinalizer) FinalizeRealtime(_ context.Context, input service.FinalizeRealtimeInput) (service.VoiceCapture, error) {
	f.inputs <- input
	status := service.VoiceCaptureStatusTranscribed
	if input.TranscriptError != "" || strings.TrimSpace(input.Transcript) == "" {
		status = service.VoiceCaptureStatusFailed
	}
	return service.VoiceCapture{
		CaptureID: uuid.NewString(), UserID: input.UserID, SessionID: input.SessionID,
		Status: status, RawTranscript: input.Transcript, TranscriptError: input.TranscriptError,
		CreatedAt: time.Now(),
	}, nil
}

type realtimeHandlerRig struct {
	server       *httptest.Server
	finalizer    *handlerRealtimeFinalizer
	ownerToken   string
	otherToken   string
	ownerSession string
	otherSession string
}

func newRealtimeHandlerRig(t *testing.T, maxConcurrent int) *realtimeHandlerRig {
	t.Helper()
	identityRepository := &handlerIdentityRepository{byHash: make(map[string]handlerGuestRecord)}
	identityService := service.NewIdentityService(identityRepository, time.Hour)
	owner, err := identityService.EnsureGuest(t.Context(), "")
	if err != nil {
		t.Fatalf("create owner identity: %v", err)
	}
	other, err := identityService.EnsureGuest(t.Context(), "")
	if err != nil {
		t.Fatalf("create other identity: %v", err)
	}
	ownerSession := "session_0123456789abcdef0123456789abcdef"
	otherSession := "session_fedcba9876543210fedcba9876543210"
	sessions := service.NewSessionService(&handlerSessionRepository{owners: map[string]string{
		ownerSession: owner.UserID,
		otherSession: other.UserID,
	}})
	finalizer := &handlerRealtimeFinalizer{inputs: make(chan service.FinalizeRealtimeInput, 16)}
	realtimeVoice := service.NewRealtimeVoiceService(
		&handlerRealtimeProvider{}, finalizer,
		service.RealtimeVoiceLimits{
			MaxFrameBytes: 3200, MaxConcurrentStreams: maxConcurrent, MaxStreamsPerUser: 1,
			IdleTimeout: time.Second, WriteTimeout: time.Second, FinishTimeout: time.Second,
		},
	)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(
		nil, identityService, sessions, nil, nil, nil, nil, nil, nil, nil, nil,
		realtimeVoice, nil, nil,
		config.IdentityConfig{GuestCookieName: realtimeTestCookieName}, log,
	)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &realtimeHandlerRig{
		server: httpServer, finalizer: finalizer,
		ownerToken: owner.Token, otherToken: other.Token,
		ownerSession: ownerSession, otherSession: otherSession,
	}
}

func TestRealtimeVoiceHandshakeRejectsUnauthenticatedCrossUserAndInvalidOrigin(t *testing.T) {
	rig := newRealtimeHandlerRig(t, 2)
	tests := []struct {
		name      string
		token     string
		sessionID string
		origin    string
		want      int
	}{
		{name: "unauthenticated", sessionID: rig.ownerSession, origin: rig.server.URL, want: http.StatusUnauthorized},
		{name: "cross user session", token: rig.ownerToken, sessionID: rig.otherSession, origin: rig.server.URL, want: http.StatusNotFound},
		{name: "invalid origin", token: rig.ownerToken, sessionID: rig.ownerSession, origin: "https://attacker.example", want: http.StatusForbidden},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, response, err := dialRealtime(t.Context(), rig, testCase.token, testCase.sessionID, testCase.origin)
			if err == nil {
				t.Fatal("websocket upgrade unexpectedly succeeded")
			}
			if response == nil {
				t.Fatalf("missing HTTP rejection response: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != testCase.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, testCase.want)
			}
		})
	}
}

func TestRealtimeVoiceRejectsInvalidFrameWithProtocolError(t *testing.T) {
	rig := newRealtimeHandlerRig(t, 2)
	conn := mustDialRealtime(t, rig, rig.ownerToken, rig.ownerSession)
	defer conn.CloseNow()
	if event := readRealtimeEvent(t, conn); event.Type != service.RealtimeVoiceEventReady {
		t.Fatalf("first event = %q, want ready", event.Type)
	}
	if err := conn.Write(t.Context(), websocket.MessageBinary, []byte{1}); err != nil {
		t.Fatalf("write invalid frame: %v", err)
	}
	event := readRealtimeEvent(t, conn)
	if event.Type != service.RealtimeVoiceEventError || event.Code != "invalid_frame" {
		t.Fatalf("event = %#v, want invalid_frame error", event)
	}
}

func TestRealtimeVoiceRejectsSameUserSecondStreamAndInstanceOverloadBeforeUpgrade(t *testing.T) {
	rig := newRealtimeHandlerRig(t, 1)
	first := mustDialRealtime(t, rig, rig.ownerToken, rig.ownerSession)
	defer first.CloseNow()
	if event := readRealtimeEvent(t, first); event.Type != service.RealtimeVoiceEventReady {
		t.Fatalf("first event = %q, want ready", event.Type)
	}

	for _, testCase := range []struct {
		name, token, sessionID string
	}{
		{name: "same user", token: rig.ownerToken, sessionID: rig.ownerSession},
		{name: "instance full", token: rig.otherToken, sessionID: rig.otherSession},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, response, err := dialRealtime(t.Context(), rig, testCase.token, testCase.sessionID, rig.server.URL)
			if err == nil || response == nil {
				t.Fatalf("upgrade result err=%v response=%v, want HTTP rejection", err, response)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", response.StatusCode)
			}
		})
	}

	writeStop(t, first)
	readUntilCompleted(t, first)
}

func TestRealtimeVoiceWebSocketReadyAudioTranscriptStopCompleted(t *testing.T) {
	rig := newRealtimeHandlerRig(t, 2)
	conn := mustDialRealtime(t, rig, rig.ownerToken, rig.ownerSession)
	defer conn.CloseNow()

	ready := readRealtimeEvent(t, conn)
	if ready.Type != service.RealtimeVoiceEventReady || ready.StreamID == "" || ready.SampleRate != 16000 {
		t.Fatalf("ready event = %#v", ready)
	}
	if err := conn.Write(t.Context(), websocket.MessageBinary, []byte{1, 0, 2, 0}); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	interim := readRealtimeEvent(t, conn)
	if interim.Type != service.RealtimeVoiceEventTranscript || interim.SentenceEnd {
		t.Fatalf("interim event = %#v", interim)
	}
	writeStop(t, conn)
	completed := readUntilCompleted(t, conn)
	if completed.Capture == nil || completed.Capture.Status != service.VoiceCaptureStatusTranscribed ||
		completed.Capture.Transcript != "Kafka 的幂等需要覆盖生产和消费两端。" {
		t.Fatalf("completed event = %#v", completed)
	}
}

func TestRealtimeVoiceDisconnectFinalizesFailureAndReleasesQuota(t *testing.T) {
	rig := newRealtimeHandlerRig(t, 1)
	conn := mustDialRealtime(t, rig, rig.ownerToken, rig.ownerSession)
	if event := readRealtimeEvent(t, conn); event.Type != service.RealtimeVoiceEventReady {
		t.Fatalf("first event = %q, want ready", event.Type)
	}
	conn.CloseNow()

	select {
	case input := <-rig.finalizer.inputs:
		if input.TranscriptError != "browser_disconnected" {
			t.Fatalf("transcript error = %q, want browser_disconnected", input.TranscriptError)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect was not finalized")
	}

	deadline := time.Now().Add(time.Second)
	for {
		retry, response, err := dialRealtime(t.Context(), rig, rig.ownerToken, rig.ownerSession, rig.server.URL)
		if err == nil {
			retry.CloseNow()
			break
		}
		if response != nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota was not released after disconnect: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func dialRealtime(ctx context.Context, rig *realtimeHandlerRig, token, sessionID, origin string) (*websocket.Conn, *http.Response, error) {
	headers := make(http.Header)
	headers.Set("Origin", origin)
	if token != "" {
		headers.Set("Cookie", realtimeTestCookieName+"="+token)
	}
	websocketURL := "ws" + strings.TrimPrefix(rig.server.URL, "http") + realtimeVoicePath + "?session_id=" + sessionID
	return websocket.Dial(ctx, websocketURL, &websocket.DialOptions{HTTPHeader: headers})
}

func mustDialRealtime(t *testing.T, rig *realtimeHandlerRig, token, sessionID string) *websocket.Conn {
	t.Helper()
	conn, response, err := dialRealtime(t.Context(), rig, token, sessionID, rig.server.URL)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("dial realtime websocket: %v", err)
	}
	return conn
}

func readRealtimeEvent(t *testing.T, conn *websocket.Conn) realtimeVoiceServerEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var event realtimeVoiceServerEvent
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read realtime event: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v, want text", messageType)
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode realtime event: %v", err)
	}
	return event
}

func writeStop(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	payload, err := json.Marshal(realtimeVoiceClientControl{Type: "stop"})
	if err != nil {
		t.Fatalf("encode stop: %v", err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, payload); err != nil {
		t.Fatalf("write stop: %v", err)
	}
}

func readUntilCompleted(t *testing.T, conn *websocket.Conn) realtimeVoiceServerEvent {
	t.Helper()
	for {
		event := readRealtimeEvent(t, conn)
		if event.Type == service.RealtimeVoiceEventError {
			t.Fatalf("unexpected error event: %#v", event)
		}
		if event.Type == service.RealtimeVoiceEventCompleted {
			return event
		}
	}
}
