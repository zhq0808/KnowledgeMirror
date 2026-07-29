package service

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"healthAgent/internal/stt"
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

func (r *fakeVoiceRepository) Claim(_ context.Context, params ClaimVoiceCaptureParams) (VoiceCapture, bool, error) {
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

func (r *fakeVoiceRepository) Complete(_ context.Context, params CompleteVoiceCaptureParams) (VoiceCapture, error) {
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
	if !found {
		return ErrVoiceCaptureNotFound
	}
	row.MessageID = messageID
	return nil
}

type voiceSTTStub struct {
	mu         sync.Mutex
	calls      int
	text       string
	confidence *float64
	err        error
}

func (p *voiceSTTStub) Name() string { return "voice_stub" }

func (p *voiceSTTStub) Transcribe(_ context.Context, _ []byte, _ string) (stt.Transcript, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return stt.Transcript{}, p.err
	}
	return stt.Transcript{
		Text: p.text, Provider: p.Name(), Model: "stub-model",
		RequestID: "req-voice-1", Confidence: p.confidence,
	}, nil
}

func floatPtr(value float64) *float64 { return &value }

func newTestVoiceService(t *testing.T, provider stt.Provider) (*VoiceCaptureService, *fakeVoiceRepository) {
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

// ---------------------------------------------------------------------------
// 确认裁决：默认自动发送，只有明确证据才拦一次
// ---------------------------------------------------------------------------

func TestVoiceCaptureConfirmationRuling(t *testing.T) {
	cases := []struct {
		name              string
		text              string
		confidence        *float64
		sttErr            error
		wantStatus        string
		wantNeedsConfirm  bool
		wantReason        string
		wantAmbiguousTerm string
	}{
		{
			name: "置信度足够且无歧义术语则直接自动发送", text: "我来讲一下这段链路的设计",
			confidence: floatPtr(0.92), wantStatus: VoiceCaptureStatusTranscribed,
			wantNeedsConfirm: false, wantReason: "",
		},
		{
			name: "置信度低于阈值先让用户确认", text: "我来讲一下这段链路的设计",
			confidence: floatPtr(0.31), wantStatus: VoiceCaptureStatusTranscribed,
			wantNeedsConfirm: true, wantReason: VoiceConfirmReasonLowConfidence,
		},
		{
			name: "供应商不返回置信度时保守要求确认", text: "我来讲一下这段链路的设计",
			confidence: nil, wantStatus: VoiceCaptureStatusTranscribed,
			wantNeedsConfirm: true, wantReason: VoiceConfirmReasonMissingConfidence,
		},
		{
			name: "命中已知误转写时提示术语歧义", text: "这里用密等保证不会重复扣款",
			confidence: floatPtr(0.95), wantStatus: VoiceCaptureStatusTranscribed,
			wantNeedsConfirm: true, wantReason: VoiceConfirmReasonAmbiguousTerms, wantAmbiguousTerm: "幂等",
		},
		{
			name: "转写失败也落一条记录并要求用户处理", sttErr: errors.New("upstream boom"),
			wantStatus: VoiceCaptureStatusFailed, wantNeedsConfirm: true,
			wantReason: VoiceConfirmReasonTranscribeFailed,
		},
		{
			name: "空转写按失败处理", text: "   ", confidence: floatPtr(0.99),
			wantStatus: VoiceCaptureStatusFailed, wantNeedsConfirm: true,
			wantReason: VoiceConfirmReasonTranscribeFailed,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &voiceSTTStub{text: testCase.text, confidence: testCase.confidence, err: testCase.sttErr}
			voice, _ := newTestVoiceService(t, provider)

			capture, err := voice.Capture(context.Background(), "usr_1", "session_1",
				[]byte(testCase.name), "audio/webm", nil)
			if err != nil {
				t.Fatalf("Capture() error = %v, want nil（转写失败也应返回记录而不是错误）", err)
			}
			if capture.Status != testCase.wantStatus {
				t.Fatalf("Status = %q, want %q", capture.Status, testCase.wantStatus)
			}
			if capture.NeedsConfirmation != testCase.wantNeedsConfirm {
				t.Fatalf("NeedsConfirmation = %t, want %t", capture.NeedsConfirmation, testCase.wantNeedsConfirm)
			}
			if capture.ConfirmationReason != testCase.wantReason {
				t.Fatalf("ConfirmationReason = %q, want %q", capture.ConfirmationReason, testCase.wantReason)
			}
			if testCase.wantAmbiguousTerm != "" {
				if len(capture.AmbiguousTerms) == 0 || capture.AmbiguousTerms[0].Term != testCase.wantAmbiguousTerm {
					t.Fatalf("AmbiguousTerms = %+v, want 首项 term = %q", capture.AmbiguousTerms, testCase.wantAmbiguousTerm)
				}
			}
			if testCase.wantStatus == VoiceCaptureStatusFailed && capture.TranscriptError == "" {
				t.Fatal("失败记录必须写明失败原因，否则前端只能提示一句无用的“出错了”")
			}
		})
	}
}

// 相同字节重复提交不能重复调用 STT：那是真金白银，而且用户并没有多说一句话。
func TestVoiceCaptureDeduplicatesIdenticalAudio(t *testing.T) {
	provider := &voiceSTTStub{text: "重复提交的同一段录音", confidence: floatPtr(0.9)}
	voice, _ := newTestVoiceService(t, provider)
	audio := []byte("same-bytes")

	first, err := voice.Capture(context.Background(), "usr_1", "session_1", audio, "audio/webm", nil)
	if err != nil {
		t.Fatalf("首次 Capture() error = %v", err)
	}
	second, err := voice.Capture(context.Background(), "usr_1", "session_1", audio, "audio/webm", nil)
	if err != nil {
		t.Fatalf("重复 Capture() error = %v", err)
	}
	if first.CaptureID != second.CaptureID {
		t.Fatalf("重复提交应命中同一条记录: %q vs %q", first.CaptureID, second.CaptureID)
	}
	if provider.calls != 1 {
		t.Fatalf("STT 调用次数 = %d, want 1", provider.calls)
	}
}

func TestVoiceCaptureRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name       string
		userID     string
		sessionID  string
		audio      []byte
		mimeType   string
		durationMs *int
	}{
		{name: "缺少用户身份", sessionID: "session_1", audio: []byte("x"), mimeType: "audio/webm"},
		{name: "缺少会话", userID: "usr_1", audio: []byte("x"), mimeType: "audio/webm"},
		{name: "空音频", userID: "usr_1", sessionID: "session_1", mimeType: "audio/webm"},
		{name: "不支持的格式", userID: "usr_1", sessionID: "session_1", audio: []byte("x"), mimeType: "application/zip"},
		{name: "超长录音", userID: "usr_1", sessionID: "session_1", audio: []byte("x"), mimeType: "audio/webm",
			durationMs: func() *int { v := 999999; return &v }()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			voice, _ := newTestVoiceService(t, &voiceSTTStub{text: "ok", confidence: floatPtr(0.9)})
			_, err := voice.Capture(context.Background(), testCase.userID, testCase.sessionID,
				testCase.audio, testCase.mimeType, testCase.durationMs)
			var inputError *VoiceInputError
			if !errors.As(err, &inputError) {
				t.Fatalf("Capture() error = %v, want *VoiceInputError", err)
			}
		})
	}
}

func TestVoiceCaptureOversizeAudioRejected(t *testing.T) {
	voice, _ := newTestVoiceService(t, &voiceSTTStub{text: "ok", confidence: floatPtr(0.9)})
	oversize := make([]byte, (1<<20)+1)
	_, err := voice.Capture(context.Background(), "usr_1", "session_1", oversize, "audio/webm", nil)
	var inputError *VoiceInputError
	if !errors.As(err, &inputError) {
		t.Fatalf("Capture() error = %v, want *VoiceInputError", err)
	}
}

// 转写超过字数上限按失败处理：一段异常长的转写多半是供应商出问题，不该塞进对话。
func TestVoiceCaptureRejectsOverlongTranscript(t *testing.T) {
	provider := &voiceSTTStub{text: strings.Repeat("长", 9000), confidence: floatPtr(0.99)}
	repo := newFakeVoiceRepository()
	voice := NewVoiceCaptureService(repo, provider, nil, VoiceLimits{
		MaxAudioBytes: 1 << 20, MaxDurationMS: 180000, MaxTranscriptChars: 8000, MinConfidence: 0.6,
	}, discardLogger())

	capture, err := voice.Capture(context.Background(), "usr_1", "session_1", []byte("audio"), "audio/webm", nil)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if capture.Status != VoiceCaptureStatusFailed || capture.ConfirmationReason != VoiceConfirmReasonTranscribeFailed {
		t.Fatalf("超长转写应判失败, got status=%q reason=%q", capture.Status, capture.ConfirmationReason)
	}
}

func TestVoiceBindMessageValidatesParams(t *testing.T) {
	voice, repo := newTestVoiceService(t, &voiceSTTStub{text: "ok", confidence: floatPtr(0.9)})
	if err := voice.BindMessage(context.Background(), "usr_1", "session_1", "", "msg_1"); err == nil {
		t.Fatal("BindMessage() 参数不完整时应报错")
	}
	if repo.bindCalls != 0 {
		t.Fatalf("参数不合法时不应打到仓储层, bindCalls = %d", repo.bindCalls)
	}
}
