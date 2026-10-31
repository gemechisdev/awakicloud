package tasks

import (
	"context"
	"strings"
	"time"
)

type progressCtxKey struct{}

// ProgressFunc is called by tools (report_status, send_message) during a bg run.
type ProgressFunc func(text string, percent *int)

// WithProgress attaches a progress reporter to ctx for background agent runs.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressCtxKey{}, fn)
}

// ProgressFromContext returns the bg progress reporter if present.
func ProgressFromContext(ctx context.Context) (ProgressFunc, bool) {
	fn, ok := ctx.Value(progressCtxKey{}).(ProgressFunc)
	return fn, ok && fn != nil
}

// ReportProgress records a progress line on the task and optionally notifies the owner.
// Throttled: at most one push every minInterval unless percent is set (always push).
func (m *Manager) ReportProgress(id, text string, percent *int) {
	text = strings.TrimSpace(text)
	if text == "" && percent == nil {
		return
	}
	const minInterval = 4 * time.Second

	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	t.UpdatedAt = now
	if text != "" {
		t.LastProgress = text
		// keep a short rolling log (last 8 lines)
		t.ProgressLog = append(t.ProgressLog, progressLine{At: now, Text: text})
		if len(t.ProgressLog) > 8 {
			t.ProgressLog = t.ProgressLog[len(t.ProgressLog)-8:]
		}
	}
	if percent != nil {
		p := *percent
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		t.Percent = &p
	}
	snap := t.Snapshot()
	notify := m.progressNotify
	last := m.lastProgressPush[id]
	force := percent != nil
	shouldPush := force || now.Sub(last) >= minInterval
	if shouldPush {
		m.lastProgressPush[id] = now
	}
	m.saveLocked()
	m.mu.Unlock()

	if shouldPush && notify != nil {
		notify(snap)
	}
}

// NotifyStart announces that a task began running.
func (m *Manager) notifyStart(t Task) {
	m.mu.Lock()
	fn := m.progressNotify
	m.mu.Unlock()
	if fn != nil {
		fn(t)
	}
}
