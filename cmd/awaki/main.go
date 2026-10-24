package main

import (
	"context"
	"fmt"
	"path/filepath"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram"
	"github.com/gemechischala/awaki/internal/config"
	"github.com/gemechischala/awaki/internal/core"
	"github.com/gemechischala/awaki/internal/core/agent"
	"github.com/gemechischala/awaki/internal/core/agent/hook"
	"github.com/gemechischala/awaki/internal/core/chat"
	"github.com/gemechischala/awaki/internal/core/prompt"
	"github.com/gemechischala/awaki/internal/llm"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/project"
tgstore "github.com/gemechischala/awaki/internal/store/telegram"
	"github.com/gemechischala/awaki/internal/logger"
	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	"github.com/gemechischala/awaki/internal/tools"
	"github.com/gemechischala/awaki/internal/tools/comm"
	"github.com/gemechischala/awaki/internal/tools/identity"
	"github.com/gemechischala/awaki/internal/tools/system"

	"github.com/rs/zerolog/log"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	logger.Init(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch, err := buildChannel(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	eng, err := buildEngine(ctx, cfg, ch)
	if err != nil {
		notifyOwnerAndBlock(ch, err)
		return
	}

	fmt.Printf("Awaki is running [%s]. Configure LLM with /providers if needed. Press Ctrl+C to stop.\n", ch.Name())
	if err := eng.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("engine run error")
	}
	fmt.Println("Shutting down.")
}

func notifyOwnerAndBlock(ch channel.Channel, providerErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reply := ch.OwnerReplier()
	msg := fmt.Sprintf("⚠️ Failed to start:\n\n%v\n\nFix environment variables and restart the container.", providerErr)
	_ = reply.SendText(ctx, msg)

	fmt.Fprintf(os.Stderr, "error: %v\nBlocking to prevent restart loop. Fix env vars and restart manually.\n", providerErr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func buildEngine(ctx context.Context, cfg *config.Config, ch channel.Channel) (*core.Engine, error) {
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}

	provStore, err := providers.NewStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("providers store: %w", err)
	}

	sched := scheduler.New(cfg.DataDir, nil, st.LoadTimezone(), scheduler.Options{
		TaskTimeout:   cfg.Scheduler.TaskTimeout,
		MaxConcurrent: cfg.Scheduler.MaxConcurrent,
	})

	extra := append(comm.ChannelTools(),
		system.ScheduleTools(sched)...,
	)
	extra = append(extra, identity.IdentityTools(st, sched.SetLocation)...)

	// LLM is optional at startup — owner configures via Telegram /provider_add.
	var client llm.LLM = llm.Unconfigured{}
	labelRef := "not configured"
	if llmProv, err := resolveLLMProvider(cfg, provStore); err == nil {
		c := llm.NewClient(*llmProv, nil)
		client = c
		labelRef = llmProv.Label
		log.Info().Str("model", llmProv.Label).Msg("LLM ready")
	} else {
		log.Warn().Err(err).Msg("LLM not configured yet — bot will accept /providers commands")
	}

	chatMgr := chat.NewManager(st, client)

	reg := tools.NewRegistry(st, chatMgr.SearchRelevant, extra...)
	if c, ok := client.(*llm.Client); ok {
		c.SetTools(reg.Definitions())
	}

	transportName := ch.Name()
	instructions := func() string {
		skills, _ := st.LoadSkills()
		label := labelRef
		if m, p, err := provStore.ActivePair(); err == nil {
			label = p.Name + " / " + m.DisplayName
		}
		return prompt.SystemPrompt(prompt.PromptContext{
			TransportName: transportName,
			ModelLabel:    label,
			Soul:          st.ReadSoul(),
			User:          st.ReadUser(),
			SkillsBlock:   store.FormatSkillsForPrompt(skills),
			Timezone:      st.LoadTimezone(),
		})
	}

	ag := agent.New(client, instructions, reg)
	ag.AddBeforeHook(hook.NewCompaction().BeforeHook())

	rebuild := func() error {
		lp, err := resolveLLMProvider(cfg, provStore)
		if err != nil {
			return err
		}
		labelRef = lp.Label
		c := llm.NewClient(*lp, nil)
		c.SetTools(reg.Definitions())
		ag.SetClient(c)
		chatMgr.SetLLM(c)
		log.Info().Str("model", lp.Label).Msg("LLM reloaded")
		return nil
	}

	
	// Project workspaces (local cache + optional Telegram durability)
	var blobs project.BlobStore = project.NewLocalBlobStore(filepath.Join(cfg.DataDir, "project-archives"))
	if ts, ok := st.(*tgstore.Store); ok {
		blobs = &project.MultiBlobStore{
			Primary:   project.NewLocalBlobStore(filepath.Join(cfg.DataDir, "project-archives")),
			Secondary: tgstore.NewProjectBlob(ts),
		}
	}
	projMgr := project.NewManager(cfg.DataDir, blobs)

return core.New(core.Params{
		Channel:      ch,
		Store:        st,
		Agent:        ag,
		Chats:        chatMgr,
		Scheduler:    sched,
		Registry:     reg,
		Providers:    provStore,
		Projects:     projMgr,
		RebuildAgent: rebuild,
	}), nil
}

// resolveLLMProvider uses the Awaki-style providers store when configured.
// Env-based resolve is only a bootstrap fallback when providers.json is empty.
func resolveLLMProvider(cfg *config.Config, ps *providers.Store) (*llm.Provider, error) {
	if m, p, err := ps.ActivePair(); err == nil {
		return providers.ToLLMProvider(p, m, cfg.LLM.Timeout)
	}
	// Bootstrap from env (optional) — still requires explicit base URL for custom
	if cfg.LLM.Provider != "" && cfg.LLM.Model != "" {
		lp, err := resolveProvider(cfg)
		if err != nil {
			return nil, fmt.Errorf("%v\n\nOr configure via Telegram: /providers", err)
		}
		lp.Timeout = cfg.LLM.Timeout
		return lp, nil
	}
	return nil, fmt.Errorf("no LLM configured. Add a provider in Telegram:\n/provider_add name|protocol|base_url|api_key\n/model_add provider|model_id|Display Name\n\nProtocols: openai_chat_completions, anthropic_messages, google_gemini, openai_responses")
}

func resolveProvider(cfg *config.Config) (*llm.Provider, error) {
	if cfg.LLM.Provider == "" {
		return nil, fmt.Errorf("AWAKI_PROVIDER is required — set to anthropic, openai, gemini, openrouter, or custom")
	}
	if cfg.LLM.Model == "" {
		return nil, fmt.Errorf("AWAKI_MODEL is required — e.g. claude-sonnet-4-6, gpt-4o, gemini-2.5-flash, or an OpenRouter slug")
	}
	return llm.ResolveProvider(cfg.LLM.Model, cfg.LLM.Provider)
}

func buildStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	// Preferred: Telegrive-style user account (MTProto) when API credentials present.
	if cfg.Telegram.APIID != 0 && cfg.Telegram.APIHash != "" && cfg.Telegram.Phone != "" {
		session := cfg.Telegram.SessionPath
		if session == "" {
			session = filepath.Join(cfg.DataDir, "telegram.session")
		}
		codeFile := cfg.Telegram.CodeFile
		if codeFile == "" {
			codeFile = filepath.Join(cfg.DataDir, "login_code.txt")
		}
		log.Info().Str("phone", cfg.Telegram.Phone).Msg("starting Telegrive-style user storage")
		return tgstore.StartUserStore(ctx, tgstore.UserConfig{
			APIID:        cfg.Telegram.APIID,
			APIHash:      cfg.Telegram.APIHash,
			Phone:        cfg.Telegram.Phone,
			Password:     cfg.Telegram.Password,
			SessionPath:  session,
			CodeFile:     codeFile,
			CacheDir:     cfg.DataDir,
			ChannelTitle: cfg.Telegram.ChannelTitle,
		})
	}
	// Legacy bot-mode storage channel
	if cfg.Telegram.StorageID != 0 && cfg.Telegram.Token != "" {
		return tgstore.New(tgstore.Config{
			Token:    cfg.Telegram.Token,
			ChatID:   cfg.Telegram.StorageID,
			CacheDir: cfg.DataDir,
		})
	}
	return local.New(cfg.DataDir)
}

func buildChannel(cfg *config.Config) (channel.Channel, error) {
	switch cfg.Transport {
	case "telegram":
		if cfg.Telegram.Token == "" {
			return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required for telegram transport")
		}
		if cfg.Telegram.OwnerID == 0 {
			return nil, fmt.Errorf("TELEGRAM_OWNER_ID is required for telegram transport")
		}
		return telegram.New(cfg.Telegram.Token, cfg.Telegram.OwnerID)
	default:
		return nil, fmt.Errorf("unknown transport: %s", cfg.Transport)
	}
}
