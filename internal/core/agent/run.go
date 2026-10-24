package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/gemechischala/awaki/internal/core/agent/hook"
	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/llm"
	tooltypes "github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"
	"time"

	"github.com/rs/zerolog/log"
)

// RunInput is everything the agent needs for one run. Messages is the prior
// history (treated as immutable); UserBlocks is the new user message. The agent
// emits every turn it produces — starting with the user turn — as a
// MessageEvent, leaving persistence entirely to the sink.
type RunInput struct {
	Messages   []types.Turn
	UserBlocks []types.ContentBlock
	// PlanMode forces a read-only tool set and a plan-oriented system prompt
	// (Awaki Android parity). No file writes, bash mutations, or git writes.
	PlanMode bool
	// ExtraSystem is appended to the system prompt for this run only (e.g. scheduled tasks).
	ExtraSystem string
}

// Run executes the agent loop synchronously, emitting typed events to the sink.
// It never touches the store: persistence is the sink's responsibility.
//
// Exactly one terminal event is emitted: DoneEvent on success or ErrorEvent on
// a hard failure. On cancellation it emits neither and returns ctx.Err() — the
// orchestrator closing the event channel is the UI's cleanup signal.
func (a *Agent) Run(ctx context.Context, input RunInput, sink transport.Sink) error {
	a.sink = sink

	runStart := time.Now()
	log.Info().Int("history", len(input.Messages)).Bool("plan_mode", input.PlanMode).Msg("agent.Run start")

	prevRegistry := a.registry
	prevInstructions := a.instructions
	var prevTools []llm.ToolDef
	if input.PlanMode {
		a.registry = a.registry.Without(
			"bash", "write_file", "edit_file", "create_file", "delete_file", "copy_file",
			"git_stage", "git_commit",
			"memory_write", "memory_delete",
			"skill_create", "skill_update", "skill_delete",
			"soul_write", "user_write",
		)
		a.instructions = func() string {
			return prevInstructions() + "\n\nPLAN MODE (read-only): Do not modify files, run mutating shell commands, or change git state. Explore and produce a concrete implementation plan only. Write tools are unavailable."
		}
		if c, ok := a.client.(*llm.Client); ok {
			prevTools = append([]llm.ToolDef(nil), c.Tools()...)
			defs := a.registry.Definitions()
			defs = append(defs, a.localToolDefs()...)
			c.SetTools(defs)
		}
		defer func() {
			a.registry = prevRegistry
			a.instructions = prevInstructions
			if c, ok := a.client.(*llm.Client); ok && prevTools != nil {
				c.SetTools(prevTools)
			}
		}()
	}

	// The new user message is persisted like any other turn — the orchestrator
	// writes it when it sees this event.
	userTurn := types.Turn{
		Role:      string(types.RoleUser),
		Content:   input.UserBlocks,
		Timestamp: time.Now(),
	}
	a.emit(transport.MessageEvent{Turn: userTurn})

	msgs := make([]types.Turn, 0, len(input.Messages)+1)
	msgs = append(msgs, input.Messages...)
	msgs = append(msgs, userTurn)

	var total types.Usage
	var finalText string // last assistant text produced; reported in DoneEvent
	var toolCallCounter int

	for iteration := 1; ; iteration++ {
		if err := ctx.Err(); err != nil {
			log.Info().Msg("agent.Run cancelled")
			return err
		}

		sys := a.instructions()
		if input.ExtraSystem != "" {
			sys = sys + "\n\n" + input.ExtraSystem
		}
				tokensBefore := hook.EstimateTokens(msgs)

		iterCtx := &hook.IterationContext{
			Iteration:     iteration,
			Model:         a.client.ModelID(),
			System:        sys,
			Messages:      msgs,
			ContextWindow: a.client.ContextWindow(),
			LLM:           a.client,
		}
		a.emit(transport.LLMStartEvent{})
		a.runBeforeHooks(ctx, iterCtx)

		// The compaction hook may have rewritten the working set in place.
		if iterCtx.Compacted {
			msgs = iterCtx.Messages
			a.emit(transport.CompactionEvent{
				Messages:     append([]types.Turn(nil), msgs...), // copy: the sink takes ownership
				BeforeTokens: tokensBefore,
				AfterTokens:  hook.EstimateTokens(msgs),
			})
		}

		callStart := time.Now()
		resp, err := a.client.Call(ctx, buildLLMMessages(msgs), llm.CallOptions{
			System:     sys,
			ExtraTools: a.localToolDefs(),
		})
		if err != nil {
			// Clean cancellation (user /stop or shutdown): exit quietly, no event.
			if errors.Is(err, context.Canceled) {
				log.Info().Msg("agent.Run cancelled during LLM call")
				return err
			}
			msg := err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				msg = "LLM provider did not respond in time. The model may be overloaded — try again later."
			}
			a.emit(transport.ErrorEvent{Error: msg})
			return err
		}
		llmDuration := time.Since(callStart)

		parsed := parseResponseBlocks(resp.Content)

		assistantTurn := types.Turn{
			Role:      string(types.RoleAssistant),
			Content:   parsed.assistantBlocks,
			Timestamp: time.Now(),
		}
		a.emit(transport.MessageEvent{Turn: assistantTurn})
		msgs = append(msgs, assistantTurn)

		if parsed.textContent != "" {
			finalText = parsed.textContent
			a.emit(transport.LLMStreamEvent{Text: parsed.textContent})
		}

		result := &hook.IterationResult{
			Response:    resp,
			LLMDuration: llmDuration,
		}

		if len(parsed.toolCalls) == 0 {
			a.runAfterHooks(ctx, iterCtx, result)
			if result.Usage != nil {
				total.Add(*result.Usage)
			}
			break
		}

		// --- tool-call loop (single emission point for tool events) ---
		toolResults, summaries := a.executeToolCallLoop(ctx, parsed.toolCalls, &toolCallCounter)
		result.ToolCalls = summaries

		toolTurn := types.Turn{
			Role:      string(types.RoleUser),
			Content:   toolResults,
			Timestamp: time.Now(),
		}
		a.emit(transport.MessageEvent{Turn: toolTurn})
		msgs = append(msgs, toolTurn)

		a.runAfterHooks(ctx, iterCtx, result)
		if result.Usage != nil {
			total.Add(*result.Usage)
		}
	}

	total.Duration = time.Since(runStart)
	log.Info().
		Dur("elapsed", total.Duration).
		Int("total_input_tokens", total.InputTokens).
		Int("total_output_tokens", total.OutputTokens).
		Int("total_tool_calls", total.ToolCalls).
		Str("total_cost", fmt.Sprintf("$%.4f", total.Cost)).
		Msg("agent.Run done")
	a.emit(transport.DoneEvent{Text: finalText, Usage: total})
	return nil
}

const maxDetailLen = 60

func (a *Agent) executeToolCallLoop(ctx context.Context, toolCalls []types.ContentBlock, counter *int) ([]types.ContentBlock, []hook.ToolCallSummary) {
	results := make([]types.ContentBlock, 0, len(toolCalls))
	summaries := make([]hook.ToolCallSummary, 0, len(toolCalls))

	toolCtx := transport.WithSink(ctx, a.sink)

	for _, tc := range toolCalls {
		input := unmarshalToolInput(tc.ToolInput)
		tool, _ := a.lookupTool(tc.ToolName)

		label := tool.Label
		if label == "" {
			label = tc.ToolName
		}
		var detail string
		if tool.DetailFunc != nil {
			detail = tool.DetailFunc(input)
			if len(detail) > maxDetailLen {
				detail = detail[:maxDetailLen] + "..."
			}
		}

		a.emit(transport.ToolStartEvent{
			Name:   tc.ToolName,
			Label:  label,
			Detail: detail,
			Phase:  tool.Phase,
		})
		*counter++

		if err := a.loopDetector.Check(tc.ToolName, tc.ToolInput); err != nil {
			a.emit(transport.LoopWarningEvent{
				Name: tc.ToolName, Reason: err.Error(),
			})
			results = append(results, types.ToolResultBlock(tc.ToolUseID, err.Error(), true))
			summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, IsErr: true})
			continue
		}

		if tool.Interactive {
			resp, err := a.handleAskUser(ctx, input)
			isErr := err != nil
			output := resp
			if isErr {
				output = err.Error()
			}
			a.emit(transport.ToolEndEvent{Name: tc.ToolName, Result: output})
			results = append(results, types.ToolResultBlock(tc.ToolUseID, output, isErr))
			summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, Duration: 0, IsErr: isErr})
			continue
		}

		start := time.Now()
		toolResult := a.executeTool(toolCtx, tc.ToolName, input)
		elapsed := time.Since(start)

		a.emit(transport.ToolEndEvent{
			Name: tc.ToolName, Result: toolResult.Output, DurationMs: elapsed.Milliseconds(),
		})

		results = append(results, types.ToolResultBlock(tc.ToolUseID, toolResult.Output, toolResult.IsErr))
		summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, Duration: elapsed, IsErr: toolResult.IsErr})
	}

	return results, summaries
}

func (a *Agent) handleAskUser(ctx context.Context, input map[string]any) (string, error) {
	question, _ := input["question"].(string)
	kind := transport.AskKind(tooltypes.GetStringOpt(input, "kind"))
	if kind == "" {
		kind = transport.AskFreeform
	}

	var options []transport.AskOption
	if raw, ok := input["options"].([]any); ok {
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				val, _ := m["value"].(string)
				lbl, _ := m["label"].(string)
				if val != "" && lbl != "" {
					options = append(options, transport.AskOption{Value: val, Label: lbl})
				}
			}
		}
	}

	// The sink delivers AskEvent with a guarantee (it must not be dropped, or
	// the agent would block here forever). If the run is cancelled the event is
	// abandoned and the ctx.Done branch below unblocks us.
	replyCh := make(chan string, 1)
	a.emit(transport.AskEvent{
		Prompt:  question,
		Kind:    kind,
		Options: options,
		ReplyCh: replyCh,
	})

	select {
	case resp := <-replyCh:
		return resp, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
