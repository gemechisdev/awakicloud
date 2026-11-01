package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gemechischala/awaki/internal/tools/types"
)

const (
	fetchTimeout  = 20 * time.Second
	maxFetchBody  = 512 * 1024
	maxPagedFetch = 400_000 // most characters we ask the reader service for
)

var defaultSelectors = []string{"article", "main", "[role=main]"}

func WebFetchTool() types.Tool {
	return types.Tool{
		Name:  "web_fetch",
		Label: "Reading webpage",
		Description: "Fetch a web page and return readable text. Tries Parallel MCP first, then Jina.ai reader, " +
			"then a direct HTTP fetch (HTML stripped). Use web_search to discover URLs first.",
		DetailFunc: func(input map[string]any) string {
			if raw, ok := input["url"].(string); ok {
				if u, err := neturl.Parse(raw); err == nil {
					return u.Host
				}
			}
			return ""
		},
		Parameters: map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The full http(s) URL to fetch",
			},
			"selector": map[string]any{
				"type":        "string",
				"description": "Optional CSS selector for direct-fetch fallback (e.g. article, main)",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"description": "Max characters to return per call (default 50000)",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Character offset to start from, for reading long pages in parts. A truncated result ends with the offset to use next.",
			},
		},
		Required: []string{"url"},
		Handler:  webFetch,
	}
}

func webFetch(ctx context.Context, input map[string]any) types.Result {
	rawURL, err := types.GetString(input, "url")
	if err != nil {
		return types.ErrResult(err)
	}
	if err := ValidatePublicURL(rawURL); err != nil {
		return types.Errf("url blocked: %v", err)
	}
	selector := types.GetStringOpt(input, "selector")
	maxChars := 50_000
	if v, ok := types.GetIntOpt(input, "max_chars"); ok && v > 0 {
		maxChars = v
	}
	offset, _ := types.GetIntOpt(input, "offset")
	if offset < 0 {
		offset = 0
	}

	var notes []string

	// 1) Parallel MCP
	if md, title, err := parallelFetch(ctx, rawURL); err == nil {
		logWeb("parallel", "web_fetch ok", nil)
		return types.Result{Output: formatFetched(rawURL, "parallel", title, md, maxChars, offset, false)}
	} else {
		notes = append(notes, "Parallel: "+err.Error())
		logWeb("parallel", "web_fetch", err)
	}

	// 2) Jina reader
	// The reader truncates to its limit, so ask for enough to cover the requested window.
	if md, title, err := jinaFetch(ctx, rawURL, min(offset+maxChars, maxPagedFetch)); err == nil {
		logWeb("jina", "web_fetch ok", nil)
		md, cut := strings.CutSuffix(md, "\n… (truncated)")
		out := formatFetched(rawURL, "jina.ai", title, md, maxChars, offset, cut)
		if len(notes) > 0 {
			out = strings.Join(notes, "\n") + "\n\n" + out
		}
		return types.Result{Output: out}
	} else {
		notes = append(notes, "Jina: "+err.Error())
		logWeb("jina", "web_fetch", err)
	}

	// 3) Direct HTTP
	direct, err := directFetch(ctx, rawURL, selector)
	if err != nil {
		notes = append(notes, "Direct: "+err.Error())
		return types.Errf("web_fetch failed:\n%s", strings.Join(notes, "\n"))
	}
	logWeb("direct", "web_fetch ok", nil)
	out := formatFetched(rawURL, "direct", "", direct, maxChars, offset, false)
	if len(notes) > 0 {
		out = strings.Join(notes, "\n") + "\n\n" + out
	}
	return types.Result{Output: out}
}

func formatFetched(origin, via, title, body string, maxChars, offset int, sourceCut bool) string {
	page, start, end, total := paginateText(strings.TrimSpace(body), offset, maxChars)
	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\nvia: %s\n", origin, via)
	if title != "" {
		fmt.Fprintf(&b, "title: %s\n", title)
	}
	b.WriteByte('\n')
	if start >= total && total > 0 {
		fmt.Fprintf(&b, "(offset %d is past the end of the page, which has %d characters)", offset, total)
		return b.String()
	}
	b.WriteString(page)
	switch {
	case end < total:
		suffix := ""
		if sourceCut {
			suffix = "+"
		}
		fmt.Fprintf(&b, "\n… [characters %d-%d of %d%s; call web_fetch again with offset=%d for the next part]", start, end, total, suffix, end)
	case sourceCut:
		fmt.Fprintf(&b, "\n… [page cut at %d characters by the reader; call web_fetch with offset=%d for more]", total, total)
	case start > 0:
		fmt.Fprintf(&b, "\n[end of page: characters %d-%d of %d]", start, end, total)
	}
	return b.String()
}

func directFetch(ctx context.Context, rawURL, selector string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("bad url: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AwakiCloudBot/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain")

	resp, err := safeHTTPClient(30*time.Second).Do(req)
	if err != nil {
		return "", fmt.Errorf("http error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/plain") || strings.Contains(ct, "application/json") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBody))
		if err != nil {
			return "", err
		}
		return string(body), nil
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxFetchBody))
	if err != nil {
		return "", fmt.Errorf("html parse: %v", err)
	}
	doc.Find("script, style, noscript, svg, nav, footer, header").Remove()

	var root *goquery.Selection
	if selector != "" {
		root = doc.Find(selector).First()
	}
	if root == nil || root.Length() == 0 {
		for _, sel := range defaultSelectors {
			root = doc.Find(sel).First()
			if root.Length() > 0 {
				break
			}
		}
	}
	if root == nil || root.Length() == 0 {
		root = doc.Find("body")
	}
	text := strings.TrimSpace(root.Text())
	// collapse whitespace
	fields := strings.Fields(text)
	text = strings.Join(fields, " ")
	if text == "" {
		return "", fmt.Errorf("no extractable text")
	}
	return text, nil
}
