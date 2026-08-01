package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"KnowledgeMirror/internal/llm"
)

const (
	FeynmanEvaluationStatusEvaluating = "evaluating"
	FeynmanEvaluationStatusProposed   = "proposed"
	FeynmanEvaluationStatusFailed     = "failed"

	FeynmanEvaluationDecisionConfirmed = "confirmed"
	FeynmanEvaluationDecisionRejected  = "rejected"

	FeynmanEvidenceScopeLearning = "learning"
)

var (
	ErrFeynmanTranscriptNotConfirmed = errors.New("请先确认转写文本")
	ErrFeynmanEvaluationUnavailable  = errors.New("费曼评估未启用")
	ErrFeynmanEvaluationNotFound     = errors.New("费曼评估不存在")
	ErrFeynmanEvaluationNotReady     = errors.New("费曼评估尚未完成")
	ErrFeynmanEvaluationDecided      = errors.New("费曼评估已处理")
	ErrFeynmanEvaluationInvalid      = errors.New("费曼评估输出非法")
)

type FeynmanDimensionEvaluation struct {
	Dimension    string   `json:"dimension"`
	Score        int      `json:"score"`
	Feedback     string   `json:"feedback"`
	OutputQuotes []string `json:"output_quotes"`
	SourceRefs   []string `json:"source_refs"`
}

type FeynmanEvidenceCandidate struct {
	Claim         string   `json:"claim"`
	Rationale     string   `json:"rationale"`
	EvidenceScope string   `json:"evidence_scope"`
	OutputQuotes  []string `json:"output_quotes"`
	SourceRefs    []string `json:"source_refs"`
}

type FeynmanEvaluationPayload struct {
	Summary             string                       `json:"summary"`
	InsufficientSources bool                         `json:"insufficient_sources"`
	Dimensions          []FeynmanDimensionEvaluation `json:"dimensions"`
	EvidenceCandidate   FeynmanEvidenceCandidate     `json:"evidence_candidate"`
}

type FeynmanSourceSnapshot struct {
	Ref           string   `json:"ref"`
	SourceChunkID string   `json:"source_chunk_id"`
	DocumentID    string   `json:"document_id"`
	VersionID     string   `json:"version_id"`
	DocumentTitle string   `json:"document_title"`
	VersionNo     int      `json:"version_no"`
	HeadingPath   []string `json:"heading_path"`
	Content       string   `json:"content"`
	TrustLevel    string   `json:"trust_level"`
}

type FeynmanEvaluationDecision struct {
	DecisionID   string
	EvaluationID string
	UserID       string
	Decision     string
	FinalPayload *FeynmanEvaluationPayload
	DecisionNote string
	DecidedBy    string
	DecidedAt    time.Time
}

type FeynmanEvaluation struct {
	EvaluationID            string
	AttemptID               string
	ConfirmationID          string
	RubricID                string
	KnowledgePointID        string
	UserID                  string
	Status                  string
	PromptVersion           string
	ModelName               string
	RetrievalRequestID      string
	ConfirmedTranscriptHash []byte
	ConfirmedTranscript     string
	Payload                 *FeynmanEvaluationPayload
	Sources                 []FeynmanSourceSnapshot
	ErrorMessage            string
	Decision                *FeynmanEvaluationDecision
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type ClaimFeynmanEvaluationParams struct {
	EvaluationID            string
	AttemptID               string
	ConfirmationID          string
	RubricID                string
	KnowledgePointID        string
	UserID                  string
	PromptVersion           string
	ModelName               string
	ConfirmedTranscriptHash []byte
}

type CompleteFeynmanEvaluationParams struct {
	EvaluationID       string
	UserID             string
	Status             string
	RetrievalRequestID string
	Payload            *FeynmanEvaluationPayload
	Sources            []FeynmanSourceSnapshot
	ErrorMessage       string
}

type DecideFeynmanEvaluationParams struct {
	DecisionID   string
	EvaluationID string
	UserID       string
	Decision     string
	FinalPayload *FeynmanEvaluationPayload
	DecisionNote string
}

type FeynmanEvaluationRepository interface {
	GetKnowledgePointTitle(ctx context.Context, userID, knowledgePointID string) (string, error)
	ClaimEvaluation(ctx context.Context, params ClaimFeynmanEvaluationParams) (FeynmanEvaluation, bool, error)
	CompleteEvaluation(ctx context.Context, params CompleteFeynmanEvaluationParams) (FeynmanEvaluation, error)
	GetEvaluationByAttempt(ctx context.Context, userID, attemptID string) (FeynmanEvaluation, error)
	GetEvaluationByID(ctx context.Context, userID, evaluationID string) (FeynmanEvaluation, error)
	DecideEvaluation(ctx context.Context, params DecideFeynmanEvaluationParams) (FeynmanEvaluation, error)
}

type FeynmanEvaluationModel interface {
	Evaluate(ctx context.Context, input FeynmanEvaluationInput) (FeynmanEvaluationPayload, error)
	ModelName() string
	PromptVersion() string
}

type FeynmanEvaluationInput struct {
	KnowledgePointTitle string
	ConfirmedTranscript string
	Rubric              KnowledgePointRubric
	RetrievedContext    string
}

type feynmanCompletionModel interface {
	Complete(ctx context.Context, messages []llm.Message) (llm.Completion, error)
}

type LLMFeynmanEvaluator struct {
	model         feynmanCompletionModel
	systemPrompt  string
	modelName     string
	promptVersion string
}

func LoadLLMFeynmanEvaluator(path, version, modelName string, model feynmanCompletionModel) (*LLMFeynmanEvaluator, error) {
	if model == nil {
		return nil, errors.New("费曼评估模型不能为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取费曼评估 Prompt 失败: %w", err)
	}
	parsed, err := template.New("feynman_evaluator").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("解析费曼评估 Prompt 失败: %w", err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct{ Version string }{Version: version}); err != nil {
		return nil, fmt.Errorf("渲染费曼评估 Prompt 失败: %w", err)
	}
	return &LLMFeynmanEvaluator{model: model, systemPrompt: rendered.String(), modelName: modelName, promptVersion: version}, nil
}

func (e *LLMFeynmanEvaluator) ModelName() string     { return e.modelName }
func (e *LLMFeynmanEvaluator) PromptVersion() string { return e.promptVersion }

func (e *LLMFeynmanEvaluator) Evaluate(ctx context.Context, input FeynmanEvaluationInput) (FeynmanEvaluationPayload, error) {
	rawInput, err := json.Marshal(input)
	if err != nil {
		return FeynmanEvaluationPayload{}, err
	}
	completion, err := e.model.Complete(ctx, []llm.Message{{Role: "system", Content: e.systemPrompt}, {Role: "user", Content: string(rawInput)}})
	if err != nil {
		return FeynmanEvaluationPayload{}, err
	}
	return parseFeynmanEvaluationPayload([]byte(completion.Content))
}

func parseFeynmanEvaluationPayload(raw []byte) (FeynmanEvaluationPayload, error) {
	trimmed := stripJSONCodeFence(bytes.TrimSpace(raw))
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var payload FeynmanEvaluationPayload
	if err := decoder.Decode(&payload); err != nil {
		return payload, fmt.Errorf("%w: %v", ErrFeynmanEvaluationInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload, fmt.Errorf("%w: JSON 存在尾随内容", ErrFeynmanEvaluationInvalid)
	}
	return payload, nil
}

func (s *FeynmanService) WithEvaluation(repo FeynmanEvaluationRepository, evaluator FeynmanEvaluationModel, retriever ChatRetriever) *FeynmanService {
	s.evaluationRepo = repo
	s.evaluator = evaluator
	s.evaluationRetriever = retriever
	return s
}

func (s *FeynmanService) EvaluateAttempt(ctx context.Context, userID, attemptID string) (FeynmanEvaluation, error) {
	if s.evaluationRepo == nil || s.evaluator == nil || s.evaluationRetriever == nil {
		return FeynmanEvaluation{}, ErrFeynmanEvaluationUnavailable
	}
	detail, err := s.GetAttempt(ctx, userID, attemptID)
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	if detail.Confirmation == nil {
		return FeynmanEvaluation{}, ErrFeynmanTranscriptNotConfirmed
	}
	rubric, err := s.GetRubric(ctx, userID, detail.Attempt.KnowledgePointID)
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	title, err := s.evaluationRepo.GetKnowledgePointTitle(ctx, userID, detail.Attempt.KnowledgePointID)
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	evaluationID, err := newUUIDv7("evaluation_id")
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	transcriptHash := sha256.Sum256([]byte(detail.Confirmation.ConfirmedTranscript))
	evaluation, claimed, err := s.evaluationRepo.ClaimEvaluation(ctx, ClaimFeynmanEvaluationParams{
		EvaluationID: evaluationID, AttemptID: attemptID, ConfirmationID: detail.Confirmation.ConfirmationID,
		RubricID: rubric.RubricID, KnowledgePointID: detail.Attempt.KnowledgePointID, UserID: userID,
		PromptVersion: s.evaluator.PromptVersion(), ModelName: s.evaluator.ModelName(), ConfirmedTranscriptHash: transcriptHash[:],
	})
	if err != nil || !claimed {
		return evaluation, err
	}

	retrieval, retrievalErr := s.evaluationRetriever.Retrieve(ctx, RetrievalQuery{
		UserID: userID, Query: title + "\n" + detail.Confirmation.ConfirmedTranscript,
		KnowledgePointID: detail.Attempt.KnowledgePointID, Purpose: DocumentPurposeAIRetrieval,
	})
	if retrievalErr != nil {
		return s.failEvaluation(ctx, evaluationID, userID, retrievalErr)
	}
	sources := make([]FeynmanSourceSnapshot, 0, len(retrieval.Passages))
	for _, passage := range retrieval.Passages {
		sources = append(sources, FeynmanSourceSnapshot{
			Ref: passage.Ref, SourceChunkID: passage.SourceChunkID, DocumentID: passage.DocumentID,
			VersionID: passage.VersionID, DocumentTitle: passage.DocumentTitle, VersionNo: passage.VersionNo,
			HeadingPath: passage.HeadingPath, Content: passage.Content, TrustLevel: passage.TrustLevel,
		})
	}
	payload, evalErr := s.evaluator.Evaluate(ctx, FeynmanEvaluationInput{
		KnowledgePointTitle: title, ConfirmedTranscript: detail.Confirmation.ConfirmedTranscript,
		Rubric: rubric, RetrievedContext: retrieval.ContextBlock,
	})
	if evalErr != nil {
		return s.failEvaluation(ctx, evaluationID, userID, evalErr)
	}
	if err := validateFeynmanEvaluationPayload(payload, detail.Confirmation.ConfirmedTranscript, sources); err != nil {
		return s.failEvaluation(ctx, evaluationID, userID, err)
	}
	return s.evaluationRepo.CompleteEvaluation(ctx, CompleteFeynmanEvaluationParams{
		EvaluationID: evaluationID, UserID: userID, Status: FeynmanEvaluationStatusProposed,
		RetrievalRequestID: retrieval.RequestID, Payload: &payload, Sources: sources,
	})
}

func (s *FeynmanService) failEvaluation(ctx context.Context, evaluationID, userID string, cause error) (FeynmanEvaluation, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	evaluation, persistErr := s.evaluationRepo.CompleteEvaluation(persistCtx, CompleteFeynmanEvaluationParams{
		EvaluationID: evaluationID, UserID: userID, Status: FeynmanEvaluationStatusFailed,
		ErrorMessage: truncateFeynmanError(cause.Error(), 2000),
	})
	if persistErr != nil {
		return FeynmanEvaluation{}, fmt.Errorf("评估失败且保存失败状态失败: %v: %w", persistErr, cause)
	}
	return evaluation, cause
}

func (s *FeynmanService) GetEvaluation(ctx context.Context, userID, attemptID string) (FeynmanEvaluation, error) {
	if s.evaluationRepo == nil {
		return FeynmanEvaluation{}, ErrFeynmanEvaluationUnavailable
	}
	return s.evaluationRepo.GetEvaluationByAttempt(ctx, strings.TrimSpace(userID), attemptID)
}

func (s *FeynmanService) DecideEvaluation(ctx context.Context, userID, evaluationID, decision string, payload *FeynmanEvaluationPayload, note string) (FeynmanEvaluation, error) {
	if s.evaluationRepo == nil {
		return FeynmanEvaluation{}, ErrFeynmanEvaluationUnavailable
	}
	decision = strings.TrimSpace(decision)
	if decision != FeynmanEvaluationDecisionConfirmed && decision != FeynmanEvaluationDecisionRejected {
		return FeynmanEvaluation{}, invalidFeynmanInput("评估决策必须是 confirmed 或 rejected")
	}
	if decision == FeynmanEvaluationDecisionConfirmed && payload == nil {
		return FeynmanEvaluation{}, invalidFeynmanInput("确认评估时必须提交最终内容")
	}
	current, err := s.evaluationRepo.GetEvaluationByID(ctx, strings.TrimSpace(userID), evaluationID)
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	if current.Status != FeynmanEvaluationStatusProposed {
		return FeynmanEvaluation{}, ErrFeynmanEvaluationNotReady
	}
	if current.Decision != nil {
		return FeynmanEvaluation{}, ErrFeynmanEvaluationDecided
	}
	if decision == FeynmanEvaluationDecisionConfirmed {
		if err := validateFeynmanEvaluationPayload(*payload, current.ConfirmedTranscript, current.Sources); err != nil {
			return FeynmanEvaluation{}, err
		}
	}
	if decision == FeynmanEvaluationDecisionRejected {
		payload = nil
	}
	decisionID, err := newUUIDv7("decision_id")
	if err != nil {
		return FeynmanEvaluation{}, err
	}
	return s.evaluationRepo.DecideEvaluation(ctx, DecideFeynmanEvaluationParams{
		DecisionID: decisionID, EvaluationID: evaluationID, UserID: strings.TrimSpace(userID),
		Decision: decision, FinalPayload: payload, DecisionNote: truncateFeynmanError(note, 1000),
	})
}

func validateFeynmanEvaluationPayload(payload FeynmanEvaluationPayload, transcript string, sources []FeynmanSourceSnapshot) error {
	if strings.TrimSpace(payload.Summary) == "" || utf8.RuneCountInString(payload.Summary) > 2000 {
		return fmt.Errorf("%w: summary 为空或过长", ErrFeynmanEvaluationInvalid)
	}
	if len(payload.Dimensions) != len(requiredRubricDimensions) {
		return fmt.Errorf("%w: 评分维度不完整", ErrFeynmanEvaluationInvalid)
	}
	validRefs := make(map[string]bool, len(sources))
	for _, source := range sources {
		validRefs[source.Ref] = true
	}
	seen := map[string]bool{}
	for _, dimension := range payload.Dimensions {
		if !slices.Contains(requiredRubricDimensions, dimension.Dimension) || seen[dimension.Dimension] || dimension.Score < 0 || dimension.Score > 100 || strings.TrimSpace(dimension.Feedback) == "" {
			return fmt.Errorf("%w: 非法评分维度 %s", ErrFeynmanEvaluationInvalid, dimension.Dimension)
		}
		seen[dimension.Dimension] = true
		if err := validateEvaluationQuotes(dimension.OutputQuotes, dimension.SourceRefs, transcript, validRefs); err != nil {
			return err
		}
	}
	candidate := payload.EvidenceCandidate
	if strings.TrimSpace(candidate.Claim) == "" || strings.TrimSpace(candidate.Rationale) == "" || candidate.EvidenceScope != FeynmanEvidenceScopeLearning {
		return fmt.Errorf("%w: 证据候选字段非法", ErrFeynmanEvaluationInvalid)
	}
	if len(candidate.OutputQuotes) == 0 {
		return fmt.Errorf("%w: 证据候选必须引用本次输出原句", ErrFeynmanEvaluationInvalid)
	}
	if err := validateEvaluationQuotes(candidate.OutputQuotes, candidate.SourceRefs, transcript, validRefs); err != nil {
		return err
	}
	if len(sources) == 0 && !payload.InsufficientSources {
		return fmt.Errorf("%w: 无可信来源时必须标记资料不足", ErrFeynmanEvaluationInvalid)
	}
	return nil
}

func validateEvaluationQuotes(outputQuotes, sourceRefs []string, transcript string, validRefs map[string]bool) error {
	for _, quote := range outputQuotes {
		quote = strings.TrimSpace(quote)
		if quote == "" || !strings.Contains(transcript, quote) {
			return fmt.Errorf("%w: 输出引用不是确认文本原句", ErrFeynmanEvaluationInvalid)
		}
	}
	for _, ref := range sourceRefs {
		if !validRefs[ref] {
			return fmt.Errorf("%w: 未知资料引用 %s", ErrFeynmanEvaluationInvalid, ref)
		}
	}
	return nil
}
