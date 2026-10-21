package agent

import "strings"

// Role describes a sub-agent configuration (ported from Awaki Android AgentRole).
type Role struct {
	ID          string
	Name        string
	Description string // shown to the parent model in the delegate tool schema
	ReadOnly    bool
	// ToolDeny removes these tools in addition to read-only defaults when ReadOnly is true.
	ToolDeny []string
	// ToolAllow if non-empty is an allow-list (RestrictTo). Empty = all tools minus denies.
	ToolAllow []string
	// SystemAppend is added to the system prompt for this role.
	SystemAppend string
}

// Built-in roles available to the delegate tool.
var BuiltinRoles = []Role{
	{
		ID:          "researcher",
		Name:        "Researcher",
		Description: "Read-only exploration: search code, read files, answer with evidence and a plan. Never edits.",
		ReadOnly:    true,
		SystemAppend: strings.TrimSpace(`
You are a researcher sub-agent. You are read-only.
- Inspect the workspace thoroughly using read/search tools only.
- Do not attempt to edit files, run mutating shell commands, or change git state.
- Finish with a structured report the parent agent can act on:
  1. Findings (facts)
  2. Evidence (paths and relevant line context)
  3. Recommended implementation steps (ordered)
  4. Risks / unknowns
`),
	},
	{
		ID:          "implementer",
		Name:        "Implementer",
		Description: "Implements a focused change: edit files, run commands, use git. Stay within the assigned scope.",
		ReadOnly:    false,
		ToolDeny:    []string{"delegate"}, // no nested teams by default
		SystemAppend: strings.TrimSpace(`
You are an implementer sub-agent.
- Do only the assigned task. Do not refactor unrelated code.
- Prefer small, verified steps: edit → run tests/build if appropriate → report.
- Other agents may be working in the same workspace; avoid rewriting files outside your scope.
- Finish with a report:
  1. What you changed (paths)
  2. How you verified
  3. What remains / hand-offs
`),
	},
	{
		ID:          "general",
		Name:        "General",
		Description: "General-purpose helper with the normal tool set (no nested delegate).",
		ReadOnly:    false,
		ToolDeny:    []string{"delegate"},
		SystemAppend: strings.TrimSpace(`
You are a focused sub-agent. Complete the assigned task and return a concise, actionable report.
Do not call delegate — you are the leaf of this work.
`),
	},
}

func roleByID(id string) (Role, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		id = "general"
	}
	// aliases
	switch id {
	case "research", "readonly", "read-only", "plan":
		id = "researcher"
	case "code", "coder", "dev", "developer":
		id = "implementer"
	case "default", "helper":
		id = "general"
	}
	for _, r := range BuiltinRoles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

func roleCatalogDescription() string {
	var b strings.Builder
	b.WriteString("Available roles:\n")
	for _, r := range BuiltinRoles {
		b.WriteString("- ")
		b.WriteString(r.ID)
		b.WriteString(": ")
		b.WriteString(r.Description)
		b.WriteByte('\n')
	}
	return b.String()
}

// readOnlyDenylist mirrors plan-mode tool removals.
var readOnlyDenylist = []string{
	"bash", "write_file", "edit_file", "create_file", "delete_file", "copy_file",
	"git_stage", "git_commit",
	"memory_write", "memory_delete",
	"skill_create", "skill_update", "skill_delete",
	"soul_write", "user_write",
	"delegate",
}
