package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type TelegramConfig struct {
	Token   string
	OwnerID int64

	// Optional bot-mode storage channel (when not using user-account MTProto).
	StorageID int64

	// User-account storage (Telegrive-style MTProto).
	APIID        int
	APIHash      string
	Phone        string
	Password     string
	SessionPath  string
	CodeFile     string
	ChannelTitle string
}

type LLMConfig struct {
	Model     string // optional bootstrap; prefer Telegram /providers
	Provider  string
	BaseURL   string
	APIKey    string
	MaxTokens int
	Timeout   time.Duration
}

type SchedulerConfig struct {
	TaskTimeout   time.Duration
	MaxConcurrent int
}

type Config struct {
	Transport string
	DataDir   string
	WorkDir   string
	LogLevel  string

	Telegram  TelegramConfig
	LLM       LLMConfig
	Scheduler SchedulerConfig
}

func env(key string) string {
	return os.Getenv(key)
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func Load() (*Config, error) {
	loadDotEnv()

	cfg := &Config{
		Transport: envDefault("AWAKI_TRANSPORT", "telegram"),
		DataDir:   env("AWAKI_DATA_DIR"),
		WorkDir:   env("AWAKI_WORK_DIR"),
		LogLevel:  envDefault("LOG_LEVEL", "info"),

		Telegram: TelegramConfig{
			Token:        env("TELEGRAM_BOT_TOKEN"),
			APIHash:      env("AWAKI_TG_API_HASH"),
			Phone:        env("AWAKI_TG_PHONE"),
			Password:     env("AWAKI_TG_PASSWORD"),
			SessionPath:  env("AWAKI_TG_SESSION"),
			CodeFile:     env("AWAKI_TG_CODE_FILE"),
			ChannelTitle: envDefault("AWAKI_TG_CHANNEL", "Awaki"),
		},
		LLM: LLMConfig{
			Model:     env("AWAKI_MODEL"),
			Provider:  env("AWAKI_PROVIDER"),
			BaseURL:   env("AWAKI_BASE_URL"),
			APIKey:    env("AWAKI_API_KEY"),
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

	if s := env("TELEGRAM_OWNER_ID"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid TELEGRAM_OWNER_ID: %w", err)
		}
		cfg.Telegram.OwnerID = id
	}

	if s := env("AWAKI_STORAGE_CHAT_ID"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid AWAKI_STORAGE_CHAT_ID: %w", err)
		}
		cfg.Telegram.StorageID = id
	}

	if s := env("AWAKI_TG_API_ID"); s != "" {
		if id, err := strconv.Atoi(s); err == nil {
			cfg.Telegram.APIID = id
		}
	}

	if s := env("AWAKI_MAX_TOKENS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			cfg.LLM.MaxTokens = v
		}
	}

	if s := env("AWAKI_LLM_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.LLM.Timeout = d
		}
	}
	if cfg.LLM.Timeout <= 0 {
		cfg.LLM.Timeout = 3 * time.Minute
	}

	if s := env("AWAKI_SCHEDULER_TASK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.Scheduler.TaskTimeout = d
		}
	}

	if s := env("AWAKI_SCHEDULER_MAX_CONCURRENT"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			cfg.Scheduler.MaxConcurrent = v
		}
	}

	return cfg, nil
}
