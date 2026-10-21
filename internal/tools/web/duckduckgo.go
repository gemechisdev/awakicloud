package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const ddgTimeout = 15 * time.Second

// searchDuckDuckGo scrapes the keyless HTML results page (Awaki WebGateway style).
func searchDuckDuckGo(ctx context.Context, query string, limit int) ([]webHit, error) {
	ctx, cancel := context.WithTimeout(ctx, ddgTimeout)
	defer cancel()

	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AwakiCloudBot/1.0)")
	req.Header.Set("Accept", "text/html")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo connection: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo HTTP %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo parse: %w", err)
	}

	var hits []webHit
	doc.Find("div.result, div.results_links, div.web-result").Each(func(_ int, s *goquery.Selection) {
		if len(hits) >= limit {
			return
		}
		a := s.Find("a.result__a").First()
		if a.Length() == 0 {
			a = s.Find("a").First()
		}
		href, ok := a.Attr("href")
		if !ok || href == "" {
			return
		}
		href = unwrapDDGRedirect(href)
		if !strings.HasPrefix(href, "http") {
			return
		}
		title := strings.TrimSpace(a.Text())
		snippet := strings.TrimSpace(s.Find("a.result__snippet, div.result__snippet, td.result-snippet").First().Text())
		if title == "" {
			title = hostOf(href)
		}
		hits = append(hits, webHit{Title: title, URL: href, Snippet: truncate(snippet, 280)})
	})

	// Fallback: any result links
	if len(hits) == 0 {
		doc.Find("a.result__a").Each(func(_ int, a *goquery.Selection) {
			if len(hits) >= limit {
				return
			}
			href, _ := a.Attr("href")
			href = unwrapDDGRedirect(href)
			if !strings.HasPrefix(href, "http") {
				return
			}
			hits = append(hits, webHit{
				Title: strings.TrimSpace(a.Text()),
				URL:   href,
			})
		})
	}

	if len(hits) == 0 {
		return nil, fmt.Errorf("duckduckgo returned no readable results")
	}
	return hits, nil
}

var ddgUDDG = regexp.MustCompile(`[?&]uddg=([^&]+)`)

func unwrapDDGRedirect(href string) string {
	href = strings.TrimSpace(href)
	if strings.Contains(href, "uddg=") {
		if m := ddgUDDG.FindStringSubmatch(href); len(m) == 2 {
			if decoded, err := url.QueryUnescape(m[1]); err == nil && strings.HasPrefix(decoded, "http") {
				return decoded
			}
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}
