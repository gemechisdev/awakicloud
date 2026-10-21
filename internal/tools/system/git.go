package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/tools/types"
)

// GitTools returns structured git helpers (Awaki Android parity).
// The agent can still use bash for advanced git; these reduce errors on common ops.
func GitTools() []types.Tool {
	return []types.Tool{
		gitTool("git_status", "Show git status (porcelain + branch).",
			map[string]any{
				"path": map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
			}, nil, func(ctx context.Context, input map[string]any) types.Result {
				return runGit(ctx, input, "status", "-sb")
			}),
		gitTool("git_diff", "Show git diff. Optionally staged only or a path.",
			map[string]any{
				"path":   map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
				"staged": map[string]any{"type": "boolean", "description": "Show staged diff only (default false)"},
				"file":   map[string]any{"type": "string", "description": "Limit to this file path"},
			}, nil, func(ctx context.Context, input map[string]any) types.Result {
				args := []string{"diff"}
				if v, ok := input["staged"].(bool); ok && v {
					args = append(args, "--cached")
				}
				if f := types.GetStringOpt(input, "file"); f != "" {
					args = append(args, "--", f)
				}
				return runGit(ctx, input, args...)
			}),
		gitTool("git_log", "Show recent commits (oneline).",
			map[string]any{
				"path":  map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
				"n":     map[string]any{"type": "integer", "description": "Number of commits (default 15)"},
				"file":  map[string]any{"type": "string", "description": "Limit to this path"},
			}, nil, func(ctx context.Context, input map[string]any) types.Result {
				n := 15
				if v, ok := input["n"].(float64); ok && v > 0 {
					n = int(v)
				}
				args := []string{"log", fmt.Sprintf("-%d", n), "--oneline", "--decorate"}
				if f := types.GetStringOpt(input, "file"); f != "" {
					args = append(args, "--", f)
				}
				return runGit(ctx, input, args...)
			}),
		gitTool("git_show", "Show a commit or object.",
			map[string]any{
				"path": map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
				"ref":  map[string]any{"type": "string", "description": "Commit SHA / ref (default HEAD)"},
			}, nil, func(ctx context.Context, input map[string]any) types.Result {
				ref := types.GetStringOpt(input, "ref")
				if ref == "" {
					ref = "HEAD"
				}
				return runGit(ctx, input, "show", "--stat", ref)
			}),
		gitTool("git_stage", "Stage files (git add). Pass paths or '.' for all.",
			map[string]any{
				"path":  map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
				"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths to stage"},
			}, []string{"files"}, func(ctx context.Context, input map[string]any) types.Result {
				files, _ := input["files"].([]any)
				if len(files) == 0 {
					return types.Errf("files is required")
				}
				args := []string{"add", "--"}
				for _, f := range files {
					if s, ok := f.(string); ok && s != "" {
						args = append(args, s)
					}
				}
				return runGit(ctx, input, args...)
			}),
		gitTool("git_commit", "Create a commit with the given message (does not push).",
			map[string]any{
				"path":    map[string]any{"type": "string", "description": "Repo root (default: cwd)"},
				"message": map[string]any{"type": "string", "description": "Commit message"},
			}, []string{"message"}, func(ctx context.Context, input map[string]any) types.Result {
				msg, err := types.GetString(input, "message")
				if err != nil {
					return types.ErrResult(err)
				}
				return runGit(ctx, input, "commit", "-m", msg)
			}),
	}
}

func gitTool(name, desc string, params map[string]any, required []string, h types.Handler) types.Tool {
	return types.Tool{
		Name:        name,
		Label:       "Git",
		Description: desc,
		Parameters:  params,
		Required:    required,
		Handler:     h,
	}
}

func runGit(ctx context.Context, input map[string]any, args ...string) types.Result {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir := types.GetStringOpt(input, "path"); dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return types.Errf("git %s: %s", strings.Join(args, " "), s)
	}
	if s == "" {
		s = "(no output)"
	}
	return types.TruncateOutput(s)
}
