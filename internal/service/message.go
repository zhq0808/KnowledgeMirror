package service

import (
	"context"
	"errors"
	"time"
)

const InterviewAgentID = "interview-agent"

var ErrClientMessageConflict = errors.New("client_message_id 已用于其他消息内容")
var ErrEmptyAssistantMessage = errors.New("assistant 消息不能为空")

// AppendUserMessageRequest 是写入一条用户消息所需的可信数据。
type AppendUserMessageRequest struct {
	UserID          string
	SessionID       string
	ClientMessageID string
	Content         string
}

// UserMessage 是已持久化的用户消息。MessageID 是后端生成的 UUIDv7 业务身份；
// 数据库内部行主键 id BIGINT 不进入业务类型。
type UserMessage struct {
	MessageID       string
	UserID          string
	SessionID       string
	ClientMessageID string
	Seq             int64
	Content         string
	CreatedAt       time.Time
}

// AppendUserMessageResult 表示消息是首次创建还是幂等命中已有记录。
type AppendUserMessageResult struct {
	Message UserMessage
	Created bool
}

// AppendAssistantMessageRequest 是写入完整模型回复所需的可信数据。
// ParentMessageID 指向本轮 user 消息的 message_id（UUID），为空表示无父消息。
type AppendAssistantMessageRequest struct {
	UserID             string
	SessionID          string
	ParentMessageID    string
	Content            string
	PromptVersion      string
	ModelName          string
	RetrievalRequestID string
}

// AssistantMessage 是已持久化的模型回复。MessageID 是后端生成的 UUIDv7 业务身份。
type AssistantMessage struct {
	MessageID string
	UserID    string
	SessionID string
	Seq       int64
	Content   string
	Retrieval *MessageRetrieval
	CreatedAt time.Time
}

// SessionRetrievalSource 是历史消息可恢复的结构化资料引用，不包含资料正文。
type SessionRetrievalSource struct {
	Ref           string
	SourceChunkID string
	DocumentID    string
	DocumentTitle string
	VersionNo     int
	HeadingPath   []string
	OriginLabel   string
	TrustLabel    string
	Truncated     bool
}

type MessageRetrieval struct {
	RequestID        string
	Status           string
	CandidateCount   int
	QuarantinedCount int
	Sources          []SessionRetrievalSource
}

// ConversationMessage 是可进入对话上下文的已完成消息。
type ConversationMessage struct {
	Seq     int64
	Role    string
	Content string
}

// SessionMessage 是「按会话读消息」接口返回的一条消息，只暴露前端渲染需要的字段。
// 对外只暴露稳定的 UUID message_id，不暴露数据库内部行主键 id。
type SessionMessage struct {
	MessageID string
	Role      string
	Content   string
	Seq       int64
	Retrieval *MessageRetrieval
	CreatedAt time.Time
}

// MessageRepository 是消息服务当前需要的最小持久化能力。
type MessageRepository interface {
	AppendUserMessage(ctx context.Context, request AppendUserMessageRequest) (AppendUserMessageResult, error)
	AppendAssistantMessage(ctx context.Context, request AppendAssistantMessageRequest) (AssistantMessage, error)
	LoadRecent(ctx context.Context, userID, sessionID string, limit int) ([]ConversationMessage, error)
	// FindAssistantReplyByID 按 turn 保存的结果消息 UUID 查已完成的 assistant 回复。
	// 找不到时返回 (zero, false, nil)，不算错误。
	FindAssistantReplyByID(ctx context.Context, userID, sessionID, messageID string) (AssistantMessage, bool, error)
	// ListMessages 按 seq 升序返回该会话已完成、未删除的 user/assistant 消息；
	// userID 由调用方传入可信身份，查询本身也按归属过滤（跟上层的会话归属校验形成双重保险）。
	ListMessages(ctx context.Context, userID, sessionID string) ([]SessionMessage, error)
}

// MessageService 编排消息持久化和幂等语义。
type MessageService struct {
	repository MessageRepository
}

func NewMessageService(repository MessageRepository) *MessageService {
	return &MessageService{repository: repository}
}

func (s *MessageService) AppendUserMessage(ctx context.Context, request AppendUserMessageRequest) (AppendUserMessageResult, error) {
	result, err := s.repository.AppendUserMessage(ctx, request)
	if err != nil {
		return AppendUserMessageResult{}, err
	}
	if !result.Created && result.Message.Content != request.Content {
		return AppendUserMessageResult{}, ErrClientMessageConflict
	}
	return result, nil
}

func (s *MessageService) AppendAssistantMessage(ctx context.Context, request AppendAssistantMessageRequest) (AssistantMessage, error) {
	if request.Content == "" {
		return AssistantMessage{}, ErrEmptyAssistantMessage
	}
	return s.repository.AppendAssistantMessage(ctx, request)
}

// LoadRecent 返回当前用户会话中最近的有效消息，并保持会话内顺序。
func (s *MessageService) LoadRecent(ctx context.Context, userID, sessionID string, limit int) ([]ConversationMessage, error) {
	if limit <= 0 {
		return []ConversationMessage{}, nil
	}
	return s.repository.LoadRecent(ctx, userID, sessionID, limit)
}

// FindReplyForTurn 按 completed turn 持久化的 result_message_id（UUID）原样恢复结果。
func (s *MessageService) FindReplyForTurn(ctx context.Context, userID, sessionID, resultMessageID string) (AssistantMessage, bool, error) {
	if resultMessageID == "" {
		return AssistantMessage{}, false, nil
	}
	return s.repository.FindAssistantReplyByID(ctx, userID, sessionID, resultMessageID)
}

// ListMessages 返回指定会话内已完成、未删除的 user/assistant 消息，按 seq 升序排列，
// 供「多 Session 切换后加载历史」这个场景使用。调用方必须先校验过会话归属
// （例如 SessionService.RequireOwned），这里的 userID 只是再加一层过滤，不是唯一防线。
func (s *MessageService) ListMessages(ctx context.Context, userID, sessionID string) ([]SessionMessage, error) {
	return s.repository.ListMessages(ctx, userID, sessionID)
}
