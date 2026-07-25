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
	Log       LogConfig       `yaml:"log"       env-prefix:"LOG_"`
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
