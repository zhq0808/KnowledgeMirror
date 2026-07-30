package stt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDashScopeParaformerSessionProtocol(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		authorization string
		workspace     string
		run           dashScopeClientEvent
		audio         []byte
		finish        dashScopeClientEvent
	}
	observed := make(chan observedRequest, 1)
	runReceived := make(chan struct{})
	allowStart := make(chan struct{})
	handlerErr := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			handlerErr <- err
			return
		}
		defer conn.CloseNow()

		var got observedRequest
		got.authorization = r.Header.Get("Authorization")
		got.workspace = r.Header.Get("X-DashScope-WorkSpace")
		if err := readClientJSON(r.Context(), conn, &got.run); err != nil {
			handlerErr <- err
			return
		}
		close(runReceived)
		<-allowStart
		if err := writeServerEvent(r.Context(), conn, got.run.Header.TaskID, "task-started", "", "", nil); err != nil {
			handlerErr <- err
			return
		}

		messageType, audio, err := conn.Read(r.Context())
		if err != nil {
			handlerErr <- err
			return
		}
		got.audio = audio
		if messageType != websocket.MessageBinary {
			handlerErr <- errors.New("audio message is not binary")
			return
		}
		if err := readClientJSON(r.Context(), conn, &got.finish); err != nil {
			handlerErr <- err
			return
		}

		endTime := int64(730)
		if err := writeServerEvent(r.Context(), conn, got.run.Header.TaskID, "result-generated", "", "", &dashScopeTestSentence{
			BeginTime: 170, Text: "Kafka 的幂", SentenceEnd: false,
		}); err != nil {
			handlerErr <- err
			return
		}
		if err := writeServerEvent(r.Context(), conn, got.run.Header.TaskID, "result-generated", "", "", &dashScopeTestSentence{
			BeginTime: 170, EndTime: &endTime, Text: "Kafka 的幂等", SentenceEnd: true,
		}); err != nil {
			handlerErr <- err
			return
		}
		if err := writeServerEvent(r.Context(), conn, got.run.Header.TaskID, "task-finished", "", "", nil); err != nil {
			handlerErr <- err
			return
		}
		observed <- got
	}))
	defer server.Close()

	provider := newTestDashScopeProvider(server.URL)
	session := provider.NewSession()
	startResult := make(chan error, 1)
	go func() { startResult <- session.Start(context.Background()) }()

	<-runReceived
	if err := session.SendAudio(context.Background(), []byte{1, 2}); !errors.Is(err, ErrRealtimeNotStarted) {
		t.Fatalf("SendAudio before task-started error = %v", err)
	}
	close(allowStart)
	if err := <-startResult; err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	audio := []byte{1, 2, 3, 4}
	if err := session.SendAudio(context.Background(), audio); err != nil {
		t.Fatalf("SendAudio failed: %v", err)
	}
	if err := session.Finish(context.Background()); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	var events []TranscriptEvent
	for event := range session.Events() {
		events = append(events, event)
	}
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3: %#v", len(events), events)
	}
	if events[0].Type != TranscriptEventResult || events[0].Text != "Kafka 的幂" || events[0].SentenceEnd {
		t.Fatalf("interim event = %#v", events[0])
	}
	if events[1].Type != TranscriptEventResult || events[1].Text != "Kafka 的幂等" || !events[1].SentenceEnd || events[1].EndTimeMS == nil || *events[1].EndTimeMS != 730 {
		t.Fatalf("final event = %#v", events[1])
	}
	if events[2].Type != TranscriptEventFinished {
		t.Fatalf("terminal event = %#v", events[2])
	}

	select {
	case err := <-handlerErr:
		t.Fatalf("fake upstream failed: %v", err)
	case got := <-observed:
		assertDashScopeRequests(t, got.authorization, got.workspace, got.run, got.audio, got.finish, session.TaskID(), audio)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fake upstream")
	}
}

func TestDashScopeParaformerHandshake401DoesNotLeakAPIKey(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected "+secret, http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := NewDashScopeParaformerProvider(DashScopeParaformerOptions{
		APIKey:       secret,
		WebSocketURL: websocketURL(server.URL),
		StartTimeout: time.Second,
	})
	err := provider.NewSession().Start(context.Background())
	if !errors.Is(err, ErrRealtimeStart) {
		t.Fatalf("Start error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked API key: %v", err)
	}
}

func TestDashScopeParaformerMapsTaskFailedWithoutLeakingMessage(t *testing.T) {
	t.Parallel()
	const secretMessage = "bad request contains private upstream detail"
	server := newDashScopeTestServer(t, func(ctx context.Context, conn *websocket.Conn, taskID string) error {
		if err := writeServerEvent(ctx, conn, taskID, "task-started", "", "", nil); err != nil {
			return err
		}
		return writeServerEvent(ctx, conn, taskID, "task-failed", "CLIENT_ERROR", secretMessage, nil)
	})
	defer server.Close()

	session := newTestDashScopeProvider(server.URL).NewSession()
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	event := <-session.Events()
	if event.Type != TranscriptEventFailed || event.UpstreamCode != "CLIENT_ERROR" || !errors.Is(event.Err, ErrRealtimeUpstream) {
		t.Fatalf("failed event = %#v", event)
	}
	if strings.Contains(event.Err.Error(), secretMessage) {
		t.Fatalf("event error leaked upstream message: %v", event.Err)
	}
}

func TestDashScopeParaformerStartTimeout(t *testing.T) {
	t.Parallel()
	server := newDashScopeTestServer(t, func(ctx context.Context, conn *websocket.Conn, _ string) error {
		_, _, err := conn.Read(ctx)
		return err
	})
	defer server.Close()

	provider := NewDashScopeParaformerProvider(DashScopeParaformerOptions{
		APIKey:       "test-key",
		WebSocketURL: websocketURL(server.URL),
		StartTimeout: 30 * time.Millisecond,
	})
	err := provider.NewSession().Start(context.Background())
	if !errors.Is(err, ErrRealtimeStart) || !strings.Contains(err.Error(), "task-started") {
		t.Fatalf("Start error = %v", err)
	}
}

func TestDashScopeParaformerFinishTimeout(t *testing.T) {
	t.Parallel()
	server := newDashScopeTestServer(t, func(ctx context.Context, conn *websocket.Conn, taskID string) error {
		if err := writeServerEvent(ctx, conn, taskID, "task-started", "", "", nil); err != nil {
			return err
		}
		var finish dashScopeClientEvent
		if err := readClientJSON(ctx, conn, &finish); err != nil {
			return err
		}
		_, _, err := conn.Read(ctx)
		return err
	})
	defer server.Close()

	provider := NewDashScopeParaformerProvider(DashScopeParaformerOptions{
		APIKey:        "test-key",
		WebSocketURL:  websocketURL(server.URL),
		StartTimeout:  time.Second,
		FinishTimeout: 30 * time.Millisecond,
	})
	session := provider.NewSession()
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	err := session.Finish(context.Background())
	if !errors.Is(err, ErrRealtimeFinish) || !strings.Contains(err.Error(), "task-finished") {
		t.Fatalf("Finish error = %v", err)
	}
}

func TestDashScopeParaformerCloseUnblocksSaturatedEventPublisher(t *testing.T) {
	session := &dashScopeParaformerSession{
		state:    realtimeStateStreaming,
		events:   make(chan TranscriptEvent, 1),
		started:  make(chan error, 1),
		done:     make(chan error, 1),
		shutdown: make(chan struct{}),
	}
	session.events <- TranscriptEvent{Type: TranscriptEventResult, Text: "fills buffer"}
	publishDone := make(chan struct{})
	go func() {
		session.publish(TranscriptEvent{Type: TranscriptEventResult, Text: "blocked result"})
		close(publishDone)
	}()

	closeDone := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind a saturated event publisher")
	}
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("event publisher did not exit after Close")
	}
	if result := <-session.done; !errors.Is(result, ErrRealtimeClosed) {
		t.Fatalf("done result = %v, want ErrRealtimeClosed", result)
	}
}

func TestDashScopeParaformerTerminalEventWaitsForBufferAndIsNotDropped(t *testing.T) {
	session := &dashScopeParaformerSession{
		state:    realtimeStateStreaming,
		events:   make(chan TranscriptEvent, 1),
		started:  make(chan error, 1),
		done:     make(chan error, 1),
		shutdown: make(chan struct{}),
	}
	session.events <- TranscriptEvent{Type: TranscriptEventResult, Text: "final sentence"}
	terminateDone := make(chan struct{})
	go func() {
		session.terminate(TranscriptEvent{Type: TranscriptEventFinished}, nil)
		close(terminateDone)
	}()

	select {
	case <-terminateDone:
		t.Fatal("terminal event was silently dropped from a saturated buffer")
	case <-time.After(20 * time.Millisecond):
	}
	if event := <-session.events; event.Type != TranscriptEventResult {
		t.Fatalf("first event = %#v, want buffered result", event)
	}
	select {
	case <-terminateDone:
	case <-time.After(time.Second):
		t.Fatal("terminate did not continue after event buffer was drained")
	}
	event, ok := <-session.events
	if !ok || event.Type != TranscriptEventFinished {
		t.Fatalf("terminal event = %#v, open=%t; want finished", event, ok)
	}
}

func assertDashScopeRequests(t *testing.T, authorization, workspace string, run dashScopeClientEvent, gotAudio []byte, finish dashScopeClientEvent, taskID string, wantAudio []byte) {
	t.Helper()
	if authorization != "Bearer test-key" || workspace != "workspace-1" {
		t.Fatalf("headers = Authorization %q, Workspace %q", authorization, workspace)
	}
	if run.Header.Action != "run-task" || run.Header.TaskID != taskID || run.Header.Streaming != "duplex" {
		t.Fatalf("run header = %#v", run.Header)
	}
	if run.Payload.TaskGroup != "audio" || run.Payload.Task != "asr" || run.Payload.Function != "recognition" || run.Payload.Model != defaultParaformerModel {
		t.Fatalf("run payload = %#v", run.Payload)
	}
	if run.Payload.Parameters == nil || run.Payload.Parameters.Format != "pcm" || run.Payload.Parameters.SampleRate != 16000 {
		t.Fatalf("run parameters = %#v", run.Payload.Parameters)
	}
	if string(gotAudio) != string(wantAudio) {
		t.Fatalf("audio = %v, want %v", gotAudio, wantAudio)
	}
	if finish.Header.Action != "finish-task" || finish.Header.TaskID != taskID || finish.Header.Streaming != "duplex" || finish.Payload.Input == nil {
		t.Fatalf("finish request = %#v", finish)
	}
}

func newTestDashScopeProvider(serverURL string) *DashScopeParaformerProvider {
	return NewDashScopeParaformerProvider(DashScopeParaformerOptions{
		APIKey:        "test-key",
		WorkspaceID:   "workspace-1",
		WebSocketURL:  websocketURL(serverURL),
		StartTimeout:  time.Second,
		FinishTimeout: time.Second,
	})
}

func newDashScopeTestServer(t *testing.T, afterRun func(context.Context, *websocket.Conn, string) error) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("Accept failed: %v", err)
			return
		}
		defer conn.CloseNow()
		var run dashScopeClientEvent
		if err := readClientJSON(r.Context(), conn, &run); err != nil {
			t.Errorf("read run-task failed: %v", err)
			return
		}
		if err := afterRun(r.Context(), conn, run.Header.TaskID); err != nil && websocket.CloseStatus(err) == -1 {
			t.Errorf("fake upstream failed: %v", err)
		}
	}))
}

func readClientJSON(ctx context.Context, conn *websocket.Conn, target any) error {
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if messageType != websocket.MessageText {
		return errors.New("client control message is not text")
	}
	return json.Unmarshal(payload, target)
}

type dashScopeTestSentence struct {
	BeginTime   int64
	EndTime     *int64
	Text        string
	SentenceEnd bool
}

func writeServerEvent(ctx context.Context, conn *websocket.Conn, taskID, event, errorCode, errorMessage string, sentence *dashScopeTestSentence) error {
	message := map[string]any{
		"header": map[string]any{
			"task_id":       taskID,
			"event":         event,
			"error_code":    errorCode,
			"error_message": errorMessage,
		},
		"payload": map[string]any{},
	}
	if sentence != nil {
		message["payload"] = map[string]any{
			"output": map[string]any{
				"sentence": map[string]any{
					"begin_time":   sentence.BeginTime,
					"end_time":     sentence.EndTime,
					"text":         sentence.Text,
					"sentence_end": sentence.SentenceEnd,
				},
			},
		}
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

func websocketURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http")
}
