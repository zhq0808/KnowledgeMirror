package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"KnowledgeMirror/internal/llm"
)

// maxCandidateOutputBytes 限制模型输出体积，避免异常长输出拖垮解析与内存。
const maxCandidateOutputBytes = 128 * 1024

// CandidateExtractionModel 是候选抽取调用 LLM 的最小接口：生产传真实客户端，测试传 fake。
type CandidateExtractionModel interface {
	Stream(ctx context.Context, messages []llm.Message, onDelta func(delta string) error) error
}

// LLMCandidateExtractor 使用版本化 Prompt 调用模型，把来源片段转换成候选提案。
//
// 它刻意只输出提案：真实 ID、可信级别、状态、掌握等级全部由后端决定，
// 模型能影响的只有「标题、说明、理由、引用哪几个片段」。
type LLMCandidateExtractor struct {
	model         CandidateExtractionModel
	systemPrompt  string
	modelName     string
	promptVersion string
}

// LoadLLMCandidateExtractor 在启动期加载并渲染 Prompt，避免运行中才发现模板缺失或损坏。
func LoadLLMCandidateExtractor(path, version, modelName string, model CandidateExtractionModel) (*LLMCandidateExtractor, error) {
	if model == nil {
		return nil, errors.New("候选抽取模型不能为空")
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, errors.New("候选抽取 Prompt 版本不能为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取候选抽取 Prompt 模板失败: %w", err)
	}
	if !strings.Contains(string(raw), "{{.Version}}") {
		return nil, errors.New("候选抽取 Prompt 模板缺少必需变量 {{.Version}}")
	}
	parsed, err := template.New("candidate_extractor").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("解析候选抽取 Prompt 模板失败: %w", err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct{ Version string }{Version: version}); err != nil {
		return nil, fmt.Errorf("渲染候选抽取 Prompt 模板失败: %w", err)
	}
	return &LLMCandidateExtractor{
		model:         model,
		systemPrompt:  rendered.String(),
		modelName:     strings.TrimSpace(modelName),
		promptVersion: version,
	}, nil
}

// ModelName / PromptVersion 供服务层记录「这批候选是谁产生的」。
func (e *LLMCandidateExtractor) ModelName() string     { return e.modelName }
func (e *LLMCandidateExtractor) PromptVersion() string { return e.promptVersion }

type candidateModelChunk struct {
	Ref         string   `json:"ref"`
	HeadingPath []string `json:"heading_path"`
	Content     string   `json:"content"`
}

type candidateModelInput struct {
	DocumentTitle string `json:"document_title"`
	DocumentKind  string `json:"document_kind"`
	// ContentOrigin 让模型知道这份资料是不是 AI 整理的：它必须在理由里保留这个标记，
	// 而不是把 AI 笔记当成权威事实来引用。
	ContentOrigin  string                `json:"content_origin"`
	AllowedTypes   []string              `json:"allowed_candidate_types"`
	MaxCandidates  int                   `json:"max_candidates"`
	SourceChunks   []candidateModelChunk `json:"source_chunks"`
	OutputContract string                `json:"output_contract"`
}

func (e *LLMCandidateExtractor) Extract(ctx context.Context, input CandidateExtractionInput) ([]CandidateProposal, error) {
	modelInput := candidateModelInput{
		DocumentTitle: input.DocumentTitle,
		DocumentKind:  input.DocumentKind,
		ContentOrigin: input.ContentOrigin,
		AllowedTypes:  input.AllowedTypes,
		MaxCandidates: input.MaxCandidates,
		SourceChunks:  make([]candidateModelChunk, 0, len(input.Chunks)),
		OutputContract: `{"candidates":[{"candidate_type":"","title":"","summary":"","reason":"",` +
			`"sources":[{"ref":"","evidence_quote":""}]}]}`,
	}
	for _, chunk := range input.Chunks {
		headingPath := chunk.HeadingPath
		if headingPath == nil {
			headingPath = []string{}
		}
		modelInput.SourceChunks = append(modelInput.SourceChunks, candidateModelChunk{
			Ref:         chunk.Ref,
			HeadingPath: headingPath,
			Content:     chunk.Content,
		})
	}
	rawInput, err := json.Marshal(modelInput)
	if err != nil {
		return nil, fmt.Errorf("序列化候选抽取输入失败: %w", err)
	}

	var output bytes.Buffer
	err = e.model.Stream(ctx, []llm.Message{
		{Role: "system", Content: e.systemPrompt},
		{Role: "user", Content: string(rawInput)},
	}, func(delta string) error {
		if output.Len()+len(delta) > maxCandidateOutputBytes {
			return fmt.Errorf("%w: 模型输出超过 %d 字节", ErrCandidateInvalidOutput, maxCandidateOutputBytes)
		}
		_, writeErr := output.WriteString(delta)
		return writeErr
	})
	if err != nil {
		return nil, err
	}
	return ParseCandidateProposals(output.Bytes())
}

// candidateOutput 用指针区分 candidates 缺失/null 与合法空数组。
type candidateOutput struct {
	Candidates *[]candidateWireProposal `json:"candidates"`
}

// candidateWireProposal 只用于严格解析模型 JSON。
// 这里不接受 status、trust_level、knowledge_point_id 等字段：
// 配合 DisallowUnknownFields，模型试图自封「已确认」会被直接拒绝。
type candidateWireProposal struct {
	CandidateType *string                `json:"candidate_type"`
	Title         *string                `json:"title"`
	Summary       *string                `json:"summary"`
	Reason        *string                `json:"reason"`
	Sources       *[]candidateWireSource `json:"sources"`
}

type candidateWireSource struct {
	Ref           *string `json:"ref"`
	EvidenceQuote *string `json:"evidence_quote"`
}

// ParseCandidateProposals 严格解析候选抽取的 JSON 输出：
// 拒绝未知字段、拒绝尾随内容、拒绝必填字段缺失，解析失败整批拒绝。
func ParseCandidateProposals(raw []byte) ([]CandidateProposal, error) {
	trimmed := bytes.TrimSpace(raw)
	// 部分模型会习惯性套一层 ```json 代码块，这属于格式噪声不是语义错误，先剥掉。
	trimmed = stripJSONCodeFence(trimmed)

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var output candidateOutput
	if err := decoder.Decode(&output); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCandidateInvalidOutput, err)
	}
	if output.Candidates == nil {
		return nil, fmt.Errorf("%w: candidates 缺失或为 null", ErrCandidateInvalidOutput)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: JSON 存在多余尾随内容", ErrCandidateInvalidOutput)
	}

	proposals := make([]CandidateProposal, 0, len(*output.Candidates))
	for index, wire := range *output.Candidates {
		if wire.CandidateType == nil || wire.Title == nil || wire.Sources == nil {
			return nil, fmt.Errorf("%w: candidates[%d] 缺少必填字段或字段为 null", ErrCandidateInvalidOutput, index)
		}
		sources := make([]CandidateProposalSource, 0, len(*wire.Sources))
		for sourceIndex, wireSource := range *wire.Sources {
			if wireSource.Ref == nil {
				return nil, fmt.Errorf("%w: candidates[%d].sources[%d] 缺少 ref", ErrCandidateInvalidOutput, index, sourceIndex)
			}
			quote := ""
			if wireSource.EvidenceQuote != nil {
				quote = *wireSource.EvidenceQuote
			}
			sources = append(sources, CandidateProposalSource{Ref: *wireSource.Ref, EvidenceQuote: quote})
		}
		proposals = append(proposals, CandidateProposal{
			CandidateType: *wire.CandidateType,
			Title:         *wire.Title,
			Summary:       optionalString(wire.Summary),
			Reason:        optionalString(wire.Reason),
			Sources:       sources,
		})
	}
	return proposals, nil
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// stripJSONCodeFence 去掉 ```json ... ``` 包裹。只处理最外层，内容本身不做改动。
func stripJSONCodeFence(raw []byte) []byte {
	if !bytes.HasPrefix(raw, []byte("```")) {
		return raw
	}
	body := raw[3:]
	if newline := bytes.IndexByte(body, '\n'); newline >= 0 {
		body = body[newline+1:]
	}
	if end := bytes.LastIndex(body, []byte("```")); end >= 0 {
		body = body[:end]
	}
	return bytes.TrimSpace(body)
}
