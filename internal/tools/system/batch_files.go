package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
)

const (
	maxGlobResults   = 200
	maxReadFiles     = 20
	maxReadFileBytes = 100 * 1024
	maxTreeDepth     = 6
	maxTreeEntries   = 300
)

// BatchFileTools ports high-value Awaki Android batch filesystem tools.
func BatchFileTools() []types.Tool {
	return []types.Tool{
		{
			Name:        "glob_files",
			Label:       "Glob files",
			Description: "Find files matching a glob pattern under a root directory (e.g. **/*.go, src/**/*.ts).",
			Parameters: map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern relative to root"},
				"root":    map[string]any{"type": "string", "description": "Root directory (default: cwd)"},
				"max":     map[string]any{"type": "integer", "description": "Max results (default 200)"},
			},
			Required: []string{"pattern"},
			Handler:  globFiles,
		},
		{
			Name:        "directory_tree",
			Label:       "Directory tree",
			Description: "Return a compact directory tree listing under a path.",
			Parameters: map[string]any{
				"path":      map[string]any{"type": "string", "description": "Directory path (default: cwd)"},
				"max_depth": map[string]any{"type": "integer", "description": "Max depth (default 3, max 6)"},
			},
			Handler: directoryTree,
		},
		{
			Name:        "read_files",
			Label:       "Reading files",
			Description: "Read multiple files in one call. Prefer this over many read_file calls.",
			Parameters: map[string]any{
				"paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
					"description": "List of file paths to read",
				},
			},
			Required: []string{"paths"},
			Handler:  readFiles,
		},
		{
			Name:        "file_info",
			Label:       "File info",
			Description: "Stat a path: size, mode, mod time, is_dir.",
			Parameters: map[string]any{
				"path": map[string]any{"type": "string", "description": "File or directory path"},
			},
			Required: []string{"path"},
			Handler:  fileInfo,
		},
	}
}

func globFiles(_ context.Context, input map[string]any) types.Result {
	pattern, err := types.GetString(input, "pattern")
	if err != nil {
		return types.ErrResult(err)
	}
	root := types.GetStringOpt(input, "root")
	if root == "" {
		root, _ = os.Getwd()
	}
	max := maxGlobResults
	if v, ok := input["max"].(float64); ok && v > 0 {
		max = int(v)
		if max > 1000 {
			max = 1000
		}
	}

	// Support ** by walking when pattern contains it
	var matches []string
	if strings.Contains(pattern, "**") {
		suffix := strings.TrimPrefix(pattern, "**/")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			ok, _ := filepath.Match(suffix, filepath.Base(path))
			if !ok {
				ok, _ = filepath.Match(suffix, rel)
			}
			// also try matching against full relative path with simplified **
			if !ok && strings.HasSuffix(pattern, suffix) {
				ok, _ = filepath.Match(strings.ReplaceAll(pattern, "**", "*"), rel)
			}
			if ok {
				matches = append(matches, rel)
			}
			if len(matches) >= max {
				return filepath.SkipAll
			}
			return nil
		})
	} else {
		full := pattern
		if !filepath.IsAbs(pattern) {
			full = filepath.Join(root, pattern)
		}
		found, err := filepath.Glob(full)
		if err != nil {
			return types.Errf("glob: %v", err)
		}
		for _, f := range found {
			rel, err := filepath.Rel(root, f)
			if err != nil {
				rel = f
			}
			matches = append(matches, rel)
			if len(matches) >= max {
				break
			}
		}
	}

	if len(matches) == 0 {
		return types.Result{Output: "(no matches)"}
	}
	return types.TruncateOutput(strings.Join(matches, "\n"))
}

func directoryTree(_ context.Context, input map[string]any) types.Result {
	root := types.GetStringOpt(input, "path")
	if root == "" {
		root, _ = os.Getwd()
	}
	maxDepth := 3
	if v, ok := input["max_depth"].(float64); ok && v > 0 {
		maxDepth = int(v)
		if maxDepth > maxTreeDepth {
			maxDepth = maxTreeDepth
		}
	}

	var b strings.Builder
	count := 0
	var walk func(dir string, prefix string, depth int)
	walk = func(dir string, prefix string, depth int) {
		if count >= maxTreeEntries || depth > maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			b.WriteString(prefix + "… error: " + err.Error() + "\n")
			return
		}
		for i, e := range entries {
			name := e.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".awaki" {
				continue
			}
			count++
			if count > maxTreeEntries {
				b.WriteString(prefix + "… (truncated)\n")
				return
			}
			connector := "├── "
			nextPrefix := prefix + "│   "
			if i == len(entries)-1 {
				connector = "└── "
				nextPrefix = prefix + "    "
			}
			if e.IsDir() {
				b.WriteString(prefix + connector + name + "/\n")
				walk(filepath.Join(dir, name), nextPrefix, depth+1)
			} else {
				b.WriteString(prefix + connector + name + "\n")
			}
		}
	}
	b.WriteString(root + "\n")
	walk(root, "", 1)
	return types.TruncateOutput(b.String())
}

func readFiles(_ context.Context, input map[string]any) types.Result {
	raw, ok := input["paths"].([]any)
	if !ok || len(raw) == 0 {
		return types.Errf("paths is required")
	}
	if len(raw) > maxReadFiles {
		return types.Errf("too many paths (max %d)", maxReadFiles)
	}
	var b strings.Builder
	for _, item := range raw {
		path, _ := item.(string)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		b.WriteString("===== " + path + " =====\n")
		if err != nil {
			b.WriteString(fmt.Sprintf("ERROR: %v\n\n", err))
			continue
		}
		if len(data) > maxReadFileBytes {
			b.WriteString(string(data[:maxReadFileBytes]))
			b.WriteString("\n... (truncated)\n\n")
		} else {
			b.Write(data)
			if !strings.HasSuffix(string(data), "\n") {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
	}
	return types.TruncateOutput(b.String())
}

func fileInfo(_ context.Context, input map[string]any) types.Result {
	path, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return types.Errf("stat: %v", err)
	}
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	return types.Result{Output: fmt.Sprintf(
		"path: %s\nkind: %s\nsize: %d\nmode: %s\nmod: %s",
		path, kind, info.Size(), info.Mode(), info.ModTime().Format(timeRFC3339),
	)}
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
