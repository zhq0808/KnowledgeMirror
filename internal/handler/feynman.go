package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

// feynmanAudioFormField 是 Push-to-Talk 录音上传的 multipart 字段名。
const feynmanAudioFormField = "audio"

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

type feynmanAudioTaskReply struct {
	AudioTaskID     string `json:"audio_task_id"`
	AttemptNo       int    `json:"attempt_no"`
	Status          string `json:"status"`
	MIMEType        string `json:"mime_type"`
	SizeBytes       int64  `json:"size_bytes"`
	DurationMs      *int   `json:"duration_ms,omitempty"`
	STTProvider     string `json:"stt_provider,omitempty"`
	STTModel        string `json:"stt_model,omitempty"`
	RawTranscript   string `json:"raw_transcript,omitempty"`
	TranscriptError string `json:"transcript_error,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type feynmanConfirmationReply struct {
	ConfirmationID      string `json:"confirmation_id"`
	RawTranscript       string `json:"raw_transcript"`
	ConfirmedTranscript string `json:"confirmed_transcript"`
	Edited              bool   `json:"edited"`
	ConfirmedBy         string `json:"confirmed_by"`
	ConfirmedAt         string `json:"confirmed_at"`
}

type feynmanAttemptReply struct {
	AttemptID        string                    `json:"attempt_id"`
	KnowledgePointID string                    `json:"knowledge_point_id"`
	Status           string                    `json:"status"`
	ActiveAudioTask  *feynmanAudioTaskReply    `json:"active_audio_task,omitempty"`
	Confirmation     *feynmanConfirmationReply `json:"confirmation,omitempty"`
	CreatedAt        string                    `json:"created_at"`
	UpdatedAt        string                    `json:"updated_at"`
}

type feynmanRubricCriterionReply struct {
	Dimension   string `json:"dimension"`
	Label       string `json:"label"`
	Weight      int    `json:"weight"`
	Description string `json:"description"`
}

type feynmanRubricReply struct {
	RubricID         string                        `json:"rubric_id"`
	KnowledgePointID string                        `json:"knowledge_point_id"`
	VersionNo        int                           `json:"version_no"`
	TemplateVersion  string                        `json:"template_version"`
	Criteria         []feynmanRubricCriterionReply `json:"criteria"`
	CreatedBy        string                        `json:"created_by"`
	CreatedAt        string                        `json:"created_at"`
}

type feynmanEvaluationDecisionReply struct {
	DecisionID   string                            `json:"decision_id"`
	Decision     string                            `json:"decision"`
	FinalPayload *service.FeynmanEvaluationPayload `json:"final_payload,omitempty"`
	DecisionNote string                            `json:"decision_note,omitempty"`
	DecidedAt    string                            `json:"decided_at"`
}

type feynmanEvaluationReply struct {
	EvaluationID        string                            `json:"evaluation_id"`
	AttemptID           string                            `json:"attempt_id"`
	ConfirmationID      string                            `json:"confirmation_id"`
	RubricID            string                            `json:"rubric_id"`
	KnowledgePointID    string                            `json:"knowledge_point_id"`
	Status              string                            `json:"status"`
	PromptVersion       string                            `json:"prompt_version"`
	ModelName           string                            `json:"model_name"`
	ConfirmedTranscript string                            `json:"confirmed_transcript"`
	Payload             *service.FeynmanEvaluationPayload `json:"payload,omitempty"`
	Sources             []service.FeynmanSourceSnapshot   `json:"sources"`
	ErrorMessage        string                            `json:"error_message,omitempty"`
	Decision            *feynmanEvaluationDecisionReply   `json:"decision,omitempty"`
	CreatedAt           string                            `json:"created_at"`
	UpdatedAt           string                            `json:"updated_at"`
}

type createFeynmanAttemptRequest struct {
	KnowledgePointID string `json:"knowledge_point_id"`
	IdempotencyKey   string `json:"idempotency_key"`
}

type confirmFeynmanTranscriptRequest struct {
	ConfirmedTranscript string `json:"confirmed_transcript"`
}

type createFeynmanRubricVersionRequest struct {
	Criteria []feynmanRubricCriterionReply `json:"criteria"`
}

type decideFeynmanEvaluationRequest struct {
	Decision     string                            `json:"decision"`
	FinalPayload *service.FeynmanEvaluationPayload `json:"final_payload"`
	DecisionNote string                            `json:"decision_note"`
}

func toFeynmanAudioTaskReply(task service.FeynmanAudioTask) feynmanAudioTaskReply {
	return feynmanAudioTaskReply{
		AudioTaskID:     task.AudioTaskID,
		AttemptNo:       task.AttemptNo,
		Status:          task.Status,
		MIMEType:        task.MIMEType,
		SizeBytes:       task.SizeBytes,
		DurationMs:      task.DurationMs,
		STTProvider:     task.STTProvider,
		STTModel:        task.STTModel,
		RawTranscript:   task.RawTranscript,
		TranscriptError: task.TranscriptError,
		CreatedAt:       task.CreatedAt.Format(time.RFC3339),
	}
}

func toFeynmanConfirmationReply(confirmation service.FeynmanTranscriptConfirmation) feynmanConfirmationReply {
	return feynmanConfirmationReply{
		ConfirmationID:      confirmation.ConfirmationID,
		RawTranscript:       confirmation.RawTranscript,
		ConfirmedTranscript: confirmation.ConfirmedTranscript,
		Edited:              confirmation.Edited,
		ConfirmedBy:         confirmation.ConfirmedBy,
		ConfirmedAt:         confirmation.ConfirmedAt.Format(time.RFC3339),
	}
}

func toFeynmanAttemptReply(detail service.FeynmanAttemptDetail) feynmanAttemptReply {
	reply := feynmanAttemptReply{
		AttemptID:        detail.Attempt.AttemptID,
		KnowledgePointID: detail.Attempt.KnowledgePointID,
		Status:           detail.Status(),
		CreatedAt:        detail.Attempt.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        detail.Attempt.UpdatedAt.Format(time.RFC3339),
	}
	if detail.ActiveAudioTask != nil {
		task := toFeynmanAudioTaskReply(*detail.ActiveAudioTask)
		reply.ActiveAudioTask = &task
	}
	if detail.Confirmation != nil {
		confirmation := toFeynmanConfirmationReply(*detail.Confirmation)
		reply.Confirmation = &confirmation
	}
	return reply
}

func toFeynmanRubricReply(rubric service.KnowledgePointRubric) feynmanRubricReply {
	criteria := make([]feynmanRubricCriterionReply, 0, len(rubric.Criteria))
	for _, c := range rubric.Criteria {
		criteria = append(criteria, feynmanRubricCriterionReply{
			Dimension:   c.Dimension,
			Label:       c.Label,
			Weight:      c.Weight,
			Description: c.Description,
		})
	}
	return feynmanRubricReply{
		RubricID:         rubric.RubricID,
		KnowledgePointID: rubric.KnowledgePointID,
		VersionNo:        rubric.VersionNo,
		TemplateVersion:  rubric.TemplateVersion,
		Criteria:         criteria,
		CreatedBy:        rubric.CreatedBy,
		CreatedAt:        rubric.CreatedAt.Format(time.RFC3339),
	}
}

func toFeynmanEvaluationReply(evaluation service.FeynmanEvaluation) feynmanEvaluationReply {
	reply := feynmanEvaluationReply{
		EvaluationID: evaluation.EvaluationID, AttemptID: evaluation.AttemptID,
		ConfirmationID: evaluation.ConfirmationID, RubricID: evaluation.RubricID,
		KnowledgePointID: evaluation.KnowledgePointID, Status: evaluation.Status,
		PromptVersion: evaluation.PromptVersion, ModelName: evaluation.ModelName,
		ConfirmedTranscript: evaluation.ConfirmedTranscript, Payload: evaluation.Payload,
		Sources: evaluation.Sources, ErrorMessage: evaluation.ErrorMessage,
		CreatedAt: evaluation.CreatedAt.Format(time.RFC3339), UpdatedAt: evaluation.UpdatedAt.Format(time.RFC3339),
	}
	if reply.Sources == nil {
		reply.Sources = []service.FeynmanSourceSnapshot{}
	}
	if evaluation.Decision != nil {
		reply.Decision = &feynmanEvaluationDecisionReply{
			DecisionID: evaluation.Decision.DecisionID, Decision: evaluation.Decision.Decision,
			FinalPayload: evaluation.Decision.FinalPayload, DecisionNote: evaluation.Decision.DecisionNote,
			DecidedAt: evaluation.Decision.DecidedAt.Format(time.RFC3339),
		}
	}
	return reply
}

// ---------------------------------------------------------------------------
// 公共校验
// ---------------------------------------------------------------------------

// isFeynmanAudioUploadPath 判断请求路径是否为语音费曼录音上传接口。
// 路径含动态 attempt_id 段，不能用精确字符串匹配，只能按前后缀判断。
func isFeynmanAudioUploadPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/feynman/attempts/") && strings.HasSuffix(path, "/audio")
}

// feynmanAudioBodyLimitBytes 复用资料上传同样的“文件上限 + multipart 开销 + 最低下限”算法。
func feynmanAudioBodyLimitBytes(maxAudioBytes int64) int64 {
	return documentBodyLimitBytes(maxAudioBytes)
}

func (s *Server) requireFeynmanUser(c *gin.Context) (userID, attemptID string, okToProceed bool) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return "", "", false
	}
	attemptID = c.Param("attempt_id")
	if !uuidPattern.MatchString(attemptID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "练习尝试ID格式错误")
		return "", "", false
	}
	return userID, attemptID, true
}

func (s *Server) failFeynmanError(c *gin.Context, action string, err error) {
	var inputError *service.FeynmanInputError
	switch {
	case errors.As(err, &inputError):
		fail(c, http.StatusBadRequest, CodeBadRequest, inputError.Message)
	case errors.Is(err, service.ErrFeynmanAttemptNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "练习尝试不存在")
	case errors.Is(err, service.ErrFeynmanKnowledgePointNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "知识点不存在")
	case errors.Is(err, service.ErrFeynmanAttemptConfirmed):
		fail(c, http.StatusConflict, CodeConflict, "练习已确认转写，不能再修改")
	case errors.Is(err, service.ErrFeynmanAudioNotReady):
		fail(c, http.StatusConflict, CodeConflict, "当前录音尚未转写成功，无法确认")
	case errors.Is(err, service.ErrFeynmanNoActiveAudio):
		fail(c, http.StatusConflict, CodeConflict, "请先完成一次录音上传")
	case errors.Is(err, service.ErrFeynmanIdempotencyMismatch):
		fail(c, http.StatusConflict, CodeConflict, "幂等键已用于另一个练习主题")
	case errors.Is(err, service.ErrFeynmanTranscriptNotConfirmed):
		fail(c, http.StatusConflict, CodeConflict, "请先确认转写文本")
	case errors.Is(err, service.ErrFeynmanEvaluationNotFound):
		fail(c, http.StatusNotFound, CodeNotFound, "费曼评估不存在")
	case errors.Is(err, service.ErrFeynmanEvaluationNotReady):
		fail(c, http.StatusConflict, CodeConflict, "费曼评估尚未完成")
	case errors.Is(err, service.ErrFeynmanEvaluationDecided):
		fail(c, http.StatusConflict, CodeConflict, "费曼评估已处理")
	case errors.Is(err, service.ErrFeynmanEvaluationUnavailable):
		fail(c, http.StatusServiceUnavailable, CodeUpstream, "费曼评估暂未启用")
	case errors.Is(err, service.ErrFeynmanEvaluationInvalid):
		fail(c, http.StatusBadGateway, CodeUpstream, "评估模型返回内容不符合约束，请重试")
	default:
		s.log.Error(action, "trace_id", TraceIDFromContext(c.Request.Context()), "error", err)
		fail(c, http.StatusInternalServerError, CodeInternal, action)
	}
}

// ---------------------------------------------------------------------------
// 练习尝试
// ---------------------------------------------------------------------------

// createFeynmanAttemptHandler 创建（或按幂等键复用）一次费曼练习尝试。
func (s *Server) createFeynmanAttemptHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	var req createFeynmanAttemptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	if !uuidPattern.MatchString(strings.TrimSpace(req.KnowledgePointID)) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "知识点ID格式错误")
		return
	}
	detail, err := s.feynman.CreateAttempt(c.Request.Context(), userID, req.KnowledgePointID, req.IdempotencyKey)
	if err != nil {
		s.failFeynmanError(c, "创建练习尝试失败", err)
		return
	}
	ok(c, toFeynmanAttemptReply(detail))
}

// getFeynmanAttemptHandler 返回练习尝试详情，供前端刷新页面后恢复到正确步骤。
func (s *Server) getFeynmanAttemptHandler(c *gin.Context) {
	userID, attemptID, proceed := s.requireFeynmanUser(c)
	if !proceed {
		return
	}
	detail, err := s.feynman.GetAttempt(c.Request.Context(), userID, attemptID)
	if err != nil {
		s.failFeynmanError(c, "查询练习尝试失败", err)
		return
	}
	ok(c, toFeynmanAttemptReply(detail))
}

// uploadFeynmanAudioHandler 上传一段 Push-to-Talk 录音并同步完成 STT 转写。
func (s *Server) uploadFeynmanAudioHandler(c *gin.Context) {
	userID, attemptID, proceed := s.requireFeynmanUser(c)
	if !proceed {
		return
	}

	fileHeader, err := c.FormFile(feynmanAudioFormField)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "请求体大小超过上限")
			return
		}
		fail(c, http.StatusBadRequest, CodeBadRequest, "请通过 audio 字段上传录音文件")
		return
	}
	if fileHeader.Size > s.feynman.Limits().MaxAudioBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "音频大小超过上限")
		return
	}

	opened, err := fileHeader.Open()
	if err != nil {
		s.failFeynmanError(c, "读取录音文件失败", err)
		return
	}
	defer func() { _ = opened.Close() }()

	// 再套一层上限：Size 来自客户端声明，不能作为唯一防线。
	content, err := io.ReadAll(io.LimitReader(opened, s.feynman.Limits().MaxAudioBytes+1))
	if err != nil {
		s.failFeynmanError(c, "读取录音文件失败", err)
		return
	}
	if int64(len(content)) > s.feynman.Limits().MaxAudioBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "音频大小超过上限")
		return
	}

	var durationMs *int
	if raw := strings.TrimSpace(c.PostForm("duration_ms")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			fail(c, http.StatusBadRequest, CodeBadRequest, "duration_ms 必须是整数")
			return
		}
		durationMs = &parsed
	}

	mimeType := fileHeader.Header.Get("Content-Type")
	detail, err := s.feynman.UploadAudio(c.Request.Context(), userID, attemptID, content, mimeType, durationMs)
	if err != nil {
		s.failFeynmanError(c, "上传录音失败", err)
		return
	}
	ok(c, toFeynmanAttemptReply(detail))
}

// confirmFeynmanTranscriptHandler 确认或修正当前有效录音的转写文本。
func (s *Server) confirmFeynmanTranscriptHandler(c *gin.Context) {
	userID, attemptID, proceed := s.requireFeynmanUser(c)
	if !proceed {
		return
	}
	var req confirmFeynmanTranscriptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	detail, err := s.feynman.ConfirmTranscript(c.Request.Context(), userID, attemptID, req.ConfirmedTranscript)
	if err != nil {
		s.failFeynmanError(c, "确认转写失败", err)
		return
	}
	ok(c, toFeynmanAttemptReply(detail))
}

func (s *Server) evaluateFeynmanAttemptHandler(c *gin.Context) {
	userID, attemptID, proceed := s.requireFeynmanUser(c)
	if !proceed {
		return
	}
	evaluation, err := s.feynman.EvaluateAttempt(c.Request.Context(), userID, attemptID)
	if err != nil {
		s.failFeynmanError(c, "评估费曼练习失败", err)
		return
	}
	ok(c, toFeynmanEvaluationReply(evaluation))
}

func (s *Server) getFeynmanEvaluationHandler(c *gin.Context) {
	userID, attemptID, proceed := s.requireFeynmanUser(c)
	if !proceed {
		return
	}
	evaluation, err := s.feynman.GetEvaluation(c.Request.Context(), userID, attemptID)
	if err != nil {
		s.failFeynmanError(c, "查询费曼评估失败", err)
		return
	}
	ok(c, toFeynmanEvaluationReply(evaluation))
}

func (s *Server) decideFeynmanEvaluationHandler(c *gin.Context) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return
	}
	evaluationID := c.Param("evaluation_id")
	if !uuidPattern.MatchString(evaluationID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "评估ID格式错误")
		return
	}
	var req decideFeynmanEvaluationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	evaluation, err := s.feynman.DecideEvaluation(c.Request.Context(), userID, evaluationID, req.Decision, req.FinalPayload, req.DecisionNote)
	if err != nil {
		s.failFeynmanError(c, "处理费曼评估失败", err)
		return
	}
	ok(c, toFeynmanEvaluationReply(evaluation))
}

// ---------------------------------------------------------------------------
// 知识点 Rubric
// ---------------------------------------------------------------------------

func (s *Server) requireFeynmanKnowledgePointUser(c *gin.Context) (userID, knowledgePointID string, okToProceed bool) {
	userID, authenticated := UserIDFromContext(c.Request.Context())
	if !authenticated {
		fail(c, http.StatusUnauthorized, CodeUnauthorized, "请先建立访客身份")
		return "", "", false
	}
	knowledgePointID = c.Param("knowledge_point_id")
	if !uuidPattern.MatchString(knowledgePointID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "知识点ID格式错误")
		return "", "", false
	}
	return userID, knowledgePointID, true
}

// getFeynmanRubricHandler 返回知识点当前生效的 Rubric；首次访问自动创建固定模板 v1。
func (s *Server) getFeynmanRubricHandler(c *gin.Context) {
	userID, knowledgePointID, proceed := s.requireFeynmanKnowledgePointUser(c)
	if !proceed {
		return
	}
	rubric, err := s.feynman.GetRubric(c.Request.Context(), userID, knowledgePointID)
	if err != nil {
		s.failFeynmanError(c, "查询知识点 Rubric 失败", err)
		return
	}
	ok(c, toFeynmanRubricReply(rubric))
}

// createFeynmanRubricVersionHandler 让用户提交一份新的 Rubric 版本。
func (s *Server) createFeynmanRubricVersionHandler(c *gin.Context) {
	userID, knowledgePointID, proceed := s.requireFeynmanKnowledgePointUser(c)
	if !proceed {
		return
	}
	var req createFeynmanRubricVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求体格式错误")
		return
	}
	criteria := make([]service.RubricCriterion, 0, len(req.Criteria))
	for _, item := range req.Criteria {
		criteria = append(criteria, service.RubricCriterion{
			Dimension:   item.Dimension,
			Label:       item.Label,
			Weight:      item.Weight,
			Description: item.Description,
		})
	}
	rubric, err := s.feynman.CreateRubricVersion(c.Request.Context(), userID, knowledgePointID, criteria)
	if err != nil {
		s.failFeynmanError(c, "创建知识点 Rubric 版本失败", err)
		return
	}
	ok(c, toFeynmanRubricReply(rubric))
}
