package service

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"KnowledgeMirror/internal/stt"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

type fakeVoiceRepository struct {
	mu        sync.Mutex
	rows      map[string]*VoiceCapture
	bySHA     map[string]string
	bindCalls int
	bindErr   error
}

func newFakeVoiceRepository() *fakeVoiceRepository {
	return &fakeVoiceRepository{rows: map[string]*VoiceCapture{}, bySHA: map[string]string{}}
}

func (r *fakeVoiceRepository) Claim(ctx context.Context, params ClaimVoiceCaptureParams) (VoiceCapture, bool, error) {
	if err := ctx.Err(); err != nil {
		return VoiceCapture{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := params.SessionID + "|" + hex.EncodeToString(params.SHA256)
	if existingID, found := r.bySHA[key]; found {
		row := r.rows[existingID]
		stale := row.Status == VoiceCaptureStatusTranscribing && row.UpdatedAt.Before(params.StaleBefore)
		if !stale {
			return *row, false, nil
		}
		row.Status = VoiceCaptureStatusTranscribing
		row.RawTranscript = ""
		row.TranscriptError = ""
		row.UpdatedAt = time.Now()
		return *row, true, nil
	}
	row := &VoiceCapture{
		CaptureID: params.CaptureID,
		UserID:    params.UserID,
		SessionID: params.SessionID,
		Status:    VoiceCaptureStatusTranscribing,
		MIMEType:  params.MIMEType,
		SizeBytes: params.SizeBytes,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	r.rows[params.CaptureID] = row
	r.bySHA[key] = params.CaptureID
	return *row, true, nil
}

func (r *fakeVoiceRepository) Complete(ctx context.Context, params CompleteVoiceCaptureParams) (VoiceCapture, error) {
	if err := ctx.Err(); err != nil {
		return VoiceCapture{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, found := r.rows[params.CaptureID]
	if !found {
		return VoiceCapture{}, ErrVoiceCaptureNotFound
	}
	if row.Status != VoiceCaptureStatusTranscribing {
		return *row, nil
	}
	row.Status = params.Status
	row.RawTranscript = params.RawTranscript
	row.TranscriptError = params.TranscriptError
	row.Confidence = params.Confidence
	row.AmbiguousTerms = params.AmbiguousTerms
	row.NeedsConfirmation = params.NeedsConfirmation
	row.ConfirmationReason = params.ConfirmationReason
	row.STTProvider = params.STTProvider
	row.STTModel = params.STTModel
	row.STTRequestID = params.STTRequestID
	row.UpdatedAt = time.Now()
	return *row, nil
}

func (r *fakeVoiceRepository) Get(_ context.Context, _, _, captureID string) (VoiceCapture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, found := r.rows[captureID]
	if !found {
		return VoiceCapture{}, ErrVoiceCaptureNotFound
	}
	return *row, nil
}

func (r *fakeVoiceRepository) BindMessage(_ context.Context, _, _, captureID, messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindCalls++
	if r.bindErr != nil {
		return r.bindErr
	}
	row, found := r.rows[captureID]
	if !found || row.Status != VoiceCaptureStatusTranscribed {
		return ErrVoiceCaptureNotFound
	}
	if row.MessageID != "" && row.MessageID != messageID {
		return ErrVoiceCaptureNotFound
	}
	row.MessageID = messageID
	return nil
}

type fakeUploadSTTProvider struct {
	transcript stt.Transcript
	err        error
	calls      int
}

func (p *fakeUploadSTTProvider) Name() string { return "fake_upload" }

func (p *fakeUploadSTTProvider) Transcribe(_ context.Context, _ []byte, _ string) (stt.Transcript, error) {
	p.calls++
	return p.transcript, p.err
}

func newTestVoiceServiceWithProvider(t *testing.T, provider stt.Provider) (*VoiceCaptureService, *fakeVoiceRepository) {
	t.Helper()
	repo := newFakeVoiceRepository()
	glossary := NewTermGlossary([]string{"幂等|密等,读百", "RocketMQ|火箭MQ"})
	limits := VoiceLimits{
		MaxAudioBytes:      1 << 20,
		MaxDurationMS:      180000,
		MaxTranscriptChars: 8000,
		MinConfidence:      0.6,
		MaxAmbiguousTerms:  5,
	}
	return NewVoiceCaptureService(repo, provider, glossary, limits, discardLogger()), repo
}

func newTestVoiceService(t *testing.T) (*VoiceCaptureService, *fakeVoiceRepository) {
	return newTestVoiceServiceWithProvider(t, nil)
}

func TestVoiceCaptureTranscribesPersistsAndDeduplicates(t *testing.T) {
	confidence := 0.9
	provider := &fakeUploadSTTProvider{transcript: stt.Transcript{
		Text: "这里用密等保证请求不会重复执行", Provider: "fake_upload",
		Model: "fake-model", RequestID: "req-1", Confidence: &confidence,
	}}
	voice, _ := newTestVoiceServiceWithProvider(t, provider)
	durationMs := 1200

	first, err := voice.Capture(context.Background(), "usr_1", "session_1", []byte("wav-data"), "audio/wav", &durationMs)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if first.Status != VoiceCaptureStatusTranscribed || first.RawTranscript == "" {
		t.Fatalf("capture = %+v, want transcribed", first)
	}
	if first.STTModel != "fake-model" || first.STTRequestID != "req-1" {
		t.Fatalf("STT metadata = %+v", first)
	}
	if len(first.AmbiguousTerms) != 1 || first.AmbiguousTerms[0].Term != "幂等" {
		t.Fatalf("ambiguous terms = %+v", first.AmbiguousTerms)
	}

	second, err := voice.Capture(context.Background(), "usr_1", "session_1", []byte("wav-data"), "audio/wav", &durationMs)
	if err != nil {
		t.Fatalf("duplicate Capture() error = %v", err)
	}
	if second.CaptureID != first.CaptureID || provider.calls != 1 {
		t.Fatalf("duplicate capture=%q first=%q provider calls=%d", second.CaptureID, first.CaptureID, provider.calls)
	}
}

func TestVoiceCapturePersistsProviderFailure(t *testing.T) {
	provider := &fakeUploadSTTProvider{err: errors.New("upstream unavailable")}
	voice, _ := newTestVoiceServiceWithProvider(t, provider)
	capture, err := voice.Capture(context.Background(), "usr_1", "session_1", []byte("wav-data"), "audio/wav", nil)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if capture.Status != VoiceCaptureStatusFailed || capture.TranscriptError != "upstream unavailable" {
		t.Fatalf("capture = %+v, want failed provider result", capture)
	}
}

func TestVoiceCaptureRequiresUploadProvider(t *testing.T) {
	voice, _ := newTestVoiceService(t)
	if voice.UploadEnabled() {
		t.Fatal("nil provider must not report upload enabled")
	}
	if _, err := voice.Capture(context.Background(), "usr_1", "session_1", []byte("wav"), "audio/wav", nil); err == nil {
		t.Fatal("Capture() without provider should fail")
	}
}

func TestVoiceFinalizeRealtimePersistsProviderResultWithoutCallingFileSTT(t *testing.T) {
	voice, _ := newTestVoiceService(t)
	durationMs := 2300

	capture, err := voice.FinalizeRealtime(context.Background(), FinalizeRealtimeInput{
		UserID: "usr_1", SessionID: "session_1", WAV: []byte("complete-wav"),
		Transcript: "这里用密等保证请求不会重复执行", Provider: "dashscope_paraformer",
		Model: "paraformer-realtime-v2", TaskID: "task-1", DurationMs: &durationMs,
	})
	if err != nil {
		t.Fatalf("FinalizeRealtime() error = %v", err)
	}
	if capture.Status != VoiceCaptureStatusTranscribed || capture.RawTranscript != "这里用密等保证请求不会重复执行" {
		t.Fatalf("capture = %+v, want 实时供应商文本的 transcribed 终态", capture)
	}
	if capture.STTProvider != "dashscope_paraformer" || capture.STTModel != "paraformer-realtime-v2" || capture.STTRequestID != "task-1" {
		t.Fatalf("供应商元数据未完整固化: %+v", capture)
	}
	if !capture.NeedsConfirmation || capture.ConfirmationReason != VoiceConfirmReasonMissingConfidence {
		t.Fatalf("实时转写无整体置信度时应要求 review: %+v", capture)
	}
	if len(capture.AmbiguousTerms) != 1 || capture.AmbiguousTerms[0].Term != "幂等" {
		t.Fatalf("未复用术语检测: %+v", capture.AmbiguousTerms)
	}
}

func TestVoiceFinalizeRealtimeFailureRecordsCannotBind(t *testing.T) {
	cases := []struct {
		name            string
		transcript      string
		transcriptError string
		wantError       string
	}{
		{name: "空文本", transcript: "   ", wantError: emptyTranscriptError},
		{name: "上游失败", transcript: "未完成的过程文字", transcriptError: "upstream_failed", wantError: "upstream_failed"},
		{name: "用户取消", transcriptError: "user_cancelled", wantError: "user_cancelled"},
		{name: "浏览器断连", transcriptError: "browser_disconnected", wantError: "browser_disconnected"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			voice, repo := newTestVoiceService(t)
			capture, err := voice.FinalizeRealtime(context.Background(), FinalizeRealtimeInput{
				UserID: "usr_1", SessionID: "session_1", WAV: []byte(testCase.name),
				Transcript: testCase.transcript, TranscriptError: testCase.transcriptError,
				Provider: "dashscope_paraformer", Model: "paraformer-realtime-v2", TaskID: "task-failed",
			})
			if err != nil {
				t.Fatalf("FinalizeRealtime() error = %v", err)
			}
			if capture.Status != VoiceCaptureStatusFailed || capture.TranscriptError != testCase.wantError {
				t.Fatalf("capture = %+v, want failed/%q", capture, testCase.wantError)
			}
			if capture.RawTranscript != "" {
				t.Fatalf("失败过程文字不能固化为原始终态转写, got %q", capture.RawTranscript)
			}
			if err := voice.BindMessage(context.Background(), "usr_1", "session_1", capture.CaptureID, "msg_1"); !errors.Is(err, ErrVoiceCaptureNotFound) {
				t.Fatalf("失败记录 BindMessage() error = %v, want ErrVoiceCaptureNotFound", err)
			}
			if repo.bindCalls != 1 {
				t.Fatalf("bindCalls = %d, want 1", repo.bindCalls)
			}
		})
	}
}

func TestVoiceFinalizeRealtimePersistsFailureAfterBrowserContextCancellation(t *testing.T) {
	voice, _ := newTestVoiceService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	capture, err := voice.FinalizeRealtime(ctx, FinalizeRealtimeInput{
		UserID: "usr_1", SessionID: "session_1", WAV: []byte("disconnected-wav"),
		Provider: "dashscope_paraformer", TranscriptError: "browser_disconnected",
	})
	if err != nil {
		t.Fatalf("FinalizeRealtime() error = %v, want 断连后仍限时固化失败记录", err)
	}
	if capture.Status != VoiceCaptureStatusFailed || capture.TranscriptError != "browser_disconnected" {
		t.Fatalf("capture = %+v, want browser_disconnected failed 终态", capture)
	}
}

func TestVoiceFinalizeRealtimeDeduplicatesAndKeepsTerminalTranscript(t *testing.T) {
	voice, _ := newTestVoiceService(t)
	input := FinalizeRealtimeInput{
		UserID: "usr_1", SessionID: "session_1", WAV: []byte("same-realtime-wav"),
		Transcript: "第一次供应商最终文本", Provider: "dashscope_paraformer", TaskID: "task-1",
	}
	first, err := voice.FinalizeRealtime(context.Background(), input)
	if err != nil {
		t.Fatalf("首次 FinalizeRealtime() error = %v", err)
	}
	input.Transcript = "重复请求试图覆盖终态"
	input.TaskID = "task-2"
	second, err := voice.FinalizeRealtime(context.Background(), input)
	if err != nil {
		t.Fatalf("重复 FinalizeRealtime() error = %v", err)
	}
	if first.CaptureID != second.CaptureID {
		t.Fatalf("重复 WAV 应命中同一记录: %q vs %q", first.CaptureID, second.CaptureID)
	}
	if second.RawTranscript != first.RawTranscript || second.STTRequestID != "task-1" {
		t.Fatalf("终态被重复固化覆盖: first=%+v second=%+v", first, second)
	}
}

func TestVoiceBindMessageValidatesParams(t *testing.T) {
	voice, repo := newTestVoiceService(t)
	if err := voice.BindMessage(context.Background(), "usr_1", "session_1", "", "msg_1"); err == nil {
		t.Fatal("BindMessage() 参数不完整时应报错")
	}
	if repo.bindCalls != 0 {
		t.Fatalf("参数不合法时不应打到仓储层, bindCalls = %d", repo.bindCalls)
	}
}
