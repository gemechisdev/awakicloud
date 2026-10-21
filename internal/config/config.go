package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type TelegramConfig struct {
	Token     string
	OwnerID   int64
	StorageID int64 // bot-mode storage chat id (legacy fallback)

	// User-account storage (Telegrive-style MTProto). When APIID+APIHash+Phone
	// are set, durable state uses a private channel owned by the user account.
	APIID        int
	APIHash      string
	Phone        string
	Password     string // 2FA cloud password
	SessionPath  string
	CodeFile     string
	ChannelTitle string
}

type LLMConfig struct {
	Model     string
	Provider  string // "anthropic", "openai", "gemini", "openrouter", "custom"
	BaseURL   string // for custom / OpenAI-compatible endpoints
	APIKey    string // optional override; otherwise provider-specific env keys are used
	MaxTokens int
	Timeout   time.Duration // per-LLM-call timeout (0 -> 3m default)
}

type SchedulerConfig struct {
	TaskTimeout   time.Duration
	MaxConcurrent int
}

type Config struct {
	Transport   string // "telegram" (default), "tui", "http", ...
	DataDir     string
	WorkDir     string // active workspace root (local disk; durable state can live in Telegram)
	LogLevel    string
	BraveAPIKey string

	Telegram  TelegramConfig
	LLM       LLMConfig
	Scheduler SchedulerConfig
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envFirst returns the first non-empty value among keys (supports AWAKI_ and legacy GOGOGOT_ prefixes).
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func Load() (*Config, error) {
	loadDotEnv()

	transport := envFirst("AWAKI_TRANSPORT", "GOGOGOT_TRANSPORT")
	if transport == "" {
		transport = "telegram"
	}

	cfg := &Config{
		Transport:   transport,
		BraveAPIKey: os.Getenv("BRAVE_API_KEY"),
		DataDir:     envFirst("AWAKI_DATA_DIR", "GOGOGOT_DATA_DIR"),
		WorkDir:     envFirst("AWAKI_WORK_DIR", "GOGOGOT_WORK_DIR"),
		LogLevel:    envDefault("LOG_LEVEL", "info"),

		Telegram: TelegramConfig{
			Token: os.Getenv("TELEGRAM_BOT_TOKEN"),
		},
		LLM: LLMConfig{
			Model:     envFirst("AWAKI_MODEL", "GOGOGOT_MODEL"),
			Provider:  envFirst("AWAKI_PROVIDER", "GOGOGOT_PROVIDER"),
			BaseURL:   envFirst("AWAKI_BASE_URL", "OPENAI_BASE_URL"),
			APIKey:    envFirst("AWAKI_API_KEY"),
			MaxTokens: 4096,
		},
		Scheduler: SchedulerConfig{
			TaskTimeout:   5 * time.Minute,
			MaxConcurrent: 2,
		},
	}

	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, ".awaki")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.DataDir, "work")
	}

	if s := os.Getenv("TELEGRAM_OWNER_ID"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid TELEGRAM_OWNER_ID: %w", err)
		}
		cfg.Telegram.OwnerID = id
	}

	if s := envFirst("AWAKI_STORAGE_CHAT_ID", "TELEGRAM_STORAGE_CHAT_ID"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid AWAKI_STORAGE_CHAT_ID: %w", err)
		}
		cfg.Telegram.StorageID = id
	}

	// Telegrive-style user account (MTProto)
	if s := envFirst("AWAKI_TG_API_ID", "TELEGRAM_API_ID"); s != "" {
		if id, err := strconv.Atoi(s); err == nil {
			cfg.Telegram.APIID = id
		}
	}
	cfg.Telegram.APIHash = envFirst("AWAKI_TG_API_HASH", "TELEGRAM_API_HASH")
	cfg.Telegram.Phone = envFirst("AWAKI_TG_PHONE", "TELEGRAM_PHONE")
	cfg.Telegram.Password = envFirst("AWAKI_TG_PASSWORD", "TELEGRAM_2FA_PASSWORD")
	cfg.Telegram.SessionPath = envFirst("AWAKI_TG_SESSION", "TELEGRAM_SESSION")
	cfg.Telegram.CodeFile = envFirst("AWAKI_TG_CODE_FILE")
	cfg.Telegram.ChannelTitle = envDefault("AWAKI_TG_CHANNEL", "Awaki")

	if s := envFirst("AWAKI_MAX_TOKENS", "GOGOGOT_MAX_TOKENS"); s != "" {
		v, err := strconv.Atoi(s)
		if err == nil && v > 0 {
			cfg.LLM.MaxTokens = v
		}
	}

	if s := envFirst("AWAKI_LLM_TIMEOUT", "GOGOGOT_LLM_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.LLM.Timeout = d
		}
	}
	if cfg.LLM.Timeout <= 0 {
		cfg.LLM.Timeout = 3 * time.Minute
	}

	if s := envFirst("AWAKI_SCHEDULER_TASK_TIMEOUT", "GOGOGOT_SCHEDULER_TASK_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err == nil && d > 0 {
			cfg.Scheduler.TaskTimeout = d
		}
	}

	if s := envFirst("AWAKI_SCHEDULER_MAX_CONCURRENT", "GOGOGOT_SCHEDULER_MAX_CONCURRENT"); s != "" {
		v, err := strconv.Atoi(s)
		if err == nil && v > 0 {
			cfg.Scheduler.MaxConcurrent = v
		}
	}

	// Sensible defaults when provider/model omitted
	if cfg.LLM.Provider == "" {
		switch {
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			cfg.LLM.Provider = "anthropic"
		case os.Getenv("OPENAI_API_KEY") != "":
			cfg.LLM.Provider = "openai"
		case os.Getenv("GEMINI_API_KEY") != "" || os.Getenv("GOOGLE_API_KEY") != "":
			cfg.LLM.Provider = "gemini"
		case os.Getenv("OPENROUTER_API_KEY") != "":
			cfg.LLM.Provider = "openrouter"
		case cfg.LLM.BaseURL != "" && (cfg.LLM.APIKey != "" || os.Getenv("OPENAI_API_KEY") != ""):
			cfg.LLM.Provider = "custom"
		}
	}
	if cfg.LLM.Model == "" {
		switch cfg.LLM.Provider {
		case "anthropic":
			cfg.LLM.Model = "claude-sonnet-4-6"
		case "openai":
			cfg.LLM.Model = "gpt-4o"
		case "gemini":
			cfg.LLM.Model = "gemini-2.5-flash"
		case "openrouter":
			cfg.LLM.Model = "anthropic/claude-sonnet-4.6"
		case "custom":
			cfg.LLM.Model = "default"
		}
	}

	return cfg, nil
}
