package core

import (
	"context"
	"fmt"
	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/core/agent"
	"github.com/gemechischala/awaki/internal/core/chat"
	"github.com/gemechischala/awaki/internal/core/prompt"
	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/mcp"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/tasks"
	"github.com/gemechischala/awaki/internal/tools"
	"github.com/gemechischala/awaki/internal/transport"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

type activeRun struct {
	cancel     context.CancelFunc
	replyInbox chan string // unbuffered; used for ask_user responses
}

type Engine struct {
	ch        channel.Channel
	agent     *agent.Agent
	store     store.Store
	chats     *chat.Manager
	scheduler *scheduler.Scheduler
	registry  *tools.Registry
	providers *providers.Store
	projects  *project.Manager
	mcp       *mcp.Manager
	tasksMgr  *tasks.Manager
	ui        uiState
	steer     steerState
	// rebuildAgent recreates the LLM client from the active provider/model pair.
	rebuildAgent func() error

	mu     sync.Mutex
	active *activeRun
}

type Params struct {
	Channel      channel.Channel
	Store        store.Store
	Agent        *agent.Agent
	Chats        *chat.Manager
	Scheduler    *scheduler.Scheduler
	Registry     *tools.Registry
	Providers    *providers.Store
	Projects     *project.Manager
	MCP          *mcp.Manager
	Tasks        *tasks.Manager
	RebuildAgent func() error
}

func New(p Params) *Engine {
	eng := &Engine{
		ch:           p.Channel,
		store:        p.Store,
		agent:        p.Agent,
		chats:        p.Chats,
		scheduler:    p.Scheduler,
		registry:     p.Registry,
		providers:    p.Providers,
		projects:     p.Projects,
		mcp:          p.MCP,
		tasksMgr:     p.Tasks,
		rebuildAgent: p.RebuildAgent,
	}

	ownerReply := p.Channel.OwnerReplier()
	p.Scheduler.SetExecutor(func(ctx context.Context, taskID, command, skill string) (string, error) {
		return eng.RunScheduledTask(ctx, ownerReply, taskID, command, skill)
	})

	return eng
}

func (e *Engine) Run(ctx context.Context) error {
	if err := e.scheduler.Start(); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	defer e.scheduler.Stop()

	// Auto-sync active project zip on a ticker (matches 30s DB snapshot cadence).
	e.startProjectAutoSync(ctx)

	return e.ch.Run(ctx, e.handleMessage)
}

func (e *Engine) Channel() channel.Channel {
	return e.ch
}

// Busy reports whether an agent run is in progress (for health status).
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active != nil
}

func (e *Engine) handleMessage(ctx context.Context, msg channel.Message) {
	// UI callbacks, reply-keyboard labels, and provider wizards
	if msg.Command == nil && e.handleUICallback(ctx, msg) {
		return
	}
	if msg.Command != nil {
		e.handleCommand(ctx, msg)
		return
	}

	e.mu.Lock()
	run := e.active
	e.mu.Unlock()

	if run != nil {
		// Prefer delivering to ask_user if the agent is waiting for input.
		select {
		case run.replyInbox <- msg.Text:
			return
		default:
		}
		// Otherwise offer Stop-now vs Queue (steer).
		e.offerSteerWhenBusy(ctx, msg)
		return
	}

	if msg.Text == "" && len(msg.Attachments) == 0 {
		return
	}

	// Agent Skill package upload (zip with SKILL.md)
	if e.trySkillPackageMessage(ctx, msg) {
		return
	}

	log.Info().
		Int("text_len", len(msg.Text)).
		Int("attachments", len(msg.Attachments)).
		Msg("engine: incoming message")

	go e.runAgent(ctx, msg)
}

func (e *Engine) handleCommand(ctx context.Context, msg channel.Message) {
	cmd := msg.Command
	switch cmd.Name {
	case channel.CmdNewChat:
		cmd.Result.Error = e.chats.Reset(ctx)
	case channel.CmdStop:
		e.stopAgent(cmd)
	case channel.CmdHistory:
		chats, err := e.store.ListChats()
		if err != nil {
			cmd.Result.Error = err
		} else {
			cmd.Result.Payload = chats
		}
	case channel.CmdMemory:
		files, err := e.store.ListMemory()
		if err != nil {
			cmd.Result.Error = err
		} else {
			cmd.Result.Payload = files
		}
	case channel.CmdSoul:
		if soul := e.store.ReadSoul(); soul != "" {
			cmd.Result.Data = map[string]string{"text": "🧬 **Soul:**\n\n" + soul}
		}
	case channel.CmdUser:
		if user := e.store.ReadUser(); user != "" {
			cmd.Result.Data = map[string]string{"text": "👤 **User:**\n\n" + user}
		}
	case channel.CmdStart:
		e.uiHome(ctx, msg)
	case channel.CmdProjects:
		e.cmdListProjects(ctx, msg)
	case channel.CmdProject:
		name := ""
		if cmd.Args != nil {
			name = cmd.Args["raw"]
		}
		e.cmdNewProject(ctx, msg, name)
	case channel.CmdProjectUpgrade:
		name := ""
		if cmd.Args != nil {
			name = cmd.Args["raw"]
		}
		e.cmdUpgradeProject(ctx, msg, name)
	case channel.CmdProjectSync:
		e.cmdSyncProject(ctx, msg)
	case channel.CmdMCP:
		e.cmdMCPList(ctx, msg)
	case channel.CmdMCPAdd:
		e.uiStartAddMCP(ctx, msg)
	case channel.CmdSkills:
		e.cmdSkills(ctx, msg)
	case channel.CmdSkillAdd:
		e.uiStartAddSkill(ctx, msg)
	case channel.CmdPlugin:
		e.cmdPlugin(ctx, msg)
	case channel.CmdHelp:
		cmd.Result.Data = map[string]string{"text": helpCommandText()}
	case channel.CmdTasks:
		e.uiTasksBoard(ctx, msg)
		return
	case channel.CmdTaskAdd:
		raw := ""
		if cmd.Args != nil {
			raw = strings.TrimSpace(cmd.Args["raw"])
		}
		e.cmdAddBackgroundTask(ctx, msg, raw)
		return
	case channel.CmdPlan:
		raw := ""
		if cmd.Args != nil {
			raw = strings.TrimSpace(cmd.Args["raw"])
		}
		if raw == "" {
			cmd.Result.Data = map[string]string{"text": "Usage: /plan <task description>\n\nRuns this turn in read-only plan mode (no file writes / mutating shell).\nExample: /plan How should we structure the payment module?"}
			return
		}
		// Hand off to agent with /plan prefix so runAgent enables PlanMode.
		go e.runAgent(ctx, channel.Message{Text: "/plan " + raw, Reply: msg.Reply})
		return
	case channel.CmdProviders:
		e.uiProvidersList(ctx, msg, 0)
	case channel.CmdModels:
		e.uiAllModels(ctx, msg, 0)
	case channel.CmdProviderAdd, channel.CmdModelAdd:
		e.handleProvidersCommand(ctx, cmd)
	}
}

func (e *Engine) reloadLLM() error {
	if e.rebuildAgent == nil {
		return nil
	}
	return e.rebuildAgent()
}

// setActive marks the engine as busy and returns a release function.
// Caller must defer the release.
func (e *Engine) setActive(run *activeRun) func() {
	e.mu.Lock()
	e.active = run
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.active = nil
		e.mu.Unlock()
	}
}

func (e *Engine) runAgent(ctx context.Context, msg channel.Message) {
	reply := msg.Reply

	// No model selected yet — guide the owner instead of failing mid-run.
	if e.providers != nil {
		if _, _, err := e.providers.ActivePair(); err != nil {
			_ = reply.SendText(ctx, "No LLM configured yet.\n\n"+
				"1) /provider_add name|protocol|base_url|api_key\n"+
				"2) /model_add provider|model_id|Display Name\n"+
				"3) /models to select\n\n"+
				"Example:\n"+
				"/provider_add OpenRouter|openai_chat_completions|https://openrouter.ai/api/v1|sk-or-v1-...\n"+
				"/model_add OpenRouter|anthropic/claude-sonnet-4.6|Claude")
			return
		}
	}

	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	run := &activeRun{
		cancel:     cancel,
		replyInbox: make(chan string),
	}
	releaseActive := e.setActive(run)
	defer func() {
		releaseActive()
		e.drainSteerAfterRun(context.WithoutCancel(ctx))
	}()

	bus, recv := transport.NewBus(64)

	ch, err := e.chats.Resolve()
	if err != nil {
		bus.Close()
		log.Error().Err(err).Msg("engine: failed to resolve chat")
		_ = reply.SendText(ctx, "Error: "+err.Error())
		return
	}
	e.bindWorkspaceForChat(ctx, ch.ID, ch.Mode, ch.ProjectSlug)

	agentCtx = transport.WithReplier(agentCtx, reply)

	blocks, cleanup := transport.ProcessAttachments(ch.ID, msg.Text, msg.Attachments)
	defer cleanup()

	planMode := false
	// /plan or "plan:" prefix enables read-only plan mode for this turn.
	if strings.HasPrefix(strings.TrimSpace(msg.Text), "/plan") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg.Text)), "plan:") {
		planMode = true
		// Strip the trigger from the first text block so the model sees the real task.
		for i := range blocks {
			if blocks[i].Type == "text" {
				txt := strings.TrimSpace(blocks[i].Text)
				txt = strings.TrimSpace(strings.TrimPrefix(txt, "/plan"))
				if strings.HasPrefix(strings.ToLower(txt), "plan:") {
					txt = strings.TrimSpace(txt[5:])
				}
				blocks[i].Text = strings.TrimSpace(txt)
				break
			}
		}
	}

	input := agent.RunInput{
		Messages:   ch.Messages(),
		UserBlocks: blocks,
		PlanMode:   planMode,
	}
	sink := newAgentSink(agentCtx, ch, bus)

	go func() {
		defer bus.Close()
		if err := e.agent.Run(agentCtx, input, sink); err != nil {
			log.Error().Err(err).Msg("engine: agent run failed")
		}
	}()

	finalText := reply.ConsumeEvents(agentCtx, recv, run.replyInbox)
	if finalText != "" {
		_ = reply.SendText(ctx, finalText)
	}

	e.chats.SummarizeIfNeeded(context.WithoutCancel(ctx), ch)
	e.syncActiveProject(context.WithoutCancel(ctx))
}

func (e *Engine) stopAgent(cmd *channel.Command) {
	e.mu.Lock()
	run := e.active
	e.mu.Unlock()

	if run == nil {
		cmd.Result.Data = map[string]string{"text": "Nothing to cancel."}
		return
	}

	run.cancel()
	cmd.Result.Data = map[string]string{"text": "⏹ Stopping..."}
}

// RunScheduledTask executes a scheduled task in the active chat.
// It runs synchronously and returns the agent's text output.
// If the agent is already busy, it returns an error so the scheduler can
// apply backoff and retry later.
func (e *Engine) RunScheduledTask(ctx context.Context, reply transport.Replier, taskID, command, skill string) (string, error) {
	e.mu.Lock()
	busy := e.active != nil
	e.mu.Unlock()
	if busy {
		return "", fmt.Errorf("agent busy, will retry")
	}

	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	agentCtx = transport.WithReplier(agentCtx, reply)

	run := &activeRun{cancel: cancel, replyInbox: make(chan string)}
	defer e.setActive(run)()

	bus, recv := transport.NewBus(64)

	ch, err := e.chats.Resolve()
	if err != nil {
		bus.Close()
		return "", fmt.Errorf("resolve chat: %w", err)
	}

	promptText := prompt.ScheduledTaskPrompt(taskID, command, skill)
	blocks := []types.ContentBlock{types.TextBlock(promptText)}

	input := agent.RunInput{
		Messages:    ch.Messages(),
		UserBlocks:  blocks,
		ExtraSystem: prompt.ScheduledTaskSystem(),
	}
	sink := newAgentSink(agentCtx, ch, bus)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer bus.Close()
		runErr = e.agent.Run(agentCtx, input, sink)
	}()

	// Drain events until the run finishes; the final text is carried explicitly
	// by DoneEvent (not reconstructed from the streaming events).
	var finalText string
	for ev := range recv {
		if d, ok := ev.(transport.DoneEvent); ok {
			finalText = d.Text
		}
	}
	<-done

	if runErr != nil {
		return "", runErr
	}

	if finalText != "" {
		if prompt.LooksLikeReminder(taskID, command) {
			finalText = prompt.FormatReminderMessage(finalText)
		}
		_ = reply.SendText(ctx, finalText)
	} else if prompt.LooksLikeReminder(taskID, command) {
		// Model returned empty — still deliver a card from the stored command.
		finalText = prompt.FormatReminderMessage(command)
		_ = reply.SendText(ctx, finalText)
	}

	return finalText, nil
}

func helpCommandText() string {
	return `Commands:

/start — main menu
/new — new conversation
/stop — cancel current agent run
/help — this list
/plan <task> — read-only plan mode for one turn
/tasks — background task board (todo-style)
/task <work> — queue a background task manually

/history — past conversations
/memory — list memory files
/soul — agent identity
/user — user profile

/providers — manage LLM providers
/models — list / select models
/provider_add name|protocol|base_url|api_key
/model_add provider|model_id|Display Name

/projects — list projects
/project <name> — create project
/upgrade [name] — upgrade chat → project
/sync — sync active project to Telegram storage`
}
