package tasks

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
)

// Tools returns agent tools for the background task board.
func Tools(m *Manager) []types.Tool {
	return []types.Tool{
		{
			Name: "bg_task_add",
			Description: "Add a background task (todo-style board). Runs without blocking the main chat. " +
				"Use for long research, multi-step work, or jobs the user wants queued. " +
				"Set auto_start=true (default) to begin when a worker slot is free.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":      map[string]any{"type": "string", "description": "Short label shown on the board"},
					"prompt":     map[string]any{"type": "string", "description": "Full instruction for the worker (defaults to title)"},
					"auto_start": map[string]any{"type": "boolean", "description": "Start when a slot is free (default true)"},
				},
			},
			Required: []string{"title"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				title, _ := input["title"].(string)
				prompt, _ := input["prompt"].(string)
				auto := true
				if v, ok := input["auto_start"].(bool); ok {
					auto = v
				}
				t, err := m.Add(title, prompt, auto)
				if err != nil {
					return types.Errf("%v", err)
				}
				return types.Result{Output: fmt.Sprintf("Added task `%s` [%s] %s\n%s",
					t.ID, t.Status, t.Title, FormatBoard(m.List()))}
			},
		},
		{
			Name:        "bg_task_list",
			Description: "List background tasks as a todo board (pending / running / completed / failed / cancelled).",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				return types.Result{Output: FormatBoard(m.List())}
			},
		},
		{
			Name:        "bg_task_get",
			Description: "Get one background task including full output/error.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			Required: []string{"id"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				id, _ := input["id"].(string)
				t, err := m.Get(strings.TrimSpace(id))
				if err != nil {
					return types.Errf("%v", err)
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s `%s` %s\nstatus: %s\n", t.Status.Emoji(), t.ID, t.Title, t.Status)
				if t.Prompt != "" && t.Prompt != t.Title {
					fmt.Fprintf(&b, "prompt: %s\n", t.Prompt)
				}
				if t.Error != "" {
					fmt.Fprintf(&b, "error: %s\n", t.Error)
				}
				if t.Output != "" {
					fmt.Fprintf(&b, "\n--- output ---\n%s\n", t.Output)
				}
				return types.Result{Output: strings.TrimSpace(b.String())}
			},
		},
		{
			Name:        "bg_task_start",
			Description: "Queue a pending or failed background task to run when a worker slot is free.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			Required: []string{"id"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				id, _ := input["id"].(string)
				if err := m.Start(strings.TrimSpace(id)); err != nil {
					return types.Errf("%v", err)
				}
				t, _ := m.Get(id)
				return types.Result{Output: fmt.Sprintf("Started/queued `%s` (%s)\n%s", t.ID, t.Status, FormatBoard(m.List()))}
			},
		},
		{
			Name:        "bg_task_stop",
			Description: "Cancel a running or pending background task.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			Required: []string{"id"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				id, _ := input["id"].(string)
				if err := m.Stop(strings.TrimSpace(id)); err != nil {
					return types.Errf("%v", err)
				}
				return types.Result{Output: fmt.Sprintf("Stop requested for `%s`\n%s", id, FormatBoard(m.List()))}
			},
		},
	}
}
