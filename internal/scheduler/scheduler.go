package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog/log"
)

type TaskExecutor func(ctx context.Context, taskID, command, skill string) (string, error)

var backoffSchedule = []time.Duration{
	30 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

type Options struct {
	TaskTimeout    time.Duration
	MaxConcurrent  int
}

func (o Options) taskTimeout() time.Duration {
	if o.TaskTimeout > 0 {
		return o.TaskTimeout
	}
	return 5 * time.Minute
}

func (o Options) maxConcurrent() int {
	if o.MaxConcurrent > 0 {
		return o.MaxConcurrent
	}
	return 2
}

type TaskState struct {
	LastRunAt         time.Time `json:"last_run_at,omitempty"`
	LastStatus        string    `json:"last_status,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastDurationMs    int64     `json:"last_duration_ms,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors,omitempty"`
}

type TaskData struct {
	ID        string    `json:"id"`
	Schedule  string    `json:"schedule"`
	Kind      string    `json:"kind,omitempty"` // cron | once | interval (empty = cron)
	Command   string    `json:"command"`
	Skill     string    `json:"skill,omitempty"`
	Label     string    `json:"label"`
	Paused    bool      `json:"paused,omitempty"`
	RunAt     time.Time `json:"run_at,omitempty"` // absolute fire time for once
	MaxRuns   int       `json:"max_runs,omitempty"` // 0 = unlimited; once defaults to 1
	Runs      int       `json:"runs,omitempty"`     // successful+attempted fires
	CreatedAt time.Time `json:"created_at"`
	State     TaskState `json:"state"`
}

type Task struct {
	TaskData
	entryID cron.EntryID
	timer   *time.Timer // one-shot
	running atomic.Bool
}

type TaskInfo struct {
	TaskData
	NextRun time.Time `json:"next_run"`
}

type Cloud interface {
	LoadSchedulesJSON() ([]byte, error)
	SaveSchedulesJSON(data []byte) error
}

type Scheduler struct {
	mu       sync.Mutex
	cron     *cron.Cron
	tasks    map[string]*Task
	path     string
	executor TaskExecutor
	sem      chan struct{}
	opts     Options
	cloud    Cloud
}

func newCron(loc *time.Location) *cron.Cron {
	return cron.New(
		cron.WithLocation(loc),
		cron.WithParser(cron.NewParser(
			cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor,
		)),
	)
}

func New(dataDir string, executor TaskExecutor, loc *time.Location, opts Options) *Scheduler {

	if loc == nil {
		loc = time.UTC
	}
	return &Scheduler{
		cron:     newCron(loc),
		tasks:    make(map[string]*Task),
		path:     filepath.Join(dataDir, "schedules.json"),
		executor: executor,
		sem:      make(chan struct{}, opts.maxConcurrent()),
		opts:     opts,
	}
}

func (s *Scheduler) SetExecutor(exec TaskExecutor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executor = exec
}

func (s *Scheduler) SetCloud(cloud Cloud) {
	s.mu.Lock()
	s.cloud = cloud
	s.mu.Unlock()
	if cloud == nil {
		return
	}
	// hydrate when local empty
	s.mu.Lock()
	empty := len(s.tasks) == 0
	s.mu.Unlock()
	if !empty {
		return
	}
	data, err := cloud.LoadSchedulesJSON()
	if err != nil || len(data) == 0 {
		return
	}
	var tasks []*Task
	if json.Unmarshal(data, &tasks) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tasks {
		if t.Kind == "" {
			t.Kind = "cron"
		}
		if t.Kind == "once" {
			if !t.RunAt.IsZero() {
				s.armOnceLocked(t)
				s.tasks[t.ID] = t
			}
			continue
		}
		entryID, err := s.cron.AddFunc(t.Schedule, s.makeRunner(t.ID, t.Command, t.Skill))
		if err != nil {
			continue
		}
		t.entryID = entryID
		s.tasks[t.ID] = t
	}
	_ = s.saveLocalLocked()
	log.Info().Int("count", len(s.tasks)).Msg("scheduler hydrated from cloud")
}

// SetLocation stops the cron, recreates it with a new timezone, and re-adds all tasks.
func (s *Scheduler) SetLocation(loc *time.Location) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx := s.cron.Stop()
	<-ctx.Done()

	s.cron = newCron(loc)
	for _, t := range s.tasks {
		if t.Kind == "once" {
			continue // timer already armed with absolute time
		}
		entryID, err := s.cron.AddFunc(t.Schedule, s.makeRunner(t.ID, t.Command, t.Skill))
		if err != nil {
			log.Error().Err(err).Str("id", t.ID).Msg("failed to re-add task after timezone change")
			continue
		}
		t.entryID = entryID
	}
	s.cron.Start()
	log.Info().Str("location", loc.String()).Int("tasks", len(s.tasks)).Msg("scheduler timezone updated")
}

func (s *Scheduler) Start() error {
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Msg("failed to load schedules")
	}
	s.cron.Start()
	log.Info().Int("tasks", len(s.tasks)).Msg("scheduler started")
	return nil
}

func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// Add registers a task. schedule accepts cron, @every, "in 5m", "at <RFC3339>", etc.
// maxRuns: 0 = unlimited (cron/interval); once always maxRuns=1.
func (s *Scheduler) Add(id, schedule, command, skill, label string) error {
	return s.AddWithOptions(id, schedule, command, skill, label, 0)
}

func (s *Scheduler) AddWithOptions(id, schedule, command, skill, label string, maxRuns int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.tasks[id]; ok {
		s.removeLocked(existing)
	}

	now := time.Now().In(s.cron.Location())
	parsed, err := ParseSchedule(schedule, now)
	if err != nil {
		return err
	}

	task := &Task{
		TaskData: TaskData{
			ID:        id,
			Schedule:  parsed.Display,
			Kind:      parsed.Kind,
			Command:   command,
			Skill:     skill,
			Label:     label,
			CreatedAt: now,
			MaxRuns:   maxRuns,
		},
	}
	if parsed.Kind == "once" {
		task.RunAt = parsed.RunAt
		task.MaxRuns = 1
		s.armOnceLocked(task)
	} else {
		expr := parsed.CronExpr
		if expr == "" {
			expr = schedule
		}
		entryID, err := s.cron.AddFunc(expr, s.makeRunner(id, command, skill))
		if err != nil {
			return fmt.Errorf("invalid schedule %q: %w", schedule, err)
		}
		task.entryID = entryID
		task.Schedule = expr
		if parsed.Display != "" {
			task.Schedule = parsed.Display
		}
		// store cron expr in Schedule for restore — use CronExpr for re-add
		task.Schedule = expr
	}

	s.tasks[id] = task
	return s.save()
}

func (s *Scheduler) removeLocked(task *Task) {
	if task.entryID != 0 {
		s.cron.Remove(task.entryID)
	}
	if task.timer != nil {
		task.timer.Stop()
		task.timer = nil
	}
	delete(s.tasks, task.ID)
}

func (s *Scheduler) armOnceLocked(task *Task) {
	delay := time.Until(task.RunAt)
	if delay < 0 {
		delay = 0
	}
	id, command, skill := task.ID, task.Command, task.Skill
	task.timer = time.AfterFunc(delay, s.makeRunner(id, command, skill))
}

func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	s.removeLocked(task)
	return s.save()
}

// Pause sets the paused flag on a task. A paused task keeps its cron entry and
// state but is skipped when it fires, until resumed. Persists across restarts.
func (s *Scheduler) Pause(id string, paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	task.Paused = paused
	return s.save()
}

func (s *Scheduler) List() []TaskInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]TaskInfo, 0, len(s.tasks))
	for _, t := range s.tasks {
		info := TaskInfo{
			TaskData: t.TaskData,
		}
		if t.Kind == "once" && !t.RunAt.IsZero() {
			info.NextRun = t.RunAt
		} else if t.entryID != 0 {
			if entry := s.cron.Entry(t.entryID); !entry.Next.IsZero() {
				info.NextRun = entry.Next
			}
		}
		out = append(out, info)
	}
	return out
}

func (s *Scheduler) makeRunner(id, command, skill string) func() {
	return func() {
		s.mu.Lock()
		task, ok := s.tasks[id]
		s.mu.Unlock()
		if !ok {
			return
		}

		if task.Paused {
			log.Info().Str("id", id).Msg("scheduler: task paused, skipping")
			return
		}

		if !task.running.CompareAndSwap(false, true) {
			log.Warn().Str("id", id).Msg("scheduler: task already running, skipping")
			return
		}
		defer task.running.Store(false)

		if task.State.ConsecutiveErrors > 0 && !task.State.LastRunAt.IsZero() {
			idx := task.State.ConsecutiveErrors - 1
			if idx >= len(backoffSchedule) {
				idx = len(backoffSchedule) - 1
			}
			cooldown := backoffSchedule[idx]
			if time.Since(task.State.LastRunAt) < cooldown {
				log.Info().
					Str("id", id).
					Int("consecutive_errors", task.State.ConsecutiveErrors).
					Dur("cooldown", cooldown).
					Msg("scheduler: backoff active, skipping")
				return
			}
		}

		s.sem <- struct{}{}
		defer func() { <-s.sem }()

		log.Info().Str("id", id).Str("command", command).Msg("scheduler firing task")
		start := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), s.opts.taskTimeout())
		defer cancel()

		output, err := s.executor(ctx, id, command, skill)

		elapsed := time.Since(start)
		state := TaskState{
			LastRunAt:      start,
			LastDurationMs: elapsed.Milliseconds(),
		}

		if err != nil {
			state.LastStatus = "error"
			state.LastError = err.Error()
			state.ConsecutiveErrors = task.State.ConsecutiveErrors + 1
			log.Error().
				Err(err).
				Str("id", id).
				Int("consecutive_errors", state.ConsecutiveErrors).
				Dur("duration", elapsed).
				Msg("scheduled task failed")
		} else {
			state.LastStatus = "ok"
			state.ConsecutiveErrors = 0
			log.Info().
				Str("id", id).
				Int("output_len", len(output)).
				Dur("duration", elapsed).
				Msg("scheduled task completed")
		}

		s.mu.Lock()
		if t, ok := s.tasks[id]; ok {
			t.State = state
			t.Runs++
			// One-shot or max_runs exhausted → remove after fire
			done := t.Kind == "once" || (t.MaxRuns > 0 && t.Runs >= t.MaxRuns)
			if done {
				s.removeLocked(t)
				log.Info().Str("id", id).Str("kind", t.Kind).Int("runs", t.Runs).Msg("scheduler: task completed and removed")
			}
			_ = s.save()
		}
		s.mu.Unlock()
	}
}

func (s *Scheduler) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}

	var tasks []*Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return err
	}

	for _, t := range tasks {
		if t.Kind == "" {
			t.Kind = "cron"
		}
		if t.Kind == "once" {
			if t.RunAt.IsZero() {
				log.Warn().Str("id", t.ID).Msg("once task missing run_at, dropping")
				continue
			}
			if time.Now().After(t.RunAt.Add(1 * time.Hour)) {
				log.Warn().Str("id", t.ID).Time("run_at", t.RunAt).Msg("once task expired, dropping")
				continue
			}
			s.armOnceLocked(t)
			s.tasks[t.ID] = t
			continue
		}
		entryID, err := s.cron.AddFunc(t.Schedule, s.makeRunner(t.ID, t.Command, t.Skill))
		if err != nil {
			log.Error().Err(err).Str("id", t.ID).Str("schedule", t.Schedule).Msg("failed to restore scheduled task")
			continue
		}
		t.entryID = entryID
		s.tasks[t.ID] = t
	}

	log.Info().Int("count", len(s.tasks)).Msg("loaded schedules from disk")
	return nil
}

func (s *Scheduler) saveLocalLocked() error {
	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

func (s *Scheduler) save() error {
	if err := s.saveLocalLocked(); err != nil {
		return err
	}
	if s.cloud != nil {
		tasks := make([]*Task, 0, len(s.tasks))
		for _, t := range s.tasks {
			tasks = append(tasks, t)
		}
		data, err := json.MarshalIndent(tasks, "", "  ")
		if err == nil {
			_ = s.cloud.SaveSchedulesJSON(data)
		}
	}
	return nil
}
