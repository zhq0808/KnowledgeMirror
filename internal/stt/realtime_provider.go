package stt

import (
	"context"
	"errors"
)

var (
	ErrRealtimeNotStarted = errors.New("实时 STT 会话尚未启动")
	ErrRealtimeStarted    = errors.New("实时 STT 会话已经启动")
	ErrRealtimeFinishing  = errors.New("实时 STT 会话正在收尾")
	ErrRealtimeClosed     = errors.New("实时 STT 会话已经关闭")
	ErrRealtimeStart      = errors.New("实时 STT 会话启动失败")
	ErrRealtimeFinish     = errors.New("实时 STT 会话收尾失败")
	ErrRealtimeUpstream   = errors.New("实时 STT 上游任务失败")
	ErrRealtimeProtocol   = errors.New("实时 STT 上游协议错误")
)

type TranscriptEventType string

const (
	TranscriptEventResult   TranscriptEventType = "result"
	TranscriptEventFinished TranscriptEventType = "finished"
	TranscriptEventFailed   TranscriptEventType = "failed"
)

// TranscriptEvent 是实时供应商事件的稳定内部表示，不向业务层泄露供应商原始报文。
type TranscriptEvent struct {
	Type         TranscriptEventType
	TaskID       string
	Text         string
	SentenceEnd  bool
	BeginTimeMS  int64
	EndTimeMS    *int64
	UpstreamCode string
	Err          error
}

// RealtimeProvider 为每次实时识别创建一个独立会话。
type RealtimeProvider interface {
	Name() string
	Model() string
	NewSession() RealtimeSession
}

// RealtimeSession 的合法生命周期为 Start -> SendAudio* -> Finish -> Close。
// Start 只有在上游确认 task-started 后才返回；因此 Start 返回前 SendAudio 必须失败。
// Finish 会等待上游 task-finished，期间 Events 仍可能产出最后一条 final。
type RealtimeSession interface {
	TaskID() string
	Start(ctx context.Context) error
	SendAudio(ctx context.Context, pcm []byte) error
	Finish(ctx context.Context) error
	Events() <-chan TranscriptEvent
	Close() error
}
