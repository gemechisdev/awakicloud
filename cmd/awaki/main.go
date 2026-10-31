package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/gemechischala/awaki/internal/health"
	"github.com/gemechischala/awaki/internal/llm"
	"github.com/gemechischala/awaki/internal/logger"
	"github.com/gemechischala/awaki/internal/mcp"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	sqlstore "github.com/gemechischala/awaki/internal/store/sqlite"
	tgstore "github.com/gemechischala/awaki/internal/store/telegram"
	"github.com/gemechischala/awaki/internal/tasks"
	"github.com/gemechischala/awaki/internal/tools"
	"github.com/gemechischala/awaki/internal/tools/comm"
	"github.com/gemechischala/awaki/internal/tools/identity"
	"github.com/gemechischala/awaki/internal/tools/system"

	"github.com/rs/zerolog/log"
)

var dbSyncer *sqlstore.Syncer

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

	// Wire DB syncer's onSuccess callback to also trigger project zip sync,
	// so project and DB stay in step on every cloud snapshot.
	if dbSyncer != nil {
		dbSyncer.SetOnSync(func() {
			go func() {
				sCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				eng.SyncActiveProject(sCtx)
			}()
		})
	}

	// HTTP health endpoints for Koyeb / Render / Railway (PORT or AWAKI_HTTP_PORT, default 8000).
	hs := health.New(health.AddrFromEnv(), func() map[string]any {
		return map[string]any{
			"service":   "awaki",
			"transport": ch.Name(),
			"busy":      eng.Busy(),
		}
	})
	if err := hs.Start(ctx); err != nil {
		log.Error().Err(err).Msg("health server failed to bind — platform may kill the instance")
	} else {
		// /health stays 200 for TCP/liveness probes. /ready stays false until
		// Telegram polling is about to start so rolling deploys keep the old
		// instance until this one can take exclusive getUpdates ownership.
		if tg, ok := ch.(interface{ SetOnReady(func()) }); ok {
			tg.SetOnReady(func() {
				hs.SetReady(true)
				log.Info().Msg("health ready; telegram polling starting")
			})
		} else {
			hs.SetReady(true)
		}
		log.Info().Msg("health server up; /ready will flip after telegram poll grace")
	}

	fmt.Printf("Awaki is running [%s]. Health on %s. Configure LLM with /providers if needed. Press Ctrl+C to stop.\n", ch.Name(), health.AddrFromEnv())
	if err := eng.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("engine run error")
	}
	// Flip ready off first so the platform can start routing to a replacement.
	hs.SetReady(false)
	if dbSyncer != nil {
		dbSyncer.Stop()
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

	// Keep HTTP health alive so the platform does not restart-loop on TCP checks.
	bg, bgStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer bgStop()
	hs := health.New(health.AddrFromEnv(), func() map[string]any {
		return map[string]any{"service": "awaki", "error": providerErr.Error()}
	})
	_ = hs.Start(bg)
	hs.SetReady(true)

	<-bg.Done()
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

	// Providers/schedules live in SQLite (and thus in versioned TG db snapshots).
	if ss, ok := st.(*sqlstore.Store); ok {
		cloud := sqlstore.ConfigCloud{S: ss}
		provStore.SetCloud(cloud)
		sched.SetCloud(cloud)
		log.Info().Msg("providers + schedules backed by sqlite cloud")
	} else if ts, ok := st.(*tgstore.Store); ok {
		provStore.SetCloud(tgstore.ProvidersCloud{S: ts})
		sched.SetCloud(tgstore.SchedulesCloud{S: ts})
		log.Info().Msg("providers + schedules cloud sync enabled (telegram storage)")
	}

	// Background task board (todo-style, in-process, max 1 concurrent on free tier).
	taskMgr := tasks.NewManager(1)
	if ss, ok := st.(*sqlstore.Store); ok {
		taskMgr.SetPersister(sqlstore.TasksPersister{S: ss})
		taskMgr.Load()
	}
	taskMgr.SetNotify(func(t tasks.Task) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		msg := tasks.FormatBoard([]tasks.Task{t})
		if t.Status == tasks.StatusCompleted && t.Output != "" {
			msg = t.Status.Emoji() + " Task `" + t.ID + "` done: **" + t.Title + "**\n\n" + t.Output
			if len(msg) > 3500 {
				msg = msg[:3500] + "…"
			}
		}
		_ = ch.OwnerReplier().SendText(ctx, msg)
	})

	extra := append(comm.ChannelTools(),
		system.ScheduleTools(sched)...,
	)
	extra = append(extra, tasks.Tools(taskMgr)...)
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
	taskMgr.SetExecutor(func(ctx context.Context, task tasks.Task) (string, error) {
		return ag.RunBackground(ctx, task.Prompt)
	})

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

	// Project workspaces: zip binaries on Telegram, pointers in SQLite.
	var blobs project.BlobStore = project.NewLocalBlobStore(filepath.Join(cfg.DataDir, "project-archives"))
	if ss, ok := st.(*sqlstore.Store); ok && cfg.Telegram.StorageID != 0 && cfg.Telegram.Token != "" {
		if api, err := tgstore.NewDBSyncAPI(cfg.Telegram.Token); err == nil {
			blobs = &project.MultiBlobStore{
				Primary: project.NewLocalBlobStore(filepath.Join(cfg.DataDir, "project-archives")),
				Secondary: &sqlstore.ProjectBlobStore{
					Store:  ss,
					API:    api,
					ChatID: cfg.Telegram.StorageID,
				},
			}
		}
	} else if ts, ok := st.(*tgstore.Store); ok {
		blobs = &project.MultiBlobStore{
			Primary:   project.NewLocalBlobStore(filepath.Join(cfg.DataDir, "project-archives")),
			Secondary: tgstore.NewProjectBlob(ts),
		}
	}
	projMgr := project.NewManager(cfg.DataDir, blobs)

	mcpMgr := mcp.NewManager(cfg.DataDir)
	if ss, ok := st.(*sqlstore.Store); ok {
		mcpMgr.Store().SetCloud(sqlstore.MCPCloud{S: ss})
		log.Info().Msg("mcp backed by sqlite cloud")
	} else if ts, ok := st.(*tgstore.Store); ok {
		mcpMgr.Store().SetCloud(tgstore.MCPCloud{S: ts})
		log.Info().Msg("mcp cloud sync enabled (telegram storage)")
	}
	reloadMCPTools := func() {
		reg.UnregisterPrefix("mcp__")
		for _, t := range mcp.BridgeTools(mcpMgr) {
			reg.Register(t)
		}
		// Keep management tools registered
		for _, t := range mcp.AgentTools(mcpMgr) {
			reg.Register(t)
		}
		if c, ok := client.(*llm.Client); ok {
			c.SetTools(reg.Definitions())
		}
		log.Info().Int("tools", len(reg.Names())).Msg("mcp: tools hot-reloaded")
	}
	mcpMgr.SetOnToolsChanged(reloadMCPTools)
	for _, t := range mcp.AgentTools(mcpMgr) {
		reg.Register(t)
	}
	mcpMgr.ConnectAll(ctx) // triggers first hot-reload via callback

	// Agent can promote the current chat → durable project (same as /upgrade).
	for _, t := range system.ProjectTools(projMgr, chatMgr.Resolve) {
		reg.Register(t)
	}
	if c, ok := client.(*llm.Client); ok {
		c.SetTools(reg.Definitions())
	}

	return core.New(core.Params{
		Channel:      ch,
		Store:        st,
		Agent:        ag,
		Chats:        chatMgr,
		Scheduler:    sched,
		Registry:     reg,
		Providers:    provStore,
		Projects:     projMgr,
		MCP:          mcpMgr,
		Tasks:        taskMgr,
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

	dbPath := filepath.Join(cfg.DataDir, "awaki.db")

	// Bot storage chat: SQLite is source of truth; Telegram holds versioned DB snapshots.
	// Startup order:
	//   1) usable local awaki.db → open it
	//   2) else restore previous version from Telegram (pinned manifest / db)
	//   3) else create a fresh database (first install)
	if cfg.Telegram.StorageID != 0 && cfg.Telegram.Token != "" {
		api, err := tgstore.NewDBSyncAPI(cfg.Telegram.Token)
		if err != nil {
			return nil, fmt.Errorf("telegram db sync api: %w", err)
		}

		if sqlstore.LocalDBUsable(dbPath) {
			log.Info().Str("path", dbPath).Msg("sqlite: using existing local database")
		} else {
			if sqlstore.LocalDBUsable(dbPath) == false && fileExists(dbPath) {
				// Corrupt / empty local file — move aside before restore.
				_ = os.Rename(dbPath, dbPath+".broken-"+fmt.Sprint(time.Now().Unix()))
				log.Warn().Msg("sqlite: local db unusable; moved aside, will try telegram")
			}
			bctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			ok, berr := sqlstore.BootstrapFromTelegram(bctx, api, cfg.Telegram.StorageID, dbPath)
			cancel()
			if ok {
				log.Info().Msg("sqlite: restored previous version from telegram cloud")
			} else {
				if berr != nil {
					log.Warn().Err(berr).Msg("sqlite: telegram restore failed; will create fresh db if needed")
				} else {
					log.Info().Msg("sqlite: no previous version on telegram; fresh database")
				}
			}
		}

		st, err := sqlstore.Open(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		// Import legacy files only when DB still has no identity/config content.
		st.MigrateFromDir(cfg.DataDir)
		dbSyncer = sqlstore.NewSyncer(st, api, cfg.Telegram.StorageID, 30*time.Second)
		dbSyncer.Start()
		log.Info().Msg("sqlite + telegram versioned db sync enabled (debounce 30s)")
		return st, nil
	}

	// Local-only: still prefer existing sqlite, else create fresh, else fs fallback.
	if sqlstore.LocalDBUsable(dbPath) {
		log.Info().Str("path", dbPath).Msg("sqlite: using existing local database")
	} else {
		log.Info().Msg("sqlite: no local db; creating fresh")
	}
	st, err := sqlstore.Open(cfg.DataDir)
	if err != nil {
		log.Warn().Err(err).Msg("sqlite open failed; falling back to filesystem store")
		return local.New(cfg.DataDir)
	}
	st.MigrateFromDir(cfg.DataDir)
	return st, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
