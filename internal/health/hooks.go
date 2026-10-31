package health

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// HookCallbacks are optional handlers the host injects after the engine is ready.
// If Secret is empty, all /hooks/* routes return 503 (disabled).
type HookCallbacks struct {
	// Secret from AWAKI_HOOK_SECRET. Required for any hook to succeed.
	Secret string

	// Agent runs a normal (or plan-mode) agent turn as the owner.
	// mode: ""|"auto" start if idle / queue if busy; "queue" always queue; "interrupt" stop current then run.
	Agent func(ctx context.Context, text string, plan bool, mode string) (result map[string]any, err error)

	// Task queues a background board task.
	Task func(ctx context.Context, title, prompt string, autoStart bool) (result map[string]any, err error)

	// Schedule adds a scheduler entry (once/cron/interval).
	Schedule func(ctx context.Context, schedule, command, label string) (result map[string]any, err error)
}

// SetHooks registers remote-trigger handlers. Safe to call before or after Start.
func (s *Server) SetHooks(h HookCallbacks) {
	s.hooks = h
	if h.Secret != "" {
		log.Info().Msg("http hooks enabled (/hooks/agent, /hooks/task, /hooks/schedule)")
	} else {
		log.Info().Msg("http hooks disabled — set AWAKI_HOOK_SECRET to enable")
	}
}

// HookSecretFromEnv reads AWAKI_HOOK_SECRET.
func HookSecretFromEnv() string {
	return strings.TrimSpace(os.Getenv("AWAKI_HOOK_SECRET"))
}

func (s *Server) registerHookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/hooks/agent", s.handleHookAgent)
	mux.HandleFunc("/hooks/task", s.handleHookTask)
	mux.HandleFunc("/hooks/schedule", s.handleHookSchedule)
}

func (s *Server) authorizeHook(w http.ResponseWriter, r *http.Request) bool {
	secret := strings.TrimSpace(s.hooks.Secret)
	if secret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "hooks disabled — set AWAKI_HOOK_SECRET",
		})
		return false
	}
	got := extractHookSecret(r)
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}

func extractHookSecret(r *http.Request) string {
	if h := r.Header.Get("X-Awaki-Secret"); h != "" {
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("Authorization"); h != "" {
		h = strings.TrimSpace(h)
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	// Query only as last resort (avoid logging URLs with secrets).
	return strings.TrimSpace(r.URL.Query().Get("secret"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readBodyLimited(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max))
}

// POST /hooks/agent
// JSON: { "text": "...", "plan": false, "mode": "auto|queue|interrupt" }
// Or raw text/plain body = text.
func (s *Server) handleHookAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	if !s.authorizeHook(w, r) {
		return
	}
	if s.hooks.Agent == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "agent hook not wired"})
		return
	}

	body, err := readBodyLimited(r, 1<<20) // 1 MiB
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	text := ""
	plan := false
	mode := "auto"
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") || (len(body) > 0 && body[0] == '{') {
		var req struct {
			Text    string `json:"text"`
			Message string `json:"message"` // alias
			Prompt  string `json:"prompt"`  // alias
			Plan    bool   `json:"plan"`
			Mode    string `json:"mode"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
		text = firstNonEmpty(req.Text, req.Message, req.Prompt)
		plan = req.Plan
		if req.Mode != "" {
			mode = strings.ToLower(strings.TrimSpace(req.Mode))
		}
	} else {
		text = strings.TrimSpace(string(body))
		if q := r.URL.Query().Get("plan"); q == "1" || strings.EqualFold(q, "true") {
			plan = true
		}
		if m := r.URL.Query().Get("mode"); m != "" {
			mode = strings.ToLower(m)
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := s.hooks.Agent(ctx, text, plan, mode)
	if err != nil {
		log.Warn().Err(err).Msg("hook agent failed")
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "ok": false})
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["ok"] = true
	writeJSON(w, http.StatusAccepted, result)
}

// POST /hooks/task
// JSON: { "title": "...", "prompt": "...", "auto_start": true }
func (s *Server) handleHookTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	if !s.authorizeHook(w, r) {
		return
	}
	if s.hooks.Task == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "task hook not wired"})
		return
	}

	body, err := readBodyLimited(r, 1<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	var req struct {
		Title     string `json:"title"`
		Prompt    string `json:"prompt"`
		AutoStart *bool  `json:"auto_start"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
	}
	title := strings.TrimSpace(req.Title)
	prompt := strings.TrimSpace(req.Prompt)
	if title == "" {
		title = prompt
	}
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title or prompt required"})
		return
	}
	auto := true
	if req.AutoStart != nil {
		auto = *req.AutoStart
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := s.hooks.Task(ctx, title, prompt, auto)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "ok": false})
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["ok"] = true
	writeJSON(w, http.StatusAccepted, result)
}

// POST /hooks/schedule
// JSON: { "schedule": "in 5m", "command": "message text", "label": "optional" }
func (s *Server) handleHookSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	if !s.authorizeHook(w, r) {
		return
	}
	if s.hooks.Schedule == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "schedule hook not wired"})
		return
	}

	body, err := readBodyLimited(r, 1<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	var req struct {
		Schedule string `json:"schedule"`
		Command  string `json:"command"`
		Message  string `json:"message"` // alias for command
		Label    string `json:"label"`
		ID       string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	cmd := firstNonEmpty(req.Command, req.Message)
	if strings.TrimSpace(req.Schedule) == "" || strings.TrimSpace(cmd) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "schedule and command required"})
		return
	}
	label := req.Label
	if label == "" {
		label = req.ID
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := s.hooks.Schedule(ctx, req.Schedule, cmd, label)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "ok": false})
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["ok"] = true
	writeJSON(w, http.StatusAccepted, result)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
