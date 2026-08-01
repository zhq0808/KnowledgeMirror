package config

import (
	"fmt"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/joho/godotenv"
)

// Config 是应用的全部配置
type Config struct {
	HTTP      HTTPConfig      `yaml:"http"     env-prefix:"HTTP_"`
	Identity  IdentityConfig  `yaml:"identity" env-prefix:"IDENTITY_"`
	Chat      ChatConfig      `yaml:"chat"     env-prefix:"CHAT_"`
	DeepSeek  LLMConfig       `yaml:"deepseek" env-prefix:"DEEPSEEK_"`
	OpenAI    OpenAIConfig    `yaml:"openai"   env-prefix:"OPENAI_"`
	Postgres  PostgresConfig  `yaml:"postgres" env-prefix:"POSTGRES_"`
	Redis     RedisConfig     `yaml:"redis"    env-prefix:"REDIS_"`
	Memory    MemoryConfig    `yaml:"memory"    env-prefix:"MEMORY_"`
	Document  DocumentConfig  `yaml:"document"  env-prefix:"DOCUMENT_"`
	Candidate CandidateConfig `yaml:"candidate" env-prefix:"CANDIDATE_"`
	Retrieval RetrievalConfig `yaml:"retrieval" env-prefix:"RETRIEVAL_"`
	Feynman   FeynmanConfig   `yaml:"feynman"   env-prefix:"FEYNMAN_"`
	Voice     VoiceConfig     `yaml:"voice"     env-prefix:"VOICE_"`
	Speech    SpeechConfig    `yaml:"speech"    env-prefix:"SPEECH_"`
	Log       LogConfig       `yaml:"log"       env-prefix:"LOG_"`
}

// SpeechConfig 控制文字转语音（TTS）：让费曼提问和追问出声，模拟面试官开口发问。
//
// 与语音输入相反，这条链路上的文本全部由本系统生成，不含用户上传内容，
// 因此没有可信度问题；这里的上限只用于防御性地控制单次合成的开销。
type SpeechConfig struct {
	Enabled bool   `yaml:"enabled"         env:"ENABLED"         env-default:"true"`
	APIKey  string `yaml:"-"               env:"API_KEY"`
	BaseURL string `yaml:"base_url"        env:"BASE_URL"        env-default:"https://api.xiaomimimo.com/v1"`
	Model   string `yaml:"model"           env:"MODEL"           env-default:"mimo-v2.5-tts"`
	Voice   string `yaml:"voice"           env:"VOICE"           env-default:"冰糖"`
	// StyleHint 是念稿风格指令，会作为 user 消息传给模型，不会被念出来。
	StyleHint      string `yaml:"style_hint"      env:"STYLE_HINT"`
	MaxTextRunes   int    `yaml:"max_text_runes"  env:"MAX_TEXT_RUNES"  env-default:"600"`
	TimeoutSeconds int    `yaml:"timeout_seconds" env:"TIMEOUT_SECONDS" env-default:"60"`
}

// VoiceConfig 控制普通对话输入区的实时 ASR 语音输入。
type VoiceConfig struct {
	Enabled                 bool                `yaml:"enabled"                   env:"ENABLED"                   env-default:"true"`
	MaxAudioBytes           int64               `yaml:"max_audio_bytes"           env:"MAX_AUDIO_BYTES"           env-default:"6291456"`
	MaxDurationMS           int                 `yaml:"max_duration_ms"           env:"MAX_DURATION_MS"           env-default:"180000"`
	MaxTranscriptChars      int                 `yaml:"max_transcript_chars"      env:"MAX_TRANSCRIPT_CHARS"      env-default:"8000"`
	MinConfidence           float64             `yaml:"min_confidence"            env:"MIN_CONFIDENCE"            env-default:"0.6"`
	MaxAmbiguousTerms       int                 `yaml:"max_ambiguous_terms"       env:"MAX_AMBIGUOUS_TERMS"       env-default:"5"`
	GlossaryPath            string              `yaml:"glossary_path"             env:"GLOSSARY_PATH"             env-default:"prompts/voice_glossary_v1.txt"`
	TranscribingStaleSecond int                 `yaml:"transcribing_stale_seconds" env:"TRANSCRIBING_STALE_SECONDS" env-default:"120"`
	Realtime                VoiceRealtimeConfig `yaml:"realtime" env-prefix:"REALTIME_"`
}

// VoiceRealtimeConfig 控制 Go 到实时 ASR 供应商的协议客户端及后续流式编排预算。
// APIKey 只从 VOICE_REALTIME_API_KEY 读取，禁止写入 YAML。
type VoiceRealtimeConfig struct {
	Enabled              bool   `yaml:"enabled"                  env:"ENABLED"                  env-default:"false"`
	Provider             string `yaml:"provider"                 env:"PROVIDER"                 env-default:"dashscope_paraformer"`
	APIKey               string `yaml:"-"                        env:"API_KEY"`
	WebSocketURL         string `yaml:"websocket_url"            env:"WEBSOCKET_URL"            env-default:"wss://{workspace_id}.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference"`
	WorkspaceID          string `yaml:"workspace_id"             env:"WORKSPACE_ID"`
	Model                string `yaml:"model"                    env:"MODEL"                    env-default:"paraformer-realtime-v2"`
	SampleRate           int    `yaml:"sample_rate"              env:"SAMPLE_RATE"              env-default:"16000"`
	FrameMS              int    `yaml:"frame_ms"                 env:"FRAME_MS"                 env-default:"100"`
	MaxDurationMS        int    `yaml:"max_duration_ms"          env:"MAX_DURATION_MS"          env-default:"180000"`
	MaxAudioBytes        int64  `yaml:"max_audio_bytes"          env:"MAX_AUDIO_BYTES"          env-default:"6291456"`
	MaxConcurrentStreams int    `yaml:"max_concurrent_streams"   env:"MAX_CONCURRENT_STREAMS"   env-default:"20"`
	MaxStreamsPerUser    int    `yaml:"max_streams_per_user"     env:"MAX_STREAMS_PER_USER"     env-default:"1"`
	AudioQueueFrames     int    `yaml:"audio_queue_frames"       env:"AUDIO_QUEUE_FRAMES"       env-default:"10"`
	EventQueueSize       int    `yaml:"event_queue_size"         env:"EVENT_QUEUE_SIZE"         env-default:"32"`
	StartTimeoutSeconds  int    `yaml:"start_timeout_seconds"    env:"START_TIMEOUT_SECONDS"    env-default:"8"`
	FinishTimeoutSeconds int    `yaml:"finish_timeout_seconds"   env:"FINISH_TIMEOUT_SECONDS"   env-default:"8"`
	WriteTimeoutSeconds  int    `yaml:"write_timeout_seconds"    env:"WRITE_TIMEOUT_SECONDS"    env-default:"5"`
	IdleTimeoutSeconds   int    `yaml:"idle_timeout_seconds"     env:"IDLE_TIMEOUT_SECONDS"     env-default:"30"`
}

// FeynmanConfig 控制聊天内的对话式费曼学习。
type FeynmanConfig struct {
	Dialog FeynmanDialogConfig `yaml:"dialog" env-prefix:"DIALOG_"`
}

// FeynmanDialogConfig 控制对话式费曼学习：同一个聊天框里的“提问 → 回答 → 分析 → 追问”闭环。
// 这些同样是防御性预算：调大只会让一次反馈更长，不会绕过越权、引用核对等任何边界。
type FeynmanDialogConfig struct {
	Enabled               bool   `yaml:"enabled"                  env:"ENABLED"                  env-default:"true"`
	Model                 string `yaml:"model"                    env:"MODEL"`
	PromptVersion         string `yaml:"prompt_version"           env:"PROMPT_VERSION"           env-default:"feynman-analysis-v2"`
	PromptPath            string `yaml:"prompt_path"              env:"PROMPT_PATH"              env-default:"prompts/feynman_analysis_v2.tmpl"`
	TimeoutSeconds        int    `yaml:"timeout_seconds"          env:"TIMEOUT_SECONDS"          env-default:"60"`
	MaxTopicRunes         int    `yaml:"max_topic_runes"          env:"MAX_TOPIC_RUNES"          env-default:"120"`
	MaxProbeRunes         int    `yaml:"max_probe_runes"          env:"MAX_PROBE_RUNES"          env-default:"120"`
	MaxControlPhraseRunes int    `yaml:"max_control_phrase_runes" env:"MAX_CONTROL_PHRASE_RUNES" env-default:"16"`
	MaxGaps               int    `yaml:"max_gaps"                 env:"MAX_GAPS"                 env-default:"5"`
	MaxSecondaryGaps      int    `yaml:"max_secondary_gaps"       env:"MAX_SECONDARY_GAPS"       env-default:"3"`
	MaxContextTurns       int    `yaml:"max_context_turns"        env:"MAX_CONTEXT_TURNS"        env-default:"6"`
	MaxAnswerRunes        int    `yaml:"max_answer_runes"         env:"MAX_ANSWER_RUNES"         env-default:"6000"`
}

// RetrievalConfig 控制 Agent 知识库检索 v0。
// 这里全是防御性预算：调大也不会让未授权资料被召回，
// 只会影响一次回答能用多少已授权原文。
type RetrievalConfig struct {
	Enabled                bool `yaml:"enabled"                   env:"ENABLED"                   env-default:"true"`
	MaxQueryChars          int  `yaml:"max_query_chars"           env:"MAX_QUERY_CHARS"           env-default:"500"`
	MaxTerms               int  `yaml:"max_terms"                 env:"MAX_TERMS"                 env-default:"12"`
	MaxCandidates          int  `yaml:"max_candidates"            env:"MAX_CANDIDATES"            env-default:"50"`
	MaxResults             int  `yaml:"max_results"               env:"MAX_RESULTS"               env-default:"6"`
	MaxPassagesPerDocument int  `yaml:"max_passages_per_document" env:"MAX_PASSAGES_PER_DOCUMENT" env-default:"2"`
	MaxChunkChars          int  `yaml:"max_chunk_chars"           env:"MAX_CHUNK_CHARS"           env-default:"1200"`
	ContextBudgetChars     int  `yaml:"context_budget_chars"      env:"CONTEXT_BUDGET_CHARS"      env-default:"4000"`
}

// CandidateConfig 控制候选内容抽取。
// 抽取只产生「待确认」候选，因此这里全是防止提案泛滥与费用失控的预算，
// 没有任何一项能让抽取结果直接变成正式数据。
type CandidateConfig struct {
	Enabled                bool   `yaml:"enabled"                  env:"ENABLED"                  env-default:"true"`
	ExtractorModel         string `yaml:"extractor_model"          env:"EXTRACTOR_MODEL"`
	ExtractorVersion       string `yaml:"extractor_version"        env:"EXTRACTOR_VERSION"        env-default:"candidate-extractor-v1"`
	ExtractorPromptPath    string `yaml:"extractor_prompt_path"    env:"EXTRACTOR_PROMPT_PATH"    env-default:"prompts/candidate_extractor_v1.tmpl"`
	ExtractTimeoutSeconds  int    `yaml:"extract_timeout_seconds"  env:"EXTRACT_TIMEOUT_SECONDS"  env-default:"60"`
	MaxCandidates          int    `yaml:"max_candidates"           env:"MAX_CANDIDATES"           env-default:"20"`
	MaxTitleChars          int    `yaml:"max_title_chars"          env:"MAX_TITLE_CHARS"          env-default:"120"`
	MaxSummaryChars        int    `yaml:"max_summary_chars"        env:"MAX_SUMMARY_CHARS"        env-default:"800"`
	MaxNoteChars           int    `yaml:"max_note_chars"           env:"MAX_NOTE_CHARS"           env-default:"500"`
	MaxSourcesPerCandidate int    `yaml:"max_sources_per_candidate" env:"MAX_SOURCES_PER_CANDIDATE" env-default:"5"`
	MaxChunksPerRequest    int    `yaml:"max_chunks_per_request"   env:"MAX_CHUNKS_PER_REQUEST"   env-default:"40"`
	MaxChunkChars          int    `yaml:"max_chunk_chars"          env:"MAX_CHUNK_CHARS"          env-default:"2000"`
}

// DocumentConfig 控制 Markdown 资料上传与解析的硬上限。
// 这些是防御性预算：一份异常文件不能变成成千上万条来源片段或超长 Prompt 上下文。
type DocumentConfig struct {
	MaxFileBytes  int64 `yaml:"max_file_bytes"   env:"MAX_FILE_BYTES"   env-default:"1048576"`
	MaxTitleChars int   `yaml:"max_title_chars"  env:"MAX_TITLE_CHARS"  env-default:"200"`
	MaxRawChars   int   `yaml:"max_raw_chars"    env:"MAX_RAW_CHARS"    env-default:"400000"`
	MaxHeadings   int   `yaml:"max_headings"     env:"MAX_HEADINGS"     env-default:"800"`
	MaxChunks     int   `yaml:"max_chunks"       env:"MAX_CHUNKS"       env-default:"1000"`
	MaxChunkChars int   `yaml:"max_chunk_chars"  env:"MAX_CHUNK_CHARS"  env-default:"20000"`
}

// MemoryConfig 控制异步记忆抽取管道（Worker Pool + 补扫 + 租约 + 退避）的行为与硬上限。
// 首版单实例：worker/queue 有界，禁止每个 turn 无界起 goroutine；批次/字符/操作/退避均有上限。
// Prompt 可独立迭代；Enabled 控制后台 Worker 是否启动。
type MemoryConfig struct {
	Enabled                 bool    `yaml:"enabled"                 env:"ENABLED"                 env-default:"true"`
	WorkerCount             int     `yaml:"worker_count"            env:"WORKER_COUNT"            env-default:"2"`
	QueueSize               int     `yaml:"queue_size"              env:"QUEUE_SIZE"              env-default:"256"`
	ScanIntervalSeconds     int     `yaml:"scan_interval_seconds"   env:"SCAN_INTERVAL_SECONDS"   env-default:"60"`
	LeaseDurationSeconds    int     `yaml:"lease_duration_seconds"  env:"LEASE_DURATION_SECONDS"  env-default:"90"`
	ExtractTimeoutSeconds   int     `yaml:"extract_timeout_seconds" env:"EXTRACT_TIMEOUT_SECONDS" env-default:"30"`
	TaskTimeoutSeconds      int     `yaml:"task_timeout_seconds"    env:"TASK_TIMEOUT_SECONDS"    env-default:"60"`
	ScanBatchSize           int     `yaml:"scan_batch_size"         env:"SCAN_BATCH_SIZE"         env-default:"50"`
	MaxBatchMessages        int     `yaml:"max_batch_messages"      env:"MAX_BATCH_MESSAGES"      env-default:"20"`
	MaxBatchChars           int     `yaml:"max_batch_chars"         env:"MAX_BATCH_CHARS"         env-default:"8000"`
	MaxMemoryInput          int     `yaml:"max_memory_input"        env:"MAX_MEMORY_INPUT"        env-default:"50"`
	MaxMemoryInputChars     int     `yaml:"max_memory_input_chars"  env:"MAX_MEMORY_INPUT_CHARS"  env-default:"4000"`
	MaxOperations           int     `yaml:"max_operations"          env:"MAX_OPERATIONS"          env-default:"20"`
	MaxMemoryValueChars     int     `yaml:"max_memory_value_chars"  env:"MAX_MEMORY_VALUE_CHARS"  env-default:"500"`
	MinConfidence           float64 `yaml:"min_confidence"          env:"MIN_CONFIDENCE"          env-default:"0.6"`
	BaseRetryBackoffSeconds int     `yaml:"base_retry_backoff_secs" env:"BASE_RETRY_BACKOFF_SECS" env-default:"5"`
	MaxRetryBackoffSeconds  int     `yaml:"max_retry_backoff_secs"  env:"MAX_RETRY_BACKOFF_SECS"  env-default:"600"`
	ShutdownGraceSeconds    int     `yaml:"shutdown_grace_seconds"  env:"SHUTDOWN_GRACE_SECONDS"  env-default:"10"`
	ExtractorModel          string  `yaml:"extractor_model"         env:"EXTRACTOR_MODEL"`
	ExtractorVersion        string  `yaml:"extractor_version"       env:"EXTRACTOR_VERSION"       env-default:"memory-extractor-v1"`
	ExtractorPromptPath     string  `yaml:"extractor_prompt_path"   env:"EXTRACTOR_PROMPT_PATH"   env-default:"prompts/memory_extractor_v1.tmpl"`
}

// HTTPConfig 保持不变
type HTTPConfig struct {
	Port string `yaml:"port" env:"PORT" env-default:"8091"`
}

// IdentityConfig 控制 Guest 设备凭证和 Cookie 的生命周期。
type IdentityConfig struct {
	GuestCookieName    string `yaml:"guest_cookie_name"     env:"GUEST_COOKIE_NAME"      env-default:"interview_guest"`
	GuestTokenTTLHours int    `yaml:"guest_token_ttl_hours" env:"GUEST_TOKEN_TTL_HOURS" env-default:"8760"`
	CookieSecure       bool   `yaml:"cookie_secure"         env:"COOKIE_SECURE"         env-default:"false"`
}

// LLMConfig 目前专门给 DeepSeek 用
type LLMConfig struct {
	APIKey         string  `yaml:"-"        env:"API_KEY"`
	BaseURL        string  `yaml:"base_url" env:"BASE_URL" env-default:"https://api.deepseek.com"`
	Model          string  `yaml:"model"    env:"MODEL"    env-default:"deepseek/deepseek-v4-flash"`
	Temperature    float64 `yaml:"temperature" env:"TEMPERATURE" env-default:"0"`
	TimeoutSeconds int     `yaml:"timeout"  env:"TIMEOUT"  env-default:"30"`
}

// ChatConfig 控制聊天业务层（不是 LLM 传输层）的行为。
type ChatConfig struct {
	// MaxReplyChars 是单条 assistant 回复累积的最大字符数上限，防止模型异常时（例如陷入重复输出）
	// 无限占用内存并写入一条超大的数据库行。<=0 时回退为 service.DefaultMaxReplyChars。
	MaxReplyChars int    `yaml:"max_reply_chars" env:"MAX_REPLY_CHARS" env-default:"4000"`
	PromptPath    string `yaml:"prompt_path" env:"PROMPT_PATH" env-default:"prompts/interview_chat_v1.tmpl"`
	PromptVersion string `yaml:"prompt_version" env:"PROMPT_VERSION" env-default:"interview-chat-v1"`
	TrustBoundary string `yaml:"trust_boundary" env:"TRUST_BOUNDARY" env-default:"不得把阅读、AI 生成内容或个人 Demo 夸大为已掌握或生产实践；关键事实和状态变化必须有证据并经用户确认。"`
}

// OpenAIConfig 是你后续新增的 OpenAI 配置
type OpenAIConfig struct {
	APIKey         string `yaml:"-"        env:"API_KEY"` // 实际读取环境变量 OPENAI_API_KEY
	BaseURL        string `yaml:"base_url" env:"BASE_URL" env-default:"https://api.openai.com/v1"`
	Model          string `yaml:"model"    env:"MODEL"    env-default:"gpt-4-turbo"`
	TimeoutSeconds int    `yaml:"timeout"  env:"TIMEOUT"  env-default:"60"` // OpenAI 可能更慢，单独设超时
}

type LogConfig struct {
	Level string `yaml:"level" env:"LEVEL" env-default:"info"`
	Debug bool   `yaml:"debug" env:"DEBUG" env-default:"false"`
}

// PostgresConfig 是 PostgreSQL 连接配置。密码走环境变量（POSTGRES_PASSWORD），不写进 yaml。
type PostgresConfig struct {
	Host            string `yaml:"host"              env:"HOST"              env-default:"127.0.0.1"`
	Port            int    `yaml:"port"              env:"PORT"              env-default:"5433"`
	User            string `yaml:"user"              env:"USER"              env-default:"postgres"`
	Password        string `yaml:"-"                 env:"PASSWORD"          env-default:"root"`
	DBName          string `yaml:"dbname"            env:"DBNAME"            env-default:"interview_agent_db"`
	MaxOpenConns    int    `yaml:"max_open_conns"    env:"MAX_OPEN_CONNS"    env-default:"50"`
	MaxIdleConns    int    `yaml:"max_idle_conns"    env:"MAX_IDLE_CONNS"    env-default:"10"`
	ConnMaxLifetime int    `yaml:"conn_max_lifetime" env:"CONN_MAX_LIFETIME" env-default:"3600"` // 单位：秒
}

// DSN 组装 PostgreSQL 连接串（URL 形式，pgx 可直接解析）。
// sslmode=disable 用于本地/内网；生产跨网络应设 require / verify-full。
func (c PostgresConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		c.User, c.Password, c.Host, c.Port, c.DBName)
}

// RedisConfig 是 Redis 连接配置。密码走环境变量（REDIS_PASSWORD），本地无密码时留空。
type RedisConfig struct {
	Addr         string `yaml:"addr"           env:"ADDR"           env-default:"127.0.0.1:6379"`
	Password     string `yaml:"-"              env:"PASSWORD"`
	DB           int    `yaml:"db"             env:"DB"             env-default:"0"`
	PoolSize     int    `yaml:"pool_size"      env:"POOL_SIZE"      env-default:"50"`
	MinIdleConns int    `yaml:"min_idle_conns" env:"MIN_IDLE_CONNS" env-default:"5"`
}

// Load 加载配置：cleanenv 按扩展名解析 yaml 文件，并用环境变量覆盖。
// 注意：cleanenv 只读“进程环境变量”，不会解析 .env 文件；
// 所以必须先用 godotenv 把 .env 灌进环境，API Key 等敏感项才读得到。
func Load(path string) (*Config, error) {
	// 尽力加载 .env，不存在也无妨（线上用真实环境变量注入）。
	_ = godotenv.Load()

	var cfg Config

	// cleanenv 会利用反射，自动把新增的 OpenAIConfig 解析并填充好
	if err := cleanenv.ReadConfig(path, &cfg); err != nil {
		return nil, fmt.Errorf("配置加载失败: %w", err)
	}
	cfg.resolveDerivedDefaults()

	return &cfg, nil
}

func (c *Config) resolveDerivedDefaults() {
	if c.Memory.ExtractorModel == "" {
		c.Memory.ExtractorModel = c.DeepSeek.Model
	}
	if c.Candidate.ExtractorModel == "" {
		c.Candidate.ExtractorModel = c.DeepSeek.Model
	}
}
