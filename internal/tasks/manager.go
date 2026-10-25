package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Executor runs a background task's work. Injected by the engine so the
// tasks package does not depend on agent internals.
type Executor func(ctx context.Context, task Task) (output string, err error)

// Persister optionally saves the board (e.g. SQLite).
type Persister interface {
	SaveTasks(tasks []Task) error
	LoadTasks() ([]Task, error)
}

// Manager is an in-process background task board (todo-list + worker pool).
type Manager struct {
	mu         sync.Mutex
	tasks      map[string]*Task
	order      []string // newest last
	exec       Executor
	persist    Persister
	maxConc    int
	running    int
	cancels    map[string]context.CancelFunc
	notify     func(task Task) // optional: message owner on completion
}

func NewManager(maxConcurrent int) *Manager {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Manager{
		tasks:   make(map[string]*Task),
		cancels: make(map[string]context.CancelFunc),
		maxConc: maxConcurrent,
	}
}

func (m *Manager) SetExecutor(exec Executor) { m.mu.Lock(); m.exec = exec; m.mu.Unlock() }
func (m *Manager) SetPersister(p Persister)  { m.mu.Lock(); m.persist = p; m.mu.Unlock() }
func (m *Manager) SetNotify(fn func(Task))   { m.mu.Lock(); m.notify = fn; m.mu.Unlock() }

func (m *Manager) Load() {
	m.mu.Lock()
	p := m.persist
	m.mu.Unlock()
	if p == nil {
		return
	}
	list, err := p.LoadTasks()
	if err != nil || len(list) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range list {
		t := list[i]
		// Running tasks from a previous process cannot continue — mark failed.
		if t.Status == StatusRunning {
			t.Status = StatusFailed
			t.Error = "interrupted by process restart"
			t.FinishedAt = time.Now()
			t.UpdatedAt = t.FinishedAt
		}
		cp := t
		m.tasks[t.ID] = &cp
		m.order = append(m.order, t.ID)
	}
	log.Info().Int("count", len(list)).Msg("bg tasks loaded")
}

func (m *Manager) saveLocked() {
	if m.persist == nil {
		return
	}
	out := make([]Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			out = append(out, *t)
		}
	}
	_ = m.persist.SaveTasks(out)
}

// Add creates a pending (or auto-started) task. Returns the task id.
func (m *Manager) Add(title, prompt string, autoStart bool) (Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, fmt.Errorf("title is required")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = title
	}
	now := time.Now()
	t := &Task{
		ID:        uuid.NewString()[:8],
		Title:     title,
		Prompt:    prompt,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	snap := *t
	m.saveLocked()
	m.mu.Unlock()

	if autoStart {
		_ = m.Start(t.ID)
		m.mu.Lock()
		if cur, ok := m.tasks[t.ID]; ok {
			snap = *cur
		}
		m.mu.Unlock()
	} else {
		m.trySchedule()
	}
	return snap, nil
}

// Start marks a pending task runnable and kicks the worker pool.
func (m *Manager) Start(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task %q not found", id)
	}
	if t.Status != StatusPending && t.Status != StatusFailed {
		m.mu.Unlock()
		return fmt.Errorf("task %q is %s (only pending/failed can start)", id, t.Status)
	}
	t.Status = StatusPending
	t.Error = ""
	t.Output = ""
	t.UpdatedAt = time.Now()
	m.saveLocked()
	m.mu.Unlock()
	m.trySchedule()
	return nil
}

func (m *Manager) trySchedule() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.running < m.maxConc {
		var next *Task
		for _, id := range m.order {
			t := m.tasks[id]
			if t != nil && t.Status == StatusPending {
				next = t
				break
			}
		}
		if next == nil {
			return
		}
		m.running++
		next.Status = StatusRunning
		next.StartedAt = time.Now()
		next.UpdatedAt = next.StartedAt
		m.saveLocked()

		ctx, cancel := context.WithCancel(context.Background())
		m.cancels[next.ID] = cancel
		id := next.ID
		prompt := next.Prompt
		title := next.Title
		exec := m.exec
		go m.runOne(ctx, id, title, prompt, exec)
	}
}

func (m *Manager) runOne(ctx context.Context, id, title, prompt string, exec Executor) {
	defer func() {
		m.mu.Lock()
		m.running--
		delete(m.cancels, id)
		m.mu.Unlock()
		m.trySchedule()
	}()

	var output string
	var err error
	if exec == nil {
		err = fmt.Errorf("no executor configured")
	} else {
		output, err = exec(ctx, Task{ID: id, Title: title, Prompt: prompt})
	}

	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	t.FinishedAt = time.Now()
	t.UpdatedAt = t.FinishedAt
	if ctx.Err() != nil {
		t.Status = StatusCancelled
		t.Error = "cancelled"
	} else if err != nil {
		t.Status = StatusFailed
		t.Error = err.Error()
		t.Output = output
	} else {
		t.Status = StatusCompleted
		t.Output = output
		t.Error = ""
	}
	snap := *t
	m.saveLocked()
	notify := m.notify
	m.mu.Unlock()

	log.Info().Str("id", id).Str("status", string(snap.Status)).Msg("bg task finished")
	if notify != nil {
		notify(snap)
	}
}

// Stop cancels a running task or drops a pending one.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task %q not found", id)
	}
	switch t.Status {
	case StatusRunning:
		if c, ok := m.cancels[id]; ok {
			c()
		}
		m.mu.Unlock()
		return nil
	case StatusPending:
		t.Status = StatusCancelled
		t.FinishedAt = time.Now()
		t.UpdatedAt = t.FinishedAt
		t.Error = "cancelled before start"
		m.saveLocked()
		m.mu.Unlock()
		return nil
	default:
		m.mu.Unlock()
		return fmt.Errorf("task %q is already %s", id, t.Status)
	}
}

func (m *Manager) Get(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %q not found", id)
	}
	return *t, nil
}

func (m *Manager) List() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			out = append(out, *t)
		}
	}
	return out
}

// FormatBoard renders a todo-style board for Telegram / tools.
func FormatBoard(tasks []Task) string {
	if len(tasks) == 0 {
		return "📋 Background tasks: (empty)"
	}
	var b strings.Builder
	b.WriteString("📋 Background tasks\n\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "%s `%s` **%s**\n", t.Status.Emoji(), t.ID, t.Title)
		fmt.Fprintf(&b, "   status: %s", t.Status)
		if !t.StartedAt.IsZero() {
			fmt.Fprintf(&b, " · started %s", t.StartedAt.Format("15:04"))
		}
		if t.Error != "" {
			fmt.Fprintf(&b, "\n   error: %s", truncate(t.Error, 120))
		}
		if t.Status == StatusCompleted && t.Output != "" {
			fmt.Fprintf(&b, "\n   result: %s", truncate(t.Output, 160))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// MemoryPersister stores the board as JSON (for tests / fallback).
type MemoryPersister struct {
	mu   sync.Mutex
	data []byte
}

func (p *MemoryPersister) SaveTasks(tasks []Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	p.data = b
	return nil
}

func (p *MemoryPersister) LoadTasks() ([]Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.data) == 0 {
		return nil, nil
	}
	var out []Task
	err := json.Unmarshal(p.data, &out)
	return out, err
}
