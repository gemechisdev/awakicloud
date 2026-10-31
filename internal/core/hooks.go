package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// HookAgent starts (or queues) an agent turn as if the owner messaged the bot.
// mode: "auto" (default) | "queue" | "interrupt"
func (e *Engine) HookAgent(ctx context.Context, text string, plan bool, mode string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty text")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "auto", "queue", "interrupt":
	default:
		return nil, fmt.Errorf("invalid mode %q (use auto|queue|interrupt)", mode)
	}

	msgText := text
	if plan && !strings.HasPrefix(strings.TrimSpace(text), "/plan") {
		msgText = "/plan " + text
	}

	reply := e.ch.OwnerReplier()
	msg := channel.Message{Text: msgText, Reply: reply}

	busy := e.Busy()

	switch mode {
	case "queue":
		n := e.steer.enqueue(msg)
		_ = reply.SendText(ctx, fmt.Sprintf("🪝 Hook queued agent turn (%d in queue):\n%s", n, truncateRunes(text, 200)))
		return map[string]any{"action": "queued", "queue_len": n, "busy": busy}, nil

	case "interrupt":
		if busy {
			e.steer.setInterrupt(msg)
			e.mu.Lock()
			run := e.active
			e.mu.Unlock()
			if run != nil && run.cancel != nil {
				run.cancel()
			}
			_ = reply.SendText(ctx, "🪝 Hook interrupt — stopping current run, then:\n"+truncateRunes(text, 200))
			return map[string]any{"action": "interrupt", "busy": true}, nil
		}
		go e.runAgent(context.WithoutCancel(ctx), msg)
		_ = reply.SendText(ctx, "🪝 Hook agent:\n"+truncateRunes(text, 200))
		return map[string]any{"action": "started", "busy": false}, nil

	default: // auto
		if busy {
			n := e.steer.enqueue(msg)
			_ = reply.SendText(ctx, fmt.Sprintf("🪝 Hook queued (agent busy, %d in queue):\n%s", n, truncateRunes(text, 200)))
			return map[string]any{"action": "queued", "queue_len": n, "busy": true}, nil
		}
		go e.runAgent(context.WithoutCancel(ctx), msg)
		_ = reply.SendText(ctx, "🪝 Hook agent:\n"+truncateRunes(text, 200))
		return map[string]any{"action": "started", "busy": false}, nil
	}
}

// HookTask adds a background task on the board.
func (e *Engine) HookTask(ctx context.Context, title, prompt string, autoStart bool) (map[string]any, error) {
	if e.tasksMgr == nil {
		return nil, fmt.Errorf("task manager not available")
	}
	t, err := e.tasksMgr.Add(title, prompt, autoStart)
	if err != nil {
		return nil, err
	}
	if reply := e.ch.OwnerReplier(); reply != nil {
		_ = reply.SendText(ctx, fmt.Sprintf("🪝 Hook task `%s` [%s] %s", t.ID, t.Status, t.Title))
	}
	return map[string]any{
		"id":         t.ID,
		"title":      t.Title,
		"status":     string(t.Status),
		"auto_start": autoStart,
	}, nil
}

// HookSchedule adds a one-shot / interval / cron job.
func (e *Engine) HookSchedule(ctx context.Context, schedule, command, label string) (map[string]any, error) {
	if e.scheduler == nil {
		return nil, fmt.Errorf("scheduler not available")
	}
	id := label
	if id == "" {
		id = "hook-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	}
	// Sanitize id for scheduler key
	id = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, id)
	if err := e.scheduler.Add(id, schedule, command, "", label); err != nil {
		return nil, err
	}
	if reply := e.ch.OwnerReplier(); reply != nil {
		_ = reply.SendText(ctx, fmt.Sprintf("🪝 Hook scheduled `%s` (%s):\n%s", id, schedule, truncateRunes(command, 200)))
	}
	log.Info().Str("id", id).Str("schedule", schedule).Msg("hook schedule added")
	return map[string]any{
		"id":       id,
		"schedule": schedule,
		"command":  command,
		"label":    label,
	}, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

