package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultRedirectURI = "http://127.0.0.1/callback"

// DiscoverAuth probes the MCP URL for protected-resource / AS metadata.
// Supports:
//   - RFC 9728 path-aware well-known URLs
//   - WWW-Authenticate resource_metadata= on 401 (Firecrawl-style)
//   - RFC 8414 authorization-server metadata on the AS host
func DiscoverAuth(ctx context.Context, resourceURL string) (*OAuthConfig, error) {
	cfg := &OAuthConfig{}
	client := &http.Client{Timeout: 15 * time.Second}

	u, err := url.Parse(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("invalid resource URL: %w", err)
	}
	path := strings.TrimSuffix(u.Path, "/")
	originBase := u.Scheme + "://" + u.Host

	var authServers []string
	var scopes []string

	// 1) RFC 9728 candidates (path-aware first — required for Firecrawl)
	candidates := []string{}
	if path != "" && path != "/" {
		// https://host/.well-known/oauth-protected-resource/v2/mcp-oauth
		candidates = append(candidates, originBase+"/.well-known/oauth-protected-resource"+path)
	}
	candidates = append(candidates,
		strings.TrimRight(resourceURL, "/")+"/.well-known/oauth-protected-resource",
		originBase+"/.well-known/oauth-protected-resource",
	)

	for _, meta := range candidates {
		as, sc, ok := fetchPRM(ctx, client, meta)
		if ok {
			authServers = as
			scopes = sc
			break
		}
	}

	// 2) Probe resource with unauthenticated MCP initialize → parse WWW-Authenticate
	if len(authServers) == 0 {
		if metaURL, ok := probeResourceMetadata(ctx, client, resourceURL); ok {
			as, sc, ok2 := fetchPRM(ctx, client, metaURL)
			if ok2 {
				authServers = as
				scopes = sc
			}
		}
	}

	// 3) Fallback: treat MCP origin as AS
	if len(authServers) == 0 {
		authServers = []string{originBase}
	}

	as := strings.TrimRight(authServers[0], "/")
	cfg.AuthServer = as
	if len(scopes) > 0 {
		cfg.Scopes = strings.Join(scopes, " ")
	}

	// AS metadata (RFC 8414) — also try issuer path variants
	asMetaURLs := []string{
		as + "/.well-known/oauth-authorization-server",
		as + "/.well-known/openid-configuration",
	}
	// path-aware AS metadata if AS URL has a path
	if asu, e := url.Parse(as); e == nil && asu.Path != "" && asu.Path != "/" {
		asMetaURLs = append([]string{
			asu.Scheme + "://" + asu.Host + "/.well-known/oauth-authorization-server" + asu.Path,
		}, asMetaURLs...)
	}

	for _, mu := range asMetaURLs {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, mu, nil)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var m struct {
			AuthorizationEndpoint string `json:"authorization_endpoint"`
			TokenEndpoint         string `json:"token_endpoint"`
			RegistrationEndpoint  string `json:"registration_endpoint"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m.AuthorizationEndpoint != "" && m.TokenEndpoint != "" {
			cfg.AuthorizationEndpoint = m.AuthorizationEndpoint
			cfg.TokenEndpoint = m.TokenEndpoint
			cfg.RegistrationEndpoint = m.RegistrationEndpoint
			break
		}
	}
	if cfg.AuthorizationEndpoint == "" || cfg.TokenEndpoint == "" {
		return nil, fmt.Errorf("could not discover OAuth endpoints for %s", resourceURL)
	}
	return cfg, nil
}

func fetchPRM(ctx context.Context, client *http.Client, metaURL string) (authServers, scopes []string, ok bool) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, nil, false
	}
	defer resp.Body.Close()
	var body struct {
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
		Resource             string   `json:"resource"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, false
	}
	if len(body.AuthorizationServers) == 0 {
		return nil, nil, false
	}
	return body.AuthorizationServers, body.ScopesSupported, true
}

// probeResourceMetadata POSTs a minimal MCP initialize and parses
// WWW-Authenticate: Bearer resource_metadata="…".
func probeResourceMetadata(ctx context.Context, client *http.Client, resourceURL string) (string, bool) {
	payload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"awaki","version":"1.0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resourceURL, strings.NewReader(payload))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	wa := resp.Header.Get("WWW-Authenticate")
	if wa == "" {
		return "", false
	}
	// resource_metadata="https://..."
	const key = "resource_metadata="
	i := strings.Index(strings.ToLower(wa), key)
	if i < 0 {
		return "", false
	}
	rest := wa[i+len(key):]
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, `"`) {
		rest = rest[1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return "", false
		}
		return rest[:j], true
	}
	// unquoted until comma/space
	for _, sep := range []string{",", " "} {
		if j := strings.Index(rest, sep); j >= 0 {
			rest = rest[:j]
			break
		}
	}
	return strings.Trim(rest, `"`), rest != ""
}

// StartAuth builds a PKCE authorize URL and pending session.
func StartAuth(ctx context.Context, srv Server) (authURL string, pending PendingAuth, err error) {
	oauth := srv.OAuth
	if oauth == nil {
		oauth = &OAuthConfig{}
	}
	if oauth.AuthorizationEndpoint == "" || oauth.TokenEndpoint == "" {
		discovered, dErr := DiscoverAuth(ctx, srv.URL)
		if dErr != nil {
			return "", PendingAuth{}, dErr
		}
		if oauth.AuthServer == "" {
			oauth.AuthServer = discovered.AuthServer
		}
		oauth.AuthorizationEndpoint = discovered.AuthorizationEndpoint
		oauth.TokenEndpoint = discovered.TokenEndpoint
		oauth.RegistrationEndpoint = discovered.RegistrationEndpoint
		if oauth.Scopes == "" && discovered.Scopes != "" {
			oauth.Scopes = discovered.Scopes
		}
	}
	redirect := oauth.RedirectURI
	if redirect == "" {
		redirect = defaultRedirectURI
	}
	clientID := oauth.ClientID
	if clientID == "" && oauth.RegistrationEndpoint != "" {
		// Dynamic Client Registration (still widely supported)
		id, regErr := dynamicRegister(ctx, oauth.RegistrationEndpoint, redirect)
		if regErr == nil {
			clientID = id
			oauth.ClientID = id
		}
	}
	if clientID == "" {
		clientID = "awaki"
	}

	verifier, challenge, err := pkceS256()
	if err != nil {
		return "", PendingAuth{}, err
	}
	state, err := randomString(24)
	if err != nil {
		return "", PendingAuth{}, err
	}

	resource := strings.TrimRight(srv.URL, "/")
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	if oauth.Scopes != "" {
		q.Set("scope", oauth.Scopes)
		if !strings.Contains(oauth.Scopes, "offline_access") {
			q.Set("scope", oauth.Scopes+" offline_access")
		}
	} else {
		q.Set("scope", "offline_access")
	}

	authURL = oauth.AuthorizationEndpoint
	if strings.Contains(authURL, "?") {
		authURL += "&" + q.Encode()
	} else {
		authURL += "?" + q.Encode()
	}

	pending = PendingAuth{
		ServerName:   srv.Name,
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  redirect,
		Resource:     resource,
		ClientID:     clientID,
		TokenURL:     oauth.TokenEndpoint,
		CreatedAt:    time.Now().UTC(),
	}
	// persist discovery + client id onto a copy for caller to save
	srv.OAuth = oauth
	return authURL, pending, nil
}

// FinishAuth exchanges the authorization code (or full redirect URL) for tokens.
func FinishAuth(ctx context.Context, pending PendingAuth, codeOrURL string, clientSecret string) (*OAuthConfig, error) {
	code := strings.TrimSpace(codeOrURL)
	if strings.HasPrefix(code, "http://") || strings.HasPrefix(code, "https://") {
		u, err := url.Parse(code)
		if err != nil {
			return nil, fmt.Errorf("invalid callback URL: %w", err)
		}
		q := u.Query()
		if st := q.Get("state"); st != "" && st != pending.State {
			return nil, fmt.Errorf("state mismatch — restart Authorize")
		}
		code = q.Get("code")
		if code == "" {
			if e := q.Get("error"); e != "" {
				return nil, fmt.Errorf("auth error: %s (%s)", e, q.Get("error_description"))
			}
			return nil, fmt.Errorf("no code in callback URL")
		}
	}
	if code == "" {
		return nil, fmt.Errorf("authorization code is empty")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", pending.RedirectURI)
	form.Set("client_id", pending.ClientID)
	form.Set("code_verifier", pending.CodeVerifier)
	form.Set("resource", pending.Resource)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pending.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token endpoint %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}
	out := &OAuthConfig{
		ClientID:      pending.ClientID,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		TokenType:     tok.TokenType,
		TokenEndpoint: pending.TokenURL,
	}
	if out.TokenType == "" {
		out.TokenType = "Bearer"
	}
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return out, nil
}

// RefreshAccessToken uses the refresh_token grant when access token is expired.
func RefreshAccessToken(ctx context.Context, oauth *OAuthConfig, resource string) error {
	if oauth == nil || oauth.RefreshToken == "" || oauth.TokenEndpoint == "" {
		return fmt.Errorf("no refresh token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", oauth.RefreshToken)
	form.Set("client_id", oauth.ClientID)
	if resource != "" {
		form.Set("resource", resource)
	}
	if oauth.ClientSecret != "" {
		form.Set("client_secret", oauth.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refresh %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return err
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("empty access_token on refresh")
	}
	oauth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		oauth.RefreshToken = tok.RefreshToken
	}
	if tok.TokenType != "" {
		oauth.TokenType = tok.TokenType
	}
	if tok.ExpiresIn > 0 {
		oauth.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return nil
}

func dynamicRegister(ctx context.Context, regURL, redirect string) (string, error) {
	uris := []string{redirect}
	// Firecrawl (and many ASes) reject urn:ietf:wg:oauth:2.0:oob; always include localhost.
	for _, extra := range []string{"http://127.0.0.1/callback", "http://localhost/callback"} {
		if extra != redirect {
			uris = append(uris, extra)
		}
	}
	payload := map[string]any{
		"client_name":                "Awaki",
		"redirect_uris":              uris,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, regURL, strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("DCR %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("DCR returned no client_id")
	}
	return out.ClientID, nil
}

func pkceS256() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
