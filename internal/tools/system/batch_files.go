package system

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

const (
	defaultGlobResults = 200
	maxGlobResults     = 2000
	globCollectCap     = 5000
	walkEntryCap       = 300000
	maxReadFiles       = 20
	readFilesMaxLines  = 400
	maxTreeDepth       = 8
	treePerDirCap      = 60
	treeTotalCap       = 1500
)

// BatchFileTools: discovery and multi-file reading tools.
func BatchFileTools() []types.Tool {
	return []types.Tool{
		{
			Name:  "glob_files",
			Label: "Glob files",
			Description: "Find files by glob pattern under a root (supports **, *, ?, [a-z], {a,b}). " +
				"A pattern without '/' (e.g. *.go) matches at any depth; src/**/*.ts matches src/a.ts and src/x/y/b.ts. " +
				"node_modules, .git and anything in .gitignore are skipped unless include_ignored=true. Results are relative to root.",
			Parameters: map[string]any{
				"pattern":         map[string]any{"type": "string", "description": "Glob pattern relative to root"},
				"root":            map[string]any{"type": "string", "description": "Root directory (default: workspace root)"},
				"max":             map[string]any{"type": "integer", "description": "Max results (default 200, max 2000)"},
				"sort":            map[string]any{"type": "string", "enum": []string{"path", "mtime"}, "description": "path (default) or mtime (newest first)"},
				"include_dirs":    map[string]any{"type": "boolean", "description": "Also return matching directories (default false)"},
				"include_ignored": map[string]any{"type": "boolean", "description": "Search ignored dirs such as node_modules and .git (default false)"},
			},
			Required: []string{"pattern"},
			Handler:  globFiles,
		},
		{
			Name:  "directory_tree",
			Label: "Directory tree",
			Description: "Compact directory tree (dirs first). Skips node_modules/.git/.gitignore'd paths unless include_ignored=true. " +
				"Directories beyond max_depth show how many entries they hold; very large directories are summarised.",
			Parameters: map[string]any{
				"path":            map[string]any{"type": "string", "description": "Directory path (default: workspace root)"},
				"max_depth":       map[string]any{"type": "integer", "description": "Max depth (default 3, max 8)"},
				"include_ignored": map[string]any{"type": "boolean", "description": "Include ignored/noisy directories (default false)"},
			},
			Handler: directoryTree,
		},
		{
			Name:  "read_files",
			Label: "Reading files",
			Description: "Read several text files in one call (up to 20). Each file gets an equal share of the ~50 KB budget and at most max_lines lines; " +
				"truncated files end with the offset to continue with read_file.",
			Parameters: map[string]any{
				"paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "List of file paths to read",
				},
				"max_lines":    map[string]any{"type": "integer", "description": "Max lines per file (default 400)"},
				"line_numbers": map[string]any{"type": "boolean", "description": "Prefix lines with line numbers (default false)"},
			},
			Required: []string{"paths"},
			Handler:  readFiles,
		},
		{
			Name:        "file_info",
			Label:       "File info",
			Description: "Stat a path: kind, size, mode, mod time, symlink target; for text files also line count and whether it is binary.",
			Parameters: map[string]any{
				"path": map[string]any{"type": "string", "description": "File or directory path"},
			},
			Required: []string{"path"},
			Handler:  fileInfo,
		},
	}
}

func defaultRoot() string {
	if r := workspace.Root(); r != "" {
		return r
	}
	return "."
}

// ------------------------------------------------------------------- glob

type globHit struct {
	rel   string
	mtime time.Time
}

func globFiles(ctx context.Context, input map[string]any) types.Result {
	pattern, err := types.GetString(input, "pattern")
	if err != nil {
		return types.ErrResult(err)
	}
	root := types.GetStringOpt(input, "root")
	if root == "" {
		root = defaultRoot()
	}
	root = workspace.Resolve(root)
	max, ok := types.GetIntOpt(input, "max")
	if !ok || max < 1 {
		max = defaultGlobResults
	}
	if max > maxGlobResults {
		max = maxGlobResults
	}
	byMtime := types.GetStringOpt(input, "sort") == "mtime"
	includeDirs := types.GetBoolDefault(input, "include_dirs", false)
	respect := !types.GetBoolDefault(input, "include_ignored", false)

	pat := filepath.ToSlash(strings.TrimSpace(pattern))
	// An absolute pattern inside root becomes relative to it.
	if filepath.IsAbs(pattern) {
		if rel, err := filepath.Rel(root, pattern); err == nil && !strings.HasPrefix(rel, "..") {
			pat = filepath.ToSlash(rel)
		}
	}
	pat = strings.TrimPrefix(strings.TrimPrefix(pat, "./"), "/")
	if pat == "" {
		return types.Errf("pattern is empty")
	}
	if !strings.Contains(pat, "/") {
		pat = "**/" + pat // like ripgrep -g / .gitignore: bare names match at any depth
	}
	re, err := compileGlob(pat)
	if err != nil {
		return types.Errf("invalid glob %q: %v", pattern, err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return types.Errf("root is not a directory: %s", root)
	}

	var hits []globHit
	total, visited := 0, 0
	truncatedWalk := false

	var walk func(dirAbs, relDir string, stack ignoreStack)
	walk = func(dirAbs, relDir string, stack ignoreStack) {
		if ctx.Err() != nil || truncatedWalk {
			return
		}
		entries, st, err := readFiltered(dirAbs, relDir, stack, respect)
		if err != nil {
			return
		}
		for _, e := range entries {
			visited++
			if visited > walkEntryCap || ctx.Err() != nil {
				truncatedWalk = true
				return
			}
			rel := e.Name()
			if relDir != "" {
				rel = relDir + "/" + e.Name()
			}
			isDir := e.IsDir()
			if (!isDir || includeDirs) && re.MatchString(rel) {
				total++
				if len(hits) < globCollectCap {
					h := globHit{rel: rel}
					if byMtime {
						if info, err := e.Info(); err == nil {
							h.mtime = info.ModTime()
						}
					}
					hits = append(hits, h)
				}
			}
			if isDir {
				walk(filepath.Join(dirAbs, e.Name()), rel, st)
			}
		}
	}
	walk(root, "", nil)

	if total == 0 {
		msg := fmt.Sprintf("No files match %q under %s", pattern, root)
		if respect {
			msg += " (node_modules, .git and .gitignore'd paths are skipped; use include_ignored=true to include them)"
		}
		return types.Result{Output: msg}
	}
	if byMtime {
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
	} else {
		sort.Slice(hits, func(i, j int) bool { return hits[i].rel < hits[j].rel })
	}
	shown := hits
	if len(shown) > max {
		shown = shown[:max]
	}
	var b strings.Builder
	for _, h := range shown {
		b.WriteString(h.rel + "\n")
	}
	if total > len(shown) {
		fmt.Fprintf(&b, "… showing %d of %d matches — narrow the pattern or raise max\n", len(shown), total)
	}
	if truncatedWalk {
		b.WriteString("… search stopped early (too many entries or cancelled); results may be incomplete\n")
	}
	return types.TruncateOutput(b.String())
}

// ------------------------------------------------------------------- tree

func directoryTree(ctx context.Context, input map[string]any) types.Result {
	path := types.GetStringOpt(input, "path")
	if path == "" {
		path = defaultRoot()
	}
	path = workspace.Resolve(path)
	depth, ok := types.GetIntOpt(input, "max_depth")
	if !ok || depth < 1 {
		depth = 3
	}
	if depth > maxTreeDepth {
		depth = maxTreeDepth
	}
	respect := !types.GetBoolDefault(input, "include_ignored", false)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return types.Errf("not a directory: %s", path)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s/\n", filepath.Base(path))
	lines := 0
	var rec func(dirAbs, relDir string, stack ignoreStack, level int)
	rec = func(dirAbs, relDir string, stack ignoreStack, level int) {
		entries, st, err := readFiltered(dirAbs, relDir, stack, respect)
		if err != nil {
			return
		}
		indent := strings.Repeat("  ", level)
		shown := 0
		for _, e := range entries {
			if ctx.Err() != nil || lines >= treeTotalCap {
				return
			}
			if shown >= treePerDirCap {
				fmt.Fprintf(&b, "%s… %d more entries\n", indent, len(entries)-shown)
				lines++
				return
			}
			shown++
			lines++
			rel := e.Name()
			if relDir != "" {
				rel = relDir + "/" + e.Name()
			}
			if !e.IsDir() {
				fmt.Fprintf(&b, "%s%s\n", indent, e.Name())
				continue
			}
			sub := filepath.Join(dirAbs, e.Name())
			if level >= depth {
				n := 0
				if kids, _, err := readFiltered(sub, rel, st, respect); err == nil {
					n = len(kids)
				}
				if n == 0 {
					fmt.Fprintf(&b, "%s%s/\n", indent, e.Name())
				} else {
					fmt.Fprintf(&b, "%s%s/  [%d entries]\n", indent, e.Name(), n)
				}
				continue
			}
			fmt.Fprintf(&b, "%s%s/\n", indent, e.Name())
			rec(sub, rel, st, level+1)
		}
	}
	rec(path, "", nil, 1)
	if lines >= treeTotalCap {
		b.WriteString("… tree truncated; pass a deeper path or lower max_depth\n")
	}
	return types.TruncateOutput(b.String())
}

// ------------------------------------------------------------- read_files

func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}

func readFiles(_ context.Context, input map[string]any) types.Result {
	paths := stringList(input["paths"])
	if len(paths) == 0 {
		return types.Errf("paths is required (array of file paths)")
	}
	if len(paths) > maxReadFiles {
		return types.Errf("too many files (%d); max %d per call", len(paths), maxReadFiles)
	}
	maxLines, ok := types.GetIntOpt(input, "max_lines")
	if !ok || maxLines < 1 {
		maxLines = readFilesMaxLines
	}
	numbers := types.GetBoolDefault(input, "line_numbers", false)

	budget := (types.MaxOutputSize - 400*len(paths)/4) / len(paths)
	if budget < 3000 {
		budget = 3000
	}

	var b strings.Builder
	for _, p := range paths {
		abs := workspace.Resolve(p)
		fmt.Fprintf(&b, "===== %s", p)
		f, err := os.Open(abs)
		if err != nil {
			fmt.Fprintf(&b, " =====\nerror: %v\n\n", err)
			continue
		}
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			f.Close()
			b.WriteString(" =====\nerror: not a regular file (is it a directory?)\n\n")
			continue
		}
		head := make([]byte, 8000)
		n, _ := io.ReadFull(f, head)
		if isBinary(head[:n]) {
			f.Close()
			fmt.Fprintf(&b, " =====\n(binary file, %s — skipped)\n\n", humanBytes(info.Size()))
			continue
		}
		_, _ = f.Seek(0, io.SeekStart)
		if info.Size() == 0 {
			f.Close()
			b.WriteString(" =====\n(empty file)\n\n")
			continue
		}
		lr, err := readLineRange(f, 1, maxLines, budget, numbers)
		f.Close()
		if err != nil {
			fmt.Fprintf(&b, " =====\nerror: %v\n\n", err)
			continue
		}
		if lr.more {
			fmt.Fprintf(&b, " (lines %d-%d of %s) =====\n", lr.first, lr.last, fmtTotal(lr))
		} else {
			b.WriteString(" =====\n")
		}
		b.WriteString(lr.text)
		if lr.more {
			fmt.Fprintf(&b, "[… %d more line(s); read_file path=%s offset=%d]\n", lr.total-lr.last, p, lr.nextLine)
		}
		b.WriteString("\n")
	}
	return types.TruncateOutput(b.String())
}

// -------------------------------------------------------------- file_info

func fileInfo(_ context.Context, input map[string]any) types.Result {
	raw, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	path := workspace.Resolve(raw)
	lst, err := os.Lstat(path)
	if err != nil {
		return types.Errf("stat: %v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", path)
	info := lst
	if lst.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		fmt.Fprintf(&b, "kind: symlink\nsymlink_target: %s\n", target)
		if st, err := os.Stat(path); err == nil {
			info = st
		} else {
			b.WriteString("target: missing (dangling symlink)\n")
		}
	}
	switch {
	case info.IsDir():
		if lst.Mode()&os.ModeSymlink == 0 {
			b.WriteString("kind: dir\n")
		}
		if entries, err := os.ReadDir(path); err == nil {
			fmt.Fprintf(&b, "entries: %d\n", len(entries))
		}
	case info.Mode().IsRegular():
		if lst.Mode()&os.ModeSymlink == 0 {
			b.WriteString("kind: file\n")
		}
		fmt.Fprintf(&b, "size: %d (%s)\n", info.Size(), humanBytes(info.Size()))
		if f, err := os.Open(path); err == nil {
			head := make([]byte, 8000)
			n, _ := io.ReadFull(f, head)
			bin := isBinary(head[:n])
			fmt.Fprintf(&b, "binary: %v\n", bin)
			if !bin && info.Size() <= maxEditFileBytes {
				_, _ = f.Seek(0, io.SeekStart)
				if lr, err := readLineRange(f, 1<<30, 1, 1, false); err == nil {
					fmt.Fprintf(&b, "lines: %d\n", lr.total)
				}
			}
			f.Close()
		}
	}
	fmt.Fprintf(&b, "mode: %s\nmod: %s", info.Mode(), info.ModTime().Format(timeRFC3339))
	return types.Result{Output: b.String()}
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
