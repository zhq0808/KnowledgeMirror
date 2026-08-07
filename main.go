// Command server 是面试训练 Agent 的 HTTP 服务入口。
package main

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/handler"
	"KnowledgeMirror/internal/llm"
	"KnowledgeMirror/internal/logger"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
	"KnowledgeMirror/internal/stt"
	"KnowledgeMirror/internal/tts"
)

// migrationsFS 把 migrations/ 下的 SQL 打进二进制，部署时无需额外携带脚本。
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

func main() {
	if err := run(); err != nil {
		slog.Error("服务启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. 加载配置（yaml + env）。配置路径可用 CONFIG_PATH 覆盖，便于容器/多环境部署。
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// 2. 初始化日志
	log := logger.New(cfg.Log.Level, cfg.Log.Debug)
	slog.SetDefault(log)

	// 3. 构建 LLM 客户端（DeepSeek）。API Key 缺省时不阻断启动，调用时走降级兜底。
	if cfg.DeepSeek.APIKey == "" {
		log.Warn("未配置 DEEPSEEK_API_KEY，对话将返回降级兜底回复（请在 .env 中填入）")
	}
	client := llm.NewDeepSeekClient(cfg.DeepSeek.APIKey, cfg.DeepSeek.BaseURL, cfg.DeepSeek.Model, cfg.DeepSeek.Temperature, time.Duration(cfg.DeepSeek.TimeoutSeconds)*time.Second)
	prompt, err := service.LoadChatPrompt(cfg.Chat.PromptPath, cfg.Chat.PromptVersion, cfg.Chat.TrustBoundary)
	if err != nil {
		return err
	}

	// 3.1 初始化 PostgreSQL（对话历史 source of truth）。连不上直接失败——不允许无存储启动。
	db, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("PostgreSQL 连接就绪", "addr", cfg.Postgres.Host+":"+strconv.Itoa(cfg.Postgres.Port), "db", cfg.Postgres.DBName)

	// 3.1b 执行数据库迁移（golang-migrate）。结构不到位不允许启动——与 source of truth fail-fast 一致。
	if err := store.RunMigrations(cfg.Postgres, migrationsFS, "migrations"); err != nil {
		return err
	}
	log.Info("数据库迁移已应用至最新版本")

	// 3.2 初始化 Redis（缓热上下文）。连不上不阻断启动，运行时降级为直读 PostgreSQL。
	rdb, err := store.NewRedis(cfg.Redis)
	if err != nil {
		log.Warn("Redis 连接失败，将降级为直读 PostgreSQL", "error", err)
		rdb = nil
	} else {
		defer rdb.Close()
		log.Info("Redis 连接就绪", "addr", cfg.Redis.Addr)
	}
	_ = rdb // TODO(P2): 注入 repository / server，当前仅完成建连与优雅关闭

	// writeTimeout 是单个请求最长处理时间（LLM 调用可能较慢，给足余量）。
	// 优雅关闭的等待时间必须 >= 它，否则在途慢请求会被提前掐断，见下方 shutdownGrace。
	const writeTimeout = 60 * time.Second

	// 4. 在 composition root 组装业务服务；HTTP handler 只依赖 service。
	identityRepository := store.NewPostgresIdentityRepository(db)
	identityService := service.NewIdentityService(identityRepository, time.Duration(cfg.Identity.GuestTokenTTLHours)*time.Hour)
	sessionRepository := store.NewPostgresSessionRepository(db)
	sessionService := service.NewSessionService(sessionRepository)
	messageRepository := store.NewPostgresMessageRepository(db)
	messageService := service.NewMessageService(messageRepository)
	turnLeaseRepository := store.NewPostgresTurnLeaseRepository(db)
	turnLeaseService := service.NewTurnLeaseService(turnLeaseRepository)
	documentRepository := store.NewPostgresDocumentRepository(db)
	documentService := service.NewDocumentService(documentRepository, service.DocumentLimits{
		MaxFileBytes:  cfg.Document.MaxFileBytes,
		MaxTitleChars: cfg.Document.MaxTitleChars,
		Parse: service.MarkdownParseLimits{
			MaxRawChars:   cfg.Document.MaxRawChars,
			MaxHeadings:   cfg.Document.MaxHeadings,
			MaxChunks:     cfg.Document.MaxChunks,
			MaxChunkChars: cfg.Document.MaxChunkChars,
		},
	})
	// 候选内容抽取：AI 只提案，正式知识点/计划/事实必须由用户在候选确认接口逐条确认。
	// 未启用或未配置模型时，抽取接口返回“未启用”，但人工确认链路照常可用。
	var candidateService *service.CandidateService
	if cfg.Candidate.Enabled {
		candidateClient := llm.NewDeepSeekClient(
			cfg.DeepSeek.APIKey,
			cfg.DeepSeek.BaseURL,
			cfg.Candidate.ExtractorModel,
			0,
			time.Duration(cfg.Candidate.ExtractTimeoutSeconds)*time.Second,
		)
		candidateExtractor, err := service.LoadLLMCandidateExtractor(
			cfg.Candidate.ExtractorPromptPath,
			cfg.Candidate.ExtractorVersion,
			cfg.Candidate.ExtractorModel,
			candidateClient,
		)
		if err != nil {
			return err
		}
		candidateService = service.NewCandidateService(
			store.NewPostgresCandidateRepository(db),
			documentService,
			candidateExtractor,
			service.CandidateLimits{
				MaxCandidates:          cfg.Candidate.MaxCandidates,
				MaxTitleChars:          cfg.Candidate.MaxTitleChars,
				MaxSummaryChars:        cfg.Candidate.MaxSummaryChars,
				MaxNoteChars:           cfg.Candidate.MaxNoteChars,
				MaxSourcesPerCandidate: cfg.Candidate.MaxSourcesPerCandidate,
				MaxChunksPerRequest:    cfg.Candidate.MaxChunksPerRequest,
				MaxChunkChars:          cfg.Candidate.MaxChunkChars,
			},
		)
		log.Info("候选内容抽取已启用", "prompt_version", cfg.Candidate.ExtractorVersion, "model", cfg.Candidate.ExtractorModel)
	} else {
		log.Info("候选内容抽取未启用（CANDIDATE_ENABLED=false）")
	}

	coachRepository := store.NewPostgresCoachRepository(db)
	var coachService *service.CoachService

	memoryRepository := store.NewPostgresMemoryRepository(db)
	memoryService := service.NewMemoryService(memoryRepository, service.MemoryExtractionLimits{
		MaxOperations:       cfg.Memory.MaxOperations,
		MaxMemoryValueChars: cfg.Memory.MaxMemoryValueChars,
		MinConfidence:       cfg.Memory.MinConfidence,
	})
	chatService := service.NewChatService(client, prompt, memoryService, service.MemoryBudget{
		MaxCount: cfg.Memory.MaxMemoryInput,
		MaxChars: cfg.Memory.MaxMemoryInputChars,
	}, cfg.Chat.MaxReplyChars)

	// 知识库检索 v0：只召回用户已确认“供 AI 检索”且片段开关打开的当前版本片段。
	// 未启用时聊天链路照常工作，只是不带资料引用。
	var retrievalService *service.RetrievalService
	if cfg.Retrieval.Enabled {
		retrievalService = service.NewRetrievalService(
			store.NewPostgresRetrievalRepository(db),
			service.RetrievalLimits{
				MaxQueryChars:          cfg.Retrieval.MaxQueryChars,
				MaxTerms:               cfg.Retrieval.MaxTerms,
				MaxCandidates:          cfg.Retrieval.MaxCandidates,
				MaxResults:             cfg.Retrieval.MaxResults,
				MaxPassagesPerDocument: cfg.Retrieval.MaxPassagesPerDocument,
				MaxChunkChars:          cfg.Retrieval.MaxChunkChars,
				ContextBudgetChars:     cfg.Retrieval.ContextBudgetChars,
			},
			log,
		)
		chatService.WithRetrieval(retrievalService)
		log.Info("知识库检索已启用",
			"max_results", cfg.Retrieval.MaxResults,
			"context_budget_chars", cfg.Retrieval.ContextBudgetChars,
		)
	} else {
		log.Info("知识库检索未启用（RETRIEVAL_ENABLED=false）")
	}

	// 记忆抽取使用独立模型实例和 Prompt，后续调优只替换模板与版本，不改异步管道。
	var memoryPipeline *service.MemoryPipeline
	if cfg.Memory.Enabled {
		memoryClient := llm.NewDeepSeekClient(
			cfg.DeepSeek.APIKey,
			cfg.DeepSeek.BaseURL,
			cfg.Memory.ExtractorModel,
			0,
			time.Duration(cfg.Memory.ExtractTimeoutSeconds)*time.Second,
		)
		memoryExtractor, err := service.LoadLLMMemoryExtractor(
			cfg.Memory.ExtractorPromptPath,
			cfg.Memory.ExtractorVersion,
			memoryClient,
		)
		if err != nil {
			return err
		}
		memoryPipeline, err = service.NewMemoryPipeline(
			memoryService,
			memoryRepository,
			memoryExtractor,
			memoryPipelineConfig(cfg.Memory),
			log,
		)
		if err != nil {
			return err
		}
		memoryPipeline.Start()
		log.Info("记忆抽取管道已启动", "workers", cfg.Memory.WorkerCount, "queue", cfg.Memory.QueueSize, "prompt_version", cfg.Memory.ExtractorVersion)
		defer func() {
			if err := memoryPipeline.Close(); err != nil {
				log.Warn("记忆抽取管道关闭异常", "error", err)
			}
		}()
	} else {
		log.Info("记忆抽取管道未启用（MEMORY_ENABLED=false）")
	}

	// 对话式费曼学习：练习是聊天里的一种会话状态，不是独立页面。
	// 它挂在 ChatService 的最前面，未启用时聊天链路完全不受影响。
	var feynmanDialogService *service.FeynmanDialogService
	if cfg.Feynman.Dialog.Enabled {
		dialogModel := cfg.Feynman.Dialog.Model
		if dialogModel == "" {
			dialogModel = cfg.DeepSeek.Model
		}
		dialogClient := llm.NewDeepSeekClient(
			cfg.DeepSeek.APIKey,
			cfg.DeepSeek.BaseURL,
			dialogModel,
			0,
			time.Duration(cfg.Feynman.Dialog.TimeoutSeconds)*time.Second,
		)
		analyzer, err := service.LoadLLMFeynmanAnswerAnalyzer(
			cfg.Feynman.Dialog.PromptPath,
			cfg.Feynman.Dialog.PromptVersion,
			dialogModel,
			dialogClient,
		)
		if err != nil {
			return err
		}
		// retrievalService 为 nil 时按“未启用检索”处理：分析照常进行，
		// 但上下文里会明确写着没有资料，模型不会因此编造引用。
		var dialogRetriever service.ChatRetriever
		if retrievalService != nil {
			dialogRetriever = retrievalService
		}
		feynmanDialogService = service.NewFeynmanDialogService(
			store.NewPostgresFeynmanPracticeRepository(db),
			analyzer,
			dialogRetriever,
			service.FeynmanDialogLimits{
				MaxControlPhraseRunes: cfg.Feynman.Dialog.MaxControlPhraseRunes,
				MaxTopicRunes:         cfg.Feynman.Dialog.MaxTopicRunes,
				MaxProbeRunes:         cfg.Feynman.Dialog.MaxProbeRunes,
				MaxGaps:               cfg.Feynman.Dialog.MaxGaps,
				MaxSecondaryGaps:      cfg.Feynman.Dialog.MaxSecondaryGaps,
				MaxContextTurns:       cfg.Feynman.Dialog.MaxContextTurns,
				MaxAnswerRunes:        cfg.Feynman.Dialog.MaxAnswerRunes,
			},
			log,
			coachRepository,
		)
		chatService.WithPractice(feynmanDialogService)
		coachService = service.NewCoachService(coachRepository, time.Now)
		log.Info("对话式费曼学习与每日教练已启用", "prompt_version", cfg.Feynman.Dialog.PromptVersion, "model", dialogModel)
	} else {
		log.Info("对话式费曼学习未启用（FEYNMAN_DIALOG_ENABLED=false）")
	}

	// 通用语音输入：普通对话输入区的 Push-to-Talk。
	// 它和上面的语音费曼练习共用同一个 sttProvider 实例：同一套供应商配置只存一份，
	// 避免两处配置飘移后“这里能转写那里不能”这种排查起来最费劲的问题。
	var voiceService *service.VoiceCaptureService
	if cfg.Voice.Enabled {
		// 词表加载失败不阻断启动：它只影响“术语可能被听错”这一条提示，
		// 转写失败、置信度偏低这两条更硬的拦截仍然生效。
		glossary, glossaryErr := service.LoadTermGlossary(cfg.Voice.GlossaryPath)
		if glossaryErr != nil {
			log.Warn("语音术语词表加载失败，术语歧义提示已关闭", "path", cfg.Voice.GlossaryPath, "error", glossaryErr)
			glossary = nil
		}
		voiceService = service.NewVoiceCaptureService(
			store.NewPostgresVoiceCaptureRepository(db),
			glossary,
			service.VoiceLimits{
				MaxAudioBytes:      cfg.Voice.MaxAudioBytes,
				MaxDurationMS:      cfg.Voice.MaxDurationMS,
				MaxTranscriptChars: cfg.Voice.MaxTranscriptChars,
				MinConfidence:      cfg.Voice.MinConfidence,
				MaxAmbiguousTerms:  cfg.Voice.MaxAmbiguousTerms,
				TranscribingStale:  time.Duration(cfg.Voice.TranscribingStaleSecond) * time.Second,
			},
			log,
		)
		log.Info("实时语音结果持久化已启用",
			"glossary_terms", glossary.Size(),
			"min_confidence", cfg.Voice.MinConfidence)
	} else {
		log.Info("通用语音输入未启用（VOICE_ENABLED=false）")
	}

	// 实时语音使用独立的 DashScope Paraformer Provider。配置不完整时只不注册 WebSocket，
	// 上面的 MiMo 文件式 STT、文字聊天和下面的 MiMo TTS 均保持可用。
	var realtimeVoiceService *service.RealtimeVoiceService
	if cfg.Voice.Realtime.Enabled && voiceService != nil &&
		cfg.Voice.Realtime.APIKey != "" && cfg.Voice.Realtime.WorkspaceID != "" {
		realtimeProvider := stt.NewDashScopeParaformerProvider(stt.DashScopeParaformerOptions{
			APIKey:          cfg.Voice.Realtime.APIKey,
			WorkspaceID:     cfg.Voice.Realtime.WorkspaceID,
			WebSocketURL:    cfg.Voice.Realtime.WebSocketURL,
			Model:           cfg.Voice.Realtime.Model,
			SampleRate:      cfg.Voice.Realtime.SampleRate,
			StartTimeout:    time.Duration(cfg.Voice.Realtime.StartTimeoutSeconds) * time.Second,
			FinishTimeout:   time.Duration(cfg.Voice.Realtime.FinishTimeoutSeconds) * time.Second,
			EventBufferSize: cfg.Voice.Realtime.EventQueueSize,
		})
		maxFrameBytes := int64(cfg.Voice.Realtime.SampleRate * 2 * cfg.Voice.Realtime.FrameMS / 1000)
		realtimeVoiceService = service.NewRealtimeVoiceService(
			realtimeProvider,
			voiceService,
			service.RealtimeVoiceLimits{
				SampleRate:           cfg.Voice.Realtime.SampleRate,
				MaxFrameBytes:        maxFrameBytes,
				MaxDuration:          time.Duration(cfg.Voice.Realtime.MaxDurationMS) * time.Millisecond,
				MaxAudioBytes:        cfg.Voice.Realtime.MaxAudioBytes,
				MaxConcurrentStreams: cfg.Voice.Realtime.MaxConcurrentStreams,
				MaxStreamsPerUser:    cfg.Voice.Realtime.MaxStreamsPerUser,
				AudioQueueFrames:     cfg.Voice.Realtime.AudioQueueFrames,
				EventQueueSize:       cfg.Voice.Realtime.EventQueueSize,
				StartTimeout:         time.Duration(cfg.Voice.Realtime.StartTimeoutSeconds) * time.Second,
				FinishTimeout:        time.Duration(cfg.Voice.Realtime.FinishTimeoutSeconds) * time.Second,
				WriteTimeout:         time.Duration(cfg.Voice.Realtime.WriteTimeoutSeconds) * time.Second,
				IdleTimeout:          time.Duration(cfg.Voice.Realtime.IdleTimeoutSeconds) * time.Second,
			},
			log,
		)
		log.Info("实时语音输入已启用", "stt_provider", realtimeProvider.Name(), "model", realtimeProvider.Model())
	} else if cfg.Voice.Realtime.Enabled {
		log.Info("实时语音输入未启用（配置不完整或通用语音输入已关闭）")
	} else {
		log.Info("实时语音输入未启用（VOICE_REALTIME_ENABLED=false）")
	}

	// 语音合成：把费曼提问和追问念出来。未配置密钥时直接不注册接口，
	// 而不是像 STT 那样造一个占位实现：念一段占位音频没任何价值，没有声音就看文字即可。
	var speechService *service.SpeechService
	if cfg.Speech.Enabled && cfg.Speech.APIKey != "" {
		speechService = service.NewSpeechService(
			tts.NewMiMoProvider(
				cfg.Speech.APIKey,
				cfg.Speech.BaseURL,
				cfg.Speech.Model,
				cfg.Speech.Voice,
				time.Duration(cfg.Speech.TimeoutSeconds)*time.Second,
			),
			service.SpeechLimits{
				MaxTextRunes: cfg.Speech.MaxTextRunes,
				StyleHint:    cfg.Speech.StyleHint,
			},
			log,
		)
		log.Info("语音合成已启用",
			"tts_provider", speechService.ProviderName(),
			"model", cfg.Speech.Model,
			"voice", cfg.Speech.Voice)
	} else if cfg.Speech.Enabled {
		log.Info("语音合成未启用（未配置 SPEECH_API_KEY）")
	} else {
		log.Info("语音合成未启用（SPEECH_ENABLED=false）")
	}

	srvHandler := handler.NewServer(chatService, identityService, sessionService, messageService, turnLeaseService, documentService, candidateService, retrievalService, coachService, nil, feynmanDialogService, voiceService, realtimeVoiceService, speechService, memoryPipeline, cfg.Identity, log).Handler()
	srv := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           srvHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	// 5. 启动监听（独立 goroutine），错误回传主协程
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP 服务启动", "port", cfg.HTTP.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// 6. 等待退出信号或启动错误
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return err
	case sig := <-sigCh:
		log.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	// 7. 真优雅关闭：停止接收新请求，等待在途请求处理完毕。
	// 等待上限必须 >= writeTimeout，再加几秒缓冲，确保最慢的在途 LLM 请求也能跑完，
	// 而不是刚等到一半就被 cancel 掐断连接（那样对慢请求来说优雅关闭形同虚设）。
	shutdownGrace := writeTimeout + 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return err
	}

	log.Info("服务已优雅关闭")
	return nil
}

// memoryPipelineConfig 把配置里的秒级参数转成抽取管道所需的 time.Duration 参数。
func memoryPipelineConfig(cfg config.MemoryConfig) service.MemoryPipelineConfig {
	return service.MemoryPipelineConfig{
		WorkerCount:      cfg.WorkerCount,
		QueueSize:        cfg.QueueSize,
		ScanInterval:     time.Duration(cfg.ScanIntervalSeconds) * time.Second,
		LeaseDuration:    time.Duration(cfg.LeaseDurationSeconds) * time.Second,
		ExtractTimeout:   time.Duration(cfg.ExtractTimeoutSeconds) * time.Second,
		TaskTimeout:      time.Duration(cfg.TaskTimeoutSeconds) * time.Second,
		ScanBatchSize:    cfg.ScanBatchSize,
		MaxBatchMessages: cfg.MaxBatchMessages,
		MaxBatchChars:    cfg.MaxBatchChars,
		MemoryInputLimit: cfg.MaxMemoryInput,
		MemoryInputChars: cfg.MaxMemoryInputChars,
		BaseRetryBackoff: time.Duration(cfg.BaseRetryBackoffSeconds) * time.Second,
		MaxRetryBackoff:  time.Duration(cfg.MaxRetryBackoffSeconds) * time.Second,
		ShutdownGrace:    time.Duration(cfg.ShutdownGraceSeconds) * time.Second,
		ExtractorModel:   cfg.ExtractorModel,
		ExtractorVersion: cfg.ExtractorVersion,
	}
}
