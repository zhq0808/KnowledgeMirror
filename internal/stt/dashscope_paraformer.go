package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	DashScopeParaformerProviderName = "dashscope_paraformer"
	defaultParaformerModel          = "paraformer-realtime-v2"
	defaultParaformerSampleRate     = 16000
	defaultRealtimeEventBuffer      = 32
)

type DashScopeParaformerOptions struct {
	APIKey          string
	WorkspaceID     string
	WebSocketURL    string
	Model           string
	SampleRate      int
	StartTimeout    time.Duration
	FinishTimeout   time.Duration
	LanguageHints   []string
	MaxSilenceMS    int
	EventBufferSize int
}

type DashScopeParaformerProvider struct {
	options DashScopeParaformerOptions
}

func NewDashScopeParaformerProvider(options DashScopeParaformerOptions) *DashScopeParaformerProvider {
	if options.Model == "" {
		options.Model = defaultParaformerModel
	}
	if options.SampleRate == 0 {
		options.SampleRate = defaultParaformerSampleRate
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = 8 * time.Second
	}
	if options.FinishTimeout <= 0 {
		options.FinishTimeout = 8 * time.Second
	}
	if len(options.LanguageHints) == 0 {
		options.LanguageHints = []string{"zh", "en"}
	}
	if options.MaxSilenceMS <= 0 {
		options.MaxSilenceMS = 600
	}
	if options.EventBufferSize <= 0 {
		options.EventBufferSize = defaultRealtimeEventBuffer
	}
	return &DashScopeParaformerProvider{options: options}
}

func (p *DashScopeParaformerProvider) Name() string { return DashScopeParaformerProviderName }

func (p *DashScopeParaformerProvider) Model() string { return p.options.Model }

func (p *DashScopeParaformerProvider) NewSession() RealtimeSession {
	return &dashScopeParaformerSession{
		options:  p.options,
		taskID:   uuid.NewString(),
		state:    realtimeStateNew,
		events:   make(chan TranscriptEvent, p.options.EventBufferSize),
		started:  make(chan error, 1),
		done:     make(chan error, 1),
		shutdown: make(chan struct{}),
	}
}

type realtimeSessionState uint8

const (
	realtimeStateNew realtimeSessionState = iota
	realtimeStateStarting
	realtimeStateStreaming
	realtimeStateFinishing
	realtimeStateClosed
)

type dashScopeParaformerSession struct {
	options DashScopeParaformerOptions
	taskID  string

	mu       sync.Mutex
	writeMu  sync.Mutex
	eventMu  sync.Mutex
	state    realtimeSessionState
	conn     *websocket.Conn
	events   chan TranscriptEvent
	started  chan error
	done     chan error
	shutdown chan struct{}
	stopOnce sync.Once
	terminal sync.Once
}

func (s *dashScopeParaformerSession) TaskID() string { return s.taskID }

func (s *dashScopeParaformerSession) Events() <-chan TranscriptEvent { return s.events }

func (s *dashScopeParaformerSession) Start(ctx context.Context) error {
	if err := s.beginStart(); err != nil {
		return err
	}
	if strings.TrimSpace(s.options.APIKey) == "" || strings.TrimSpace(s.options.WebSocketURL) == "" {
		err := fmt.Errorf("%w: DashScope 实时转写未配置", ErrNotConfigured)
		s.failStart(err)
		return err
	}

	startCtx, cancel := withConfiguredTimeout(ctx, s.options.StartTimeout)
	defer cancel()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+s.options.APIKey)
	if s.options.WorkspaceID != "" {
		headers.Set("X-DashScope-WorkSpace", s.options.WorkspaceID)
	}
	conn, response, err := websocket.Dial(startCtx, s.options.WebSocketURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		if status != 0 {
			err = fmt.Errorf("%w: WebSocket 握手状态 %d", ErrRealtimeStart, status)
		} else if errors.Is(startCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w: 等待 WebSocket 握手超时", ErrRealtimeStart)
		} else {
			err = fmt.Errorf("%w: WebSocket 握手失败", ErrRealtimeStart)
		}
		s.failStart(err)
		return err
	}

	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	request, err := json.Marshal(s.runTaskRequest())
	if err != nil {
		s.failStart(fmt.Errorf("%w: 构造 run-task 失败", ErrRealtimeProtocol))
		return fmt.Errorf("%w: 构造 run-task 失败", ErrRealtimeProtocol)
	}
	if err := conn.Write(startCtx, websocket.MessageText, request); err != nil {
		err = fmt.Errorf("%w: 发送 run-task 失败", ErrRealtimeStart)
		s.failStart(err)
		return err
	}

	go s.readLoop()
	select {
	case err := <-s.started:
		return err
	case <-startCtx.Done():
		err := fmt.Errorf("%w: 等待 task-started 超时", ErrRealtimeStart)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return err
	}
}

func (s *dashScopeParaformerSession) SendAudio(ctx context.Context, pcm []byte) error {
	s.mu.Lock()
	state := s.state
	conn := s.conn
	s.mu.Unlock()

	switch state {
	case realtimeStateNew, realtimeStateStarting:
		return ErrRealtimeNotStarted
	case realtimeStateFinishing:
		return ErrRealtimeFinishing
	case realtimeStateClosed:
		return ErrRealtimeClosed
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return fmt.Errorf("PCM 音频帧必须是非空偶数字节")
	}
	return s.write(ctx, conn, websocket.MessageBinary, pcm)
}

func (s *dashScopeParaformerSession) Finish(ctx context.Context) error {
	s.mu.Lock()
	state := s.state
	if state == realtimeStateStreaming {
		s.state = realtimeStateFinishing
	}
	conn := s.conn
	s.mu.Unlock()

	switch state {
	case realtimeStateNew, realtimeStateStarting:
		return ErrRealtimeNotStarted
	case realtimeStateFinishing:
		return ErrRealtimeFinishing
	case realtimeStateClosed:
		return ErrRealtimeClosed
	}

	request, err := json.Marshal(s.finishTaskRequest())
	if err != nil {
		return fmt.Errorf("%w: 构造 finish-task 失败", ErrRealtimeProtocol)
	}
	finishCtx, cancel := withConfiguredTimeout(ctx, s.options.FinishTimeout)
	defer cancel()
	if err := s.write(finishCtx, conn, websocket.MessageText, request); err != nil {
		err = fmt.Errorf("%w: 发送 finish-task 失败", ErrRealtimeFinish)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return err
	}

	select {
	case err := <-s.done:
		return err
	case <-finishCtx.Done():
		err := fmt.Errorf("%w: 等待 task-finished 超时", ErrRealtimeFinish)
		s.signalShutdown()
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return err
	}
}

func (s *dashScopeParaformerSession) Close() error {
	s.signalShutdown()
	s.terminate(TranscriptEvent{}, ErrRealtimeClosed)
	return nil
}

func (s *dashScopeParaformerSession) beginStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case realtimeStateNew:
		s.state = realtimeStateStarting
		return nil
	case realtimeStateClosed:
		return ErrRealtimeClosed
	default:
		return ErrRealtimeStarted
	}
}

func (s *dashScopeParaformerSession) failStart(err error) {
	s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
}

func (s *dashScopeParaformerSession) write(ctx context.Context, conn *websocket.Conn, messageType websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if conn == nil {
		return ErrRealtimeClosed
	}
	if err := conn.Write(ctx, messageType, payload); err != nil {
		return fmt.Errorf("写入实时 STT 上游失败: %w", err)
	}
	return nil
}

func (s *dashScopeParaformerSession) readLoop() {
	for {
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn == nil {
			return
		}
		messageType, payload, err := conn.Read(context.Background())
		if err != nil {
			s.mu.Lock()
			closed := s.state == realtimeStateClosed
			s.mu.Unlock()
			if !closed {
				err = fmt.Errorf("读取实时 STT 上游失败")
				s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
			}
			return
		}
		if messageType != websocket.MessageText {
			err := fmt.Errorf("%w: 上游返回非文本控制事件", ErrRealtimeProtocol)
			s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
			return
		}
		if !s.handleServerEvent(payload) {
			return
		}
	}
}

func (s *dashScopeParaformerSession) handleServerEvent(payload []byte) bool {
	var event dashScopeServerEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		err = fmt.Errorf("%w: 无法解析上游事件", ErrRealtimeProtocol)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return false
	}
	if event.Header.TaskID != s.taskID {
		err := fmt.Errorf("%w: 上游 task_id 不一致", ErrRealtimeProtocol)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return false
	}

	switch event.Header.Event {
	case "task-started":
		s.mu.Lock()
		if s.state != realtimeStateStarting {
			s.mu.Unlock()
			err := fmt.Errorf("%w: 非法 task-started 状态", ErrRealtimeProtocol)
			s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
			return false
		}
		s.state = realtimeStateStreaming
		s.mu.Unlock()
		s.started <- nil
		return true
	case "result-generated":
		s.publish(TranscriptEvent{
			Type:        TranscriptEventResult,
			TaskID:      s.taskID,
			Text:        event.Payload.Output.Sentence.Text,
			SentenceEnd: event.Payload.Output.Sentence.SentenceEnd,
			BeginTimeMS: event.Payload.Output.Sentence.BeginTime,
			EndTimeMS:   event.Payload.Output.Sentence.EndTime,
		})
		return true
	case "task-finished":
		s.terminate(TranscriptEvent{Type: TranscriptEventFinished, TaskID: s.taskID}, nil)
		return false
	case "task-failed":
		err := fmt.Errorf("%w: %s", ErrRealtimeUpstream, event.Header.ErrorCode)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, UpstreamCode: event.Header.ErrorCode, Err: err}, err)
		return false
	default:
		err := fmt.Errorf("%w: 未知上游事件", ErrRealtimeProtocol)
		s.terminate(TranscriptEvent{Type: TranscriptEventFailed, TaskID: s.taskID, Err: err}, err)
		return false
	}
}

func (s *dashScopeParaformerSession) terminate(event TranscriptEvent, result error) {
	s.terminal.Do(func() {
		s.mu.Lock()
		previousState := s.state
		s.state = realtimeStateClosed
		conn := s.conn
		s.conn = nil
		s.mu.Unlock()

		s.eventMu.Lock()
		if event.Type != "" {
			select {
			case s.events <- event:
			case <-s.shutdown:
			}
		}
		s.signalShutdown()
		close(s.events)
		s.eventMu.Unlock()
		if previousState == realtimeStateStarting {
			s.started <- result
		}
		s.done <- result
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	})
}

func (s *dashScopeParaformerSession) signalShutdown() {
	s.stopOnce.Do(func() { close(s.shutdown) })
}

func (s *dashScopeParaformerSession) publish(event TranscriptEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.mu.Lock()
	closed := s.state == realtimeStateClosed
	s.mu.Unlock()
	if !closed {
		select {
		case s.events <- event:
		case <-s.shutdown:
		}
	}
}

func (s *dashScopeParaformerSession) runTaskRequest() dashScopeClientEvent {
	return dashScopeClientEvent{
		Header: dashScopeClientHeader{Action: "run-task", TaskID: s.taskID, Streaming: "duplex"},
		Payload: dashScopeClientPayload{
			TaskGroup: "audio",
			Task:      "asr",
			Function:  "recognition",
			Model:     s.options.Model,
			Parameters: &dashScopeParameters{
				Format:                       "pcm",
				SampleRate:                   s.options.SampleRate,
				LanguageHints:                s.options.LanguageHints,
				MaxSentenceSilence:           s.options.MaxSilenceMS,
				DisfluencyRemovalEnabled:     false,
				PunctuationPredictionEnabled: true,
				SemanticPunctuationEnabled:   false,
				Heartbeat:                    true,
			},
			Input: map[string]any{},
		},
	}
}

func (s *dashScopeParaformerSession) finishTaskRequest() dashScopeClientEvent {
	return dashScopeClientEvent{
		Header:  dashScopeClientHeader{Action: "finish-task", TaskID: s.taskID, Streaming: "duplex"},
		Payload: dashScopeClientPayload{Input: map[string]any{}},
	}
}

func withConfiguredTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

type dashScopeClientEvent struct {
	Header  dashScopeClientHeader  `json:"header"`
	Payload dashScopeClientPayload `json:"payload"`
}

type dashScopeClientHeader struct {
	Action    string `json:"action"`
	TaskID    string `json:"task_id"`
	Streaming string `json:"streaming"`
}

type dashScopeClientPayload struct {
	TaskGroup  string               `json:"task_group,omitempty"`
	Task       string               `json:"task,omitempty"`
	Function   string               `json:"function,omitempty"`
	Model      string               `json:"model,omitempty"`
	Parameters *dashScopeParameters `json:"parameters,omitempty"`
	Input      map[string]any       `json:"input"`
}

type dashScopeParameters struct {
	Format                       string   `json:"format"`
	SampleRate                   int      `json:"sample_rate"`
	LanguageHints                []string `json:"language_hints"`
	MaxSentenceSilence           int      `json:"max_sentence_silence"`
	DisfluencyRemovalEnabled     bool     `json:"disfluency_removal_enabled"`
	PunctuationPredictionEnabled bool     `json:"punctuation_prediction_enabled"`
	SemanticPunctuationEnabled   bool     `json:"semantic_punctuation_enabled"`
	Heartbeat                    bool     `json:"heartbeat"`
}

type dashScopeServerEvent struct {
	Header struct {
		TaskID       string `json:"task_id"`
		Event        string `json:"event"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	} `json:"header"`
	Payload struct {
		Output struct {
			Sentence struct {
				BeginTime   int64  `json:"begin_time"`
				EndTime     *int64 `json:"end_time"`
				Text        string `json:"text"`
				SentenceEnd bool   `json:"sentence_end"`
			} `json:"sentence"`
		} `json:"output"`
	} `json:"payload"`
}
