// Package service 编排身份、会话和聊天等业务用例。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"KnowledgeMirror/internal/llm"
)

// DefaultMaxReplyChars 是未显式配置时 assistant 单条回复累积的最大字符数上限。
// 用来防止模型异常（例如陷入重复输出）时无限占用内存，并避免写入一条超大的数据库行。
const DefaultMaxReplyChars = 4000

// truncationNotice 附加在被截断的回复末尾，让用户和落库内容都清楚这段话没有完整生成。
const truncationNotice = "\n\n（回复过长，已截断）"

// memoryContextItemPrefix 是每条已确认记忆在数据块里的固定前缀，配合类型标签构成固定格式。
const memoryContextItemPrefix = "- "

// memoryOmittedNoticeFormat 在因长度预算丢弃记忆时补一行可见提示，避免“静默截断”让模型误以为召回已完整。
const memoryOmittedNoticeFormat = "（另有 %d 条已确认记忆因长度预算未展示，如需完整信息请向用户确认）"

// memoryValueNeutralizer 把记忆内容里的换行、回车、制表折叠成空格。
// 记忆内容来自历史对话，若保留换行，一条记忆就能伪造出新的段落标题或“系统指令行”，
// 从而在 Prompt 里冒充更高优先级的约束。折叠成单行后，memory_value 只能作为一条背景数据存在。
var memoryValueNeutralizer = strings.NewReplacer(
	"\r\n", " ",
	"\r", " ",
	"\n", " ",
	"\t", " ",
)

// ChatModel 是聊天服务需要的最小模型能力。
type ChatModel interface {
	Timeout() time.Duration
	ModelName() string
	Stream(ctx context.Context, messages []llm.Message, onDelta func(delta string) error) error
}

// ChatMemoryReader 提供当前用户跨 Session 的已确认长期记忆。
type ChatMemoryReader interface {
	ListCurrentMemories(ctx context.Context, userID string, budget MemoryBudget) ([]Memory, error)
}

// ChatRetriever 提供已授权资料的检索能力。nil 表示未启用知识库检索。
type ChatRetriever interface {
	Retrieve(ctx context.Context, query RetrievalQuery) (RetrievalResult, error)
}

// ChatPracticeRouter 让“对话内练习”有机会先接管一条消息。
//
// 费曼学习不是独立页面，而是同一个聊天框里的一种会话状态，所以它必须挂在 Stream 的
// 最前面：返回 handled=true 时本轮不再调用聊天模型，回复由练习链路自己产生。
// 返回 handled=false 时一切照旧走自由对话——练习链路的任何故障都只能降级，不能阻断聊天。
type ChatPracticeRouter interface {
	Handle(ctx context.Context, request ChatStreamRequest) (handled bool, content string, err error)
}

// ChatStreamRequest 是一次流式对话所需的全部输入与回调。
// 用结构体而不是一长串参数，是因为检索还需要 SessionID/TraceID 做审计，
// 且引用列表要在首个内容增量之前先回传给接口层。
type ChatStreamRequest struct {
	UserID    string
	SessionID string
	TraceID   string
	History   []ConversationMessage
	// Message 是本轮用户原文，UserMessageID 是它落库后的 message_id。
	// 练习链路需要这两个字段：前者用于意图识别，后者用于同一条消息重试时回放上次结果，
	// 避免重复调用模型并把状态多推进一轮。
	Message       string
	UserMessageID string
	// OnRetrieval 在模型调用前被调一次，供接口层先下发引用来源；可为 nil。
	OnRetrieval func(result RetrievalResult) error
	OnDelta     func(delta string) error
}

// ChatService 编排聊天上下文和模型调用。
type ChatService struct {
	model         ChatModel
	prompt        *ChatPrompt
	memories      ChatMemoryReader
	retrieval     ChatRetriever
	practice      ChatPracticeRouter
	memoryBudget  MemoryBudget
	maxReplyChars int
}

// NewChatService 构造聊天服务。Prompt 必须已在启动期加载并校验。
func NewChatService(model ChatModel, prompt *ChatPrompt, memories ChatMemoryReader, memoryBudget MemoryBudget, maxReplyChars int) *ChatService {
	if maxReplyChars <= 0 {
		maxReplyChars = DefaultMaxReplyChars
	}
	return &ChatService{
		model:         model,
		prompt:        prompt,
		memories:      memories,
		memoryBudget:  memoryBudget,
		maxReplyChars: maxReplyChars,
	}
}

// WithRetrieval 在组装根注入知识库检索。
// 作为可选能力单独挂载，而不是再往构造函数加一个参数：
// 未注入时聊天链路照常可用，只是不带资料引用。
func (s *ChatService) WithRetrieval(retrieval ChatRetriever) *ChatService {
	s.retrieval = retrieval
	return s
}

// WithPractice 在组装根注入对话内练习路由（当前是费曼学习）。
// 同样按可选能力挂载：未注入时聊天链路完全不受影响。
func (s *ChatService) WithPractice(practice ChatPracticeRouter) *ChatService {
	s.practice = practice
	return s
}

func (s *ChatService) Timeout() time.Duration {
	return s.model.Timeout()
}

func (s *ChatService) PromptVersion() string {
	return s.prompt.Version()
}

func (s *ChatService) ModelName() string {
	return s.model.ModelName()
}

// Stream 组装 system prompt 和服务端读取的可信会话历史，流式调用模型，并把完整回复内容攒好返回。
//
// OnDelta 只负责把每段增量转发给调用方（handler 再转成 SSE 帧），不承担任何累积/截断逻辑——
// 这些属于业务规则，由 service 统一负责，避免 handler 里堆业务判断。
// 达到 maxReplyChars 上限时，附加一段截断提示后正常收尾（返回 nil error），
// 因为客户端已经看到了前面这部分内容，不应该被当作一次调用失败。
func (s *ChatService) Stream(ctx context.Context, request ChatStreamRequest) (string, error) {
	// 练习链路优先：它可能把这条消息判定为一次费曼回答，从而完全替代本轮模型调用。
	if s.practice != nil {
		handled, content, practiceErr := s.practice.Handle(ctx, request)
		if practiceErr != nil {
			return content, practiceErr
		}
		if handled {
			return content, nil
		}
	}

	userFactSummary, err := s.loadUserFactSummary(ctx, request.UserID)
	if err != nil {
		return "", fmt.Errorf("加载用户记忆失败: %w", err)
	}
	retrievedContext, err := s.loadRetrievedContext(ctx, request)
	if err != nil {
		return "", err
	}
	systemPrompt, err := s.prompt.Render(userFactSummary, retrievedContext)
	if err != nil {
		return "", fmt.Errorf("构建 system prompt 失败: %w", err)
	}
	messages := make([]llm.Message, 0, len(request.History)+1)
	messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	for _, message := range request.History {
		messages = append(messages, llm.Message{Role: message.Role, Content: message.Content})
	}

	var content []byte
	charCount := 0
	truncated := false
	err = s.model.Stream(ctx, messages, func(delta string) error {
		deltaRunes := []rune(delta)
		remaining := s.maxReplyChars - charCount
		if len(deltaRunes) > remaining {
			if remaining > 0 {
				allowed := string(deltaRunes[:remaining])
				content = append(content, allowed...)
				charCount += remaining
				if err := request.OnDelta(allowed); err != nil {
					return err
				}
			}
			truncated = true
			return errReplyTruncated
		}
		content = append(content, delta...)
		charCount += len(deltaRunes)
		return request.OnDelta(delta)
	})
	if errors.Is(err, errReplyTruncated) {
		err = nil
	}
	if err != nil {
		return string(content), err
	}
	if truncated {
		if notifyErr := request.OnDelta(truncationNotice); notifyErr != nil {
			return string(content), notifyErr
		}
		content = append(content, truncationNotice...)
	}
	return string(content), nil
}

// loadRetrievedContext 执行知识库检索并返回可直接进入 Prompt 的受控数据块。
//
// 检索是增强能力而不是对话的必要条件，所以故障时降级继续对话；
// 但降级绝不能静默——必须把“本轮没用资料”写进上下文，否则模型会照常编造引用。
func (s *ChatService) loadRetrievedContext(ctx context.Context, request ChatStreamRequest) (string, error) {
	if s.retrieval == nil {
		return retrievalDisabledNotice, nil
	}
	query := latestUserMessage(request.History)
	if query == "" {
		return retrievalDisabledNotice, nil
	}

	result, err := s.retrieval.Retrieve(ctx, RetrievalQuery{
		UserID:    request.UserID,
		SessionID: request.SessionID,
		TraceID:   request.TraceID,
		Query:     query,
		Purpose:   DocumentPurposeAIRetrieval,
	})
	if err != nil {
		// 输入类错误（例如提问过长）不应该把整轮对话拖坠，一律按降级处理。
		block := result.ContextBlock
		if strings.TrimSpace(block) == "" {
			block = retrievalFailedNotice
		}
		if request.OnRetrieval != nil {
			result.Status = RetrievalStatusFailed
			if callbackErr := request.OnRetrieval(result); callbackErr != nil {
				return "", callbackErr
			}
		}
		return block, nil
	}

	if request.OnRetrieval != nil {
		if err := request.OnRetrieval(result); err != nil {
			return "", err
		}
	}
	return result.ContextBlock, nil
}

// latestUserMessage 取历史里最后一条用户消息作为检索查询。
func latestUserMessage(history []ConversationMessage) string {
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" {
			return strings.TrimSpace(history[index].Content)
		}
	}
	return ""
}

func (s *ChatService) loadUserFactSummary(ctx context.Context, userID string) (string, error) {
	if s.memories == nil {
		return "", nil
	}
	// 数量硬上限下推到存储层 LIMIT；字符预算留在渲染层按整行累加，
	// 让丢弃发生在展示层，才能补一行可见提示，避免存储层静默截断关键安全信息。
	memories, err := s.memories.ListCurrentMemories(ctx, userID, MemoryBudget{MaxCount: s.memoryBudget.MaxCount})
	if err != nil {
		return "", err
	}

	lines := make([]string, 0, len(memories))
	for _, memory := range memories {
		if line := renderMemoryLine(memory); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}

	kept := make([]string, 0, len(lines))
	used := 0
	for _, line := range lines {
		cost := utf8.RuneCountInString(line)
		// 至少保留最新一条完整记忆；不切断单条内容，避免丢掉项目边界等关键事实的后半段。
		if len(kept) > 0 && s.memoryBudget.MaxChars > 0 && used+cost > s.memoryBudget.MaxChars {
			break
		}
		kept = append(kept, line)
		used += cost
	}

	if omitted := len(lines) - len(kept); omitted > 0 {
		kept = append(kept, fmt.Sprintf(memoryOmittedNoticeFormat, omitted))
	}
	return strings.Join(kept, "\n"), nil
}

// renderMemoryLine 把一条已确认记忆渲染成固定格式的单行背景数据；空内容返回空串以跳过。
//
// 记忆内容来自历史对话，只能作为背景事实呈现，不能当成可执行指令：
//   - 加类型标签构成固定格式，便于模型区分数据与指令。
//   - 折叠换行/制表，避免一条记忆伪造出新的“系统指令行”冒充更高优先级约束。
func renderMemoryLine(memory Memory) string {
	value := strings.TrimSpace(memoryValueNeutralizer.Replace(memory.MemoryValue))
	if value == "" {
		return ""
	}
	return fmt.Sprintf("%s[%s] %s", memoryContextItemPrefix, memoryTypeLabel(memory.MemoryType), value)
}

// memoryTypeLabel 把记忆类型归一到允许集合；未知或缺失时统一标为 other，避免把脏标签直接写进 Prompt。
func memoryTypeLabel(memoryType string) string {
	memoryType = strings.TrimSpace(memoryType)
	if _, ok := allowedMemoryTypes[memoryType]; ok {
		return memoryType
	}
	return "other"
}

// errReplyTruncated 只在 Stream 内部用来打断 model.Stream 的读取循环，从不对外暴露。
var errReplyTruncated = errors.New("assistant 回复已达到最大长度上限")
