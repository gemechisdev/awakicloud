package system

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

const (
	grepTimeout        = 45 * time.Second
	grepDefaultMax     = 40
	grepHardMax        = 200
	grepMaxLineRunes   = 240
	grepMaxOutputRunes = 12000
)

// GrepTool is a flexible, token-efficient content search (rg preferred, grep fallback).
func GrepTool() types.Tool {
	return types.Tool{
		Name:  "grep",
		Label: "Grepping",
		Description: "Search file contents with a flexible regex (or fixed string). Prefer this over bash grep/rg — " +
			"output is compacted for low token use. Supports include/exclude globs, context lines, case fold, " +
			"word match, output modes (content|files|count), and head limits. Uses ripgrep when available.",
		DetailFunc: func(input map[string]any) string {
			p, _ := input["pattern"].(string)
			if p == "" {
				p, _ = input["query"].(string)
			}
			return p
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Search pattern (regex by default). Alias: query.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Alias for pattern",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "File or directory to search (default: workspace root or .)",
				},
				"include": map[string]any{
					"description": "Glob(s) to include, e.g. \"*.go\" or [\"*.ts\",\"*.tsx\"]",
				},
				"exclude": map[string]any{
					"description": "Glob(s) to exclude, e.g. \"vendor/**\" or [\"node_modules\",\"dist\"]",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "ripgrep file type shorthand: go, py, js, ts, rust, java, …",
				},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Case-insensitive match (-i). Default false.",
				},
				"fixed_string": map[string]any{
					"type":        "boolean",
					"description": "Treat pattern as literal string (-F), not regex.",
				},
				"word": map[string]any{
					"type":        "boolean",
					"description": "Match whole words only (-w).",
				},
				"multiline": map[string]any{
					"type":        "boolean",
					"description": "Allow matches to span lines (rg -U). Ignored on plain grep.",
				},
				"context": map[string]any{
					"type":        "integer",
					"description": "Lines of context before and after each match (-C).",
				},
				"before": map[string]any{
					"type":        "integer",
					"description": "Context lines before match (-B).",
				},
				"after": map[string]any{
					"type":        "integer",
					"description": "Context lines after match (-A).",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Max match lines / files / counts to return (default 40, hard cap 200).",
				},
				"output_mode": map[string]any{
					"type":        "string",
					"enum":        []string{"content", "files", "count"},
					"description": "content = path:line:text (default); files = paths only; count = path:n",
				},
				"hidden": map[string]any{
					"type":        "boolean",
					"description": "Include hidden files/dirs (default false for rg).",
				},
				"max_filesize": map[string]any{
					"type":        "string",
					"description": "Skip files larger than this (rg), e.g. \"1M\", \"512K\".",
				},
			},
		},
		Required:     []string{}, // pattern or query required in handler
		ParallelSafe: true,
		Handler:      grepHandler,
	}
}

func grepHandler(ctx context.Context, input map[string]any) types.Result {
	pattern := strings.TrimSpace(types.GetStringOpt(input, "pattern"))
	if pattern == "" {
		pattern = strings.TrimSpace(types.GetStringOpt(input, "query"))
	}
	if pattern == "" {
		return types.Errf("pattern (or query) is required")
	}

	path := strings.TrimSpace(types.GetStringOpt(input, "path"))
	if path == "" {
		if root := workspace.Root(); root != "" {
			path = root
		} else {
			path = "."
		}
	} else {
		path = workspace.Resolve(path) // relative paths mean "relative to the workspace", like every other file tool
	}

	maxResults := grepDefaultMax
	if v, ok := input["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
	}
	if maxResults > grepHardMax {
		maxResults = grepHardMax
	}

	mode := strings.ToLower(strings.TrimSpace(types.GetStringOpt(input, "output_mode")))
	if mode == "" {
		mode = "content"
	}
	switch mode {
	case "content", "files", "count", "files_with_matches":
		if mode == "files_with_matches" {
			mode = "files"
		}
	default:
		return types.Errf("output_mode must be content|files|count")
	}

	opts := grepOpts{
		Pattern:         pattern,
		Path:            path,
		Include:         stringOrSlice(input["include"]),
		Exclude:         stringOrSlice(input["exclude"]),
		Type:            types.GetStringOpt(input, "type"),
		CaseInsensitive: boolOpt(input, "case_insensitive"),
		FixedString:     boolOpt(input, "fixed_string"),
		Word:            boolOpt(input, "word"),
		Multiline:       boolOpt(input, "multiline"),
		Context:         intOpt(input, "context"),
		Before:          intOpt(input, "before"),
		After:           intOpt(input, "after"),
		MaxResults:      maxResults,
		Mode:            mode,
		Hidden:          boolOpt(input, "hidden"),
		MaxFilesize:     types.GetStringOpt(input, "max_filesize"),
	}

	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()

	out, bin, err := runGrep(ctx, opts)
	if err != nil && out == "" {
		if ctx.Err() == context.DeadlineExceeded {
			return types.Errf("grep timed out after %s", grepTimeout)
		}
		return types.Errf("grep failed (%s): %v", bin, err)
	}

	formatted, truncated := formatGrepOutput(out, opts)
	if formatted == "" {
		return types.Result{Output: "no matches"}
	}
	if truncated {
		formatted += fmt.Sprintf("\n… truncated (max_results=%d). Narrow path/include or raise max_results.", opts.MaxResults)
	}
	header := fmt.Sprintf("grep via %s · mode=%s · path=%s\n", bin, opts.Mode, shortPath(opts.Path))
	return types.Result{Output: header + formatted}
}

type grepOpts struct {
	Pattern         string
	Path            string
	Include         []string
	Exclude         []string
	Type            string
	CaseInsensitive bool
	FixedString     bool
	Word            bool
	Multiline       bool
	Context         int
	Before          int
	After           int
	MaxResults      int
	Mode            string
	Hidden          bool
	MaxFilesize     string
}

func runGrep(ctx context.Context, o grepOpts) (output, bin string, err error) {
	if path, e := exec.LookPath("rg"); e == nil {
		args := buildGrepRgArgs(o)
		cmd := exec.CommandContext(ctx, path, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		runErr := cmd.Run()
		out := strings.TrimSpace(buf.String())
		// rg exit 1 = no match
		if runErr != nil && out == "" {
			if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", "rg", nil
			}
			return out, "rg", runErr
		}
		return out, "rg", nil
	}

	args := buildGNUGrepArgs(o)
	cmd := exec.CommandContext(ctx, "grep", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if runErr != nil && out == "" {
		if ee, ok := runErr.(*exec.ExitError); ok && (ee.ExitCode() == 1) {
			return "", "grep", nil
		}
		return out, "grep", runErr
	}
	return out, "grep", nil
}

func buildGrepRgArgs(o grepOpts) []string {
	args := []string{
		"--line-number",
		"--color", "never",
		"--no-heading",
		"-m", strconv.Itoa(o.MaxResults),
	}
	switch o.Mode {
	case "files":
		args = []string{"--files-with-matches", "--color", "never", "-m", "1"}
	case "count":
		args = []string{"--count", "--color", "never"}
	}
	if o.CaseInsensitive {
		args = append(args, "-i")
	}
	if o.FixedString {
		args = append(args, "-F")
	}
	if o.Word {
		args = append(args, "-w")
	}
	if o.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	if o.Mode == "content" {
		if o.Context > 0 {
			args = append(args, "-C", strconv.Itoa(o.Context))
		} else {
			if o.Before > 0 {
				args = append(args, "-B", strconv.Itoa(o.Before))
			}
			if o.After > 0 {
				args = append(args, "-A", strconv.Itoa(o.After))
			}
		}
	}
	if o.Hidden {
		args = append(args, "--hidden")
	}
	if o.MaxFilesize != "" {
		args = append(args, "--max-filesize", o.MaxFilesize)
	}
	if o.Type != "" {
		args = append(args, "--type", o.Type)
	}
	for _, g := range o.Include {
		args = append(args, "-g", g)
	}
	for _, g := range o.Exclude {
		if !strings.HasPrefix(g, "!") {
			g = "!" + g
		}
		args = append(args, "-g", g)
	}
	// sensible default ignores for token/noise control
	if len(o.Exclude) == 0 {
		for _, g := range []string{"!node_modules/**", "!vendor/**", "!.git/**", "!dist/**", "!build/**", "!*.min.js", "!*.map"} {
			args = append(args, "-g", g)
		}
	}
	args = append(args, "--", o.Pattern, o.Path)
	return args
}

func buildGNUGrepArgs(o grepOpts) []string {
	args := []string{"-R", "-n", "-I", "-H"}
	if o.CaseInsensitive {
		args = append(args, "-i")
	}
	if o.FixedString {
		args = append(args, "-F")
	} else {
		// ripgrep patterns are extended-style (a|b, x+, \( literal paren); plain grep would read them as basic regex.
		args = append(args, "-E")
	}
	if o.Word {
		args = append(args, "-w")
	}
	if o.Mode == "files" {
		args = append(args, "-l")
	}
	if o.Mode == "count" {
		args = append(args, "-c")
	}
	if o.Mode == "content" {
		if o.Context > 0 {
			args = append(args, "-C", strconv.Itoa(o.Context))
		} else {
			if o.Before > 0 {
				args = append(args, "-B", strconv.Itoa(o.Before))
			}
			if o.After > 0 {
				args = append(args, "-A", strconv.Itoa(o.After))
			}
		}
	}
	for _, g := range o.Include {
		args = append(args, "--include="+g)
	}
	for _, g := range o.Exclude {
		args = append(args, "--exclude="+g, "--exclude-dir="+strings.TrimSuffix(g, "/**"))
	}
	if len(o.Exclude) == 0 {
		for _, d := range []string{"node_modules", "vendor", ".git", "dist", "build"} {
			args = append(args, "--exclude-dir="+d)
		}
	}
	args = append(args, "--", o.Pattern, o.Path)
	return args
}

func formatGrepOutput(raw string, o grepOpts) (string, bool) {
	if raw == "" {
		return "", false
	}
	lines := strings.Split(raw, "\n")
	truncated := false
	if len(lines) > o.MaxResults {
		lines = lines[:o.MaxResults]
		truncated = true
	}
	var b strings.Builder
	totalRunes := 0
	for i, line := range lines {
		line = compactGrepLine(line)
		r := []rune(line)
		if len(r) > grepMaxLineRunes {
			line = string(r[:grepMaxLineRunes]) + "…"
		}
		if totalRunes+len([]rune(line)) > grepMaxOutputRunes {
			truncated = true
			b.WriteString(fmt.Sprintf("… output size cap (%d more lines omitted)\n", len(lines)-i))
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		totalRunes += len([]rune(line)) + 1
	}
	return strings.TrimRight(b.String(), "\n"), truncated
}

func compactGrepLine(line string) string {
	// Collapse excessive spaces; keep path:line structure.
	return strings.TrimRight(line, "\r")
}

func shortPath(p string) string {
	if root := workspace.Root(); root != "" {
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}

func stringOrSlice(v any) []string {
	switch x := v.(type) {
	case string:
		x = strings.TrimSpace(x)
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return x
	default:
		return nil
	}
}

func boolOpt(input map[string]any, key string) bool {
	v, ok := input[key]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true") || b == "1"
	default:
		return false
	}
}

func intOpt(input map[string]any, key string) int {
	v, ok := input[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return 0
}
