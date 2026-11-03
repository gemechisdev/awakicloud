// Package permission defines session permission modes for the agent tool surface.
//
// Modes follow the common agent-runtime model (plan / ask / auto):
//   - plan: read-only tool set; model must produce a plan, not mutate state
//   - ask:  all tools visible; mutating tools pause for explicit owner approval
//   - auto: full tool use without per-call approval (default)
//
// Scope is per chat (sticky). A one-shot /plan turn still forces plan for that run.
package permission

import (
	"fmt"
	"strings"
)

// Mode is the active permission posture for a chat session.
type Mode string

const (
	ModeAuto Mode = "auto"
	ModeAsk  Mode = "ask"
	ModePlan Mode = "plan"
)

// Parse accepts auto|ask|plan (and common aliases). Empty → ModeAuto.
func Parse(s string) (Mode, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "auto", "full", "full_auto", "full-auto", "agent":
		return ModeAuto, nil
	case "ask", "approve", "manual", "default":
		return ModeAsk, nil
	case "plan", "readonly", "read-only", "read_only":
		return ModePlan, nil
	default:
		return "", fmt.Errorf("unknown permission mode %q (use auto, ask, or plan)", s)
	}
}

func (m Mode) String() string {
	if m == "" {
		return string(ModeAuto)
	}
	return string(m)
}

func (m Mode) Title() string {
	switch m {
	case ModeAsk:
		return "Ask"
	case ModePlan:
		return "Plan"
	default:
		return "Auto"
	}
}

func (m Mode) Emoji() string {
	switch m {
	case ModeAsk:
		return "🛡️"
	case ModePlan:
		return "📋"
	default:
		return "⚡"
	}
}

func (m Mode) Description() string {
	switch m {
	case ModeAsk:
		return "Mutating tools need your Approve/Deny before they run."
	case ModePlan:
		return "Read-only: explore and plan only — no writes, shell mutations, or git writes."
	default:
		return "Full tool access; agent may mutate files, run shell, and call tools freely."
	}
}

// SystemPromptSection is injected into the agent system prompt for this run.
func (m Mode) SystemPromptSection() string {
	switch m {
	case ModePlan:
		return strings.TrimSpace(`
# Permission mode: PLAN (read-only)
You are in plan mode. Explore and produce a concrete implementation plan.
Do not modify files, run mutating shell commands, change git state, or spawn work that mutates the environment.
Write / mutate tools are unavailable. If the user wants execution, tell them to switch to Auto or Ask (/mode).
`)
	case ModeAsk:
		return strings.TrimSpace(`
# Permission mode: ASK (approval required)
Mutating tools require explicit owner approval before they execute.
Prefer read-only exploration first. When a mutation is necessary, call the tool — the runtime will pause for Approve/Deny.
If a tool is denied, adapt the plan; do not retry the same mutation in a tight loop.
`)
	default:
		return strings.TrimSpace(`
# Permission mode: AUTO
You may use tools as needed, including mutations. Keep changes scoped and intentional.
`)
	}
}

// PlanDenylist is the tool names removed entirely in plan mode.
// Keep aligned with researcher read-only denylist / historical PlanMode strip.
var PlanDenylist = []string{
	"bash", "write_file", "edit_file", "apply_patch", "create_file", "delete_file", "copy_file", "move_file", "make_dir",
	"git_stage", "git_commit", "git_checkout", "git_restore", "git_stash", "git_reset", "git_branch_update", "git_sync", "git_clone",
	"memory_write", "memory_append", "memory_delete",
	"skill_create", "skill_update", "skill_delete", "skill_run",
	"soul_write", "user_write",
	"schedule_add", "schedule_remove", "schedule_pause",
	"bg_task_add", "bg_task_start", "bg_task_stop",
	"web_download", "web_request",
	"promote_project",
	"delegate",
}

// readOnlyAllow is tools that never require approval in ask mode.
var readOnlyAllow = map[string]struct{}{
	"read_file": {}, "read_files": {}, "list_files": {}, "glob_files": {},
	"directory_tree": {}, "file_info": {}, "system_info": {},
	"web_search": {}, "web_fetch": {},
	"grep":        {},
	"memory_list": {}, "memory_read": {}, "memory_search": {},
	"skill_list": {}, "skill_read": {}, "skill_files": {}, "skill_read_file": {},
	"soul_read": {}, "user_read": {},
	"recall": {}, "task_plan": {}, "report_status": {},
	"schedule_list": {},
	"bg_task_list":  {}, "bg_task_get": {},
	"git_status": {}, "git_diff": {}, "git_log": {}, "git_show": {}, "git_blame": {}, "git_branch": {},
	"ask_user": {}, "send_message": {}, // communication, not env mutation
}

// RequiresApproval reports whether toolName needs owner approval in ask mode.
// Unknown tools (including MCP bridges) fail closed → approval required.
func RequiresApproval(toolName string) bool {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return true
	}
	if _, ok := readOnlyAllow[name]; ok {
		return false
	}
	// MCP and anything else: ask.
	return true
}

// Classified reports whether a tool has an explicit permission decision: it is
// either read-only (never needs approval) or removed in plan mode. A newly added
// tool that is neither would silently be available in plan mode, so the tools
// package test fails until its author classifies it here.
func Classified(toolName string) bool {
	if _, ok := readOnlyAllow[toolName]; ok {
		return true
	}
	for _, d := range PlanDenylist {
		if d == toolName {
			return true
		}
	}
	return false
}

// IsPlanDenied reports whether a tool must not run in plan mode. The agent loop
// uses it as a hard backstop on top of removing the tools from the definitions.
func IsPlanDenied(toolName string) bool {
	for _, d := range PlanDenylist {
		if d == toolName {
			return true
		}
	}
	return false
}
