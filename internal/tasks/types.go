package tasks

import "time"

// Status is a todo-style lifecycle for background work.
type Status string

const (
	StatusPending   Status = "pending"    // queued, not started
	StatusRunning   Status = "running"    // worker executing
	StatusCompleted Status = "completed"  // finished successfully
	StatusFailed    Status = "failed"     // finished with error
	StatusCancelled Status = "cancelled"  // stopped by user/agent
)

func (s Status) Emoji() string {
	switch s {
	case StatusPending:
		return "⬜"
	case StatusRunning:
		return "🔄"
	case StatusCompleted:
		return "✅"
	case StatusFailed:
		return "❌"
	case StatusCancelled:
		return "⏹"
	default:
		return "•"
	}
}

// Task is one background work item (todo-list entry + optional agent prompt).
type Task struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Prompt     string    `json:"prompt,omitempty"` // work instruction; defaults to title
	Status     Status    `json:"status"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Snapshot is a concurrency-safe copy for listing.
func (t Task) Snapshot() Task { return t }
