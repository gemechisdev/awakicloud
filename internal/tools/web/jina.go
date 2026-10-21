package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const jinaTimeout = 30 * time.Second

// jinaFetch reads a URL through Jina's keyless reader (r.jina.ai).
// Same idea as Awaki JinaProtocol — no API key required for basic use.
func jinaFetch(ctx context.Context, pageURL string, maxChars int) (markdown string, title string, err error) {
	if maxChars <= 0 {
		maxChars = 50_000
	}
	ctx, cancel := context.WithTimeout(ctx, jinaTimeout)
	defer cancel()

	readerURL := "https://r.jina.ai/" + pageURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readerURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AwakiCloudBot/1.0)")
	req.Header.Set("X-Return-Format", "markdown")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("jina connection: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars)+4096))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("jina HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", "", fmt.Errorf("jina returned empty body")
	}
	if len(text) > maxChars {
		text = text[:maxChars] + "\n… (truncated)"
	}
	// Optional title line: "Title: ..."
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "title:") {
			title = strings.TrimSpace(line[6:])
			break
		}
	}
	return text, title, nil
}
