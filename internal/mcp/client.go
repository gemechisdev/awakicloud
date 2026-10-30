package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tool is a discovered MCP tool.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// Session is a live connection to one MCP server.
type Session struct {
	Name   string
	Config Server

	mu     sync.Mutex
	idSeq  atomic.Int64
	tools  []Tool
	closed bool

	// stdio
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.ReadCloser

	// http
	httpClient *http.Client
	endpoint   string
	headers    map[string]string
	sessionID  string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Connect opens a session for the given server config.
func Connect(ctx context.Context, srv Server) (*Session, error) {
	if err := srv.Validate(); err != nil {
		return nil, err
	}
	s := &Session{Name: srv.Name, Config: srv}
	switch srv.Transport() {
	case TransportStdio:
		if err := s.connectStdio(ctx, srv); err != nil {
			return nil, err
		}
	case TransportHTTP:
		if err := s.connectHTTP(ctx, srv, false); err != nil {
			return nil, err
		}
	case TransportSSE:
		if err := s.connectHTTP(ctx, srv, true); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q", srv.Transport())
	}
	if err := s.initialize(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := s.refreshTools(ctx); err != nil {
		// non-fatal: server may not expose tools
		s.tools = nil
	}
	return s, nil
}

func (s *Session) connectStdio(ctx context.Context, srv Server) error {
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	env := os.Environ()
	for k, v := range srv.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", srv.Command, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReader(stdout)
	s.stderr = stderr
	// Drain stderr so the process does not block
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			// optional: log
		}
	}()
	return nil
}

func (s *Session) connectHTTP(ctx context.Context, srv Server, _ bool) error {
	s.httpClient = &http.Client{Timeout: 60 * time.Second}
	s.endpoint = srv.URL
	s.headers = map[string]string{}
	for k, v := range srv.Headers {
		s.headers[k] = v
	}
	// Apply stored OAuth access token (refresh if expired).
	if srv.OAuth != nil {
		oauth := srv.OAuth
		if !srv.HasValidToken() && oauth.RefreshToken != "" {
			_ = RefreshAccessToken(ctx, oauth, strings.TrimRight(srv.URL, "/"))
		}
		if oauth.AccessToken != "" {
			typ := oauth.TokenType
			if typ == "" {
				typ = "Bearer"
			}
			s.headers["Authorization"] = typ + " " + oauth.AccessToken
		}
	}
	return nil
}

func (s *Session) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "awaki",
			"version": "1.0.0",
		},
	}
	var result map[string]any
	if err := s.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	// notify initialized (stdio / older servers)
	_ = s.notify(ctx, "notifications/initialized", map[string]any{})
	return nil
}

func (s *Session) refreshTools(ctx context.Context) error {
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := s.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return err
	}
	tools := make([]Tool, 0, len(result.Tools))
	for _, t := range result.Tools {
		tools = append(tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
	return nil
}

// Tools returns the last discovered tool list.
func (s *Session) Tools() []Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out
}

// CallTool invokes tools/call.
func (s *Session) CallTool(ctx context.Context, name string, arguments map[string]any) (string, error) {
	params := map[string]any{
		"name":      name,
		"arguments": arguments,
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := s.call(ctx, "tools/call", params, &result); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range result.Content {
		if c.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	out := b.String()
	if result.IsError {
		return out, fmt.Errorf("tool error: %s", out)
	}
	if out == "" {
		out = "(empty result)"
	}
	return out, nil
}

func (s *Session) call(ctx context.Context, method string, params any, out any) error {
	id := s.idSeq.Add(1)
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	raw, err := s.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("rpc %s: %s", method, resp.Error.Message)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func (s *Session) notify(ctx context.Context, method string, params any) error {
	// notifications have no id
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if s.stdin != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err = s.stdin.Write(append(data, '\n'))
		return err
	}
	// HTTP notifications: best-effort POST
	if s.httpClient != nil {
		_, _ = s.httpPOST(ctx, data)
	}
	return nil
}

func (s *Session) roundTrip(ctx context.Context, req rpcRequest) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if s.stdin != nil {
		return s.stdioRoundTrip(data)
	}
	return s.httpRoundTrip(ctx, data)
}

func (s *Session) stdioRoundTrip(data []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session closed")
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	line, err := s.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(line), nil
}

func (s *Session) httpRoundTrip(ctx context.Context, data []byte) ([]byte, error) {
	return s.httpPOST(ctx, data)
}

func (s *Session) httpPOST(ctx context.Context, data []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	if s.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.sessionID = sid
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	// Streamable HTTP may return SSE-framed body; take first data line with JSON
	if bytes.Contains(body, []byte("data:")) {
		for _, line := range bytes.Split(body, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if len(payload) > 0 && payload[0] == '{' {
					return payload, nil
				}
			}
		}
	}
	return bytes.TrimSpace(body), nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
