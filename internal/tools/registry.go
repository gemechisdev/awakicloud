package tools

import (
	"context"
	llmtypes "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/tools/memory"
	"github.com/gemechischala/awaki/internal/tools/recall"
	"github.com/gemechischala/awaki/internal/tools/skills"
	systemtools "github.com/gemechischala/awaki/internal/tools/system"
	"github.com/gemechischala/awaki/internal/tools/types"
	webtools "github.com/gemechischala/awaki/internal/tools/web"

	"github.com/rs/zerolog/log"
)

type Registry struct {
	tt map[string]types.Tool
}

// ChatSearchFunc searches past chats by semantic relevance.
type ChatSearchFunc = store.ChatSearchFunc

func NewRegistry(st store.Store, braveAPIKey string, searchFn ChatSearchFunc, extra ...types.Tool) *Registry {
	all := []types.Tool{
		systemtools.BashTool(),
		systemtools.EditFileTool(),
		systemtools.SystemInfoTool(),
		systemtools.FileSearchTool(),
	}
	all = append(all, systemtools.FileTools()...)
	all = append(all, systemtools.BatchFileTools()...)
	all = append(all, systemtools.GitTools()...)
	all = append(all, webtools.WebSearchTool(braveAPIKey))
	all = append(all, webtools.WebFetchTool())
	all = append(all, webtools.WebRequestTool())
	all = append(all, webtools.WebDownloadTool())
	all = append(all, memory.MemoryTools(st)...)
	all = append(all, recall.RecallTool(searchFn))
	all = append(all, skills.SkillTools(st)...)
	all = append(all, extra...)

	r := &Registry{tt: make(map[string]types.Tool, len(all))}
	for _, t := range all {
		r.tt[t.Name] = t
	}
	return r
}

func (r *Registry) Execute(ctx context.Context, name string, input map[string]any) types.Result {
	t, ok := r.tt[name]
	if !ok {
		log.Warn().Str("name", name).Msg("tool dispatch: unknown tool")
		return types.Errf("unknown tool: %s", name)
	}
	return t.Handler(ctx, input)
}

func (r *Registry) Definitions() []llmtypes.ToolDef {
	out := make([]llmtypes.ToolDef, 0, len(r.tt))
	for _, t := range r.tt {
		out = append(out, llmtypes.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Required:    t.Required,
		})
	}
	return out
}

func (r *Registry) Lookup(name string) (types.Tool, bool) {
	t, ok := r.tt[name]
	return t, ok
}

func (r *Registry) Register(t types.Tool) {
	r.tt[t.Name] = t
}


// RestrictTo returns a new registry containing only the named tools.
// Unknown names are ignored. Used for plan mode / role-scoped agents.
func (r *Registry) RestrictTo(names []string) *Registry {
	allow := make(map[string]struct{}, len(names))
	for _, n := range names {
		allow[n] = struct{}{}
	}
	out := &Registry{tt: make(map[string]types.Tool, len(names))}
	for name, tool := range r.tt {
		if _, ok := allow[name]; ok {
			out.tt[name] = tool
		}
	}
	return out
}

// Without removes the named tools (e.g. write tools in plan mode).
func (r *Registry) Without(names ...string) *Registry {
	deny := make(map[string]struct{}, len(names))
	for _, n := range names {
		deny[n] = struct{}{}
	}
	out := &Registry{tt: make(map[string]types.Tool, len(r.tt))}
	for name, tool := range r.tt {
		if _, ok := deny[name]; !ok {
			out.tt[name] = tool
		}
	}
	return out
}

// Has reports whether a tool is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.tt[name]
	return ok
}
