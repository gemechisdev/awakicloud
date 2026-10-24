package system

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/tools/types"
)

func ScheduleTools(sched *scheduler.Scheduler) []types.Tool {
	return []types.Tool{
		{
			Name:  "schedule_add",
			Label: "Scheduling task",
			Description: "Schedule work for yourself later. Supports one-shot timers, intervals, and cron. " +
				"When it fires, YOU wake up with full tools/memory/skills to execute the command. Persists across restarts.\n\n" +
				"schedule forms:\n" +
				"• One-shot relative: 'in 5m', 'in 2h', 'once:30m', '+10m'\n" +
				"• One-shot absolute: 'at 2026-10-25T15:00:00Z', 'at 15:30' (today/tomorrow local)\n" +
				"• Interval: 'every 5m', 'every 1h', '@every 30m'\n" +
				"• Cron (5-field): '0 8 * * *' (daily 8:00), '*/15 * * * *'\n" +
				"• Descriptors: '@hourly', '@daily', '@weekly'\n" +
				"One-shot tasks auto-delete after they fire. Use max_runs to limit intervals.",
			Parameters: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "Unique id, e.g. 'remind-water', 'morning-news'",
				},
				"schedule": map[string]any{
					"type":        "string",
					"description": "When to run: 'in 5m', 'every 10m', cron, 'at RFC3339', …",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Natural-language instruction for yourself when woken (NOT a shell command).",
				},
				"skill": map[string]any{
					"type":        "string",
					"description": "Optional skill name to follow when this task fires.",
				},
				"label": map[string]any{
					"type":        "string",
					"description": "Short human-readable label",
				},
				"max_runs": map[string]any{
					"type":        "integer",
					"description": "Optional max fires (0=unlimited). One-shot is always 1. e.g. every 5m with max_runs=3.",
				},
			},
			Required: []string{"id", "schedule", "command"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				if sched == nil {
					return types.Result{Output: "scheduler not available", IsErr: true}
				}
				id, err := types.GetString(input, "id")
				if err != nil {
					return types.ErrResult(err)
				}
				schedule, err := types.GetString(input, "schedule")
				if err != nil {
					return types.ErrResult(err)
				}
				command, err := types.GetString(input, "command")
				if err != nil {
					return types.ErrResult(err)
				}
				skill := types.GetStringOpt(input, "skill")
				label := types.GetStringOpt(input, "label")
				maxRuns := 0
				if v, ok := input["max_runs"].(float64); ok && v > 0 {
					maxRuns = int(v)
				}
				if err := sched.AddWithOptions(id, schedule, command, skill, label, maxRuns); err != nil {
					return types.Errf("failed to schedule: %v", err)
				}
				var next string
				for _, info := range sched.List() {
					if info.ID == id {
						mr := "∞"
						if info.MaxRuns > 0 {
							mr = fmt.Sprintf("%d", info.MaxRuns)
						}
						next = fmt.Sprintf(" kind=%s next=%s runs=%d/%s",
							info.Kind, info.NextRun.Format("2006-01-02 15:04:05 MST"), info.Runs, mr)
						break
					}
				}
				return types.Result{Output: fmt.Sprintf("scheduled %q (%s)%s", id, schedule, next)}
			},
		},
		{
			Name:        "schedule_list",
			Label:       "Listing schedules",
			Description: "List all scheduled tasks (one-shot, interval, and cron) with next run times and state.",
			Parameters:  map[string]any{},
			Handler: func(_ context.Context, _ map[string]any) types.Result {
				if sched == nil {
					return types.Result{Output: "scheduler not available", IsErr: true}
				}
				tasks := sched.List()
				if len(tasks) == 0 {
					return types.Result{Output: "(no scheduled tasks)"}
				}
				data, err := json.MarshalIndent(tasks, "", "  ")
				if err != nil {
					return types.Errf("marshal error: %v", err)
				}
				return types.Result{Output: string(data)}
			},
		},
		{
			Name:        "schedule_remove",
			Label:       "Removing schedule",
			Description: "Remove a scheduled task by ID (one-shot, interval, or cron).",
			Parameters: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The task ID to remove",
				},
			},
			Required: []string{"id"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				if sched == nil {
					return types.Result{Output: "scheduler not available", IsErr: true}
				}
				id, err := types.GetString(input, "id")
				if err != nil {
					return types.ErrResult(err)
				}
				if err := sched.Remove(id); err != nil {
					return types.Errf("failed to remove: %v", err)
				}
				return types.Result{Output: fmt.Sprintf("removed scheduled task %q", id)}
			},
		},
		{
			Name:        "schedule_pause",
			Label:       "Pausing schedule",
			Description: "Pause or resume a scheduled task. Persists across restarts.",
			Parameters: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The task ID to pause or resume",
				},
				"paused": map[string]any{
					"type":        "boolean",
					"description": "true to pause, false to resume",
				},
			},
			Required: []string{"id", "paused"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				if sched == nil {
					return types.Result{Output: "scheduler not available", IsErr: true}
				}
				id, err := types.GetString(input, "id")
				if err != nil {
					return types.ErrResult(err)
				}
				paused, ok := input["paused"].(bool)
				if !ok {
					return types.Errf("paused must be a boolean")
				}
				if err := sched.Pause(id, paused); err != nil {
					return types.Errf("failed to pause/resume: %v", err)
				}
				action := "paused"
				if !paused {
					action = "resumed"
				}
				return types.Result{Output: fmt.Sprintf("%s scheduled task %q", action, id)}
			},
		},
	}
}
