package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ToolsChangedFunc is called after connect/disconnect so the host can hot-reload agent tools.
type ToolsChangedFunc func()

// Manager owns configured servers and live sessions.
type Manager struct {
	store *Store

	mu       sync.Mutex
	sessions map[string]*Session
	pending  map[string]PendingAuth // server name -> PKCE session

	onToolsChanged ToolsChangedFunc
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		store:    NewStore(dataDir),
		sessions: make(map[string]*Session),
		pending:  make(map[string]PendingAuth),
	}
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) SetOnToolsChanged(fn ToolsChangedFunc) {
	m.mu.Lock()
	m.onToolsChanged = fn
	m.mu.Unlock()
}

func (m *Manager) notifyToolsChanged() {
	m.mu.Lock()
	fn := m.onToolsChanged
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *Manager) ListConfigured() ([]Server, error) {
	return m.store.List()
}

func (m *Manager) Add(srv Server) error {
	return m.store.Upsert(srv)
}

func (m *Manager) Remove(name string) error {
	m.Disconnect(name)
	m.mu.Lock()
	delete(m.pending, name)
	m.mu.Unlock()
	return m.store.Delete(name)
}

func (m *Manager) SetDisabled(name string, disabled bool) error {
	if disabled {
		m.Disconnect(name)
	}
	return m.store.SetDisabled(name, disabled)
}

func (m *Manager) ConnectAll(ctx context.Context) {
	list, err := m.store.List()
	if err != nil {
		log.Warn().Err(err).Msg("mcp: list configs")
		return
	}
	for _, srv := range list {
		if srv.Disabled {
			continue
		}
		if err := m.Connect(ctx, srv.Name); err != nil {
			log.Warn().Err(err).Str("server", srv.Name).Msg("mcp: connect failed")
		}
	}
	m.notifyToolsChanged()
}

func (m *Manager) Connect(ctx context.Context, name string) error {
	srv, ok, err := m.store.Get(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mcp server %q not found", name)
	}
	if srv.Disabled {
		return fmt.Errorf("mcp server %q is disabled", name)
	}
	m.mu.Lock()
	if old, exists := m.sessions[name]; exists {
		_ = old.Close()
		delete(m.sessions, name)
	}
	m.mu.Unlock()

	sess, err := Connect(ctx, srv)
	if err != nil {
		// Surface auth-needed clearly for HTTP
		if isUnauthorized(err) {
			return fmt.Errorf("unauthorized — use Authorize for %q (OAuth or API key)", name)
		}
		return err
	}
	// If token was refreshed during connect, persist it
	if srv.OAuth != nil && srv.HasValidToken() {
		_ = m.store.Upsert(srv)
	}
	m.mu.Lock()
	m.sessions[name] = sess
	m.mu.Unlock()
	log.Info().Str("server", name).Int("tools", len(sess.Tools())).Msg("mcp: connected")
	m.notifyToolsChanged()
	return nil
}

func (m *Manager) Disconnect(name string) {
	m.mu.Lock()
	if s, ok := m.sessions[name]; ok {
		_ = s.Close()
		delete(m.sessions, name)
	}
	m.mu.Unlock()
	m.notifyToolsChanged()
}

func (m *Manager) DisconnectAll() {
	m.mu.Lock()
	for name, s := range m.sessions {
		_ = s.Close()
		delete(m.sessions, name)
	}
	m.mu.Unlock()
	m.notifyToolsChanged()
}

func (m *Manager) StatusLine() string {
	list, _ := m.store.List()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(list) == 0 {
		return "No MCP servers configured."
	}
	var parts []string
	for _, srv := range list {
		state := "configured"
		if srv.Disabled {
			state = "disabled"
		} else if _, ok := m.sessions[srv.Name]; ok {
			state = "connected"
		}
		auth := ""
		if srv.OAuth != nil && srv.OAuth.AccessToken != "" {
			auth = " oauth"
		} else if len(srv.Headers) > 0 {
			auth = " key"
		}
		parts = append(parts, fmt.Sprintf("%s (%s/%s%s)", srv.Name, srv.Transport(), state, auth))
	}
	return strings.Join(parts, "\n")
}

func (m *Manager) LiveTools() []LiveTool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LiveTool
	for name, sess := range m.sessions {
		for _, t := range sess.Tools() {
			out = append(out, LiveTool{
				Server:    name,
				Tool:      t,
				Qualified: fmt.Sprintf("mcp__%s__%s", sanitize(name), sanitize(t.Name)),
			})
		}
	}
	return out
}

func (m *Manager) Call(ctx context.Context, server, tool string, args map[string]any) (string, error) {
	m.mu.Lock()
	sess, ok := m.sessions[server]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("mcp server %q is not connected", server)
	}
	return sess.CallTool(ctx, tool, args)
}

func (m *Manager) CallQualified(ctx context.Context, qualified string, args map[string]any) (string, error) {
	server, tool, ok := ParseQualified(qualified)
	if !ok {
		return "", fmt.Errorf("invalid mcp tool name %q", qualified)
	}
	return m.Call(ctx, server, tool, args)
}

// BeginOAuth starts PKCE and returns the browser authorize URL.
func (m *Manager) BeginOAuth(ctx context.Context, name string) (authURL string, err error) {
	srv, ok, err := m.store.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("mcp server %q not found", name)
	}
	if srv.Transport() == TransportStdio {
		return "", fmt.Errorf("OAuth is for http/sse servers only")
	}
	url, pending, err := StartAuth(ctx, srv)
	if err != nil {
		return "", err
	}
	// Persist discovery fields on the server config
	if srv.OAuth == nil {
		srv.OAuth = &OAuthConfig{}
	}
	// StartAuth may have filled discovery via DiscoverAuth into a local copy —
	// re-discover into srv for persistence
	if disc, dErr := DiscoverAuth(ctx, srv.URL); dErr == nil {
		srv.OAuth.AuthServer = disc.AuthServer
		srv.OAuth.AuthorizationEndpoint = disc.AuthorizationEndpoint
		srv.OAuth.TokenEndpoint = disc.TokenEndpoint
		srv.OAuth.RegistrationEndpoint = disc.RegistrationEndpoint
	}
	if pending.ClientID != "" {
		srv.OAuth.ClientID = pending.ClientID
	}
	_ = m.store.Upsert(srv)

	m.mu.Lock()
	m.pending[name] = pending
	m.mu.Unlock()
	return url, nil
}

// CompleteOAuth finishes the flow with a pasted code or redirect URL.
func (m *Manager) CompleteOAuth(ctx context.Context, name, codeOrURL string) error {
	m.mu.Lock()
	pending, ok := m.pending[name]
	m.mu.Unlock()
	if !ok || time.Since(pending.CreatedAt) > 15*time.Minute {
		return fmt.Errorf("no active authorize session for %q — tap Authorize again", name)
	}
	srv, found, err := m.store.Get(name)
	if err != nil || !found {
		return fmt.Errorf("server %q not found", name)
	}
	secret := ""
	if srv.OAuth != nil {
		secret = srv.OAuth.ClientSecret
	}
	tok, err := FinishAuth(ctx, pending, codeOrURL, secret)
	if err != nil {
		return err
	}
	if srv.OAuth == nil {
		srv.OAuth = &OAuthConfig{}
	}
	srv.OAuth.ClientID = tok.ClientID
	srv.OAuth.AccessToken = tok.AccessToken
	srv.OAuth.RefreshToken = tok.RefreshToken
	srv.OAuth.TokenType = tok.TokenType
	srv.OAuth.ExpiresAt = tok.ExpiresAt
	if tok.TokenEndpoint != "" {
		srv.OAuth.TokenEndpoint = tok.TokenEndpoint
	}
	if err := m.store.Upsert(srv); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.pending, name)
	m.mu.Unlock()
	return m.Connect(ctx, name)
}

type LiveTool struct {
	Server    string
	Tool      Tool
	Qualified string
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

func ParseQualified(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, "mcp__")
	i := strings.Index(rest, "__")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}
