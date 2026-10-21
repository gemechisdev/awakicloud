package web

import (
	"context"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
)

// WebSearchTool searches the web (Parallel MCP → DuckDuckGo).
func WebSearchTool() types.Tool {
	return types.Tool{
		Name:  "web_search",
		Label: "Searching the web",
		Description: "Search the web for current information. Uses Parallel Search MCP first (keyless), " +
			"then DuckDuckGo as fallback. Returns titles, URLs, and snippets.",
		DetailFunc: func(input map[string]any) string {
			s, _ := input["query"].(string)
			return s
		},
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 5, max 10)",
			},
		},
		Required: []string{"query"},
		Handler:  webSearch,
	}
}

func webSearch(ctx context.Context, input map[string]any) types.Result {
	query, err := types.GetString(input, "query")
	if err != nil {
		return types.ErrResult(err)
	}
	limit := 5
	if v, ok := input["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 10 {
			limit = 10
		}
	}

	var notes []string

	// 1) Parallel MCP (keyless)
	hits, err := parallelSearch(ctx, query, limit)
	if err == nil && len(hits) > 0 {
		logWeb("parallel", "web_search ok", nil)
		return types.Result{Output: formatHits(hits, "parallel")}
	}
	if err != nil {
		notes = append(notes, "Parallel: "+err.Error())
		logWeb("parallel", "web_search", err)
	}

	// 2) DuckDuckGo HTML (keyless)
	hits, err = searchDuckDuckGo(ctx, query, limit)
	if err == nil && len(hits) > 0 {
		logWeb("duckduckgo", "web_search ok", nil)
		out := formatHits(hits, "duckduckgo")
		if len(notes) > 0 {
			out = strings.Join(notes, "\n") + "\n\n" + out
		}
		return types.Result{Output: out}
	}
	if err != nil {
		notes = append(notes, "DuckDuckGo: "+err.Error())
		logWeb("duckduckgo", "web_search", err)
	}

	return types.Errf("web_search failed:\n%s", strings.Join(notes, "\n"))
}

