package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/scheduler"
)

func (e *Engine) uiSchedulesBoard(ctx context.Context, msg channel.Message) {
	if e.scheduler == nil {
		_ = msg.Reply.SendText(ctx, "Scheduler is not available.")
		return
	}
	list := e.scheduler.List()
	// Sort by next run (soonest first); zero NextRun at the end
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].NextRun, list[j].NextRun
		if a.IsZero() && b.IsZero() {
			return list[i].ID < list[j].ID
		}
		if a.IsZero() {
			return false
		}
		if b.IsZero() {
			return true
		}
		return a.Before(b)
	})

	var b strings.Builder
	b.WriteString("⏰ Scheduled reminders & jobs\n\n")
	if len(list) == 0 {
		b.WriteString("None. Ask the agent to set a reminder, e.g.\n")
		b.WriteString("“Remind me in 2 hours to stretch”\nor use the schedule_add tool.\n")
	} else {
		b.WriteString(fmt.Sprintf("%d scheduled:\n\n", len(list)))
		for _, t := range list {
			b.WriteString(formatScheduleLine(t))
			b.WriteByte('\n')
		}
	}

	rows := [][]channel.Button{
		{{Text: "🔄 Refresh", CallbackData: "ui:schedules"}},
	}
	// Action rows (cap at ~8 to keep keyboard manageable)
	const maxRows = 8
	shown := 0
	for _, t := range list {
		if shown >= maxRows {
			break
		}
		label := scheduleButtonLabel(t)
		rows = append(rows, []channel.Button{
			{Text: label, CallbackData: "ui:sched:" + t.ID},
		})
		shown++
	}
	if len(list) > maxRows {
		rows = append(rows, []channel.Button{
			{Text: fmt.Sprintf("…and %d more", len(list)-maxRows), CallbackData: "ui:schedules"},
		})
	}
	rows = append(rows, []channel.Button{
		{Text: "📋 Tasks", CallbackData: "ui:tasks"},
		{Text: "⬅ Home", CallbackData: "ui:home"},
	})

	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func formatScheduleLine(t scheduler.TaskInfo) string {
	status := "▶️"
	if t.Paused {
		status = "⏸"
	}
	label := t.Label
	if label == "" {
		label = t.ID
	}
	next := "—"
	if !t.NextRun.IsZero() {
		next = t.NextRun.Local().Format("Mon 15:04")
		// relative hint
		d := time.Until(t.NextRun)
		if d > 0 && d < 48*time.Hour {
			next += fmt.Sprintf(" (%s)", formatDurationShort(d))
		}
	}
	kind := t.Kind
	if kind == "" {
		kind = "cron"
	}
	return fmt.Sprintf("%s `%s` **%s**\n   %s · next %s · `%s`",
		status, t.ID, truncateUI(label, 40), kind, next, truncateUI(t.Schedule, 24))
}

func scheduleButtonLabel(t scheduler.TaskInfo) string {
	prefix := "▶️"
	if t.Paused {
		prefix = "⏸"
	}
	name := t.Label
	if name == "" {
		name = t.ID
	}
	return prefix + " " + truncateUI(name, 28)
}

func formatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return "soon"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (e *Engine) uiScheduleDetail(ctx context.Context, msg channel.Message, id string) {
	if e.scheduler == nil {
		return
	}
	list := e.scheduler.List()
	var t *scheduler.TaskInfo
	for i := range list {
		if list[i].ID == id {
			t = &list[i]
			break
		}
	}
	if t == nil {
		_ = msg.Reply.SendText(ctx, "Schedule not found: "+id)
		e.uiSchedulesBoard(ctx, msg)
		return
	}

	var b strings.Builder
	status := "active"
	if t.Paused {
		status = "paused"
	}
	fmt.Fprintf(&b, "⏰ `%s`\n\n", t.ID)
	if t.Label != "" {
		fmt.Fprintf(&b, "**%s**\n", t.Label)
	}
	fmt.Fprintf(&b, "Status: %s\n", status)
	fmt.Fprintf(&b, "Schedule: `%s`\n", t.Schedule)
	kind := t.Kind
	if kind == "" {
		kind = "cron"
	}
	fmt.Fprintf(&b, "Kind: %s\n", kind)
	if !t.NextRun.IsZero() {
		fmt.Fprintf(&b, "Next run: %s\n", t.NextRun.Local().Format(time.RFC1123))
	}
	if t.MaxRuns > 0 {
		fmt.Fprintf(&b, "Runs: %d / %d\n", t.Runs, t.MaxRuns)
	} else if t.Runs > 0 {
		fmt.Fprintf(&b, "Runs so far: %d\n", t.Runs)
	}
	if t.Command != "" {
		cmd := t.Command
		if len([]rune(cmd)) > 500 {
			cmd = string([]rune(cmd)[:500]) + "…"
		}
		fmt.Fprintf(&b, "\nCommand:\n%s\n", cmd)
	}
	if t.Skill != "" {
		fmt.Fprintf(&b, "\nSkill: %s\n", t.Skill)
	}
	if t.State.LastStatus != "" {
		fmt.Fprintf(&b, "\nLast: %s", t.State.LastStatus)
		if !t.State.LastRunAt.IsZero() {
			fmt.Fprintf(&b, " @ %s", t.State.LastRunAt.Local().Format("15:04 Jan 2"))
		}
		if t.State.LastError != "" {
			fmt.Fprintf(&b, "\nError: %s", truncateUI(t.State.LastError, 200))
		}
		b.WriteByte('\n')
	}

	var actionRow []channel.Button
	if t.Paused {
		actionRow = append(actionRow, channel.Button{Text: "▶️ Resume", CallbackData: "ui:schedresume:" + id})
	} else {
		actionRow = append(actionRow, channel.Button{Text: "⏸ Pause", CallbackData: "ui:schedpause:" + id})
	}
	actionRow = append(actionRow, channel.Button{Text: "🗑 Remove", CallbackData: "ui:schedrm:" + id})

	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: b.String(),
		Rows: [][]channel.Button{
			actionRow,
			{{Text: "📋 All schedules", CallbackData: "ui:schedules"}},
			{{Text: "⬅ Home", CallbackData: "ui:home"}},
		},
	})
}

func (e *Engine) uiSchedulePause(ctx context.Context, msg channel.Message, id string, paused bool) {
	if e.scheduler == nil {
		return
	}
	if err := e.scheduler.Pause(id, paused); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	e.uiScheduleDetail(ctx, msg, id)
}

func (e *Engine) uiScheduleRemove(ctx context.Context, msg channel.Message, id string) {
	if e.scheduler == nil {
		return
	}
	if err := e.scheduler.Remove(id); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, "Removed schedule: "+id)
	e.uiSchedulesBoard(ctx, msg)
}
