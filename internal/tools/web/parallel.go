package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Parallel free keyless Search MCP — same endpoint as Awaki Android ParallelProtocol.
const (
	parallelMCPURL        = "https://search.parallel.ai/mcp"
	mcpProtocolVersion    = "2025-03-26"
	parallelDefaultTimeout = 25 * time.Second
)

var (
	parallelSessionOnce sync.Once
	parallelSessionID   string
)

func getParallelSessionID() string {
	parallelSessionOnce.Do(func() {
		parallelSessionID = fmt.Sprintf("%d", time.Now().UnixNano())
	})
	return parallelSessionID
}

type mcpRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type webHit struct {
	Title   string
	URL     string
	Snippet string
}

func parallelMCPPost(ctx context.Context, body any, expectID int) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parallelMCPURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("parallel MCP HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	msg := parseMCPMessage(string(raw), expectID)
	if msg == nil {
		return nil, fmt.Errorf("parallel MCP: no readable JSON-RPC message")
	}
	var rpc mcpRPC
	if err := json.Unmarshal(msg, &rpc); err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("parallel MCP: %s", rpc.Error.Message)
	}
	return rpc.Result, nil
}

func parseMCPMessage(body string, expectedID int) json.RawMessage {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var probe map[string]any
		if json.Unmarshal([]byte(trimmed), &probe) == nil {
			if id, ok := probe["id"].(float64); !ok || int(id) == expectedID {
				return json.RawMessage(trimmed)
			}
		}
	}
	// text/event-stream: last data: event with matching id
	var found json.RawMessage
	var event strings.Builder
	flush := func() {
		if event.Len() == 0 {
			return
		}
		s := event.String()
		event.Reset()
		var probe map[string]any
		if json.Unmarshal([]byte(s), &probe) != nil {
			return
		}
		if id, ok := probe["id"].(float64); ok && int(id) != expectedID {
			return
		}
		found = json.RawMessage(s)
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "data:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.TrimSpace(line) == "":
			flush()
		}
	}
	flush()
	return found
}

func ensureParallelSession(ctx context.Context) {
	// Best-effort initialize; tools/call often works without a sticky session.
	initBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "awaki", "version": "1"},
		},
	}
	_, _ = parallelMCPPost(ctx, initBody, 1)
	notify := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	payload, _ := json.Marshal(notify)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parallelMCPURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func parallelToolCall(ctx context.Context, id int, tool string, args map[string]any) (string, error) {
	ensureParallelSession(ctx)
	params, _ := json.Marshal(map[string]any{
		"name":      tool,
		"arguments": args,
	})
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  json.RawMessage(params),
	}
	result, err := parallelMCPPost(ctx, body, id)
	if err != nil {
		return "", err
	}
	// result.content[].text
	var parsed struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return string(result), nil
	}
	var b strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" || c.Type == "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(c.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if parsed.IsError {
		return "", fmt.Errorf("%s", text)
	}
	if text == "" {
		return "", fmt.Errorf("parallel tool returned no text")
	}
	return text, nil
}

func parallelSearch(ctx context.Context, query string, limit int) ([]webHit, error) {
	ctx, cancel := context.WithTimeout(ctx, parallelDefaultTimeout)
	defer cancel()
	args := map[string]any{
		"objective":      "Find current, relevant web pages for: " + query,
		"search_queries": []string{query},
		"session_id":     getParallelSessionID(),
	}
	text, err := parallelToolCall(ctx, 2, "web_search", args)
	if err != nil {
		return nil, err
	}
	hits := parseParallelSearch(text, limit)
	if len(hits) == 0 {
		return nil, fmt.Errorf("parallel search returned no usable links")
	}
	return hits, nil
}

func parallelFetch(ctx context.Context, pageURL string) (markdown string, title string, err error) {
	ctx, cancel := context.WithTimeout(ctx, parallelDefaultTimeout)
	defer cancel()
	args := map[string]any{
		"urls":         []string{pageURL},
		"full_content": true,
		"session_id":   getParallelSessionID(),
	}
	text, err := parallelToolCall(ctx, 3, "web_fetch", args)
	if err != nil {
		return "", "", err
	}
	md, title := parseParallelFetch(text)
	if strings.TrimSpace(md) == "" {
		return "", "", fmt.Errorf("parallel fetch returned empty content")
	}
	return md, title, nil
}

func parseParallelSearch(text string, limit int) []webHit {
	trimmed := strings.TrimSpace(text)
	// Structured results array
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var results []map[string]any
		if strings.HasPrefix(trimmed, "{") {
			var wrap map[string]any
			if json.Unmarshal([]byte(trimmed), &wrap) == nil {
				if arr, ok := wrap["results"].([]any); ok {
					for _, item := range arr {
						if m, ok := item.(map[string]any); ok {
							results = append(results, m)
						}
					}
				}
			}
		} else {
			_ = json.Unmarshal([]byte(trimmed), &results)
		}
		var hits []webHit
		for _, item := range results {
			u, _ := item["url"].(string)
			if u == "" || !strings.HasPrefix(u, "http") {
				continue
			}
			title, _ := item["title"].(string)
			snippet := firstString(item, "snippet", "description", "excerpt")
			if excerpts, ok := item["excerpts"].([]any); ok && snippet == "" {
				for _, e := range excerpts {
					if s, ok := e.(string); ok && s != "" {
						snippet = s
						break
					}
				}
			}
			if title == "" {
				title = hostOf(u)
			}
			hits = append(hits, webHit{Title: title, URL: u, Snippet: truncate(snippet, 400)})
			if len(hits) >= limit {
				break
			}
		}
		if len(hits) > 0 {
			return hits
		}
	}
	return hitsFromMarkdown(trimmed, limit)
}

func parseParallelFetch(text string) (markdown, title string) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var first map[string]any
		if strings.HasPrefix(trimmed, "{") {
			var wrap map[string]any
			if json.Unmarshal([]byte(trimmed), &wrap) == nil {
				if arr, ok := wrap["results"].([]any); ok && len(arr) > 0 {
					first, _ = arr[0].(map[string]any)
				} else {
					first = wrap
				}
			}
		} else {
			var arr []map[string]any
			if json.Unmarshal([]byte(trimmed), &arr) == nil && len(arr) > 0 {
				first = arr[0]
			}
		}
		if first != nil {
			title, _ = first["title"].(string)
			if full, ok := first["full_content"].(string); ok && strings.TrimSpace(full) != "" {
				return full, title
			}
			if excerpts, ok := first["excerpts"].([]any); ok {
				var parts []string
				for _, e := range excerpts {
					if s, ok := e.(string); ok && s != "" {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					return strings.Join(parts, "\n\n"), title
				}
			}
		}
	}
	return trimmed, ""
}

var (
	mdLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)
	bareURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)
)

func hitsFromMarkdown(markdown string, limit int) []webHit {
	seen := map[string]bool{}
	var hits []webHit
	for _, m := range mdLinkRe.FindAllStringSubmatch(markdown, -1) {
		u := strings.TrimRight(m[2], ".,>")
		if seen[u] || !strings.HasPrefix(u, "http") {
			continue
		}
		seen[u] = true
		title := strings.TrimSpace(m[1])
		if title == "" {
			title = hostOf(u)
		}
		hits = append(hits, webHit{Title: title, URL: u})
		if len(hits) >= limit {
			return hits
		}
	}
	if len(hits) == 0 {
		for _, u := range bareURLRe.FindAllString(markdown, -1) {
			u = strings.TrimRight(u, ".,>")
			if seen[u] {
				continue
			}
			seen[u] = true
			hits = append(hits, webHit{Title: hostOf(u), URL: u})
			if len(hits) >= limit {
				break
			}
		}
	}
	return hits
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func hostOf(raw string) string {
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "www.")
	if i := strings.IndexAny(raw, "/?"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "link"
	}
	return raw
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func formatHits(hits []webHit, via string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "via: %s\n\n", via)
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
		if h.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", h.Snippet)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "no results"
	}
	return b.String()
}

func logWeb(via, step string, err error) {
	if err != nil {
		log.Warn().Err(err).Str("via", via).Msg(step)
	} else {
		log.Info().Str("via", via).Msg(step)
	}
}
