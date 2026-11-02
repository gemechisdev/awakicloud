package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/core/agent/hook"
	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/llm"
	"github.com/gemechischala/awaki/internal/permission"
	tooltypes "github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"

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
	// Deprecated in favor of PermissionMode=plan; still honored as one-shot override.
	PlanMode bool
	// PermissionMode is the sticky session posture: auto | ask | plan.
	// Empty defaults to auto. PlanMode=true forces plan for this run.
	PermissionMode string
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
	effectiveMode, _ := permission.Parse(input.PermissionMode)
	if input.PlanMode {
		effectiveMode = permission.ModePlan
	}
	a.runPermMode = effectiveMode
	defer func() { a.runPermMode = permission.ModeAuto }()

	log.Info().
		Int("history", len(input.Messages)).
		Str("permission_mode", effectiveMode.String()).
		Bool("plan_mode_flag", input.PlanMode).
		Msg("agent.Run start")

	prevRegistry := a.registry
	prevInstructions := a.instructions
	var prevTools []llm.ToolDef
	if effectiveMode == permission.ModePlan {
		a.registry = a.registry.Without(permission.PlanDenylist...)
		// Also strip local mutating tools from definitions path via lookup still works for deny at exec
	}
	modeSection := effectiveMode.SystemPromptSection()
	a.instructions = func() string {
		base := prevInstructions()
		if modeSection != "" {
			return base + "\n\n" + modeSection
		}
		return base
	}
	if effectiveMode == permission.ModePlan {
		if c, ok := a.client.(*llm.Client); ok {
			prevTools = append([]llm.ToolDef(nil), c.Tools()...)
			defs := a.registry.Definitions()
			defs = append(defs, a.localToolDefs()...)
			c.SetTools(defs)
		}
	}
	defer func() {
		a.registry = prevRegistry
		a.instructions = prevInstructions
		if c, ok := a.client.(*llm.Client); ok && prevTools != nil {
			c.SetTools(prevTools)
		}
	}()

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
	var contID string
	var contMsgCount int // messages already covered by server-side continuation

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
		allMsgs := buildLLMMessages(msgs)
		sendMsgs := allMsgs
		prevID := contID
		if prevID != "" && a.client.SupportsContinuation() && contMsgCount > 0 && contMsgCount < len(allMsgs) {
			sendMsgs = allMsgs[contMsgCount:]
		} else {
			prevID = "" // full context this turn
		}
		resp, err := a.client.Call(ctx, sendMsgs, llm.CallOptions{
			System:     sys,
			ExtraTools: a.localToolDefs(),
			PreviousID: prevID,
		})
		if err != nil && prevID != "" {
			// Server lost the chain — retry once with full history.
			resp, err = a.client.Call(ctx, allMsgs, llm.CallOptions{
				System:     sys,
				ExtraTools: a.localToolDefs(),
				Stateless:  true,
			})
			contID = ""
			contMsgCount = 0
		}
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
		if resp.ContinuationID != "" {
			contID = resp.ContinuationID
		}

		parsed := parseResponseBlocks(resp.Content)

		assistantTurn := types.Turn{
			Role:      string(types.RoleAssistant),
			Content:   parsed.assistantBlocks,
			Timestamp: time.Now(),
		}
		a.emit(transport.MessageEvent{Turn: assistantTurn})
		msgs = append(msgs, assistantTurn)
		contMsgCount = len(buildLLMMessages(msgs))

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
		// Keep contMsgCount at pre-tool length so the next Call sends only tool results.
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

// defaultToolTimeout is the per-tool timeout when none is specified.
const defaultToolTimeout = 60 * time.Second

func (a *Agent) executeToolCallLoop(ctx context.Context, toolCalls []types.ContentBlock, counter *int) ([]types.ContentBlock, []hook.ToolCallSummary) {
	results := make([]types.ContentBlock, 0, len(toolCalls))
	summaries := make([]hook.ToolCallSummary, 0, len(toolCalls))

	toolCtx := transport.WithSink(ctx, a.sink)

	// Partition tool calls: ParallelSafe read-only tools run concurrently,
	// everything else (mutating, interactive, plan-denied, ask-required) runs
	// sequentially in order. Results are reassembled in original call order.
	parallelIdx := make([]int, 0)
	sequentialIdx := make([]int, 0)
	for i, tc := range toolCalls {
		tool, _ := a.lookupTool(tc.ToolName)
		// Only parallelize non-interactive, non-denied tools that are ParallelSafe.
		if tool.ParallelSafe && !tool.Interactive &&
			!(a.runPermMode == permission.ModePlan && permission.IsPlanDenied(tc.ToolName)) &&
			!(a.runPermMode == permission.ModeAsk && permission.RequiresApproval(tc.ToolName)) {
			parallelIdx = append(parallelIdx, i)
		} else {
			sequentialIdx = append(sequentialIdx, i)
		}
	}

	// Run parallel tools concurrently with a per-tool timeout wrapper.
	if len(parallelIdx) > 0 {
		var mu sync.Mutex
		wg := sync.WaitGroup{}
		parResults := make(map[int]types.ContentBlock, len(parallelIdx))
		parSummaries := make(map[int]hook.ToolCallSummary, len(parallelIdx))

		for _, i := range parallelIdx {
			tc := toolCalls[i]
			wg.Add(1)
			go func(idx int, tcall types.ContentBlock) {
				defer wg.Done()
				in := unmarshalToolInput(tcall.ToolInput)
				tl, _ := a.lookupTool(tcall.ToolName)

				label := tl.Label
				if label == "" {
					label = tcall.ToolName
				}
				var detail string
				if tl.DetailFunc != nil {
					detail = tl.DetailFunc(in)
					if len(detail) > maxDetailLen {
						detail = detail[:maxDetailLen] + "..."
					}
				}

				a.emit(transport.ToolStartEvent{
					Name:   tcall.ToolName,
					Label:  label,
					Detail: detail,
					Phase:  tl.Phase,
				})
				*counter++

				tCtx, cancel := context.WithTimeout(toolCtx, defaultToolTimeout)
				defer cancel()
				start := time.Now()
				toolResult := a.executeTool(tCtx, tcall.ToolName, in)
				elapsed := time.Since(start)

				a.emit(transport.ToolEndEvent{
					Name: tcall.ToolName, Result: toolResult.Output, DurationMs: elapsed.Milliseconds(),
				})

				mu.Lock()
				parResults[idx] = types.ToolResultBlock(tcall.ToolUseID, toolResult.Output, toolResult.IsErr)
				parSummaries[idx] = hook.ToolCallSummary{Name: tcall.ToolName, Duration: elapsed, IsErr: toolResult.IsErr}
				mu.Unlock()
			}(i, tc)
		}
		wg.Wait()

		for _, i := range parallelIdx {
			results = append(results, parResults[i])
			summaries = append(summaries, parSummaries[i])
		}
	}

	// Run remaining tools sequentially (preserves order for mutating/interactive tools).
	for _, i := range sequentialIdx {
		tc := toolCalls[i]
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

		// Hard-deny denylisted tools even if still reachable via local map / stale defs.
		if a.runPermMode == permission.ModePlan && permission.IsPlanDenied(tc.ToolName) {
			msg := "tool unavailable in plan mode: " + tc.ToolName
			a.emit(transport.ToolEndEvent{Name: tc.ToolName, Result: msg})
			results = append(results, types.ToolResultBlock(tc.ToolUseID, msg, true))
			summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, IsErr: true})
			continue
		}

		if err := a.loopDetector.Check(tc.ToolName, tc.ToolInput); err != nil {
			a.emit(transport.LoopWarningEvent{
				Name: tc.ToolName, Reason: err.Error(),
			})
			results = append(results, types.ToolResultBlock(tc.ToolUseID, err.Error(), true))
			summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, IsErr: true})
			continue
		}

		// Ask mode: pause mutating tools for owner approval.
		if a.runPermMode == permission.ModeAsk && permission.RequiresApproval(tc.ToolName) && !tool.Interactive {
			approved, reason, err := a.requestToolApproval(ctx, tc.ToolName, input, detail)
			if err != nil {
				out := err.Error()
				a.emit(transport.ToolEndEvent{Name: tc.ToolName, Result: out})
				results = append(results, types.ToolResultBlock(tc.ToolUseID, out, true))
				summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, IsErr: true})
				continue
			}
			if !approved {
				out := "Permission denied by owner"
				if reason != "" {
					out += ": " + reason
				}
				a.emit(transport.ToolEndEvent{Name: tc.ToolName, Result: out})
				results = append(results, types.ToolResultBlock(tc.ToolUseID, out, true))
				summaries = append(summaries, hook.ToolCallSummary{Name: tc.ToolName, IsErr: true})
				continue
			}
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

		tCtx, cancel := context.WithTimeout(toolCtx, defaultToolTimeout)
		defer cancel()
		start := time.Now()
		toolResult := a.executeTool(tCtx, tc.ToolName, input)
		elapsed := time.Since(start)
		cancel()

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

// requestToolApproval blocks until the owner Approves or Denies a mutating tool.
func (a *Agent) requestToolApproval(ctx context.Context, name string, input map[string]any, detail string) (approved bool, reason string, err error) {
	prompt := "Allow tool `" + name + "`?"
	if detail != "" {
		prompt += "\n" + detail
	}
	// Compact input preview for the owner.
	if len(input) > 0 {
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		if len(keys) > 6 {
			keys = keys[:6]
		}
		var parts []string
		for _, k := range keys {
			v := fmt.Sprint(input[k])
			if len(v) > 80 {
				v = v[:80] + "…"
			}
			parts = append(parts, k+"="+v)
		}
		if len(parts) > 0 {
			prompt += "\n" + strings.Join(parts, " · ")
		}
	}

	replyCh := make(chan string, 1)
	a.emit(transport.AskEvent{
		Prompt: prompt,
		Kind:   transport.AskChoice,
		Options: []transport.AskOption{
			{Value: "approve", Label: "✅ Approve"},
			{Value: "deny", Label: "❌ Deny"},
		},
		ReplyCh: replyCh,
	})

	select {
	case resp := <-replyCh:
		resp = strings.TrimSpace(strings.ToLower(resp))
		switch resp {
		case "approve", "yes", "y", "allow", "ok":
			return true, "", nil
		default:
			return false, resp, nil
		}
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
}
