package system

import (
	"bufio"
	"context"
	"errors"
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
	readDefaultLimit = 2000
	readMaxLimit     = 20000
	readMaxLineBytes = 4000
	readCountCap     = 64 << 20 // stop counting lines after scanning this many bytes
	listDefaultMax   = 500
)

func pathDetail(input map[string]any) string {
	if p, ok := input["path"].(string); ok {
		return filepath.Base(p)
	}
	return ""
}

func FileTools() []types.Tool {
	return []types.Tool{
		{
			Name:  "read_file",
			Label: "Reading file",
			Description: "Read a text file. Large files are paged: output is capped at ~50 KB / 2000 lines per call and ends with the exact offset to continue from. " +
				"Use offset/limit to read a window (e.g. after grep gives a line number). Set line_numbers=true to see line numbers. Binary files are rejected.",
			DetailFunc: pathDetail,
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path, or path relative to the workspace root",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "1-based line to start from (default 1)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max lines to return (default 2000, max 20000)",
				},
				"line_numbers": map[string]any{
					"type":        "boolean",
					"description": "Prefix each line with its line number (default false). Do not include the prefix when copying text into edit_file.",
				},
			},
			Required: []string{"path"},
			Handler:  readFile,
		},
		{
			Name:  "write_file",
			Label: "Writing file",
			Description: "Write the whole content of a file, creating parent directories. Replaces any existing file (the result says so and shows a diff) — " +
				"for changes to an existing file prefer edit_file or apply_patch. Set create_only=true to fail instead of overwriting. " +
				"Writes are atomic and keep the existing file's permissions; new files that start with #! become executable.",
			DetailFunc: pathDetail,
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path, or path relative to the workspace root",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to write",
				},
				"create_only": map[string]any{
					"type":        "boolean",
					"description": "Fail if the file already exists (default false)",
				},
			},
			Required: []string{"path", "content"},
			Handler:  writeFile,
		},
		{
			Name:  "list_files",
			Label: "Listing files",
			Description: "List a directory (directories first, sorted). Shows sizes; details=true adds modified time and mode. " +
				"For a recursive view use directory_tree; to find files by name use glob_files.",
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path (defaults to the workspace root)",
				},
				"details": map[string]any{
					"type":        "boolean",
					"description": "Include modification time and permissions (default false)",
				},
				"hidden": map[string]any{
					"type":        "boolean",
					"description": "Include dotfiles (default true)",
				},
				"max": map[string]any{
					"type":        "integer",
					"description": "Max entries (default 500)",
				},
			},
			Handler: listFiles,
		},
	}
}

// ------------------------------------------------------------------- read

type lineRange struct {
	text       string
	first      int // first line emitted (0 if none)
	last       int
	total      int
	totalExact bool
	more       bool // lines remain after last
	nextLine   int  // line to pass as offset to continue
}

// readLineRange streams r and returns lines [offset, offset+limit) subject to a
// byte budget. It never loads more than one line at a time (+ the budget), so
// it is safe on very large files.
func readLineRange(r io.Reader, offset, limit, maxBytes int, numbers bool) (lineRange, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var out strings.Builder
	lr := lineRange{totalExact: true}
	lineNo := 0
	emitted := 0
	stopped := false // output complete; only count from here on
	scanned := 0

	for !stopped {
		line, err := br.ReadString('\n')
		if len(line) == 0 && err != nil {
			if err != io.EOF {
				return lr, err
			}
			break
		}
		scanned += len(line)
		lineNo++
		if lineNo == 1 {
			line = strings.TrimPrefix(line, utf8BOM)
		}
		if lineNo >= offset {
			text := strings.TrimRight(line, "\r\n")
			if len(text) > readMaxLineBytes {
				text = types.CutHead(text, readMaxLineBytes) + fmt.Sprintf(" … [line clipped, %d bytes]", len(text))
			}
			entry := text + "\n"
			if numbers {
				entry = fmt.Sprintf("%6d  %s", lineNo, entry)
			}
			if emitted >= limit || (emitted > 0 && out.Len()+len(entry) > maxBytes) {
				lr.more = true
				lr.nextLine = lineNo
				stopped = true
				// this line was read but not emitted; it is still counted below
				lr.total = lineNo
				break
			}
			out.WriteString(entry)
			if emitted == 0 {
				lr.first = lineNo
			}
			emitted++
			lr.last = lineNo
		}
		if err == io.EOF {
			break
		}
	}
	lr.text = out.String()
	if !stopped {
		lr.total = lineNo
		return lr, nil
	}

	// Count the remaining lines (cheaply) so the caller can report the total.
	for {
		chunk, err := br.ReadSlice('\n')
		scanned += len(chunk)
		if err == nil {
			lr.total++
		} else if errors.Is(err, bufio.ErrBufferFull) {
			// long line; keep consuming until its newline
		} else {
			if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
				lr.total++
			}
			break
		}
		if scanned > readCountCap {
			lr.totalExact = false
			break
		}
	}
	return lr, nil
}

func fmtTotal(lr lineRange) string {
	if lr.totalExact {
		return fmt.Sprintf("%d", lr.total)
	}
	return fmt.Sprintf("%d+", lr.total)
}

func readFile(_ context.Context, input map[string]any) types.Result {
	path, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	path = workspace.Resolve(path)
	offset, ok := types.GetIntOpt(input, "offset")
	if !ok || offset < 1 {
		offset = 1
	}
	limit, ok := types.GetIntOpt(input, "limit")
	if !ok || limit < 1 {
		limit = readDefaultLimit
	}
	if limit > readMaxLimit {
		limit = readMaxLimit
	}
	numbers := types.GetBoolDefault(input, "line_numbers", false)

	f, err := os.Open(path)
	if err != nil {
		return types.Errf("read error: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return types.Errf("read error: %v", err)
	}
	if info.IsDir() {
		return types.Errf("%s is a directory — use list_files or directory_tree", path)
	}
	head := make([]byte, 8000)
	n, _ := io.ReadFull(f, head)
	if isBinary(head[:n]) {
		return types.Errf("%s looks like a binary file (%d bytes); read_file only handles text. Use file_info, or bash (file, xxd, strings) to inspect it.", path, info.Size())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return types.Errf("read error: %v", err)
	}
	if info.Size() == 0 {
		return types.Result{Output: "(empty file)"}
	}

	lr, err := readLineRange(f, offset, limit, types.MaxOutputSize-600, numbers)
	if err != nil {
		return types.Errf("read error: %v", err)
	}
	if lr.first == 0 {
		return types.Errf("offset %d is past the end of %s (%s lines)", offset, path, fmtTotal(lr))
	}

	partial := offset > 1 || lr.more
	if !partial {
		return types.Result{Output: lr.text}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s lines %d-%d of %s]\n", filepath.Base(path), lr.first, lr.last, fmtTotal(lr))
	b.WriteString(lr.text)
	if lr.more {
		remaining := lr.total - lr.last
		fmt.Fprintf(&b, "[… %d more line(s); continue with offset=%d]", remaining, lr.nextLine)
	}
	return types.Result{Output: b.String()}
}

// ------------------------------------------------------------------ write

func writeFile(_ context.Context, input map[string]any) types.Result {
	rawPath, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	content, hasContent := input["content"].(string)
	if !hasContent {
		return types.Errf("content is required (use an empty string to create an empty file)")
	}
	createOnly := types.GetBoolDefault(input, "create_only", false)

	path := workspace.Resolve(rawPath)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	unlock := lockPath(path)
	defer unlock()

	mode := os.FileMode(0o644)
	if strings.HasPrefix(content, "#!") {
		mode = 0o755
	}
	var oldText string
	existed := false
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return types.Errf("%s is a directory", path)
		}
		if createOnly {
			return types.Errf("%s already exists (create_only=true). Use edit_file / apply_patch to change it, or omit create_only to overwrite.", path)
		}
		existed = true
		mode = info.Mode().Perm()
		if info.Size() <= 1<<20 {
			if data, err := os.ReadFile(path); err == nil && !isBinary(data) {
				oldText = normalizeEOL(strings.TrimPrefix(string(data), utf8BOM))
			}
		}
	} else if !os.IsNotExist(err) {
		return types.Errf("stat error: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return types.Errf("mkdir error: %v", err)
	}
	if err := writeFileAtomic(path, []byte(content), mode); err != nil {
		return types.Errf("write error: %v", err)
	}

	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	if !existed {
		return types.Result{Output: fmt.Sprintf("Created %s (%d bytes, %d lines)", path, len(content), lines)}
	}
	var b strings.Builder
	diff, added, removed := renderDiff(oldText, normalizeEOL(content))
	if diff == "" && oldText != "" && added == 0 && removed == 0 {
		fmt.Fprintf(&b, "Overwrote %s with identical content (%d bytes)", path, len(content))
		return types.Result{Output: b.String()}
	}
	fmt.Fprintf(&b, "OVERWROTE existing %s (%d bytes, %d lines; +%d -%d lines)", path, len(content), lines, added, removed)
	if diff != "" {
		b.WriteString("\n" + types.CutHead(diff, 3000))
	}
	return types.TruncateOutput(b.String())
}

// ------------------------------------------------------------------- list

func listFiles(_ context.Context, input map[string]any) types.Result {
	path := types.GetStringOpt(input, "path")
	if path == "" {
		if root := workspace.Root(); root != "" {
			path = root
		} else {
			path = "."
		}
	}
	path = workspace.Resolve(path)
	details := types.GetBoolDefault(input, "details", false)
	hidden := types.GetBoolDefault(input, "hidden", true)
	max, ok := types.GetIntOpt(input, "max")
	if !ok || max < 1 {
		max = listDefaultMax
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return types.Errf("readdir error: %v", err)
	}
	type row struct {
		name string
		dir  bool
		e    os.DirEntry
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		if !hidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(path, e.Name())); err == nil && st.IsDir() {
				isDir = true
			}
		}
		rows = append(rows, row{e.Name(), isDir, e})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].dir != rows[j].dir {
			return rows[i].dir
		}
		return strings.ToLower(rows[i].name) < strings.ToLower(rows[j].name)
	})
	if len(rows) == 0 {
		return types.Result{Output: "(empty directory)"}
	}

	var b strings.Builder
	shown := rows
	if len(shown) > max {
		shown = shown[:max]
	}
	for _, r := range shown {
		name := r.name
		if r.dir {
			name += "/"
		}
		info, ierr := r.e.Info()
		if r.e.Type()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(filepath.Join(path, r.name)); err == nil {
				name += " -> " + target
			}
		}
		switch {
		case ierr != nil:
			fmt.Fprintf(&b, "%s\n", name)
		case details:
			size := "-"
			if !r.dir {
				size = humanBytes(info.Size())
			}
			fmt.Fprintf(&b, "%-10s %8s  %s  %s\n", info.Mode().String(), size, info.ModTime().Format(time.DateTime), name)
		case r.dir:
			fmt.Fprintf(&b, "%s\n", name)
		default:
			fmt.Fprintf(&b, "%s  (%s)\n", name, humanBytes(info.Size()))
		}
	}
	if len(rows) > max {
		fmt.Fprintf(&b, "… %d more entries (raise max, or use glob_files)\n", len(rows)-max)
	}
	return types.TruncateOutput(b.String())
}
