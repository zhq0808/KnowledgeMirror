package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appLogger "KnowledgeMirror/internal/logger"
	"KnowledgeMirror/internal/stt"
)

type fakeRealtimeProvider struct{ session *fakeRealtimeSession }

func (p *fakeRealtimeProvider) Name() string                    { return "fake_realtime" }
func (p *fakeRealtimeProvider) Model() string                   { return "fake-model" }
func (p *fakeRealtimeProvider) NewSession() stt.RealtimeSession { return p.session }

type fakeRealtimeSession struct {
	taskID string
	events chan stt.TranscriptEvent

	mu        sync.Mutex
	sent      [][]byte
	start     func(context.Context) error
	send      func(context.Context, []byte) error
	finish    func(context.Context, *fakeRealtimeSession) error
	closeCall int
	terminal  sync.Once
}

func newFakeRealtimeSession() *fakeRealtimeSession {
	return &fakeRealtimeSession{taskID: "task-realtime-1", events: make(chan stt.TranscriptEvent, 16)}
}

func (s *fakeRealtimeSession) TaskID() string                     { return s.taskID }
func (s *fakeRealtimeSession) Events() <-chan stt.TranscriptEvent { return s.events }
func (s *fakeRealtimeSession) Start(ctx context.Context) error {
	if s.start != nil {
		return s.start(ctx)
	}
	return nil
}
func (s *fakeRealtimeSession) SendAudio(ctx context.Context, pcm []byte) error {
	s.mu.Lock()
	s.sent = append(s.sent, append([]byte(nil), pcm...))
	s.mu.Unlock()
	if s.send != nil {
		return s.send(ctx, pcm)
	}
	return nil
}
func (s *fakeRealtimeSession) Finish(ctx context.Context) error {
	if s.finish != nil {
		return s.finish(ctx, s)
	}
	s.terminate(stt.TranscriptEvent{Type: stt.TranscriptEventFinished, TaskID: s.taskID})
	return nil
}

func (s *fakeRealtimeSession) terminate(event stt.TranscriptEvent) {
	s.terminal.Do(func() {
		s.events <- event
		close(s.events)
	})
}
func (s *fakeRealtimeSession) Close() error {
	s.mu.Lock()
	s.closeCall++
	s.mu.Unlock()
	return nil
}

type fakeRealtimeStream struct {
	inputs chan RealtimeVoiceInput
	events chan RealtimeVoiceEvent
	read   func(context.Context) (RealtimeVoiceInput, error)
	write  func(context.Context, RealtimeVoiceEvent) error
}

func newFakeRealtimeStream(inputs ...RealtimeVoiceInput) *fakeRealtimeStream {
	inputCapacity := len(inputs)
	if inputCapacity < 16 {
		inputCapacity = 16
	}
	inputCh := make(chan RealtimeVoiceInput, inputCapacity)
	for _, input := range inputs {
		inputCh <- input
	}
	return &fakeRealtimeStream{inputs: inputCh, events: make(chan RealtimeVoiceEvent, 32)}
}

func (s *fakeRealtimeStream) Read(ctx context.Context) (RealtimeVoiceInput, error) {
	if s.read != nil {
		return s.read(ctx)
	}
	select {
	case <-ctx.Done():
		return RealtimeVoiceInput{}, ctx.Err()
	case input := <-s.inputs:
		return input, nil
	}
}
func (s *fakeRealtimeStream) Write(ctx context.Context, event RealtimeVoiceEvent) error {
	if s.write != nil {
		return s.write(ctx, event)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.events <- event:
		return nil
	}
}

type recordingRealtimeFinalizer struct {
	mu     sync.Mutex
	inputs []FinalizeRealtimeInput
}

func (f *recordingRealtimeFinalizer) FinalizeRealtime(_ context.Context, input FinalizeRealtimeInput) (VoiceCapture, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	status := VoiceCaptureStatusTranscribed
	if input.TranscriptError != "" || input.Transcript == "" {
		status = VoiceCaptureStatusFailed
	}
	return VoiceCapture{
		CaptureID: "capture-fake", UserID: input.UserID, SessionID: input.SessionID,
		Status: status, SizeBytes: int64(len(input.WAV)), RawTranscript: input.Transcript,
		TranscriptError: input.TranscriptError,
	}, nil
}

func (f *recordingRealtimeFinalizer) lastInput(t *testing.T) FinalizeRealtimeInput {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		t.Fatal("FinalizeRealtime() was not called")
	}
	return f.inputs[len(f.inputs)-1]
}

func testRealtimeLimits() RealtimeVoiceLimits {
	return RealtimeVoiceLimits{
		SampleRate: 16000, MaxDuration: time.Second, MaxAudioBytes: 1024,
		MaxConcurrentStreams: 2, MaxStreamsPerUser: 1,
		AudioQueueFrames: 2, EventQueueSize: 2,
		StartTimeout: 100 * time.Millisecond, FinishTimeout: 100 * time.Millisecond,
		WriteTimeout: 25 * time.Millisecond, IdleTimeout: 25 * time.Millisecond,
	}
}

func TestRealtimeVoiceNormalStopDrainsAudioAndIncludesFinishFinal(t *testing.T) {
	voice, _ := newTestVoiceService(t)
	session := newFakeRealtimeSession()
	session.events <- stt.TranscriptEvent{Type: stt.TranscriptEventResult, Text: "第一句过程", BeginTimeMS: 0}
	session.events <- stt.TranscriptEvent{Type: stt.TranscriptEventResult, Text: "第一句。", SentenceEnd: true, BeginTimeMS: 0}
	session.finish = func(_ context.Context, session *fakeRealtimeSession) error {
		session.mu.Lock()
		sentCount := len(session.sent)
		session.mu.Unlock()
		if sentCount != 2 {
			t.Errorf("Finish() 前 SendAudio 次数 = %d, want 2", sentCount)
		}
		session.events <- stt.TranscriptEvent{Type: stt.TranscriptEventResult, Text: "最后半句。", SentenceEnd: true, BeginTimeMS: 1000}
		session.terminate(stt.TranscriptEvent{Type: stt.TranscriptEventFinished, TaskID: session.taskID})
		return nil
	}
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, voice, RealtimeVoiceLimits{
		SampleRate: 16000, MaxAudioBytes: 1024, AudioQueueFrames: 2, EventQueueSize: 2,
	})
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{1, 0, 2, 0}},
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{3, 0, 4, 0}},
		RealtimeVoiceInput{Type: RealtimeVoiceInputStop},
	)

	capture, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", stream)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if capture.Status != VoiceCaptureStatusTranscribed || capture.RawTranscript != "第一句。最后半句。" {
		t.Fatalf("capture = %+v, want 包含 finish 后 final 的完整文本", capture)
	}
	if capture.SizeBytes != 52 {
		t.Fatalf("WAV size = %d, want 44-byte header + 8-byte PCM", capture.SizeBytes)
	}
	if service.active != 0 || len(service.activeUsers) != 0 {
		t.Fatalf("并发额度未释放: active=%d users=%v", service.active, service.activeUsers)
	}
	var gotCompleted bool
	for len(stream.events) > 0 {
		if event := <-stream.events; event.Type == RealtimeVoiceEventCompleted {
			gotCompleted = true
		}
	}
	if !gotCompleted {
		t.Fatal("正常结束未发送 completed 终态")
	}
}

func TestRealtimeVoiceMetricsAndLogsExcludeSensitivePayloads(t *testing.T) {
	session := newFakeRealtimeSession()
	session.events <- stt.TranscriptEvent{Type: stt.TranscriptEventResult, Text: "绝不能写入日志的转写正文", BeginTimeMS: 0}
	session.events <- stt.TranscriptEvent{Type: stt.TranscriptEventResult, Text: "最终文本", SentenceEnd: true, BeginTimeMS: 0}
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: make([]byte, 3200)},
		RealtimeVoiceInput{Type: RealtimeVoiceInputStop},
	)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	limits := testRealtimeLimits()
	limits.MaxAudioBytes = 4096
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, &recordingRealtimeFinalizer{}, limits, logger)

	if _, err := service.Run(context.Background(), "usr_sensitive", "session_sensitive", "stream_1", stream); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	reservation, err := service.Reserve("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve("usr_1"); !errors.Is(err, ErrRealtimeVoiceUserLimit) {
		t.Fatalf("same-user Reserve() error = %v, want user limit", err)
	}
	reservation.Release()

	metrics := service.Metrics()
	if metrics.ActiveStreams != 0 || metrics.RejectedUserLimit != 1 || metrics.RejectedInstanceLimit != 0 {
		t.Fatalf("capacity metrics = %+v", metrics)
	}
	if metrics.FirstTranscriptCount != 1 || metrics.FinishCount != 1 || metrics.AudioDurationTotalMS != 100 {
		t.Fatalf("latency/audio metrics = %+v", metrics)
	}
	if metrics.UpstreamErrors != 0 {
		t.Fatalf("UpstreamErrors = %d, want 0", metrics.UpstreamErrors)
	}
	logText := logs.String()
	for _, secret := range []string{"绝不能写入日志的转写正文", "最终文本", "usr_sensitive", "session_sensitive", "api-key"} {
		if strings.Contains(logText, secret) {
			t.Fatalf("log contains sensitive payload %q: %s", secret, logText)
		}
	}
	for _, field := range []string{"stream_id", "result_code", "audio_duration_ms", "first_transcript_latency_ms", "finish_latency_ms"} {
		if !strings.Contains(logText, field) {
			t.Fatalf("log missing field %q: %s", field, logText)
		}
	}
}

func TestRealtimeVoiceDebugLogsIncludeProviderTranscript(t *testing.T) {
	session := newFakeRealtimeSession()
	session.events <- stt.TranscriptEvent{
		Type: stt.TranscriptEventResult, Text: "用于定位混合语种的上游原文", SentenceEnd: true,
	}
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: make([]byte, 3200)},
		RealtimeVoiceInput{Type: RealtimeVoiceInputStop},
	)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	limits := testRealtimeLimits()
	limits.MaxAudioBytes = 4096
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, &recordingRealtimeFinalizer{}, limits, logger)

	if _, err := service.Run(context.Background(), "usr_1", "session_1", "stream_debug", stream); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	logText := logs.String()
	if !strings.Contains(logText, "实时 ASR 识别事件文本") ||
		!strings.Contains(logText, "用于定位混合语种的上游原文") {
		t.Fatalf("debug log missing upstream transcript: %s", logText)
	}
}

func TestRealtimeVoiceLogsInheritTraceID(t *testing.T) {
	session := newFakeRealtimeSession()
	session.events <- stt.TranscriptEvent{
		Type: stt.TranscriptEventResult, Text: "trace test", SentenceEnd: true,
	}
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: make([]byte, 3200)},
		RealtimeVoiceInput{Type: RealtimeVoiceInputStop},
	)
	var logs bytes.Buffer
	log := slog.New(&appLogger.ContextHandler{
		Handler: slog.NewJSONHandler(&logs, nil),
	})
	limits := testRealtimeLimits()
	limits.MaxAudioBytes = 4096
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, &recordingRealtimeFinalizer{}, limits, log)
	ctx := context.WithValue(context.Background(), appLogger.TraceIDKey, "trace-realtime-123")

	if _, err := service.Run(ctx, "usr_1", "session_1", "stream_trace", stream); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	logText := logs.String()
	if !strings.Contains(logText, `"trace_id":"trace-realtime-123"`) {
		t.Fatalf("realtime ASR logs missing inherited trace_id: %s", logText)
	}
	if count := strings.Count(logText, `"trace_id":"trace-realtime-123"`); count < 6 {
		t.Fatalf("trace_id should cover the realtime lifecycle, got %d records: %s", count, logText)
	}
}

func TestRealtimeVoiceFailurePathsFinalizeAndReleaseQuota(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeRealtimeSession, *fakeRealtimeStream, *RealtimeVoiceLimits)
		wantErr   error
		wantAudit string
	}{
		{
			name: "upstream failed",
			configure: func(session *fakeRealtimeSession, _ *fakeRealtimeStream, _ *RealtimeVoiceLimits) {
				session.terminate(stt.TranscriptEvent{Type: stt.TranscriptEventFailed, Err: stt.ErrRealtimeUpstream})
			},
			wantErr: stt.ErrRealtimeUpstream, wantAudit: "upstream_failed",
		},
		{
			name: "user cancelled",
			configure: func(_ *fakeRealtimeSession, stream *fakeRealtimeStream, _ *RealtimeVoiceLimits) {
				stream.inputs <- RealtimeVoiceInput{Type: RealtimeVoiceInputCancel}
			},
			wantErr: ErrRealtimeVoiceUserCancelled, wantAudit: "user_cancelled",
		},
		{
			name: "browser disconnected",
			configure: func(_ *fakeRealtimeSession, stream *fakeRealtimeStream, _ *RealtimeVoiceLimits) {
				stream.read = func(context.Context) (RealtimeVoiceInput, error) {
					return RealtimeVoiceInput{}, io.EOF
				}
			},
			wantErr: ErrRealtimeVoiceBrowserClosed, wantAudit: "browser_disconnected",
		},
		{
			name: "idle timeout",
			configure: func(_ *fakeRealtimeSession, stream *fakeRealtimeStream, _ *RealtimeVoiceLimits) {
				stream.read = func(ctx context.Context) (RealtimeVoiceInput, error) {
					<-ctx.Done()
					return RealtimeVoiceInput{}, ctx.Err()
				}
			},
			wantErr: ErrRealtimeVoiceIdleTimeout, wantAudit: "idle_timeout",
		},
		{
			name: "duration limit",
			configure: func(_ *fakeRealtimeSession, stream *fakeRealtimeStream, limits *RealtimeVoiceLimits) {
				limits.MaxDuration = 10 * time.Millisecond
				limits.IdleTimeout = time.Second
				stream.read = func(ctx context.Context) (RealtimeVoiceInput, error) {
					<-ctx.Done()
					return RealtimeVoiceInput{}, ctx.Err()
				}
			},
			wantErr: ErrRealtimeVoiceDurationLimit, wantAudit: "duration_limit",
		},
		{
			name: "slow upstream",
			configure: func(session *fakeRealtimeSession, stream *fakeRealtimeStream, limits *RealtimeVoiceLimits) {
				limits.IdleTimeout = 250 * time.Millisecond
				session.send = func(ctx context.Context, _ []byte) error {
					<-ctx.Done()
					return ctx.Err()
				}
				stream.inputs <- RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{1, 0}}
			},
			wantErr: ErrRealtimeVoiceUpstreamSlow, wantAudit: "upstream_slow",
		},
		{
			name: "finish timeout",
			configure: func(session *fakeRealtimeSession, stream *fakeRealtimeStream, _ *RealtimeVoiceLimits) {
				session.finish = func(ctx context.Context, _ *fakeRealtimeSession) error {
					<-ctx.Done()
					return ctx.Err()
				}
				stream.inputs <- RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{1, 0}}
				stream.inputs <- RealtimeVoiceInput{Type: RealtimeVoiceInputStop}
			},
			wantErr: ErrRealtimeVoiceFinishTimeout, wantAudit: "finish_timeout",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			session := newFakeRealtimeSession()
			stream := newFakeRealtimeStream()
			limits := testRealtimeLimits()
			finalizer := &recordingRealtimeFinalizer{}
			testCase.configure(session, stream, &limits)
			service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, finalizer, limits)

			capture, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", stream)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("Run() error = %v, want %v", err, testCase.wantErr)
			}
			if capture.Status != VoiceCaptureStatusFailed {
				t.Fatalf("capture status = %q, want failed", capture.Status)
			}
			if input := finalizer.lastInput(t); input.TranscriptError != testCase.wantAudit {
				t.Fatalf("TranscriptError = %q, want %q", input.TranscriptError, testCase.wantAudit)
			}
			if service.active != 0 || len(service.activeUsers) != 0 {
				t.Fatalf("失败后并发额度未释放: active=%d users=%v", service.active, service.activeUsers)
			}
			metrics := service.Metrics()
			if testCase.wantAudit == "upstream_failed" || testCase.wantAudit == "upstream_slow" || testCase.wantAudit == "finish_timeout" {
				if metrics.UpstreamErrors != 1 {
					t.Fatalf("UpstreamErrors = %d, want 1 for %s", metrics.UpstreamErrors, testCase.wantAudit)
				}
			} else if metrics.UpstreamErrors != 0 {
				t.Fatalf("UpstreamErrors = %d, want 0 for %s", metrics.UpstreamErrors, testCase.wantAudit)
			}
			session.mu.Lock()
			closeCalls := session.closeCall
			session.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("provider Close() calls = %d, want 1 after all workers exit", closeCalls)
			}
		})
	}
}

func TestRealtimeVoiceStartFailureClosesSessionAndReleasesQuota(t *testing.T) {
	session := newFakeRealtimeSession()
	session.start = func(context.Context) error { return stt.ErrRealtimeStart }
	service := NewRealtimeVoiceService(
		&fakeRealtimeProvider{session: session}, &recordingRealtimeFinalizer{}, testRealtimeLimits(),
	)

	_, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", newFakeRealtimeStream())
	if !errors.Is(err, stt.ErrRealtimeStart) {
		t.Fatalf("Run() error = %v, want ErrRealtimeStart", err)
	}
	if service.active != 0 || len(service.activeUsers) != 0 {
		t.Fatalf("start failure leaked quota: active=%d users=%v", service.active, service.activeUsers)
	}
	session.mu.Lock()
	closeCalls := session.closeCall
	session.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("provider Close() calls = %d, want 1", closeCalls)
	}
}

func TestRealtimeVoiceAudioQueueBackpressureIsBounded(t *testing.T) {
	session := newFakeRealtimeSession()
	session.send = func(ctx context.Context, _ []byte) error {
		<-ctx.Done()
		return ctx.Err()
	}
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{1, 0}},
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{2, 0}},
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{3, 0}},
	)
	limits := testRealtimeLimits()
	limits.AudioQueueFrames = 1
	limits.WriteTimeout = time.Second
	finalizer := &recordingRealtimeFinalizer{}
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, finalizer, limits)

	_, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", stream)
	if !errors.Is(err, ErrRealtimeVoiceAudioBackpressure) {
		t.Fatalf("Run() error = %v, want ErrRealtimeVoiceAudioBackpressure", err)
	}
	if got := finalizer.lastInput(t).TranscriptError; got != "audio_backpressure" {
		t.Fatalf("TranscriptError = %q, want audio_backpressure", got)
	}
	if len(session.sent) > 1 {
		t.Fatalf("blocked upstream accepted %d frames, want at most one in-flight frame", len(session.sent))
	}
}

func TestRealtimeVoiceConcurrencyLimitsAndRelease(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstSession := newFakeRealtimeSession()
	firstStream := newFakeRealtimeStream()
	firstStream.read = func(ctx context.Context) (RealtimeVoiceInput, error) {
		select {
		case <-firstStarted:
		default:
			close(firstStarted)
		}
		select {
		case <-ctx.Done():
			return RealtimeVoiceInput{}, ctx.Err()
		case <-releaseFirst:
			return RealtimeVoiceInput{Type: RealtimeVoiceInputCancel}, nil
		}
	}
	limits := testRealtimeLimits()
	limits.MaxConcurrentStreams = 1
	limits.IdleTimeout = time.Second
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: firstSession}, &recordingRealtimeFinalizer{}, limits)
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", firstStream)
		firstDone <- err
	}()
	<-firstStarted

	_, err := service.Run(context.Background(), "usr_1", "session_2", "stream_2", newFakeRealtimeStream())
	if !errors.Is(err, ErrRealtimeVoiceUserLimit) {
		t.Fatalf("same user Run() error = %v, want ErrRealtimeVoiceUserLimit", err)
	}
	_, err = service.Run(context.Background(), "usr_2", "session_2", "stream_2", newFakeRealtimeStream())
	if !errors.Is(err, ErrRealtimeVoiceInstanceLimit) {
		t.Fatalf("second user Run() error = %v, want ErrRealtimeVoiceInstanceLimit", err)
	}

	close(releaseFirst)
	if err := <-firstDone; !errors.Is(err, ErrRealtimeVoiceUserCancelled) {
		t.Fatalf("first Run() error = %v, want user cancellation", err)
	}
	if service.active != 0 || len(service.activeUsers) != 0 {
		t.Fatalf("并发额度未释放: active=%d users=%v", service.active, service.activeUsers)
	}
}

func TestRealtimeVoiceDownstreamTimeoutFinalizesFailure(t *testing.T) {
	session := newFakeRealtimeSession()
	session.events <- stt.TranscriptEvent{
		Type: stt.TranscriptEventResult, Text: "不能静默丢失的最终句。",
		SentenceEnd: true, BeginTimeMS: 0,
	}
	stream := newFakeRealtimeStream(RealtimeVoiceInput{Type: RealtimeVoiceInputStop})
	stream.write = func(ctx context.Context, event RealtimeVoiceEvent) error {
		if event.Type == RealtimeVoiceEventReady {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	finalizer := &recordingRealtimeFinalizer{}
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, finalizer, testRealtimeLimits())

	_, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", stream)
	if !errors.Is(err, ErrRealtimeVoiceDownstreamSlow) {
		t.Fatalf("Run() error = %v, want ErrRealtimeVoiceDownstreamSlow", err)
	}
	input := finalizer.lastInput(t)
	if input.TranscriptError != "downstream_slow" || input.Transcript != "不能静默丢失的最终句。" {
		t.Fatalf("finalized input = %+v, want final text plus downstream_slow audit", input)
	}
}

func TestRealtimeVoiceAudioLimitBoundsFinalizedWAV(t *testing.T) {
	session := newFakeRealtimeSession()
	stream := newFakeRealtimeStream(
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{1, 0, 2, 0}},
		RealtimeVoiceInput{Type: RealtimeVoiceInputAudio, PCM: []byte{3, 0}},
	)
	limits := testRealtimeLimits()
	limits.MaxAudioBytes = 48
	limits.AudioQueueFrames = 4
	finalizer := &recordingRealtimeFinalizer{}
	service := NewRealtimeVoiceService(&fakeRealtimeProvider{session: session}, finalizer, limits)

	_, err := service.Run(context.Background(), "usr_1", "session_1", "stream_1", stream)
	if !errors.Is(err, ErrRealtimeVoiceAudioLimit) {
		t.Fatalf("Run() error = %v, want ErrRealtimeVoiceAudioLimit", err)
	}
	input := finalizer.lastInput(t)
	if int64(len(input.WAV)) != limits.MaxAudioBytes {
		t.Fatalf("finalized WAV bytes = %d, want hard limit %d", len(input.WAV), limits.MaxAudioBytes)
	}
	if input.TranscriptError != "audio_limit" {
		t.Fatalf("TranscriptError = %q, want audio_limit", input.TranscriptError)
	}
}

func TestRealtimeVoiceEventQueueOverwritesInterimAndPreservesFinal(t *testing.T) {
	queue := newRealtimeVoiceEventQueue(2)
	ctx := context.Background()
	if err := queue.Push(ctx, RealtimeVoiceEvent{Type: RealtimeVoiceEventTranscript, SentenceID: 1, Text: "旧过程"}, false); err != nil {
		t.Fatal(err)
	}
	if err := queue.Push(ctx, RealtimeVoiceEvent{Type: RealtimeVoiceEventTranscript, SentenceID: 1, Text: "新过程"}, false); err != nil {
		t.Fatal(err)
	}
	if err := queue.Push(ctx, RealtimeVoiceEvent{Type: RealtimeVoiceEventTranscript, SentenceID: 1, Text: "最终句", SentenceEnd: true}, true); err != nil {
		t.Fatal(err)
	}
	queue.mu.Lock()
	queued := len(queue.items)
	queue.mu.Unlock()
	if queued != 2 {
		t.Fatalf("event queue length = %d, want bounded length 2", queued)
	}

	first, ok, err := queue.Pop(ctx)
	if err != nil || !ok || first.Text != "新过程" {
		t.Fatalf("first Pop() = %+v, %t, %v; want latest interim", first, ok, err)
	}
	second, ok, err := queue.Pop(ctx)
	if err != nil || !ok || second.Text != "最终句" || !second.SentenceEnd {
		t.Fatalf("second Pop() = %+v, %t, %v; want reliable final", second, ok, err)
	}
}
