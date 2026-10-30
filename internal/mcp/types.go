package mcp

import (
	"fmt"
	"time"
)

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// Server is a persisted MCP server configuration.
type Server struct {
	Name     string            `json:"name"`
	Type     string            `json:"type,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`

	// OAuth (optional). Tokens are filled after Authorize flow.
	OAuth *OAuthConfig `json:"oauth,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// OAuthConfig holds client settings and issued tokens for a remote MCP server.
type OAuthConfig struct {
	// ClientID is optional pre-registered id; empty → try DCR then public "awaki".
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// AuthServer is optional override; empty → discover from resource metadata.
	AuthServer string `json:"auth_server,omitempty"`
	Scopes     string `json:"scopes,omitempty"`
	// RedirectURI used in the flow (default urn:ietf:wg:oauth:2.0:oob for paste-back).
	RedirectURI string `json:"redirect_uri,omitempty"`

	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`

	// Cached discovery
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
}

// PendingAuth is an in-memory PKCE session waiting for the user to paste a code.
type PendingAuth struct {
	ServerName   string
	State        string
	CodeVerifier string
	RedirectURI  string
	Resource     string
	ClientID     string
	TokenURL     string
	CreatedAt    time.Time
}

type ConfigFile struct {
	Servers []Server `json:"mcpServers"`
}

func (s Server) Transport() string {
	if s.Type != "" {
		return s.Type
	}
	if s.Command != "" {
		return TransportStdio
	}
	if s.URL != "" {
		return TransportHTTP
	}
	return TransportStdio
}

func (s Server) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	t := s.Transport()
	switch t {
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("stdio server %q needs a command", s.Name)
		}
	case TransportHTTP, TransportSSE:
		if s.URL == "" {
			return fmt.Errorf("%s server %q needs a url", t, s.Name)
		}
	default:
		return fmt.Errorf("unknown transport type %q for %q", t, s.Name)
	}
	return nil
}

func (s Server) HasValidToken() bool {
	if s.OAuth == nil || s.OAuth.AccessToken == "" {
		return false
	}
	if s.OAuth.ExpiresAt.IsZero() {
		return true
	}
	return time.Now().Before(s.OAuth.ExpiresAt.Add(-30 * time.Second))
}
