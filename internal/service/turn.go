package service

import (
	"context"
	"errors"
	"time"
)

// ErrTurnLeaseConflict 表示该 Session 已有另一个未过期的 active turn，本次获取必须被拒绝（对应未来的 409）。
var ErrTurnLeaseConflict = errors.New("session 已有进行中的 turn")

// ErrTurnInProgress 表示同一 client_message_id 对应的 turn 仍在处理中，重复请求不能成为新的执行者。
var ErrTurnInProgress = errors.New("turn 仍在处理中")

// ErrTurnLeaseLost 表示当前执行者的 attempt 已过期或已被其他请求接管，不能再写入结果。
var ErrTurnLeaseLost = errors.New("turn 租约已失效")

// TurnLeaseStatus 是 turn 租约的状态取值，对应数据库 agent_turn_lease.status。
type TurnLeaseStatus string

const (
	TurnLeaseActive    TurnLeaseStatus = "active"
	TurnLeaseCompleted TurnLeaseStatus = "completed"
	TurnLeaseFailed    TurnLeaseStatus = "failed"
)

// DefaultTurnLeaseDuration 是未显式指定租期时的默认租约时长。
// 需要覆盖"调用 LLM + 写 SSE"的正常耗时，但不能长到进程崩溃后卡住 Session 太久。
const DefaultTurnLeaseDuration = 45 * time.Second

// TurnLease 是当前持有（或历史命中）的一条 turn 租约记录。
// ID 是租约表自己的数据库内部主键，仅服务端内部使用，不向客户端暴露；
// UserMessageID/ResultMessageID 是消息的 UUID 业务身份（关联 episodic.message_id）。
type TurnLease struct {
	ID              int64
	SessionID       string
	UserID          string
	ClientMessageID string
	Status          TurnLeaseStatus
	AttemptNo       int64
	UserMessageID   string
	ResultMessageID string
	LeaseExpiresAt  time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AcquireTurnLeaseRequest 是获取（或续期/复用）一个 turn 租约所需的信息。
type AcquireTurnLeaseRequest struct {
	UserID          string
	SessionID       string
	ClientMessageID string
	Content         string
	LeaseDuration   time.Duration
}

// AcquireTurnLeaseResult 描述本次获取的结果。
//
// Acquired=true 表示调用方现在持有这条 active 租约（新建、过期抢占重建，或同一 client_message_id 续期/复用）。
// Acquired=false 且 err=nil 表示命中了同一 client_message_id 的历史终态记录（completed/failed）——
// 不是错误，而是"这条用户消息之前已经处理完了"，具体怎么恢复结果留给后续的结果恢复协议处理。
type AcquireTurnLeaseResult struct {
	Lease       TurnLease
	UserMessage UserMessage
	Acquired    bool
}

// CompleteTurnRequest 把当前 attempt 的 assistant 结果落库并将 turn 原子推进到 completed。
// UserMessageID 是本轮 user 消息的 UUID message_id。
type CompleteTurnRequest struct {
	UserID             string
	SessionID          string
	ClientMessageID    string
	AttemptNo          int64
	UserMessageID      string
	Content            string
	PromptVersion      string
	ModelName          string
	RetrievalRequestID string
}

// ReleaseTurnLeaseRequest 是释放（结束）一个 turn 租约所需的信息。
type ReleaseTurnLeaseRequest struct {
	UserID          string
	SessionID       string
	ClientMessageID string
	AttemptNo       int64
	Status          TurnLeaseStatus // 只能是 completed 或 failed，表示这个 turn 走到了终态
}

// TurnLeaseRepository 是 turn 租约需要的最小持久化能力；具体的短事务和并发裁决在实现里完成。
type TurnLeaseRepository interface {
	Acquire(ctx context.Context, request AcquireTurnLeaseRequest) (AcquireTurnLeaseResult, error)
	Complete(ctx context.Context, request CompleteTurnRequest) (AssistantMessage, error)
	Release(ctx context.Context, request ReleaseTurnLeaseRequest) error
}

// TurnLeaseService 编排 turn 租约获取/释放的入参校验，具体并发语义交给 repository。
type TurnLeaseService struct {
	repository TurnLeaseRepository
}

func NewTurnLeaseService(repository TurnLeaseRepository) *TurnLeaseService {
	return &TurnLeaseService{repository: repository}
}

// Acquire 获取（或续期/复用）Session 的 active turn 租约。
func (s *TurnLeaseService) Acquire(ctx context.Context, request AcquireTurnLeaseRequest) (AcquireTurnLeaseResult, error) {
	if request.UserID == "" || request.SessionID == "" || request.ClientMessageID == "" || request.Content == "" {
		return AcquireTurnLeaseResult{}, errors.New("获取 turn 租约缺少必要标识")
	}
	if request.LeaseDuration <= 0 {
		request.LeaseDuration = DefaultTurnLeaseDuration
	}
	return s.repository.Acquire(ctx, request)
}

// Complete 在一个短事务中写入 assistant 结果并完成 turn；attempt_no 防止旧执行者晚到覆盖。
func (s *TurnLeaseService) Complete(ctx context.Context, request CompleteTurnRequest) (AssistantMessage, error) {
	if request.UserID == "" || request.SessionID == "" || request.ClientMessageID == "" ||
		request.AttemptNo <= 0 || request.UserMessageID == "" {
		return AssistantMessage{}, errors.New("完成 turn 缺少必要标识")
	}
	if request.Content == "" {
		return AssistantMessage{}, ErrEmptyAssistantMessage
	}
	return s.repository.Complete(ctx, request)
}

// Release 把未成功完成的 turn 标记为 failed，归还占用。成功路径必须走 Complete。
func (s *TurnLeaseService) Release(ctx context.Context, request ReleaseTurnLeaseRequest) error {
	if request.UserID == "" || request.SessionID == "" || request.ClientMessageID == "" || request.AttemptNo <= 0 {
		return errors.New("释放 turn 租约缺少必要标识")
	}
	if request.Status != TurnLeaseFailed {
		return errors.New("释放 turn 租约只能标记为 failed，成功路径必须调用 Complete")
	}
	return s.repository.Release(ctx, request)
}
