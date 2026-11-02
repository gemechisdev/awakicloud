package memory

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/tools/types"
)

const (
	maxMemoryBytes    = 256 * 1024
	previewRunes      = 80
	defaultSearchHits = 20
	maxSearchHits     = 100
)

// nameLocks serialises read-modify-write on one memory file so two tool calls
// (parallel tools, sub-agents, background tasks) cannot lose each other's update.
var nameLocks sync.Map

func lockName(file string) func() {
	key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(file), ".md"))
	v, _ := nameLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// preview is the first meaningful line of a file (heading markers stripped).
func preview(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>-* "))
		if line != "" && line != "---" {
			return clip(line, previewRunes)
		}
	}
	return ""
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// suggestNames lists memory files whose names resemble the requested one.
func suggestNames(st store.Store, want string) string {
	files, err := st.ListMemory()
	if err != nil || len(files) == 0 {
		return ""
	}
	stem := strings.ToLower(strings.TrimSuffix(want, ".md"))
	var close, all []string
	for _, f := range files {
		all = append(all, f.Name)
		n := strings.ToLower(strings.TrimSuffix(f.Name, ".md"))
		if stem != "" && (strings.Contains(n, stem) || strings.Contains(stem, n)) {
			close = append(close, f.Name)
		}
	}
	sort.Strings(all)
	if len(close) > 0 {
		return "\nDid you mean: " + strings.Join(close, ", ")
	}
	if len(all) > 15 {
		all = all[:15]
	}
	return "\nAvailable files: " + strings.Join(all, ", ")
}

func fileParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func MemoryTools(st store.Store) []types.Tool {
	return []types.Tool{
		{
			Name:  "memory_list",
			Label: "Listing memories",
			Description: "List all files in your persistent memory with a one-line preview of each. Memory survives across all conversations. " +
				"Check this at the start of each conversation to recall what you know; use memory_search to find a detail without opening every file.",
			Parameters:   map[string]any{},
			ParallelSafe: true,
			Handler: func(_ context.Context, _ map[string]any) types.Result {
				files, err := st.ListMemory()
				if err != nil {
					return types.Errf("error listing memory: %v", err)
				}
				if len(files) == 0 {
					return types.Result{Output: "(no memory files yet)"}
				}
				sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
				var b strings.Builder
				for _, f := range files {
					fmt.Fprintf(&b, "%s  (%d bytes)", f.Name, f.Size)
					if p := preview(f.Content); p != "" {
						fmt.Fprintf(&b, "  — %s", p)
					}
					b.WriteByte('\n')
				}
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:         "memory_read",
			Label:        "Checking memory",
			Description:  "Read a specific memory file. Your memory is organized as markdown files by topic.",
			Parameters:   map[string]any{"file": fileParam("Name of the memory file to read, e.g. owner.md")},
			Required:     []string{"file"},
			ParallelSafe: true,
			Handler: func(_ context.Context, input map[string]any) types.Result {
				file, err := types.GetString(input, "file")
				if err != nil {
					return types.ErrResult(err)
				}
				content, err := st.ReadMemory(file)
				if err != nil {
					msg := err.Error()
					if isNotFound(err) {
						msg += suggestNames(st, file)
					}
					return types.Result{Output: msg, IsErr: true}
				}
				return types.TruncateOutput(content)
			},
		},
		{
			Name:  "memory_search",
			Label: "Searching memory",
			Description: "Search all memory files for text, without opening each one. A multi-word query matches lines containing ALL of the words (case-insensitive); " +
				"regex=true treats the query as a regular expression. Returns file, line number and the matching line.",
			Parameters: map[string]any{
				"query":       fileParam("Words to look for (or a regex when regex=true)"),
				"regex":       map[string]any{"type": "boolean", "description": "Treat query as a regular expression (default false)"},
				"max_results": map[string]any{"type": "integer", "description": "Max matching lines to return (default 20, max 100)"},
			},
			Required:     []string{"query"},
			ParallelSafe: true,
			Handler: func(_ context.Context, input map[string]any) types.Result {
				query, err := types.GetString(input, "query")
				if err != nil {
					return types.ErrResult(err)
				}
				max, ok := types.GetIntOpt(input, "max_results")
				if !ok || max < 1 {
					max = defaultSearchHits
				}
				if max > maxSearchHits {
					max = maxSearchHits
				}
				var match func(line string) bool
				if types.GetBool(input, "regex") {
					re, err := regexp.Compile("(?i)" + query)
					if err != nil {
						return types.Errf("invalid regex: %v", err)
					}
					match = re.MatchString
				} else {
					terms := strings.Fields(strings.ToLower(query))
					match = func(line string) bool {
						l := strings.ToLower(line)
						for _, t := range terms {
							if !strings.Contains(l, t) {
								return false
							}
						}
						return true
					}
				}
				files, err := st.ListMemory()
				if err != nil {
					return types.Errf("error searching memory: %v", err)
				}
				sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
				var b strings.Builder
				shown, total := 0, 0
				lastFile := ""
				for _, f := range files {
					for i, line := range strings.Split(f.Content, "\n") {
						if strings.TrimSpace(line) == "" || !match(line) {
							continue
						}
						total++
						if shown >= max {
							continue
						}
						if f.Name != lastFile {
							fmt.Fprintf(&b, "%s\n", f.Name)
							lastFile = f.Name
						}
						fmt.Fprintf(&b, "  %d: %s\n", i+1, clip(strings.TrimSpace(line), 200))
						shown++
					}
				}
				if total == 0 {
					return types.Result{Output: fmt.Sprintf("No matches for %q in %d memory file(s).", query, len(files))}
				}
				if total > shown {
					fmt.Fprintf(&b, "… %d more matching line(s); narrow the query or raise max_results\n", total-shown)
				}
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:  "memory_write",
			Label: "Saving to memory",
			Description: "Write or REPLACE a whole memory file. Organize knowledge into topic files (e.g. owner.md, server.md, tasks.md). " +
				"To add a note without rewriting the file use memory_append; when rewriting, read the file first so nothing is lost.",
			Parameters: map[string]any{
				"file":    fileParam("Name of the memory file, e.g. owner.md"),
				"content": fileParam("The full content for this memory file in markdown format"),
			},
			Required: []string{"file", "content"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				file, err := types.GetString(input, "file")
				if err != nil {
					return types.ErrResult(err)
				}
				content, err := types.GetString(input, "content")
				if err != nil {
					return types.ErrResult(err)
				}
				if len(content) > maxMemoryBytes {
					return types.Errf("content is %d bytes; a memory file is limited to %d. Split it into several topic files.", len(content), maxMemoryBytes)
				}
				unlock := lockName(file)
				defer unlock()
				old, readErr := st.ReadMemory(file)
				if err := st.WriteMemory(file, content); err != nil {
					return types.Errf("error writing memory: %v", err)
				}
				if readErr != nil {
					return types.Result{Output: fmt.Sprintf("memory file %q created (%d bytes)", file, len(content))}
				}
				return types.Result{Output: fmt.Sprintf("memory file %q updated (%d → %d bytes)", file, len(old), len(content))}
			},
		},
		{
			Name:  "memory_append",
			Label: "Adding to memory",
			Description: "Append text to the end of a memory file (created if missing) without rewriting it — safe against losing existing notes. " +
				"Use for adding a fact, decision or log line.",
			Parameters: map[string]any{
				"file":    fileParam("Name of the memory file, e.g. owner.md"),
				"content": fileParam("Text to add at the end"),
			},
			Required: []string{"file", "content"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				file, err := types.GetString(input, "file")
				if err != nil {
					return types.ErrResult(err)
				}
				add, err := types.GetString(input, "content")
				if err != nil {
					return types.ErrResult(err)
				}
				unlock := lockName(file)
				defer unlock()
				old, readErr := st.ReadMemory(file)
				if readErr != nil && !isNotFound(readErr) {
					return types.Errf("error reading memory: %v", readErr)
				}
				merged := add
				if readErr == nil && old != "" {
					sep := ""
					if !strings.HasSuffix(old, "\n") {
						sep = "\n"
					}
					merged = old + sep + add
				}
				if !strings.HasSuffix(merged, "\n") {
					merged += "\n"
				}
				if len(merged) > maxMemoryBytes {
					return types.Errf("file would be %d bytes; the limit is %d. Move older notes into another file (memory_write) first.", len(merged), maxMemoryBytes)
				}
				if err := st.WriteMemory(file, merged); err != nil {
					return types.Errf("error writing memory: %v", err)
				}
				verb := "appended to"
				if readErr != nil {
					verb = "created"
				}
				return types.Result{Output: fmt.Sprintf("memory file %q %s (%d bytes total)", file, verb, len(merged))}
			},
		},
		{
			Name:        "memory_delete",
			Label:       "Deleting memory",
			Description: "Delete a memory file that is no longer needed. Use to prune outdated or irrelevant information.",
			Parameters:  map[string]any{"file": fileParam("Name of the memory file to delete, e.g. old_tasks.md")},
			Required:    []string{"file"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				file, err := types.GetString(input, "file")
				if err != nil {
					return types.ErrResult(err)
				}
				unlock := lockName(file)
				defer unlock()
				if err := st.DeleteMemory(file); err != nil {
					return types.Errf("error deleting memory: %v", err)
				}
				return types.Result{Output: fmt.Sprintf("memory file %q deleted", file)}
			},
		},
	}
}
