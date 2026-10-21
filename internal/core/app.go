package core

import (
	"context"
	"fmt"
	"strings"
	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/core/agent"
	"github.com/gemechischala/awaki/internal/core/chat"
	"github.com/gemechischala/awaki/internal/core/prompt"
	"github.com/gemechischala/awaki/internal/transport"
	"github.com/gemechischala/awaki/internal/scheduler"
	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/tools"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/providers"
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

	return e.ch.Run(ctx, e.handleMessage)
}

func (e *Engine) Channel() channel.Channel {
	return e.ch
}

func (e *Engine) handleMessage(ctx context.Context, msg channel.Message) {
	if msg.Command == nil && e.handleProviderCallback(ctx, msg) {
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
		select {
		case run.replyInbox <- msg.Text:
			return
		default:
		}
		_ = msg.Reply.SendText(ctx, "Still working on the previous task, please wait...")
		return
	}

	if msg.Text == "" && len(msg.Attachments) == 0 {
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
	case channel.CmdProviders, channel.CmdModels, channel.CmdProviderAdd, channel.CmdModelAdd:
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
	defer e.setActive(run)()

	bus, recv := transport.NewBus(64)

	ch, err := e.chats.Resolve()
	if err != nil {
		bus.Close()
		log.Error().Err(err).Msg("engine: failed to resolve chat")
		_ = reply.SendText(ctx, "Error: "+err.Error())
		return
	}

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
		Messages:   ch.Messages(),
		UserBlocks: blocks,
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
		_ = reply.SendText(ctx, finalText)
	}

	return finalText, nil
}