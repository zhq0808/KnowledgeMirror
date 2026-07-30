// Package handler 提供 HTTP 接口层：路由、中间件、DTO、统一响应。
package handler

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"healthAgent/internal/config"
	"healthAgent/internal/service"
)

// memoryNotifier 抽象“turn 完成后向异步抽取管道投递 session_id”，便于测试注入空实现或省略。
type memoryNotifier interface {
	Notify(sessionID string)
}

// Server 持有 HTTP 层依赖，并挂载路由。
type Server struct {
	chat           *service.ChatService
	identity       *service.IdentityService
	sessions       *service.SessionService
	messages       *service.MessageService
	turnLeases     *service.TurnLeaseService
	documents      *service.DocumentService
	candidates     *service.CandidateService
	retrieval      *service.RetrievalService
	feynman        *service.FeynmanService
	practice       *service.FeynmanDialogService
	voice          *service.VoiceCaptureService
	realtimeVoice  *service.RealtimeVoiceService
	speech         *service.SpeechService
	memory         memoryNotifier
	identityConfig config.IdentityConfig
	log            *slog.Logger
	engine         *gin.Engine
}

// NewServer 构建 HTTP Server 并注册路由与中间件。memory 可为 nil（关闭异步抽取时不投递）；
// feynman 可为 nil（未配置 STT/知识点时语音费曼练习接口不注册）；
// practice 可为 nil（未启用对话式费曼学习时不下发练习状态）；
// voice 可为 nil（未配置 STT 或关闭语音输入时录音接口不注册）；
// realtimeVoice 可为 nil（实时配置不完整时 WebSocket 路由不注册）；
// speech 可为 nil（未配置 TTS 或关闭语音合成时朗读接口不注册）。
func NewServer(chat *service.ChatService, identity *service.IdentityService, sessions *service.SessionService, messages *service.MessageService, turnLeases *service.TurnLeaseService, documents *service.DocumentService, candidates *service.CandidateService, retrieval *service.RetrievalService, feynman *service.FeynmanService, practice *service.FeynmanDialogService, voice *service.VoiceCaptureService, realtimeVoice *service.RealtimeVoiceService, speech *service.SpeechService, memory memoryNotifier, identityConfig config.IdentityConfig, log *slog.Logger) *Server {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		chat:           chat,
		identity:       identity,
		sessions:       sessions,
		messages:       messages,
		turnLeases:     turnLeases,
		documents:      documents,
		candidates:     candidates,
		retrieval:      retrieval,
		feynman:        feynman,
		practice:       practice,
		voice:          voice,
		realtimeVoice:  realtimeVoice,
		speech:         speech,
		memory:         memory,
		identityConfig: identityConfig,
		log:            log,
		engine:         gin.New(), // 不用 gin.Default()，用我们自己的中间件（日志/recover）
	}
	s.routes()
	return s
}

// maxBodyBytes 是默认请求体大小上限（2MB）。
// 文本录入足够；资料上传另有 multipart 与单文件上限，见 documentBodyLimitBytes。
const maxBodyBytes = 2 << 20

// Markdown 上传至少允许 4 MiB 请求体；文件上限更大时额外预留 1 MiB multipart 开销。
const (
	minDocumentBodyLimitBytes = 4 << 20
	multipartOverheadBytes    = 1 << 20
)

func documentBodyLimitBytes(maxFileBytes int64) int64 {
	limit := maxFileBytes + multipartOverheadBytes
	if limit < minDocumentBodyLimitBytes {
		return minDocumentBodyLimitBytes
	}
	return limit
}

// routes 注册中间件与路由。
func (s *Server) routes() {
	s.engine.Use(traceMiddleware())
	s.engine.Use(recoverMiddleware(s.log))
	s.engine.Use(accessLogMiddleware(s.log))
	// 全局请求体上限；资料上传和录音上传路径由各自的路由级中间件放宽到更大的上限，
	// 这里必须跳过它们，否则内层 http.MaxBytesReader 仍会被外层的小上限截断。
	s.engine.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost &&
			(c.Request.URL.Path == "/api/v1/documents" ||
				c.Request.URL.Path == voiceCapturePath ||
				isFeynmanAudioUploadPath(c.Request.URL.Path)) {
			c.Next()
			return
		}
		bodyLimitMiddleware(maxBodyBytes)(c)
	})

	s.engine.NoRoute(func(c *gin.Context) {
		fail(c, http.StatusNotFound, CodeNotFound, "接口不存在")
	})
	s.engine.NoMethod(func(c *gin.Context) {
		fail(c, http.StatusMethodNotAllowed, CodeMethodNA, "方法不允许")
	})

	s.engine.GET("/health", s.healthHandler)

	// 业务路由。竖切片逐步加入。
	v1 := s.engine.Group("/api/v1")
	{
		v1.POST("/guest", s.guestHandler)

		protected := v1.Group("")
		protected.Use(authMiddleware(s.identity, s.identityConfig.GuestCookieName, s.log))
		protected.POST("/sessions", s.createSessionHandler)
		protected.GET("/sessions", s.listSessionsHandler)
		protected.GET("/sessions/:session_id/messages", s.listSessionMessagesHandler)
		protected.POST("/chat/stream", s.chatStreamHandler)

		// 对话式费曼学习：只读当前会话的练习状态，供刷新/切会话后恢复状态条。
		// 练习本身没有任何写接口：开始/暂停/跳过全部走 /chat/stream 的自然语言。
		if s.practice != nil {
			protected.GET("/feynman/practice-state", s.getFeynmanPracticeStateHandler)
		}

		// 通用语音输入：只负责把录音转成文本，转完仍旧由 /chat/stream 承接，
		// 文字和语音共用同一套分析流程，这里不存在第二条业务链路。
		if s.voice != nil {
			voice := protected.Group("/voice")
			voice.POST("/captures",
				bodyLimitMiddleware(feynmanAudioBodyLimitBytes(s.voice.Limits().MaxAudioBytes)),
				s.createVoiceCaptureHandler)
			voice.GET("/captures/:capture_id", s.getVoiceCaptureHandler)
		}
		if s.realtimeVoice != nil {
			protected.GET("/voice/realtime", s.realtimeVoiceHandler)
		}

		// 语音合成：把已经展示给用户的费曼提问念出来。
		// 只输出音频，不写入任何状态；调不通时前端退回纯文字，不阻断练习。
		if s.speech != nil {
			protected.POST("/speech", s.createSpeechHandler)
		}

		// 资料治理：上传、解析、版本、来源片段、用途确认。
		// 这些接口只改变资料状态，不产生知识点，也不改变任何掌握状态。
		if s.documents != nil {
			documents := protected.Group("/documents")
			documents.POST("", bodyLimitMiddleware(documentBodyLimitBytes(s.documents.Limits().MaxFileBytes)), s.uploadDocumentHandler)
			documents.GET("", s.listDocumentsHandler)
			documents.GET("/:document_id", s.getDocumentHandler)
			documents.PATCH("/:document_id", s.updateDocumentHandler)
			documents.DELETE("/:document_id", s.deleteDocumentHandler)
			documents.POST("/:document_id/parse/retry", s.retryDocumentParseHandler)
			documents.PUT("/:document_id/usages", s.confirmDocumentUsagesHandler)
			documents.GET("/:document_id/versions", s.listDocumentVersionsHandler)
			documents.GET("/:document_id/versions/:version_id", s.getDocumentVersionHandler)
			documents.GET("/:document_id/chunks", s.listDocumentChunksHandler)
			documents.PATCH("/:document_id/chunks/:chunk_id", s.updateDocumentChunkHandler)

			// 候选内容：AI 抽取结果只能是「待确认」，正式数据必须由用户在下面的接口逐条确认。
			if s.candidates != nil {
				documents.POST("/:document_id/candidates/extract", s.extractDocumentCandidatesHandler)
			}
		}

		if s.candidates != nil {
			candidates := protected.Group("/candidates")
			candidates.GET("", s.listCandidatesHandler)
			candidates.GET("/:candidate_id", s.getCandidateHandler)
			candidates.PATCH("/:candidate_id", s.updateCandidateHandler)
			candidates.POST("/:candidate_id/confirm", s.confirmCandidateHandler)
			candidates.POST("/:candidate_id/merge", s.mergeCandidateHandler)
			candidates.POST("/:candidate_id/archive", s.archiveCandidateHandler)
			candidates.POST("/:candidate_id/reject", s.rejectCandidateHandler)

			protected.GET("/knowledge-points", s.listKnowledgePointsHandler)
		}

		// 知识库检索预览：只返回当前用户已授权“供 AI 检索”的片段命中情况，
		// 不改变任何资料状态，也不产生任何掌握状态。
		if s.retrieval != nil {
			protected.POST("/retrieval/preview", s.retrievalPreviewHandler)
		}

		// 语音费曼练习：录音、转写确认、版本化 Rubric、可信来源评估与人工证据决策。
		if s.feynman != nil {
			attempts := protected.Group("/feynman/attempts")
			attempts.POST("", s.createFeynmanAttemptHandler)
			attempts.GET("/:attempt_id", s.getFeynmanAttemptHandler)
			attempts.POST("/:attempt_id/audio",
				bodyLimitMiddleware(feynmanAudioBodyLimitBytes(s.feynman.Limits().MaxAudioBytes)),
				s.uploadFeynmanAudioHandler)
			attempts.POST("/:attempt_id/confirm", s.confirmFeynmanTranscriptHandler)
			attempts.POST("/:attempt_id/evaluate", s.evaluateFeynmanAttemptHandler)
			attempts.GET("/:attempt_id/evaluation", s.getFeynmanEvaluationHandler)
			protected.POST("/feynman/evaluations/:evaluation_id/decision", s.decideFeynmanEvaluationHandler)

			protected.GET("/knowledge-points/:knowledge_point_id/rubric", s.getFeynmanRubricHandler)
			protected.POST("/knowledge-points/:knowledge_point_id/rubric/versions", s.createFeynmanRubricVersionHandler)
		}
	}
}

// Handler 返回底层 http.Handler，供 http.Server 使用。
func (s *Server) Handler() http.Handler {
	return s.engine
}
