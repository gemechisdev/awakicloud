package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

const (
	maxEditFileBytes = 10 * 1024 * 1024
	maxEditsPerCall  = 50
	utf8BOM          = "\xef\xbb\xbf"
)

// EditFileTool is the agent's primary way to modify existing files.
//
// Design goals (what makes it cheap for the model to use correctly):
//   - refuses ambiguous matches and shows where they are, instead of silently
//     editing the first one
//   - tolerates whitespace / indentation / CRLF drift when the match is still unambiguous
//   - on failure, shows the closest text in the file so one retry is enough
//   - several hunks in one call, applied atomically
//   - returns a diff, so no verification read is needed
//   - preserves file mode, BOM and line endings; writes atomically
func EditFileTool() types.Tool {
	return types.Tool{
		Name:  "edit_file",
		Label: "Editing file",
		Description: "Edit an existing file by replacing text. Safer and cheaper than write_file for any change to a file that already exists.\n" +
			"How to use: read/grep first, then pass an old_string copied exactly from the file and long enough to be UNIQUE (include neighbouring lines). " +
			"If old_string matches several places the edit is refused and each match is listed — add context, or set replace_all=true to change all of them.\n" +
			"Several changes to one file: pass `edits` (array of {old_string,new_string,...}); they apply in order and all-or-nothing, so a failed hunk leaves the file untouched.\n" +
			"regex=true treats old_string as a Go RE2 pattern (multiline anchors on; $1 / ${name} in new_string; $$ for a literal $). " +
			"dry_run=true previews the diff without writing. expected_replacements=N fails unless exactly N matches exist (safe bulk rename).\n" +
			"new_string may be empty to delete text. Indentation/whitespace/CRLF differences are tolerated when the match is still unambiguous; " +
			"line endings, BOM and file permissions are preserved. The result includes a diff — no need to re-read the file. " +
			"An empty old_string on a missing or empty file creates it.",
		DetailFunc: pathDetail,
		Parameters: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path, or path relative to the workspace root",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Text to find, copied exactly (multi-line is fine). Must be unique unless replace_all/expected_replacements is set. Not used together with `edits`.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement text; empty string deletes the match. With regex=true may reference captures ($1, ${name}).",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "Replace every occurrence instead of requiring a unique match (default false).",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"description": "Treat old_string as a Go RE2 regular expression (default false = literal text).",
			},
			"expected_replacements": map[string]any{
				"type":        "integer",
				"description": "If set, replace all matches but fail without changes unless there are exactly this many.",
			},
			"dry_run": map[string]any{
				"type":        "boolean",
				"description": "Validate and return the diff without writing the file.",
			},
			"edits": map[string]any{
				"type":        "array",
				"description": "Multiple hunks for the same file, applied in order, atomically. Each hunk may set old_string, new_string, replace_all, regex, expected_replacements. Later hunks see the result of earlier ones.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string":            map[string]any{"type": "string", "description": "Text to find"},
						"new_string":            map[string]any{"type": "string", "description": "Replacement (empty deletes)"},
						"replace_all":           map[string]any{"type": "boolean", "description": "Replace every occurrence"},
						"regex":                 map[string]any{"type": "boolean", "description": "Treat old_string as RE2 regex"},
						"expected_replacements": map[string]any{"type": "integer", "description": "Require exactly this many matches"},
					},
					"required": []string{"old_string", "new_string"},
				},
			},
		},
		Required: []string{"path"},
		Handler:  editFile,
	}
}

// pathLocks serialises read-modify-write per file so parallel tool calls
// (or sub-agents) editing the same file can't clobber each other.
var pathLocks sync.Map // real path -> *sync.Mutex

func lockPath(p string) func() {
	v, _ := pathLocks.LoadOrStore(p, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func editFile(ctx context.Context, input map[string]any) types.Result {
	if err := ctx.Err(); err != nil {
		return types.ErrResult(err)
	}
	rawPath, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	specs, err := parseEditSpecs(input)
	if err != nil {
		return types.ErrResult(err)
	}
	dryRun := types.GetBool(input, "dry_run")

	path := workspace.Resolve(rawPath)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // edit the link target, don't replace the link itself
	}
	unlock := lockPath(path)
	defer unlock()

	info, statErr := os.Stat(path)
	creating := false
	switch {
	case statErr == nil && info.IsDir():
		return types.Errf("%s is a directory, not a file", path)
	case statErr == nil && info.Size() > maxEditFileBytes:
		return types.Errf("%s is %d bytes; edit_file handles files up to %d bytes — use bash/sed for larger files", path, info.Size(), maxEditFileBytes)
	case statErr != nil && !os.IsNotExist(statErr):
		return types.Errf("stat error: %v", statErr)
	case statErr != nil:
		creating = true
	}

	var original string
	var bom, crlf bool
	mode := os.FileMode(0o644)
	if !creating {
		mode = info.Mode().Perm()
		data, err := os.ReadFile(path)
		if err != nil {
			return types.Errf("read error: %v", err)
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return types.Errf("%s looks like a binary file; edit_file only edits text", path)
		}
		original = string(data)
		if strings.HasPrefix(original, utf8BOM) {
			bom = true
			original = strings.TrimPrefix(original, utf8BOM)
		}
		if n := strings.Count(original, "\r\n"); n > 0 && n == strings.Count(original, "\n") {
			crlf = true
			original = strings.ReplaceAll(original, "\r\n", "\n")
		}
	}

	// Creation: empty old_string on a missing/empty file.
	if len(specs) == 1 && specs[0].Old == "" && !specs[0].Regex {
		if !creating && original != "" {
			return types.Errf("old_string is empty but %s already has content; pass the text to replace, or use write_file to overwrite the whole file", path)
		}
		return finishEdit(path, "", normalizeEOL(specs[0].New), nil, creating, dryRun, mode, false, false)
	}
	if creating {
		return types.Errf("file not found: %s (resolved from %q). Use write_file to create it, or edit_file with an empty old_string", path, rawPath)
	}

	content := original
	var notes []string
	for i, spec := range specs {
		spec.Old, spec.New = normalizeEOL(spec.Old), normalizeEOL(spec.New)
		res, err := applyEdit(content, spec)
		if err != nil {
			if len(specs) > 1 {
				return types.Errf("edits[%d] of %d failed: %v\nNo changes were written (edits are all-or-nothing).", i+1, len(specs), err)
			}
			return types.ErrResult(err)
		}
		content = res.Content
		if res.Note != "" {
			if len(specs) > 1 {
				notes = append(notes, fmt.Sprintf("edits[%d]: %s", i+1, res.Note))
			} else {
				notes = append(notes, res.Note)
			}
		}
	}
	if content == original {
		return types.Errf("edit would not change the file (replacement is identical to the existing text)")
	}
	return finishEdit(path, original, content, notes, false, dryRun, mode, bom, crlf)
}

func finishEdit(path, original, content string, notes []string, creating, dryRun bool, mode os.FileMode, bom, crlf bool) types.Result {
	out := content
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if bom {
		out = utf8BOM + out
	}

	diff, added, removed := renderDiff(original, content)
	var b strings.Builder
	switch {
	case creating && dryRun:
		fmt.Fprintf(&b, "DRY RUN — would create %s (%d bytes)", path, len(out))
	case creating:
		fmt.Fprintf(&b, "Created %s (%d bytes)", path, len(out))
	case dryRun:
		fmt.Fprintf(&b, "DRY RUN — no changes written to %s (+%d -%d lines)", path, added, removed)
	default:
		fmt.Fprintf(&b, "Edited %s (+%d -%d lines)", path, added, removed)
	}
	for _, n := range notes {
		b.WriteString("\nNote: " + n)
	}
	if !creating && strings.HasSuffix(strings.ToLower(path), ".json") && json.Valid([]byte(original)) && !json.Valid([]byte(content)) {
		b.WriteString("\nWARNING: the file was valid JSON before this edit and is not valid JSON now — check commas/brackets.")
	}
	if diff != "" && !creating {
		b.WriteString("\n" + diff)
	}

	if !dryRun {
		if creating {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return types.Errf("mkdir error: %v", err)
			}
		}
		if err := writeFileAtomic(path, []byte(out), mode); err != nil {
			return types.Errf("write error: %v", err)
		}
	}
	return types.TruncateOutput(b.String())
}

// writeFileAtomic writes via temp file + rename so a crash or a concurrent
// reader never sees a half-written file, and keeps the original permissions.
// Falls back to an in-place write when the directory isn't writable.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".awaki-edit-*")
	if err != nil {
		return os.WriteFile(path, data, mode)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return os.WriteFile(path, data, mode)
	}
	return nil
}

func normalizeEOL(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// parseEditSpecs accepts either the single old_string/new_string form or the
// `edits` array. Top-level replace_all / regex / expected_replacements act as
// defaults for every hunk.
func parseEditSpecs(input map[string]any) ([]editSpec, error) {
	defAll := types.GetBool(input, "replace_all")
	defRegex := types.GetBool(input, "regex")
	defExp := intField(input, "expected_replacements")

	oldStr, hasOld := input["old_string"].(string)
	newStr, hasNew := input["new_string"].(string)

	raw, hasEdits := input["edits"]
	if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
		var parsed []any // some models send the array as a JSON string
		if json.Unmarshal([]byte(s), &parsed) == nil {
			raw = parsed
		}
	}
	if hasEdits && raw != nil {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return nil, errors.New("edits must be a non-empty array of {old_string, new_string}")
		}
		if (hasOld && oldStr != "") || (hasNew && newStr != "") {
			return nil, errors.New("pass either old_string/new_string or edits, not both")
		}
		if len(list) > maxEditsPerCall {
			return nil, fmt.Errorf("too many edits (%d, max %d)", len(list), maxEditsPerCall)
		}
		specs := make([]editSpec, 0, len(list))
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("edits[%d] must be an object", i+1)
			}
			o, _ := m["old_string"].(string)
			if o == "" {
				return nil, fmt.Errorf("edits[%d]: old_string is required", i+1)
			}
			n, okN := m["new_string"].(string)
			if !okN {
				return nil, fmt.Errorf("edits[%d]: new_string is required (use \"\" to delete text)", i+1)
			}
			spec := editSpec{Old: o, New: n, ReplaceAll: defAll, Regex: defRegex, Expected: defExp}
			if v, ok := m["replace_all"].(bool); ok {
				spec.ReplaceAll = v
			}
			if v, ok := m["regex"].(bool); ok {
				spec.Regex = v
			}
			if _, ok := m["expected_replacements"]; ok {
				spec.Expected = intField(m, "expected_replacements")
			}
			specs = append(specs, spec)
		}
		return specs, nil
	}

	if !hasNew {
		return nil, errors.New("new_string is required (use an empty string to delete text), or pass edits")
	}
	return []editSpec{{Old: oldStr, New: newStr, ReplaceAll: defAll, Regex: defRegex, Expected: defExp}}, nil
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
