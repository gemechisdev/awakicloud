package system

import (
	"context"
	"github.com/gemechischala/awaki/internal/tasks"
	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"
)

func AgentTools(tp *TaskPlan) []types.Tool {
	return []types.Tool{
		TaskPlanTool(tp),
		reportStatusTool(),
		sendMessageTool(),
		askUserTool(),
		showActionsTool(),
	}
}

func reportStatusTool() types.Tool {
	return types.Tool{
		Name:        "report_status",
		Label:       "Updating status",
		Description: "Update the visible status shown to the user. Use during long tasks to communicate what you are working on.",
		Parameters: map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "Short status text, e.g. 'Analyzing Russian market data...'",
			},
			"percent": map[string]any{
				"type":        "integer",
				"description": "Optional progress percentage 0-100 for measurable operations",
			},
		},
		Required: []string{"text"},
		Handler: func(ctx context.Context, input map[string]any) types.Result {
			text, _ := input["text"].(string)
			var pct *int
			if v, ok := input["percent"].(float64); ok {
				i := int(v)
				pct = &i
			}
			if sink, ok := transport.SinkFromContext(ctx); ok {
				sink.Emit(transport.ProgressEvent{Status: text, Percent: pct})
			}
			if fn, ok := tasks.ProgressFromContext(ctx); ok {
				fn(text, pct)
			}
			return types.Result{Output: "status updated"}
		},
	}
}

func sendMessageTool() types.Tool {
	return types.Tool{
		Name:        "send_message",
		Label:       "Sending message",
		Description: "Send an intermediate message to the user without ending your current task. Use to share findings, progress updates, or important information mid-run.",
		Parameters: map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "Message text to send",
			},
			"level": map[string]any{
				"type":        "string",
				"enum":        []string{"info", "success", "warning"},
				"description": "Optional message level: info (default), success, or warning",
			},
		},
		Required: []string{"text"},
		Handler: func(ctx context.Context, input map[string]any) types.Result {
			text, _ := input["text"].(string)
			level, _ := input["level"].(string)
			if sink, ok := transport.SinkFromContext(ctx); ok {
				sink.Emit(transport.MidMessageEvent{
					Text:  text,
					Level: transport.MessageLevel(level),
				})
			}
			if fn, ok := tasks.ProgressFromContext(ctx); ok {
				fn(text, nil)
			}
			return types.Result{Output: "message sent"}
		},
	}
}

func askUserTool() types.Tool {
	return types.Tool{
		Name:        "ask_user",
		Label:       "Asking user",
		Interactive: true,
		Description: "Ask the user a question and wait for their response. Use for clarification, confirmation, choices, or a native Telegram poll. The agent pauses until the user replies.",
		Parameters: map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question to ask (also used as the poll question when kind=poll)",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"freeform", "confirm", "choice", "poll"},
				"description": "Interaction type: freeform (open text, default), confirm (yes/no), choice (inline buttons), poll (native Telegram poll)",
			},
			"options": map[string]any{
				"type":        "array",
				"description": "For choice/poll: array of {value, label} objects or plain strings (2–10 for poll)",
			},
			"allows_multiple": map[string]any{
				"type":        "boolean",
				"description": "Poll only: allow selecting multiple options (default false)",
			},
			"grace_sec": map[string]any{
				"type":        "integer",
				"description": "Poll only: seconds to wait after the last vote change before accepting the answer when allows_multiple is true (default 3)",
			},
			"is_anonymous": map[string]any{
				"type":        "boolean",
				"description": "Poll only: anonymous votes (default false so the owner answer is visible)",
			},
		},
		Required: []string{"question"},
		Handler: func(_ context.Context, _ map[string]any) types.Result {
			return types.Result{Output: "ok"}
		},
	}
}

func showActionsTool() types.Tool {
	return types.Tool{
		Name:        "show_actions",
		Label:       "Showing actions",
		Interactive: false,
		ParallelSafe: true,
		Description: "Show interactive buttons the user can tap. Each button injects its value as a user message to the agent (or answers a pending ask_user). Use for quick choices mid-run without blocking. Prefer ask_user when you must wait for an answer before continuing.",
		Parameters: map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "Optional text above the buttons",
			},
			"actions": map[string]any{
				"type":        "array",
				"description": "Array of {value, label} — value is what the agent receives when the button is tapped",
			},
		},
		Required: []string{"actions"},
		Handler: func(ctx context.Context, input map[string]any) types.Result {
			prompt, _ := input["prompt"].(string)
			var buttons []transport.AskOption
			if raw, ok := input["actions"].([]any); ok {
				for _, r := range raw {
					if m, ok := r.(map[string]any); ok {
						val, _ := m["value"].(string)
						lbl, _ := m["label"].(string)
						if lbl == "" {
							lbl = val
						}
						if val == "" {
							val = lbl
						}
						if val != "" {
							buttons = append(buttons, transport.AskOption{Value: val, Label: lbl})
						}
					} else if s, ok := r.(string); ok && s != "" {
						buttons = append(buttons, transport.AskOption{Value: s, Label: s})
					}
				}
			}
			if len(buttons) == 0 {
				return types.Errf("actions required")
			}
			if sink, ok := transport.SinkFromContext(ctx); ok {
				sink.Emit(transport.ButtonsEvent{Prompt: prompt, Buttons: buttons})
			}
			return types.Result{Output: "buttons shown"}
		},
	}
}
