package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/service"
)

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

type candidateReply struct {
	CandidateID   string `json:"candidate_id"`
	DocumentID    string `json:"document_id,omitempty"`
	VersionID     string `json:"version_id,omitempty"`
	CandidateType string `json:"candidate_type"`
	Status        string `json:"status"`
	// SourceContentOrigin 是抽取时的资料来源快照。AI 整理的内容会一直带着这个标记。
	SourceContentOrigin string `json:"source_content_origin"`
	TrustLevel          string `json:"trust_level"`

	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Reason  string `json:"reason,omitempty"`

	TargetKnowledgePointID string `json:"target_knowledge_point_id,omitempty"`
	MergedIntoCandidateID  string `json:"merged_into_candidate_id,omitempty"`
	ConfirmedOutcome       string `json:"confirmed_outcome,omitempty"`
	DecisionNote           string `json:"decision_note,omitempty"`
	ExtractorModel         string `json:"extractor_model,omitempty"`
	ExtractorVersion       string `json:"extractor_version,omitempty"`

	Sources     []candidateSourceReply `json:"sources"`
	ConfirmedAt *string                `json:"confirmed_at,omitempty"`
	CreatedAt   string                 `json:"created_at"`
	UpdatedAt   string                 `json:"updated_at"`
}

type candidateSourceReply struct {
	SourceChunkID string `json:"source_chunk_id"`
	SourceOrder   int16  `json:"source_order"`
	EvidenceQuote string `json:"evidence_quote,omitempty"`
}

type knowledgePointReply struct {
	KnowledgePointID string `json:"knowledge_point_id"`
	Title            string `json:"title"`
	Description      string `json:"description,omitempty"`
	Status           string `json:"status"`
	// MasteryUIState 是 UI 空状态，不是掌握等级。新知识点一律「暂无证据」。
	MasteryUIState string `json:"mastery_ui_state"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

type extractCandidatesReply struct {
	Candidates   []candidateReply `json:"candidates"`
	AllowedTypes []string         `json:"allowed_candidate_types"`
	Proposed     int              `json:"proposed"`
	// Filtered 是模型提出但超出允许类型、被系统丢弃的条数。
	Filtered int `json:"filtered"`
	// Duplicated 是与已有待确认候选重复、被跳过的条数。
	Duplicated int `json:"duplicated"`
}

type confirmCandidateReply struct {
	Candidate      candidateReply       `json:"candidate"`
	KnowledgePoint *knowledgePointReply `json:"knowledge_point,omitempty"`
}

type candidatePayloadRequest struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

type confirmCandidateRequest struct {
	// KnowledgePointID 非空表示「关联已有知识点」而不是新建。
	KnowledgePointID string                   `json:"knowledge_point_id"`
	Payload          *candidatePayloadRequest `json:"payload"`
	DecisionNote     string                   `json:"decision_note"`
}

type mergeCandidateRequest struct {
	IntoCandidateID string `json:"into_candidate_id"`
	DecisionNote    string `json:"decision_note"`
}

type resolveCandidateRequest struct {
	DecisionNote string `json:"decision_note"`
}

func toCandidateReply(candidate service.ContentCandidate) candidateReply {
	sources := make([]candidateSourceReply, 0, len(candidate.Sources))
	for _, source := range candidate.Sources {
		sources = append(sources, candidateSourceReply{
			SourceChunkID: source.SourceChunkID,
			SourceOrder:   source.SourceOrder,
			EvidenceQuote: source.EvidenceQuote,
		})
	}
	reply := candidateReply{
		CandidateID:            candidate.CandidateID,
		DocumentID:             candidate.DocumentID,
		VersionID:              candidate.VersionID,
		CandidateType:          candidate.CandidateType,
		Status:                 candidate.Status,
		SourceContentOrigin:    candidate.SourceContentOrigin,
		TrustLevel:             candidate.TrustLevel,
		Title:                  candidate.Payload.Title,
		Summary:                candidate.Payload.Summary,
		Reason:                 candidate.Payload.Reason,
		TargetKnowledgePointID: candidate.TargetKnowledgePointID,
		MergedIntoCandidateID:  candidate.MergedIntoCandidateID,
		ConfirmedOutcome:       candidate.ConfirmedOutcome,
		DecisionNote:           candidate.DecisionNote,
		ExtractorModel:         candidate.ExtractorModel,
		ExtractorVersion:       candidate.ExtractorVersion,
		Sources:                sources,
		CreatedAt:              candidate.CreatedAt.Format(time.RFC3339),
		UpdatedAt:              candidate.UpdatedAt.Format(time.RFC3339),
	}
	if candidate.ConfirmedAt != nil {
		confirmedAt := candidate.ConfirmedAt.Format(time.RFC3339)
		reply.ConfirmedAt = &confirmedAt
	}
	return reply
}

func toKnowledgePointReply(point service.KnowledgePoint) knowledgePointReply {
	return knowledgePointReply{
		KnowledgePointID: point.KnowledgePointID,
		Title:            point.Title,
		Description:      point.Description,
		Status:           point.Status,
		MasteryUIState:   point.MasteryUIState(),
		CreatedAt:        point.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        point.UpdatedAt.Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// 公共校验
// ---------------------------------------------------------------------------

func (s *Server) requireCandidateUser(c *gin.Context) (userID, candidateID string, okToProceed bool) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return "", "", false
	}
	candidateID = c.Param("candidate_id")
	if !uuidPattern.MatchString(candidateID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "候选ID格式错误")
		return "", "", false
	}
	return userID, candidateID, true
}

func (s *Server) failCandidateError(c *gin.Context, action string, err error) {
	var inputError *service.CandidateInputError
	var documentInputError *service.DocumentInputError
	switch {
	case errors.As(err, &inputError):
		fail(c, http.StatusBadRequest, CodeBadRequest, inputError.Message)
	case errors.As(err, &documentInputError):
		fail(c, http.StatusBadRequest, CodeBadRequest, documentInputError.Message)
	case errors.Is(err, service.ErrCandidateNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "候选内容不存在")
	case errors.Is(err, service.ErrDocumentNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "资料不存在")
	case errors.Is(err, service.ErrCandidateResolved):
		fail(c, http.StatusConflict, CodeConflict, "候选内容已处理，不能重复处理")
	case errors.Is(err, service.ErrCandidateDocumentArchived):
		fail(c, http.StatusConflict, CodeConflict, "“仅归档”的资料不参与候选抽取")
	case errors.Is(err, service.ErrCandidateNoAllowedType):
		fail(c, http.StatusConflict, CodeConflict, "请先确认资料的类别与用途，再抽取候选")
	case errors.Is(err, service.ErrCandidateExtractorUnavailable):
		fail(c, http.StatusServiceUnavailable, CodeUpstream, "候选抽取暂未启用")
	case errors.Is(err, service.ErrCandidateInvalidOutput):
		s.log.Warn(action, "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusBadGateway, CodeUpstream, "候选抽取结果未通过校验，未保存任何内容，可稍后重试")
	default:
		s.log.Error(action, "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, action)
	}
}

// ---------------------------------------------------------------------------
// 抽取
// ---------------------------------------------------------------------------

// extractDocumentCandidatesHandler 从资料当前版本抽取候选内容。
//
// 抽取只产生「待确认」候选：不写知识点、不写计划、不写可信事实，
// 也不产生任何掌握状态。每条候选都必须引用来源片段，否则整批不保存。
func (s *Server) extractDocumentCandidatesHandler(c *gin.Context) {
	userID, documentID, proceed := s.requireDocumentUser(c)
	if !proceed {
		return
	}
	result, err := s.candidates.Extract(c.Request.Context(), userID, documentID)
	if err != nil {
		s.failCandidateError(c, "抽取候选内容失败", err)
		return
	}
	candidates := make([]candidateReply, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		candidates = append(candidates, toCandidateReply(candidate))
	}
	ok(c, extractCandidatesReply{
		Candidates:   candidates,
		AllowedTypes: result.AllowedTypes,
		Proposed:     result.Proposed,
		Filtered:     result.Filtered,
		Duplicated:   result.Duplicated,
	})
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (s *Server) listCandidatesHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	query := service.CandidateQuery{
		Status: strings.TrimSpace(c.Query("status")),
	}
	if documentID := strings.TrimSpace(c.Query("document_id")); documentID != "" {
		if !uuidPattern.MatchString(documentID) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "资料ID格式错误")
			return
		}
		query.DocumentID = documentID
	}
	if candidateType := strings.TrimSpace(c.Query("candidate_type")); candidateType != "" {
		query.CandidateTypes = []string{candidateType}
	}

	candidates, err := s.candidates.List(c.Request.Context(), userID, query)
	if err != nil {
		s.failCandidateError(c, "查询候选列表失败", err)
		return
	}
	reply := make([]candidateReply, 0, len(candidates))
	for _, candidate := range candidates {
		reply = append(reply, toCandidateReply(candidate))
	}
	ok(c, reply)
}

func (s *Server) getCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	candidate, err := s.candidates.Get(c.Request.Context(), userID, candidateID)
	if err != nil {
		s.failCandidateError(c, "查询候选内容失败", err)
		return
	}
	ok(c, toCandidateReply(candidate))
}

// listKnowledgePointsHandler 返回正式知识点，供「关联已有项」选择。
func (s *Server) listKnowledgePointsHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	points, err := s.candidates.ListKnowledgePoints(c.Request.Context(), userID)
	if err != nil {
		s.failCandidateError(c, "查询知识点列表失败", err)
		return
	}
	reply := make([]knowledgePointReply, 0, len(points))
	for _, point := range points {
		reply = append(reply, toKnowledgePointReply(point))
	}
	ok(c, reply)
}

// ---------------------------------------------------------------------------
// 用户处理：修改 / 确认 / 关联 / 合并 / 仅归档 / 拒绝
// ---------------------------------------------------------------------------

// updateCandidateHandler 修改待确认候选的正文。改完仍然是候选。
func (s *Server) updateCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	var request candidatePayloadRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	candidate, err := s.candidates.Modify(c.Request.Context(), userID, candidateID, service.CandidatePayload{
		Title:   request.Title,
		Summary: request.Summary,
		Reason:  request.Reason,
	})
	if err != nil {
		s.failCandidateError(c, "修改候选内容失败", err)
		return
	}
	ok(c, toCandidateReply(candidate))
}

// confirmCandidateHandler 是候选进入正式链路的唯一入口。
//
// 携带 knowledge_point_id 表示关联已有知识点；不携带则按类型走各自边界：
// 知识点候选进入知识库（UI「暂无证据」），计划任务与 JD 要求只到「待接入」，
// 项目事实只成为待核实事实，参考资料不创建知识点。
func (s *Server) confirmCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	var request confirmCandidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	knowledgePointID := strings.TrimSpace(request.KnowledgePointID)
	if knowledgePointID != "" && !uuidPattern.MatchString(knowledgePointID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "知识点ID格式错误")
		return
	}

	serviceRequest := service.ConfirmCandidateRequest{
		UserID:           userID,
		CandidateID:      candidateID,
		KnowledgePointID: knowledgePointID,
		DecisionNote:     request.DecisionNote,
	}
	if request.Payload != nil {
		serviceRequest.Payload = &service.CandidatePayload{
			Title:   request.Payload.Title,
			Summary: request.Payload.Summary,
			Reason:  request.Payload.Reason,
		}
	}

	result, err := s.candidates.Confirm(c.Request.Context(), serviceRequest)
	if err != nil {
		s.failCandidateError(c, "确认候选内容失败", err)
		return
	}
	reply := confirmCandidateReply{Candidate: toCandidateReply(result.Candidate)}
	if result.KnowledgePoint != nil {
		point := toKnowledgePointReply(*result.KnowledgePoint)
		reply.KnowledgePoint = &point
	}
	ok(c, reply)
}

func (s *Server) mergeCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	var request mergeCandidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	intoCandidateID := strings.TrimSpace(request.IntoCandidateID)
	if !uuidPattern.MatchString(intoCandidateID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "合并目标候选ID格式错误")
		return
	}
	candidate, err := s.candidates.Merge(c.Request.Context(), userID, candidateID, intoCandidateID, request.DecisionNote)
	if err != nil {
		s.failCandidateError(c, "合并候选内容失败", err)
		return
	}
	ok(c, toCandidateReply(candidate))
}

func (s *Server) archiveCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	var request resolveCandidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	candidate, err := s.candidates.Archive(c.Request.Context(), userID, candidateID, request.DecisionNote)
	if err != nil {
		s.failCandidateError(c, "归档候选内容失败", err)
		return
	}
	ok(c, toCandidateReply(candidate))
}

func (s *Server) rejectCandidateHandler(c *gin.Context) {
	userID, candidateID, proceed := s.requireCandidateUser(c)
	if !proceed {
		return
	}
	var request resolveCandidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	candidate, err := s.candidates.Reject(c.Request.Context(), userID, candidateID, request.DecisionNote)
	if err != nil {
		s.failCandidateError(c, "拒绝候选内容失败", err)
		return
	}
	ok(c, toCandidateReply(candidate))
}
