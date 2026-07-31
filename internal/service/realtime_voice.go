package service

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"healthAgent/internal/stt"
)

var (
	ErrRealtimeVoiceUserLimit         = errors.New("同一用户已有实时语音流")
	ErrRealtimeVoiceInstanceLimit     = errors.New("实时语音服务并发已满")
	ErrRealtimeVoiceAudioBackpressure = errors.New("实时语音音频队列已满")
	ErrRealtimeVoiceUpstreamSlow      = errors.New("实时语音上游写入超时")
	ErrRealtimeVoiceDownstreamSlow    = errors.New("实时语音下游写入超时")
	ErrRealtimeVoiceIdleTimeout       = errors.New("实时语音输入空闲超时")
	ErrRealtimeVoiceFinishTimeout     = errors.New("实时语音上游收尾超时")
	ErrRealtimeVoiceDurationLimit     = errors.New("实时语音录音时长超过上限")
	ErrRealtimeVoiceAudioLimit        = errors.New("实时语音音频大小超过上限")
	ErrRealtimeVoiceBrowserClosed     = errors.New("实时语音浏览器连接已断开")
	ErrRealtimeVoiceUserCancelled     = errors.New("实时语音已由用户取消")
)

type RealtimeVoiceInputType string

const (
	RealtimeVoiceInputAudio  RealtimeVoiceInputType = "audio"
	RealtimeVoiceInputStop   RealtimeVoiceInputType = "stop"
	RealtimeVoiceInputCancel RealtimeVoiceInputType = "cancel"
)

type RealtimeVoiceInput struct {
	Type RealtimeVoiceInputType
	PCM  []byte
}

type RealtimeVoiceEventType string

const (
	RealtimeVoiceEventReady      RealtimeVoiceEventType = "ready"
	RealtimeVoiceEventTranscript RealtimeVoiceEventType = "transcript"
	RealtimeVoiceEventCompleted  RealtimeVoiceEventType = "completed"
	RealtimeVoiceEventError      RealtimeVoiceEventType = "error"
)

type RealtimeVoiceEvent struct {
	Type        RealtimeVoiceEventType
	Seq         int64
	StreamID    string
	SampleRate  int
	SentenceID  int
	Text        string
	SentenceEnd bool
	BeginTimeMS int64
	EndTimeMS   *int64
	Capture     *VoiceCapture
	Code        string
	Message     string
	Retryable   bool
}

// RealtimeVoiceStream is the browser-facing transport boundary. Task 4 adapts
// this interface to the same-origin WebSocket protocol.
type RealtimeVoiceStream interface {
	Read(ctx context.Context) (RealtimeVoiceInput, error)
	Write(ctx context.Context, event RealtimeVoiceEvent) error
}

type RealtimeVoiceFinalizer interface {
	FinalizeRealtime(ctx context.Context, input FinalizeRealtimeInput) (VoiceCapture, error)
}

type RealtimeVoiceLimits struct {
	SampleRate           int
	MaxFrameBytes        int64
	MaxDuration          time.Duration
	MaxAudioBytes        int64
	MaxConcurrentStreams int
	MaxStreamsPerUser    int
	AudioQueueFrames     int
	EventQueueSize       int
	StartTimeout         time.Duration
	FinishTimeout        time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
}

type RealtimeVoiceService struct {
	provider  stt.RealtimeProvider
	finalizer RealtimeVoiceFinalizer
	limits    RealtimeVoiceLimits
	log       *slog.Logger
	metrics   realtimeVoiceMetrics

	mu          sync.Mutex
	active      int
	activeUsers map[string]int
}

type realtimeVoiceMetrics struct {
	rejectedUser             atomic.Int64
	rejectedInstance         atomic.Int64
	firstTranscriptCount     atomic.Int64
	firstTranscriptLatencyMS atomic.Int64
	finishCount              atomic.Int64
	finishLatencyMS          atomic.Int64
	audioDurationMS          atomic.Int64
	upstreamErrors           atomic.Int64
}

// RealtimeVoiceMetrics is a text-free snapshot suitable for metrics export.
type RealtimeVoiceMetrics struct {
	ActiveStreams                 int
	RejectedUserLimit             int64
	RejectedInstanceLimit         int64
	FirstTranscriptCount          int64
	FirstTranscriptLatencyTotalMS int64
	FinishCount                   int64
	FinishLatencyTotalMS          int64
	AudioDurationTotalMS          int64
	UpstreamErrors                int64
}

// RealtimeVoiceReservation atomically holds one user and instance stream slot.
// The WebSocket handler reserves before upgrading so rejected connections remain HTTP responses.
type RealtimeVoiceReservation struct {
	service  *RealtimeVoiceService
	userID   string
	consumed bool
	released bool
	mu       sync.Mutex
}

func NewRealtimeVoiceService(provider stt.RealtimeProvider, finalizer RealtimeVoiceFinalizer, limits RealtimeVoiceLimits, logs ...*slog.Logger) *RealtimeVoiceService {
	limits = normalizeRealtimeVoiceLimits(limits)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if len(logs) > 0 && logs[0] != nil {
		log = logs[0]
	}
	return &RealtimeVoiceService{
		provider: provider, finalizer: finalizer, limits: limits, log: log,
		activeUsers: make(map[string]int),
	}
}

func (s *RealtimeVoiceService) Limits() RealtimeVoiceLimits { return s.limits }

// Metrics returns aggregate counters without audio, transcript text, credentials, or user identifiers.
func (s *RealtimeVoiceService) Metrics() RealtimeVoiceMetrics {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	return RealtimeVoiceMetrics{
		ActiveStreams:                 active,
		RejectedUserLimit:             s.metrics.rejectedUser.Load(),
		RejectedInstanceLimit:         s.metrics.rejectedInstance.Load(),
		FirstTranscriptCount:          s.metrics.firstTranscriptCount.Load(),
		FirstTranscriptLatencyTotalMS: s.metrics.firstTranscriptLatencyMS.Load(),
		FinishCount:                   s.metrics.finishCount.Load(),
		FinishLatencyTotalMS:          s.metrics.finishLatencyMS.Load(),
		AudioDurationTotalMS:          s.metrics.audioDurationMS.Load(),
		UpstreamErrors:                s.metrics.upstreamErrors.Load(),
	}
}

func (s *RealtimeVoiceService) Run(ctx context.Context, userID, sessionID, streamID string, stream RealtimeVoiceStream) (VoiceCapture, error) {
	reservation, err := s.Reserve(userID)
	if err != nil {
		return VoiceCapture{}, err
	}
	return s.RunReserved(ctx, reservation, sessionID, streamID, stream)
}

// Reserve atomically claims capacity before the HTTP connection is upgraded.
func (s *RealtimeVoiceService) Reserve(userID string) (*RealtimeVoiceReservation, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, invalidVoiceInput("实时语音用户参数为空")
	}
	if err := s.acquire(userID); err != nil {
		return nil, err
	}
	return &RealtimeVoiceReservation{service: s, userID: userID}, nil
}

// Release returns reserved capacity. It is safe to call more than once.
func (r *RealtimeVoiceReservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return
	}
	r.released = true
	r.mu.Unlock()
	r.service.release(r.userID)
}

// RunReserved runs a stream using capacity acquired by Reserve.
func (s *RealtimeVoiceService) RunReserved(ctx context.Context, reservation *RealtimeVoiceReservation, sessionID, streamID string, stream RealtimeVoiceStream) (VoiceCapture, error) {
	if reservation == nil || reservation.service != s {
		return VoiceCapture{}, invalidVoiceInput("实时语音并发额度无效")
	}
	reservation.mu.Lock()
	if reservation.released || reservation.consumed {
		reservation.mu.Unlock()
		return VoiceCapture{}, invalidVoiceInput("实时语音并发额度已失效")
	}
	reservation.consumed = true
	userID := reservation.userID
	reservation.mu.Unlock()
	defer reservation.Release()

	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	streamID = strings.TrimSpace(streamID)
	if userID == "" || sessionID == "" || streamID == "" || stream == nil {
		return VoiceCapture{}, invalidVoiceInput("实时语音流参数不完整")
	}
	if s.provider == nil || s.finalizer == nil {
		return VoiceCapture{}, errors.New("实时语音服务未配置")
	}

	session := s.provider.NewSession()
	if session == nil {
		return VoiceCapture{}, errors.New("实时语音供应商返回空会话")
	}
	defer session.Close()
	startedAt := time.Now()
	resultCode := "completed"
	var audioDurationMS int
	var firstTranscriptLatencyMS atomic.Int64
	var finishLatencyMS atomic.Int64
	firstTranscriptLatencyMS.Store(-1)
	finishLatencyMS.Store(-1)
	defer func() {
		attributes := []any{
			"stream_id", streamID,
			"provider", s.provider.Name(),
			"model", s.provider.Model(),
			"result_code", resultCode,
			"audio_duration_ms", audioDurationMS,
			"first_transcript_latency_ms", firstTranscriptLatencyMS.Load(),
			"finish_latency_ms", finishLatencyMS.Load(),
			"active_streams", s.Metrics().ActiveStreams,
		}
		if resultCode == "completed" {
			s.log.Info("实时语音流结束", attributes...)
		} else {
			s.log.Warn("实时语音流结束", attributes...)
		}
	}()

	startCtx, startCancel := context.WithTimeout(ctx, s.limits.StartTimeout)
	err := session.Start(startCtx)
	startCancel()
	if err != nil {
		resultCode = realtimeVoiceErrorCode(err)
		s.metrics.upstreamErrors.Add(1)
		return VoiceCapture{}, err
	}
	if err := s.writeEvent(ctx, stream, RealtimeVoiceEvent{
		Type: RealtimeVoiceEventReady, StreamID: streamID, SampleRate: s.limits.SampleRate,
	}); err != nil {
		resultCode = realtimeVoiceErrorCode(err)
		return VoiceCapture{}, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, groupCtx := errgroup.WithContext(runCtx)
	audioQueue := make(chan []byte, s.limits.AudioQueueFrames)
	eventQueue := newRealtimeVoiceEventQueue(s.limits.EventQueueSize)
	var pcm []byte
	aggregator := newRealtimeTranscriptAggregator()
	var stopAt atomic.Int64
	var firstTranscript sync.Once

	group.Go(func() error {
		defer close(audioQueue)
		return s.readBrowser(groupCtx, stream, audioQueue, &pcm, startedAt, &stopAt)
	})
	group.Go(func() error {
		return s.writeUpstream(groupCtx, session, audioQueue)
	})
	group.Go(func() error {
		defer eventQueue.Close()
		return s.readUpstream(groupCtx, session, eventQueue, aggregator, func() {
			firstTranscript.Do(func() {
				latency := time.Since(startedAt).Milliseconds()
				firstTranscriptLatencyMS.Store(latency)
				s.metrics.firstTranscriptCount.Add(1)
				s.metrics.firstTranscriptLatencyMS.Add(latency)
			})
		})
	})

	var downstreamErr error
	for {
		event, ok, popErr := eventQueue.Pop(groupCtx)
		if popErr != nil || !ok {
			break
		}
		if err := s.writeEvent(groupCtx, stream, event); err != nil {
			downstreamErr = err
			cancel()
			break
		}
	}
	groupErr := group.Wait()
	if downstreamErr != nil {
		groupErr = downstreamErr
	}
	if stoppedAt := stopAt.Load(); stoppedAt > 0 {
		latency := time.Since(time.Unix(0, stoppedAt)).Milliseconds()
		finishLatencyMS.Store(latency)
		s.metrics.finishCount.Add(1)
		s.metrics.finishLatencyMS.Add(latency)
	}

	wav := encodePCM16WAV(pcm, s.limits.SampleRate)
	durationMS := pcmDurationMS(len(pcm), s.limits.SampleRate)
	audioDurationMS = durationMS
	s.metrics.audioDurationMS.Add(int64(durationMS))
	finalizeInput := FinalizeRealtimeInput{
		UserID: userID, SessionID: sessionID, WAV: wav,
		Transcript: aggregator.FinalText(), Provider: s.provider.Name(), Model: s.provider.Model(),
		TaskID: session.TaskID(),
	}
	if durationMS > 0 {
		finalizeInput.DurationMs = &durationMS
	}
	if groupErr != nil {
		finalizeInput.TranscriptError = realtimeVoiceAuditError(groupErr)
		resultCode = finalizeInput.TranscriptError
		if isRealtimeVoiceUpstreamError(groupErr) {
			s.metrics.upstreamErrors.Add(1)
		}
	}
	capture, finalizeErr := s.finalizer.FinalizeRealtime(ctx, finalizeInput)
	if finalizeErr != nil {
		resultCode = "finalize_failed"
		return VoiceCapture{}, finalizeErr
	}

	if groupErr != nil {
		if downstreamErr == nil {
			terminalErr := s.writeEvent(ctx, stream, RealtimeVoiceEvent{
				Type: RealtimeVoiceEventError, Code: realtimeVoiceErrorCode(groupErr),
				Message: "实时转写中断，已保留失败记录", Retryable: true,
			})
			if terminalErr != nil {
				return capture, terminalErr
			}
		}
		return capture, groupErr
	}
	if capture.Status != VoiceCaptureStatusTranscribed {
		err := errors.New("实时 STT 返回空最终文本")
		resultCode = "empty_transcript"
		if writeErr := s.writeEvent(ctx, stream, RealtimeVoiceEvent{
			Type: RealtimeVoiceEventError, Code: "empty_transcript", Message: err.Error(), Retryable: true,
		}); writeErr != nil {
			return capture, writeErr
		}
		return capture, err
	}
	if err := s.writeEvent(ctx, stream, RealtimeVoiceEvent{Type: RealtimeVoiceEventCompleted, Capture: &capture}); err != nil {
		return capture, err
	}
	return capture, nil
}

func (s *RealtimeVoiceService) readBrowser(ctx context.Context, stream RealtimeVoiceStream, audioQueue chan<- []byte, pcm *[]byte, startedAt time.Time, stopAt *atomic.Int64) error {
	var acceptedBytes int64
	for {
		remaining := s.limits.MaxDuration - time.Since(startedAt)
		if remaining <= 0 {
			return ErrRealtimeVoiceDurationLimit
		}
		readTimeout := s.limits.IdleTimeout
		durationBound := remaining < readTimeout
		if durationBound {
			readTimeout = remaining
		}
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		input, err := stream.Read(readCtx)
		cancel()
		if err != nil {
			var inputError *VoiceInputError
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				return ctx.Err()
			case errors.As(err, &inputError):
				return err
			case errors.Is(err, context.DeadlineExceeded) && durationBound:
				return ErrRealtimeVoiceDurationLimit
			case errors.Is(err, context.DeadlineExceeded):
				return ErrRealtimeVoiceIdleTimeout
			case errors.Is(err, io.EOF):
				return ErrRealtimeVoiceBrowserClosed
			default:
				return fmt.Errorf("%w: %v", ErrRealtimeVoiceBrowserClosed, err)
			}
		}
		switch input.Type {
		case RealtimeVoiceInputStop:
			stopAt.CompareAndSwap(0, time.Now().UnixNano())
			return nil
		case RealtimeVoiceInputCancel:
			return ErrRealtimeVoiceUserCancelled
		case RealtimeVoiceInputAudio:
			if len(input.PCM) == 0 || len(input.PCM)%2 != 0 {
				return invalidVoiceInput("PCM 音频帧必须是非空偶数字节")
			}
			if int64(len(input.PCM)) > s.limits.MaxFrameBytes {
				return invalidVoiceInput("PCM 音频帧大小超过上限")
			}
			acceptedBytes += int64(len(input.PCM))
			if acceptedBytes > s.limits.MaxAudioBytes-44 {
				return ErrRealtimeVoiceAudioLimit
			}
			frame := append([]byte(nil), input.PCM...)
			select {
			case audioQueue <- frame:
				*pcm = append(*pcm, frame...)
			default:
				return ErrRealtimeVoiceAudioBackpressure
			}
		default:
			return invalidVoiceInput("未知实时语音输入类型: %s", input.Type)
		}
	}
}

func (s *RealtimeVoiceService) writeUpstream(ctx context.Context, session stt.RealtimeSession, audioQueue <-chan []byte) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-audioQueue:
			if !ok {
				finishCtx, cancel := context.WithTimeout(ctx, s.limits.FinishTimeout)
				err := session.Finish(finishCtx)
				finishCtxErr := finishCtx.Err()
				cancel()
				if errors.Is(finishCtxErr, context.DeadlineExceeded) {
					return fmt.Errorf("%w: %v", ErrRealtimeVoiceFinishTimeout, err)
				}
				return err
			}
			writeCtx, cancel := context.WithTimeout(ctx, s.limits.WriteTimeout)
			err := session.SendAudio(writeCtx, frame)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("%w: %v", ErrRealtimeVoiceUpstreamSlow, err)
				}
				return err
			}
		}
	}
}

func (s *RealtimeVoiceService) readUpstream(ctx context.Context, session stt.RealtimeSession, queue *realtimeVoiceEventQueue, aggregator *realtimeTranscriptAggregator, observeTranscript func()) error {
	var seq int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-session.Events():
			if !ok {
				return errors.New("实时 STT 事件流未返回终态")
			}
			switch event.Type {
			case stt.TranscriptEventResult:
				sentenceID, accepted := aggregator.Apply(event)
				if !accepted {
					continue
				}
				observeTranscript()
				seq++
				out := RealtimeVoiceEvent{
					Type: RealtimeVoiceEventTranscript, Seq: seq, SentenceID: sentenceID,
					Text: event.Text, SentenceEnd: event.SentenceEnd,
					BeginTimeMS: event.BeginTimeMS, EndTimeMS: event.EndTimeMS,
				}
				if err := queue.Push(ctx, out, event.SentenceEnd); err != nil {
					return err
				}
			case stt.TranscriptEventFinished:
				return nil
			case stt.TranscriptEventFailed:
				if event.Err != nil {
					return event.Err
				}
				return stt.ErrRealtimeUpstream
			default:
				return stt.ErrRealtimeProtocol
			}
		}
	}
}

func (s *RealtimeVoiceService) writeEvent(ctx context.Context, stream RealtimeVoiceStream, event RealtimeVoiceEvent) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.limits.WriteTimeout)
	defer cancel()
	if err := stream.Write(writeCtx, event); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(writeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", ErrRealtimeVoiceDownstreamSlow, err)
		}
		return fmt.Errorf("%w: %v", ErrRealtimeVoiceBrowserClosed, err)
	}
	return nil
}

func (s *RealtimeVoiceService) acquire(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeUsers[userID] >= s.limits.MaxStreamsPerUser {
		s.metrics.rejectedUser.Add(1)
		s.log.Warn("实时语音流被拒绝", "reason", "user_limit", "active_streams", s.active)
		return ErrRealtimeVoiceUserLimit
	}
	if s.active >= s.limits.MaxConcurrentStreams {
		s.metrics.rejectedInstance.Add(1)
		s.log.Warn("实时语音流被拒绝", "reason", "instance_limit", "active_streams", s.active)
		return ErrRealtimeVoiceInstanceLimit
	}
	s.active++
	s.activeUsers[userID]++
	return nil
}

func (s *RealtimeVoiceService) release(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.activeUsers[userID]--
	if s.activeUsers[userID] == 0 {
		delete(s.activeUsers, userID)
	}
}

func normalizeRealtimeVoiceLimits(limits RealtimeVoiceLimits) RealtimeVoiceLimits {
	if limits.SampleRate <= 0 {
		limits.SampleRate = 16000
	}
	if limits.MaxFrameBytes <= 0 {
		limits.MaxFrameBytes = 3200
	}
	if limits.MaxDuration <= 0 {
		limits.MaxDuration = 180 * time.Second
	}
	if limits.MaxAudioBytes <= 44 {
		limits.MaxAudioBytes = 6 * 1024 * 1024
	}
	if limits.MaxConcurrentStreams <= 0 {
		limits.MaxConcurrentStreams = 20
	}
	if limits.MaxStreamsPerUser <= 0 {
		limits.MaxStreamsPerUser = 1
	}
	if limits.AudioQueueFrames <= 0 {
		limits.AudioQueueFrames = 10
	}
	if limits.EventQueueSize <= 0 {
		limits.EventQueueSize = 32
	}
	if limits.StartTimeout <= 0 {
		limits.StartTimeout = 8 * time.Second
	}
	if limits.FinishTimeout <= 0 {
		limits.FinishTimeout = 8 * time.Second
	}
	if limits.WriteTimeout <= 0 {
		limits.WriteTimeout = 5 * time.Second
	}
	if limits.IdleTimeout <= 0 {
		limits.IdleTimeout = 30 * time.Second
	}
	return limits
}

type realtimeTranscriptAggregator struct {
	mu        sync.Mutex
	nextID    int
	byBegin   map[int64]int
	committed map[int]string
	order     []int
}

func newRealtimeTranscriptAggregator() *realtimeTranscriptAggregator {
	return &realtimeTranscriptAggregator{
		byBegin: make(map[int64]int), committed: make(map[int]string),
	}
}

func (a *realtimeTranscriptAggregator) Apply(event stt.TranscriptEvent) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return 0, false
	}
	id, exists := a.byBegin[event.BeginTimeMS]
	if !exists {
		a.nextID++
		id = a.nextID
		a.byBegin[event.BeginTimeMS] = id
	}
	if _, finalized := a.committed[id]; finalized && !event.SentenceEnd {
		return id, false
	}
	if event.SentenceEnd {
		if _, exists := a.committed[id]; !exists {
			a.order = append(a.order, id)
		}
		a.committed[id] = text
	}
	return id, true
}

func (a *realtimeTranscriptAggregator) FinalText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var builder strings.Builder
	for _, id := range a.order {
		builder.WriteString(a.committed[id])
	}
	return builder.String()
}

type realtimeVoiceQueuedEvent struct {
	event    RealtimeVoiceEvent
	reliable bool
}

type realtimeVoiceEventQueue struct {
	mu      sync.Mutex
	items   []realtimeVoiceQueuedEvent
	limit   int
	closed  bool
	changed chan struct{}
}

func newRealtimeVoiceEventQueue(limit int) *realtimeVoiceEventQueue {
	return &realtimeVoiceEventQueue{limit: limit, changed: make(chan struct{}, 1)}
}

func (q *realtimeVoiceEventQueue) Push(ctx context.Context, event RealtimeVoiceEvent, reliable bool) error {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return context.Canceled
		}
		if !reliable {
			for index := range q.items {
				if !q.items[index].reliable && q.items[index].event.SentenceID == event.SentenceID {
					q.items[index].event = event
					q.mu.Unlock()
					q.signal()
					return nil
				}
			}
		}
		if len(q.items) >= q.limit {
			interimIndex := -1
			for index := range q.items {
				if !q.items[index].reliable {
					interimIndex = index
					break
				}
			}
			if interimIndex >= 0 {
				q.items = append(q.items[:interimIndex], q.items[interimIndex+1:]...)
			} else if !reliable {
				q.mu.Unlock()
				return nil
			} else {
				q.mu.Unlock()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-q.changed:
					continue
				}
			}
		}
		q.items = append(q.items, realtimeVoiceQueuedEvent{event: event, reliable: reliable})
		q.mu.Unlock()
		q.signal()
		return nil
	}
}

func (q *realtimeVoiceEventQueue) Pop(ctx context.Context) (RealtimeVoiceEvent, bool, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			q.signal()
			return item.event, true, nil
		}
		if q.closed {
			q.mu.Unlock()
			return RealtimeVoiceEvent{}, false, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return RealtimeVoiceEvent{}, false, ctx.Err()
		case <-q.changed:
		}
	}
}

func (q *realtimeVoiceEventQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *realtimeVoiceEventQueue) signal() {
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func encodePCM16WAV(pcm []byte, sampleRate int) []byte {
	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+len(pcm)))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(wav[32:34], 2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav
}

func pcmDurationMS(bytes, sampleRate int) int {
	if sampleRate <= 0 {
		return 0
	}
	return bytes * 1000 / (sampleRate * 2)
}

func realtimeVoiceAuditError(err error) string {
	switch {
	case errors.Is(err, ErrRealtimeVoiceUserCancelled), errors.Is(err, context.Canceled):
		return "user_cancelled"
	case errors.Is(err, ErrRealtimeVoiceBrowserClosed), errors.Is(err, io.EOF):
		return "browser_disconnected"
	default:
		return realtimeVoiceErrorCode(err)
	}
}

func realtimeVoiceErrorCode(err error) string {
	var inputError *VoiceInputError
	switch {
	case errors.As(err, &inputError):
		return "invalid_frame"
	case errors.Is(err, ErrRealtimeVoiceAudioBackpressure):
		return "audio_backpressure"
	case errors.Is(err, ErrRealtimeVoiceUpstreamSlow):
		return "upstream_slow"
	case errors.Is(err, ErrRealtimeVoiceDownstreamSlow):
		return "downstream_slow"
	case errors.Is(err, ErrRealtimeVoiceIdleTimeout):
		return "idle_timeout"
	case errors.Is(err, ErrRealtimeVoiceFinishTimeout):
		return "finish_timeout"
	case errors.Is(err, ErrRealtimeVoiceDurationLimit):
		return "duration_limit"
	case errors.Is(err, ErrRealtimeVoiceAudioLimit):
		return "audio_limit"
	case errors.Is(err, ErrRealtimeVoiceBrowserClosed):
		return "browser_disconnected"
	case errors.Is(err, ErrRealtimeVoiceUserCancelled), errors.Is(err, context.Canceled):
		return "user_cancelled"
	default:
		return "upstream_failed"
	}
}

func isRealtimeVoiceUpstreamError(err error) bool {
	switch realtimeVoiceErrorCode(err) {
	case "upstream_failed", "upstream_slow", "finish_timeout":
		return true
	default:
		return false
	}
}
