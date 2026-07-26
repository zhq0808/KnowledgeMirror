package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// retrievalPreviewRequest 是「看看 AI 能查到什么」的请求体。
// 预算字段只能把服务端上限调小，传再大的值也会被 clamp 回去。
type retrievalPreviewRequest struct {
	Query              string `json:"query"`
	KnowledgePointID   string `json:"knowledge_point_id"`
	MaxResults         int    `json:"max_results"`
	ContextBudgetChars int    `json:"context_budget_chars"`
}

type retrievalPassageResponse struct {
	Ref           string   `json:"ref"`
	SourceChunkID string   `json:"source_chunk_id"`
	DocumentID    string   `json:"document_id"`
	VersionID     string   `json:"version_id"`
	DocumentTitle string   `json:"document_title"`
	VersionNo     int      `json:"version_no"`
	HeadingPath   []string `json:"heading_path"`
	Content       string   `json:"content"`
	Truncated     bool     `json:"truncated"`
	ContentOrigin string   `json:"content_origin"`
	OriginLabel   string   `json:"origin_label"`
	TrustLevel    string   `json:"trust_level"`
	TrustLabel    string   `json:"trust_label"`
	Score         float64  `json:"score"`
	CharCost      int      `json:"char_cost"`
}

type retrievalExclusionResponse struct {
	SourceChunkID string `json:"source_chunk_id"`
	DocumentID    string `json:"document_id"`
	Reason        string `json:"reason"`
	Detail        string `json:"detail,omitempty"`
}

type retrievalPreviewResponse struct {
	RequestID      string                       `json:"request_id"`
	Query          string                       `json:"query"`
	Terms          []string                     `json:"terms"`
	Status         string                       `json:"status"`
	CandidateCount int                          `json:"candidate_count"`
	PromptChars    int                          `json:"prompt_chars"`
	DurationMillis int64                        `json:"duration_ms"`
	Passages       []retrievalPassageResponse   `json:"passages"`
	Excluded       []retrievalExclusionResponse `json:"excluded"`
}

// retrievalSourceResponse 是聊天 SSE `sources` 帧的载荷：只带引用标识，不重复回传正文。
type retrievalSourceResponse struct {
	Ref           string   `json:"ref"`
	SourceChunkID string   `json:"source_chunk_id"`
	DocumentID    string   `json:"document_id"`
	DocumentTitle string   `json:"document_title"`
	VersionNo     int      `json:"version_no"`
	HeadingPath   []string `json:"heading_path"`
	OriginLabel   string   `json:"origin_label"`
	TrustLabel    string   `json:"trust_label"`
	Truncated     bool     `json:"truncated"`
}

type retrievalSourcesResponse struct {
	RequestID      string                    `json:"request_id,omitempty"`
	Status         string                    `json:"status"`
	CandidateCount int                       `json:"candidate_count"`
	Sources        []retrievalSourceResponse `json:"sources"`
	// QuarantinedCount 让前端能显示“有片段因疑似注入被隔离”，而不是悄悄少了几条。
	QuarantinedCount int `json:"quarantined_count"`
}

func newRetrievalSourcesResponse(result service.RetrievalResult) retrievalSourcesResponse {
	response := retrievalSourcesResponse{
		RequestID:      result.RequestID,
		Status:         result.Status,
		CandidateCount: result.CandidateCount,
		Sources:        make([]retrievalSourceResponse, 0, len(result.Passages)),
	}
	for _, passage := range result.Passages {
		response.Sources = append(response.Sources, retrievalSourceResponse{
			Ref:           passage.Ref,
			SourceChunkID: passage.SourceChunkID,
			DocumentID:    passage.DocumentID,
			DocumentTitle: passage.DocumentTitle,
			VersionNo:     passage.VersionNo,
			HeadingPath:   headingPathOrEmpty(passage.HeadingPath),
			OriginLabel:   passage.OriginLabel,
			TrustLabel:    service.TrustLevelLabel(passage.TrustLevel),
			Truncated:     passage.Truncated,
		})
	}
	for _, excluded := range result.Excluded {
		if excluded.Reason == service.RetrievalExcludedInjection {
			response.QuarantinedCount++
		}
	}
	return response
}

func newPersistedRetrievalSourcesResponse(retrieval *service.MessageRetrieval) *retrievalSourcesResponse {
	if retrieval == nil {
		return nil
	}
	response := &retrievalSourcesResponse{
		RequestID:        retrieval.RequestID,
		Status:           retrieval.Status,
		CandidateCount:   retrieval.CandidateCount,
		QuarantinedCount: retrieval.QuarantinedCount,
		Sources:          make([]retrievalSourceResponse, 0, len(retrieval.Sources)),
	}
	for _, source := range retrieval.Sources {
		response.Sources = append(response.Sources, retrievalSourceResponse{
			Ref:           source.Ref,
			SourceChunkID: source.SourceChunkID,
			DocumentID:    source.DocumentID,
			DocumentTitle: source.DocumentTitle,
			VersionNo:     source.VersionNo,
			HeadingPath:   headingPathOrEmpty(source.HeadingPath),
			OriginLabel:   source.OriginLabel,
			TrustLabel:    source.TrustLabel,
			Truncated:     source.Truncated,
		})
	}
	return response
}

// retrievalPreviewHandler 返回当前用户「已授权供 AI 检索」的片段命中情况。
//
// 刻意不返回渲染好的 ContextBlock：那是给模型的内部格式，
// 暴露给前端只会诱导前端自己拼 Prompt，绕过服务端的可信边界。
func (s *Server) retrievalPreviewHandler(c *gin.Context) {
	userID, authorized := UserIDFromContext(c.Request.Context())
	if !authorized {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}

	var req retrievalPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}

	result, err := s.retrieval.Retrieve(c.Request.Context(), service.RetrievalQuery{
		UserID:             userID,
		TraceID:            TraceIDFromContext(c.Request.Context()),
		Query:              req.Query,
		Purpose:            service.DocumentPurposeAIRetrieval,
		KnowledgePointID:   req.KnowledgePointID,
		MaxResults:         req.MaxResults,
		ContextBudgetChars: req.ContextBudgetChars,
	})
	if err != nil {
		var inputErr *service.RetrievalInputError
		if errors.As(err, &inputErr) {
			fail(c, http.StatusBadRequest, CodeBadRequest, inputErr.Message)
			return
		}
		s.log.Error("知识库检索失败",
			"trace_id", TraceIDFromContext(c.Request.Context()),
			"error", err,
		)
		fail(c, http.StatusInternalServerError, CodeInternal, "检索服务暂时不可用")
		return
	}

	response := retrievalPreviewResponse{
		RequestID:      result.RequestID,
		Query:          result.Query,
		Terms:          result.Terms,
		Status:         result.Status,
		CandidateCount: result.CandidateCount,
		PromptChars:    result.PromptChars,
		DurationMillis: result.Duration.Milliseconds(),
		Passages:       make([]retrievalPassageResponse, 0, len(result.Passages)),
		Excluded:       make([]retrievalExclusionResponse, 0, len(result.Excluded)),
	}
	if response.Terms == nil {
		response.Terms = []string{}
	}
	for _, passage := range result.Passages {
		response.Passages = append(response.Passages, retrievalPassageResponse{
			Ref:           passage.Ref,
			SourceChunkID: passage.SourceChunkID,
			DocumentID:    passage.DocumentID,
			VersionID:     passage.VersionID,
			DocumentTitle: passage.DocumentTitle,
			VersionNo:     passage.VersionNo,
			HeadingPath:   headingPathOrEmpty(passage.HeadingPath),
			Content:       passage.Content,
			Truncated:     passage.Truncated,
			ContentOrigin: passage.ContentOrigin,
			OriginLabel:   passage.OriginLabel,
			TrustLevel:    passage.TrustLevel,
			TrustLabel:    service.TrustLevelLabel(passage.TrustLevel),
			Score:         passage.Score,
			CharCost:      passage.CharCost,
		})
	}
	for _, excluded := range result.Excluded {
		response.Excluded = append(response.Excluded, retrievalExclusionResponse{
			SourceChunkID: excluded.SourceChunkID,
			DocumentID:    excluded.DocumentID,
			Reason:        excluded.Reason,
			Detail:        excluded.Detail,
		})
	}
	ok(c, response)
}

func headingPathOrEmpty(path []string) []string {
	if path == nil {
		return []string{}
	}
	return path
}
