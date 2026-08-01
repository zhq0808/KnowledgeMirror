package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"KnowledgeMirror/internal/llm"
)

const maxExtractionOutputBytes = 128 * 1024

// ExtractionInput 是一次记忆抽取的模型输入：已分配 M 引用的当前记忆 + 已分配 N 引用的本批消息。
// 真实 user_id / memory_id / message_id / version 全部在后端映射表里，不进入这里交给模型。
type ExtractionInput struct {
	ExistingMemories []ExistingMemoryRef
	BatchMessages    []BatchMessageRef
}

// MemoryExtractor 负责调用真实 LLM，把一批对话转换成可校验、可落库的记忆操作。
// 当前先使用基础 Prompt 跑通完整抽取链路，后续只迭代 Prompt 内容和版本，不替换抽取流程。
type MemoryExtractor interface {
	Extract(ctx context.Context, input ExtractionInput) ([]LLMMemoryOperation, error)
}

// MemoryExtractionModel 是记忆抽取器调用 LLM 的接口。生产环境传入真实 LLM Client；
// 单元测试传入 fake。这里只声明抽取流程实际使用的 Stream 方法，避免绑定某个具体模型实现。
type MemoryExtractionModel interface {
	Stream(ctx context.Context, messages []llm.Message, onDelta func(delta string) error) error
}

// LLMMemoryExtractor 使用版本化 Prompt 调用模型，并把严格 JSON 输出解析为临时引用操作。
type LLMMemoryExtractor struct {
	model        MemoryExtractionModel
	systemPrompt string
}

// LoadLLMMemoryExtractor 在启动期加载并渲染 Prompt，避免运行中才发现模板缺失或损坏。
func LoadLLMMemoryExtractor(path, version string, model MemoryExtractionModel) (*LLMMemoryExtractor, error) {
	if model == nil {
		return nil, fmt.Errorf("记忆抽取模型不能为空")
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("记忆抽取 Prompt 版本不能为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取记忆抽取 Prompt 模板失败: %w", err)
	}
	if !strings.Contains(string(raw), "{{.Version}}") {
		return nil, fmt.Errorf("记忆抽取 Prompt 模板缺少必需变量 {{.Version}}")
	}
	parsed, err := template.New("memory_extractor").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("解析记忆抽取 Prompt 模板失败: %w", err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct{ Version string }{Version: version}); err != nil {
		return nil, fmt.Errorf("渲染记忆抽取 Prompt 模板失败: %w", err)
	}
	return &LLMMemoryExtractor{model: model, systemPrompt: rendered.String()}, nil
}

type extractionModelMemory struct {
	Ref         string `json:"ref"`
	MemoryType  string `json:"memory_type"`
	MemoryValue string `json:"memory_value"`
}

type extractionModelMessage struct {
	Ref     string `json:"ref"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type extractionModelInput struct {
	ExistingMemories []extractionModelMemory  `json:"existing_memories"`
	NewMessages      []extractionModelMessage `json:"new_messages"`
}

func (e *LLMMemoryExtractor) Extract(ctx context.Context, input ExtractionInput) ([]LLMMemoryOperation, error) {
	modelInput := extractionModelInput{
		ExistingMemories: make([]extractionModelMemory, 0, len(input.ExistingMemories)),
		NewMessages:      make([]extractionModelMessage, 0, len(input.BatchMessages)),
	}
	for _, item := range input.ExistingMemories {
		modelInput.ExistingMemories = append(modelInput.ExistingMemories, extractionModelMemory{
			Ref:         item.Ref,
			MemoryType:  item.Memory.MemoryType,
			MemoryValue: item.Memory.MemoryValue,
		})
	}
	for _, message := range input.BatchMessages {
		modelInput.NewMessages = append(modelInput.NewMessages, extractionModelMessage{
			Ref:     message.Ref,
			Role:    message.Role,
			Content: message.Content,
		})
	}
	rawInput, err := json.Marshal(modelInput)
	if err != nil {
		return nil, fmt.Errorf("序列化记忆抽取输入失败: %w", err)
	}

	var output bytes.Buffer
	err = e.model.Stream(ctx, []llm.Message{
		{Role: "system", Content: e.systemPrompt},
		{Role: "user", Content: string(rawInput)},
	}, func(delta string) error {
		if output.Len()+len(delta) > maxExtractionOutputBytes {
			return fmt.Errorf("%w: 模型输出超过 %d 字节", ErrMemoryInvalidOperation, maxExtractionOutputBytes)
		}
		_, writeErr := output.WriteString(delta)
		return writeErr
	})
	if err != nil {
		return nil, err
	}
	return ParseExtractionOperations(output.Bytes())
}

// ExtractionLease 是一次成功抢占的 Session 抽取执行权：本批要处理的结果 seq 窗口 (From, To] + 租约令牌。
type ExtractionLease struct {
	SessionID     string
	UserID        string
	LeaseToken    string
	FromResultSeq int64
	ToResultSeq   int64
}

// ExtractionTurn 是一个 completed turn 的一问一答，ResultSeq 是 assistant 结果消息在 Session 内的 seq。
type ExtractionTurn struct {
	ResultSeq        int64
	UserMessage      BatchMessageRef
	AssistantMessage BatchMessageRef
}

// ExtractionRepository 是异步抽取管道所需的持久化能力（游标/租约/批次/失败退避/补扫）。
// 与 MemoryRepository 分开定义，职责更聚焦；两者可由同一个 Postgres 实现承担。
type ExtractionRepository interface {
	// LookupSessionUser 按 session_id 查询归属用户；会话不存在或已删除时 found=false（通知丢弃）。
	LookupSessionUser(ctx context.Context, sessionID string) (userID string, found bool, err error)
	// AcquireExtractionLease 在短事务内抢占该 Session 的抽取执行权：
	// 已有未过期租约、尚未到重试时间、或没有积压时返回 acquired=false；成功时盖上新 lease_token 并返回工作窗口。
	AcquireExtractionLease(ctx context.Context, sessionID, userID string, leaseDuration time.Duration) (ExtractionLease, bool, error)
	// LoadExtractionBatch 读取 result seq 落在 (fromSeq, toSeq] 的 completed turns 的一问一答，按结果 seq 升序。
	LoadExtractionBatch(ctx context.Context, sessionID, userID string, fromSeq, toSeq int64) ([]ExtractionTurn, error)
	// RecordExtractionFailure 清空租约、累加 consecutive_failures、按退避设置 next_retry_at 和 last_error_code。
	// lease_token 不匹配（已被接管）时不做任何改动，返回 nil。
	RecordExtractionFailure(ctx context.Context, sessionID, userID, leaseToken, errorCode string, baseBackoff, maxBackoff time.Duration) error
	// ScanExtractionBacklog 返回有积压且当前可执行（无未过期租约、已过重试时间）的 Session，用于启动/定时补扫。
	ScanExtractionBacklog(ctx context.Context, limit int) ([]string, error)
}

// MemoryAccess 是抽取管道对记忆存储的最小依赖：加载当前记忆 + 应用抽取结果。由 *MemoryService 满足。
type MemoryAccess interface {
	ListCurrentMemories(ctx context.Context, userID string, budget MemoryBudget) ([]Memory, error)
	ApplyExtraction(ctx context.Context, input ApplyExtractionInput) (ApplyExtractionResult, error)
}

// NewLeaseToken 生成一个抽取租约令牌 UUIDv7。
func NewLeaseToken() (string, error) {
	return newUUIDv7("lease_token")
}
