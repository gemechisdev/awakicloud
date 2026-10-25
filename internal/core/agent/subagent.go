package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/llm"
	"github.com/gemechischala/awaki/internal/tools"
	toolTypes "github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"

	"github.com/rs/zerolog/log"
)

const (
	maxSubagentDepth     = 2
	defaultSubagentIters = 24 // soft limit via context timeout primarily
	subagentTimeout      = 8 * time.Minute
)

type depthKey struct{}

func withDepth(ctx context.Context, d int) context.Context {
	return context.WithValue(ctx, depthKey{}, d)
}

func depthFrom(ctx context.Context) int {
	if v, ok := ctx.Value(depthKey{}).(int); ok {
		return v
	}
	return 0
}

// registerDelegateTool installs the local `delegate` tool on this agent.
func (a *Agent) registerDelegateTool() {
	a.localTools["delegate"] = toolTypes.Tool{
		Name:  "delegate",
		Label: "Delegating",
		Description: "Spawn a specialist sub-agent to work on a focused task and return its final report. " +
			"Use for parallel research, isolated implementation, or long exploration without cluttering this conversation. " +
			roleCatalogDescription() +
			"The sub-agent does not see this chat history — put every fact it needs into the task text.",
		DetailFunc: func(input map[string]any) string {
			role, _ := input["role"].(string)
			task, _ := input["task"].(string)
			if len(task) > 50 {
				task = task[:50] + "…"
			}
			if role == "" {
				role = "general"
			}
			return role + ": " + task
		},
		Parameters: map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "Full task brief for the sub-agent. Include paths, constraints, and definition of done.",
			},
			"role": map[string]any{
				"type":        "string",
				"description": "Role id: researcher | implementer | general (default general)",
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "Optional workspace scope reminder, e.g. 'only under internal/api'",
			},
		},
		Required: []string{"task"},
		Handler:  a.handleDelegate,
	}
}

func (a *Agent) handleDelegate(ctx context.Context, input map[string]any) toolTypes.Result {
	task, err := toolTypes.GetString(input, "task")
	if err != nil {
		return toolTypes.ErrResult(err)
	}
	roleID := toolTypes.GetStringOpt(input, "role")
	scope := toolTypes.GetStringOpt(input, "scope")

	role, ok := roleByID(roleID)
	if !ok {
		return toolTypes.Errf("unknown role %q — use researcher, implementer, or general", roleID)
	}

	d := depthFrom(ctx)
	if d >= maxSubagentDepth {
		return toolTypes.Errf("delegate depth limit (%d) reached — complete the work yourself or narrow the task", maxSubagentDepth)
	}

	report, runErr := a.runSubagent(ctx, role, task, scope, d+1)
	if runErr != nil {
		log.Warn().Err(runErr).Str("role", role.ID).Msg("subagent failed")
		return toolTypes.Errf("sub-agent (%s) failed: %v", role.ID, runErr)
	}
	if strings.TrimSpace(report) == "" {
		report = "(sub-agent finished with empty report)"
	}
	return toolTypes.Result{Output: fmt.Sprintf("[sub-agent:%s]\n%s", role.ID, report)}
}

// runSubagent creates a child Agent with a filtered tool set and runs it to completion.
// The child's sink is a recording sink so the parent only receives the final text
// (parent UI still sees ToolStart/End for the outer delegate call).
func (a *Agent) runSubagent(ctx context.Context, role Role, task, scope string, depth int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, subagentTimeout)
	defer cancel()
	ctx = withDepth(ctx, depth)

	childReg := a.buildRoleRegistry(role)
	parentInstr := a.instructions
	childInstr := func() string {
		base := parentInstr()
		var b strings.Builder
		b.WriteString(base)
		b.WriteString("\n\n")
		b.WriteString(role.SystemAppend)
		if scope != "" {
			b.WriteString("\n\nYour scope in this workspace: ")
			b.WriteString(scope)
			b.WriteString("\nStay inside it unless the task explicitly requires otherwise.")
		}
		b.WriteString("\n\nYou were delegated by another agent. There is no user watching you. ")
		b.WriteString("Your final message is the only thing returned — make it a complete report.")
		return b.String()
	}

	child := New(a.client, childInstr, childReg)
	// New() always registers delegate; apply role policy.
	delete(child.localTools, "delegate")
	denyDelegate := role.ReadOnly
	for _, n := range role.ToolDeny {
		if n == "delegate" {
			denyDelegate = true
			break
		}
	}
	// Allow one nested level only when the role permits and depth allows.
	if !denyDelegate && depth < maxSubagentDepth {
		child.registerDelegateTool()
	}

	// Align LLM tool list with the child registry (serialized — client is shared).
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if c, ok := a.client.(*llm.Client); ok {
		prev := append([]llm.ToolDef(nil), c.Tools()...)
		defs := childReg.Definitions()
		defs = append(defs, child.localToolDefs()...)
		c.SetTools(defs)
		defer c.SetTools(prev)
	}

	rec := &recordingSink{}
	input := RunInput{
		Messages:   nil, // fresh context — task carries the brief
		UserBlocks: []types.ContentBlock{types.TextBlock(task)},
		PlanMode:   role.ReadOnly,
	}

	log.Info().Str("role", role.ID).Int("depth", depth).Int("task_len", len(task)).Msg("subagent start")
	start := time.Now()
	err := child.Run(ctx, input, rec)
	log.Info().Str("role", role.ID).Dur("elapsed", time.Since(start)).Err(err).Msg("subagent done")
	if err != nil && rec.finalText == "" {
		return "", err
	}
	return rec.finalText, nil
}

func (a *Agent) buildRoleRegistry(role Role) *tools.Registry {
	reg := a.registry
	if len(role.ToolAllow) > 0 {
		return reg.RestrictTo(role.ToolAllow)
	}
	deny := append([]string{}, role.ToolDeny...)
	if role.ReadOnly {
		deny = append(deny, readOnlyDenylist...)
	}
	if len(deny) > 0 {
		return reg.Without(deny...)
	}
	return reg
}

// recordingSink captures the DoneEvent text from a sub-agent run.
type recordingSink struct {
	mu        sync.Mutex
	finalText string
	events    int
}

func (s *recordingSink) Emit(ev transport.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	if e, ok := ev.(transport.DoneEvent); ok {
		s.finalText = e.Text
	}
}


// RunBackground runs a general-purpose worker agent to completion and returns
// its final text. Used by the background task board (async, not the main UI run).
func (a *Agent) RunBackground(ctx context.Context, prompt string) (string, error) {
	role, ok := roleByID("general")
	if !ok {
		role = BuiltinRoles[len(BuiltinRoles)-1]
	}
	// Prevent nested background boards inside workers.
	role.ToolDeny = append(append([]string{}, role.ToolDeny...),
		"bg_task_add", "bg_task_list", "bg_task_get", "bg_task_start", "bg_task_stop",
		"schedule_add", "schedule_remove",
	)
	return a.runSubagent(ctx, role, prompt, "", 1)
}
