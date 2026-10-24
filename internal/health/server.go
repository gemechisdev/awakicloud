package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// StatusFunc returns a lightweight snapshot for / and /health JSON.
type StatusFunc func() map[string]any

// Server is a tiny HTTP listener for platform health checks (Koyeb, Render, Railway, …).
type Server struct {
	addr   string
	status StatusFunc
	ready  atomic.Bool
	srv    *http.Server
}

// AddrFromEnv picks the listen address.
// Prefer PORT (Koyeb/Render/Heroku), then AWAKI_HTTP_PORT, default :8000.
func AddrFromEnv() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("AWAKI_HTTP_PORT")
	}
	if port == "" {
		port = "8000"
	}
	// allow full addr like 0.0.0.0:8000
	if len(port) > 0 && port[0] == ':' {
		return port
	}
	if _, err := net.LookupPort("tcp", port); err == nil || isDigits(port) {
		return ":" + port
	}
	return port
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func New(addr string, status StatusFunc) *Server {
	if addr == "" {
		addr = AddrFromEnv()
	}
	if status == nil {
		status = func() map[string]any {
			return map[string]any{"ok": true}
		}
	}
	s := &Server{addr: addr, status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/ready", s.handleReady)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// SetReady marks the process as ready for /ready (after Telegram polling starts).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("pong\n"))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": false})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": true})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	st := s.status()
	st["status"] = "ok"
	st["time"] = time.Now().UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(st)
}

// Start begins listening in the background. Returns when the listener is bound
// or if bind fails. Call Shutdown on process exit.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("health listen %s: %w", s.addr, err)
	}
	log.Info().Str("addr", ln.Addr().String()).Msg("http health server listening")
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("health server stopped")
		}
	}()
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shCtx)
	}()
	return nil
}
