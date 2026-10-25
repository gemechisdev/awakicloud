package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/tasks"
)

func (e *Engine) uiTasksBoard(ctx context.Context, msg channel.Message) {
	if e.tasksMgr == nil {
		_ = msg.Reply.SendText(ctx, "Background tasks are not enabled.")
		return
	}
	list := e.tasksMgr.List()
	var b strings.Builder
	b.WriteString(tasks.FormatBoard(list))
	b.WriteString("\n\n")
	b.WriteString("➕ Add manually: `/task <what to do>`\n")
	b.WriteString("Example: `/task Research free VPS options and summarize`\n")
	b.WriteString("Tasks run like agent-created ones (same worker, same notify).")

	rows := [][]channel.Button{
		{{Text: "🔄 Refresh", CallbackData: "ui:tasks"}},
	}
	// Action rows for recent tasks (newest last → show last 6)
	start := 0
	if len(list) > 6 {
		start = len(list) - 6
	}
	for _, t := range list[start:] {
		switch t.Status {
		case tasks.StatusPending:
			rows = append(rows, []channel.Button{
				{Text: "▶️ " + truncateUI(t.Title, 28), CallbackData: "ui:taskstart:" + t.ID},
				{Text: "⏹", CallbackData: "ui:taskstop:" + t.ID},
			})
		case tasks.StatusRunning:
			rows = append(rows, []channel.Button{
				{Text: "🔄 " + truncateUI(t.Title, 28), CallbackData: "ui:taskget:" + t.ID},
				{Text: "⏹ Stop", CallbackData: "ui:taskstop:" + t.ID},
			})
		case tasks.StatusCompleted, tasks.StatusFailed, tasks.StatusCancelled:
			rows = append(rows, []channel.Button{
				{Text: t.Status.Emoji() + " " + truncateUI(t.Title, 32), CallbackData: "ui:taskget:" + t.ID},
			})
		}
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Home", CallbackData: "ui:home"}})

	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) cmdAddBackgroundTask(ctx context.Context, msg channel.Message, raw string) {
	if e.tasksMgr == nil {
		_ = msg.Reply.SendText(ctx, "Background tasks are not enabled.")
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		_ = msg.Reply.SendText(ctx, "Usage: /task <what to do>\n\n"+
			"Queues a background task that runs the same way as agent-created ones.\n"+
			"Example: /task Summarize the latest news about Ethiopia tech\n\n"+
			"See board: /tasks")
		return
	}
	title := raw
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:77]) + "…"
	}
	t, err := e.tasksMgr.Add(title, raw, true)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Failed to add task: "+err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf(
		"➕ Background task queued\n\n%s `%s` **%s**\nstatus: %s\n\nI'll notify you when it finishes. Board: /tasks",
		t.Status.Emoji(), t.ID, t.Title, t.Status,
	))
}

func (e *Engine) uiTaskStop(ctx context.Context, msg channel.Message, id string) {
	if e.tasksMgr == nil {
		return
	}
	if err := e.tasksMgr.Stop(id); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	e.uiTasksBoard(ctx, msg)
}

func (e *Engine) uiTaskStart(ctx context.Context, msg channel.Message, id string) {
	if e.tasksMgr == nil {
		return
	}
	if err := e.tasksMgr.Start(id); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	e.uiTasksBoard(ctx, msg)
}

func (e *Engine) uiTaskGet(ctx context.Context, msg channel.Message, id string) {
	if e.tasksMgr == nil {
		return
	}
	t, err := e.tasksMgr.Get(id)
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s `%s` **%s**\nstatus: %s\n", t.Status.Emoji(), t.ID, t.Title, t.Status)
	if t.Prompt != "" && t.Prompt != t.Title {
		fmt.Fprintf(&b, "\nPrompt:\n%s\n", t.Prompt)
	}
	if t.Error != "" {
		fmt.Fprintf(&b, "\nError:\n%s\n", t.Error)
	}
	if t.Output != "" {
		out := t.Output
		if len([]rune(out)) > 3000 {
			out = string([]rune(out)[:3000]) + "…"
		}
		fmt.Fprintf(&b, "\n--- output ---\n%s\n", out)
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: b.String(),
		Rows: [][]channel.Button{
			{{Text: "📋 Board", CallbackData: "ui:tasks"}},
			{{Text: "⬅ Home", CallbackData: "ui:home"}},
		},
	})
}
