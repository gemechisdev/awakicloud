package agent

import (
	"context"
	"sync"
	"github.com/gemechischala/awaki/internal/core/agent/hook"
	"github.com/gemechischala/awaki/internal/transport"
	"github.com/gemechischala/awaki/internal/llm"
	llmTypes "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/tools"
	"github.com/gemechischala/awaki/internal/tools/system"
	toolTypes "github.com/gemechischala/awaki/internal/tools/types"
)

type Agent struct {
	client       llm.LLM
	instructions func() string
	registry     *tools.Registry
	localTools   map[string]toolTypes.Tool
	taskPlan     *system.TaskPlan
	loopDetector *hook.LoopDetector
	beforeHooks  []hook.BeforeIterationFunc
	afterHooks   []hook.AfterIterationFunc
	sink         transport.Sink
	subMu        sync.Mutex // serializes sub-agent client tool swaps
}

func New(client llm.LLM, instructions func() string, registry *tools.Registry) *Agent {
	tp := system.NewTaskPlan()
	localTools := system.AgentTools(tp)

	ltMap := make(map[string]toolTypes.Tool, len(localTools))
	for _, t := range localTools {
		ltMap[t.Name] = t
	}

	a := &Agent{
		client:       client,
		instructions: instructions,
		registry:     registry,
		taskPlan:     tp,
		localTools:   ltMap,
	}
	a.registerDelegateTool()

	a.loopDetector = hook.NewLoopDetector(0)
	a.AddBeforeHook(hook.LoggingBeforeIteration())
	a.AddAfterHook(hook.LoggingAfterIteration(
		client.InputPricePerM(), client.OutputPricePerM(),
	))
	a.AddAfterHook(hook.UsageAfterIteration(
		client.InputPricePerM(), client.OutputPricePerM(),
	))

	return a
}

func (a *Agent) AddBeforeHook(fn hook.BeforeIterationFunc) {
	a.beforeHooks = append(a.beforeHooks, fn)
}

func (a *Agent) AddAfterHook(fn hook.AfterIterationFunc) {
	a.afterHooks = append(a.afterHooks, fn)
}

func (a *Agent) ModelLabel() string {
	return a.client.ModelLabel()
}

// emit sends an event to the run's sink. The sink (the orchestrator) decides
// what to persist and what to forward to the UI.
func (a *Agent) emit(ev transport.Event) {
	a.sink.Emit(ev)
}

func (a *Agent) runBeforeHooks(ctx context.Context, ic *hook.IterationContext) {
	for _, h := range a.beforeHooks {
		h(ctx, ic)
	}
}

func (a *Agent) runAfterHooks(ctx context.Context, ic *hook.IterationContext, result *hook.IterationResult) {
	for _, h := range a.afterHooks {
		h(ctx, ic, result)
	}
}

func (a *Agent) localToolDefs() []llmTypes.ToolDef {
	defs := make([]llmTypes.ToolDef, 0, len(a.localTools))
	for _, t := range a.localTools {
		defs = append(defs, llmTypes.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Required:    t.Required,
		})
	}
	return defs
}

func (a *Agent) lookupTool(name string) (toolTypes.Tool, bool) {
	if t, ok := a.localTools[name]; ok {
		return t, true
	}
	return a.registry.Lookup(name)
}

func (a *Agent) executeLocal(ctx context.Context, name string, input map[string]any) (toolTypes.Result, bool) {
	t, ok := a.localTools[name]
	if !ok {
		return toolTypes.Result{}, false
	}
	return t.Handler(ctx, input), true
}

func (a *Agent) executeTool(ctx context.Context, name string, input map[string]any) toolTypes.Result {
	if result, handled := a.executeLocal(ctx, name, input); handled {
		return result
	}
	return a.registry.Execute(ctx, name, input)
}


// SetClient swaps the LLM backend (used when the user selects another provider/model).
func (a *Agent) SetClient(client llm.LLM) {
	a.client = client
}

// SetRegistry swaps the tool registry and refreshes client tool defs when possible.
func (a *Agent) SetRegistry(reg *tools.Registry) {
	a.registry = reg
	if c, ok := a.client.(*llm.Client); ok {
		defs := reg.Definitions()
		defs = append(defs, a.localToolDefs()...)
		c.SetTools(defs)
	}
}
