package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

// apply_patch applies a unified diff (git diff / diff -u output, or the same
// written by hand) to one or many files, atomically.
//
// It is deliberately forgiving about what LLMs get wrong in diffs: line
// numbers and hunk counts are only hints (hunks are located by their context,
// nearest to the stated position wins), headers without numbers ("@@") work,
// and whitespace-only context differences are tolerated.

type patchLine struct {
	kind byte // ' ', '-', '+'
	text string
}

type patchHunk struct {
	oldStart  int // 1-based; 0 = insert at top; -1 = unknown
	hasCounts bool
	lines     []patchLine
	oldNoNL   bool
	newNoNL   bool
	marker    bool
}

type filePatch struct {
	oldPath, newPath string
	create, remove   bool
	rename           bool
	binary           bool
	hunks            []patchHunk
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func ApplyPatchTool() types.Tool {
	return types.Tool{
		Name:  "apply_patch",
		Label: "Applying patch",
		Description: "Apply a unified diff (git diff / diff -u format) to one or more files — create, modify, delete or rename — all-or-nothing. " +
			"Best for coordinated multi-file or multi-hunk changes; for one small change use edit_file. " +
			"Hunks are located by their context lines (line numbers are only hints), so approximately-right diffs still apply; if a hunk's context is not found the error shows the closest text. " +
			"Each hunk needs ~3 lines of unchanged context; prefix lines with ' ' (context), '-' (remove), '+' (add). Use /dev/null as the old path to create a file and as the new path to delete one. " +
			"dry_run=true validates without writing.",
		Parameters: map[string]any{
			"patch": map[string]any{
				"type":        "string",
				"description": "The unified diff text",
			},
			"root": map[string]any{
				"type":        "string",
				"description": "Directory that relative paths in the patch are resolved against (default: workspace root)",
			},
			"strip": map[string]any{
				"type":        "integer",
				"description": "Leading path components to remove (default: auto — strips a/ and b/ prefixes)",
			},
			"dry_run": map[string]any{
				"type":        "boolean",
				"description": "Check that the patch applies and report what would change, without writing",
			},
		},
		Required: []string{"patch"},
		Handler:  applyPatch,
	}
}

// ------------------------------------------------------------------ parse

func cleanPatchPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '\t'); i >= 0 { // "path<TAB>timestamp"
		p = p[:i]
	}
	p = strings.Trim(p, `"`)
	return p
}

func isHunkTerminator(lines []string, i int) bool {
	l := lines[i]
	if strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "diff --git ") {
		return true
	}
	return strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
}

func parsePatch(text string) ([]filePatch, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var out []filePatch
	var cur *filePatch
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			cur = &filePatch{}
			i++
		case strings.HasPrefix(l, "rename from "):
			if cur == nil {
				cur = &filePatch{}
			}
			cur.rename = true
			cur.oldPath = cleanPatchPath(strings.TrimPrefix(l, "rename from "))
			i++
		case strings.HasPrefix(l, "rename to "):
			if cur == nil {
				cur = &filePatch{}
			}
			cur.rename = true
			cur.newPath = cleanPatchPath(strings.TrimPrefix(l, "rename to "))
			i++
		case strings.HasPrefix(l, "Binary files ") || strings.HasPrefix(l, "GIT binary patch"):
			if cur == nil {
				cur = &filePatch{}
			}
			cur.binary = true
			i++
		case strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ "):
			if cur == nil || len(cur.hunks) > 0 {
				flush()
				cur = &filePatch{}
			}
			if !cur.rename {
				cur.oldPath = cleanPatchPath(strings.TrimPrefix(l, "--- "))
				cur.newPath = cleanPatchPath(strings.TrimPrefix(lines[i+1], "+++ "))
			}
			if cleanPatchPath(strings.TrimPrefix(l, "--- ")) == "/dev/null" {
				cur.create = true
			}
			if cleanPatchPath(strings.TrimPrefix(lines[i+1], "+++ ")) == "/dev/null" {
				cur.remove = true
			}
			i += 2
		case strings.HasPrefix(l, "@@"):
			if cur == nil {
				return nil, fmt.Errorf("patch line %d: hunk found before any file header (--- / +++)", i+1)
			}
			h := patchHunk{oldStart: -1}
			if m := hunkHeaderRe.FindStringSubmatch(l); m != nil {
				fmt.Sscanf(m[1], "%d", &h.oldStart)
				h.hasCounts = true
			}
			i++
			for i < len(lines) && !isHunkTerminator(lines, i) {
				b := lines[i]
				switch {
				case b == "":
					h.lines = append(h.lines, patchLine{' ', ""})
				case b[0] == ' ' || b[0] == '-' || b[0] == '+':
					h.lines = append(h.lines, patchLine{b[0], b[1:]})
				case b[0] == '\\':
					h.marker = true
					if n := len(h.lines); n > 0 {
						if h.lines[n-1].kind == '-' {
							h.oldNoNL = true
						} else if h.lines[n-1].kind == '+' {
							h.newNoNL = true
						} else {
							h.oldNoNL, h.newNoNL = true, true
						}
					}
				default:
					return nil, fmt.Errorf("patch line %d: unexpected line inside a hunk (every hunk line must start with ' ', '-' or '+'): %q", i+1, clipLine(b, 80))
				}
				i++
			}
			// drop trailing blank context produced by the end of the patch text
			for n := len(h.lines); n > 0 && h.lines[n-1].kind == ' ' && h.lines[n-1].text == "" && i >= len(lines) && !h.hasCounts; n = len(h.lines) {
				h.lines = h.lines[:n-1]
			}
			cur.hunks = append(cur.hunks, h)
		default:
			i++ // index/mode/similarity lines, commentary, etc.
		}
	}
	flush()
	if len(out) == 0 {
		return nil, fmt.Errorf("no file changes found in the patch — expected unified diff format with ---/+++ headers and @@ hunks")
	}
	for k := range out {
		p := &out[k]
		if p.binary && len(p.hunks) == 0 {
			return nil, fmt.Errorf("binary patches are not supported (%s)", firstNonEmpty(p.newPath, p.oldPath))
		}
		if p.oldPath == "/dev/null" {
			p.create = true
		}
		if p.newPath == "/dev/null" {
			p.remove = true
		}
		if p.oldPath == "" && p.newPath == "" {
			return nil, fmt.Errorf("a file section has no path")
		}
	}
	return out, nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func stripPatchPath(p string, strip int, auto bool) string {
	if p == "/dev/null" || p == "" {
		return p
	}
	if auto {
		if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
			return p[2:]
		}
		return p
	}
	for i := 0; i < strip; i++ {
		j := strings.IndexByte(p, '/')
		if j < 0 {
			return p
		}
		p = p[j+1:]
	}
	return p
}

// ------------------------------------------------------------------ apply

type hunkResult struct {
	notes []string
}

func splitLines(content string) (lines []string, endsNL bool) {
	if content == "" {
		return nil, false
	}
	endsNL = strings.HasSuffix(content, "\n")
	content = strings.TrimSuffix(content, "\n")
	return strings.Split(content, "\n"), endsNL
}

func joinLines(lines []string, endsNL bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, "\n")
	if endsNL {
		s += "\n"
	}
	return s
}

func blockEq(lines []string, at int, block []string, key bool) bool {
	for j, b := range block {
		l := lines[at+j]
		if key {
			if fuzzyKey(l) != fuzzyKey(b) {
				return false
			}
		} else if l != b {
			return false
		}
	}
	return true
}

// findBlock returns the index of block in lines nearest to expected (>=minPos).
func findBlock(lines, block []string, expected, minPos int) (pos int, tolerant bool, err error) {
	for _, key := range []bool{false, true} {
		var cands []int
		for i := minPos; i+len(block) <= len(lines); i++ {
			if blockEq(lines, i, block, key) {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			continue
		}
		if expected < 0 {
			if len(cands) > 1 {
				return 0, key, fmt.Errorf("context matches %d places and the hunk has no line number to choose between them; add more context lines", len(cands))
			}
			return cands[0], key, nil
		}
		best, bestD, ties := cands[0], abs(cands[0]-expected), 0
		for _, c := range cands[1:] {
			if d := abs(c - expected); d < bestD {
				best, bestD, ties = c, d, 0
			} else if d == bestD {
				ties++
			}
		}
		if ties > 0 && bestD > 0 {
			return 0, key, fmt.Errorf("context matches several places equally far from the stated line %d; add more context lines", expected+1)
		}
		return best, key, nil
	}
	return 0, false, os.ErrNotExist
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func applyHunks(content string, hunks []patchHunk) (string, []string, error) {
	lines, endsNL := splitLines(content)
	delta, prevStart := 0, 0
	var notes []string
	for hi, h := range hunks {
		var oldBlock, newBlock []string
		for _, pl := range h.lines {
			switch pl.kind {
			case ' ':
				oldBlock = append(oldBlock, pl.text)
				newBlock = append(newBlock, pl.text)
			case '-':
				oldBlock = append(oldBlock, pl.text)
			case '+':
				newBlock = append(newBlock, pl.text)
			}
		}
		expected := -1
		if h.oldStart >= 0 {
			expected = h.oldStart - 1 + delta
			if h.oldStart == 0 {
				expected = 0
			}
		}
		var pos int
		if len(oldBlock) == 0 { // pure insertion without context
			switch {
			case h.oldStart > 0:
				pos = h.oldStart + delta
			case h.oldStart == 0:
				pos = 0
			default:
				pos = len(lines)
			}
			if pos > len(lines) {
				pos = len(lines)
			}
		} else {
			p, tolerant, err := findBlock(lines, oldBlock, expected, prevStart)
			if err != nil {
				msg := fmt.Sprintf("hunk %d did not apply", hi+1)
				if h.oldStart > 0 {
					msg += fmt.Sprintf(" (stated at line %d)", h.oldStart)
				}
				if err == os.ErrNotExist {
					msg += ": its context/removed lines were not found in the file."
					if hint := closestBlock(joinLines(lines, true), strings.Join(oldBlock, "\n")); hint != "" {
						if k := strings.LastIndex(hint, "\nRetry with"); k >= 0 {
							hint = hint[:k]
						}
						msg += "\n" + hint
					}
					return "", nil, fmt.Errorf("%s", msg)
				}
				return "", nil, fmt.Errorf("%s: %v", msg, err)
			}
			pos = p
			if tolerant {
				notes = append(notes, fmt.Sprintf("hunk %d matched ignoring whitespace differences", hi+1))
			}
			if expected >= 0 && pos != expected {
				notes = append(notes, fmt.Sprintf("hunk %d applied at line %d (offset %+d from the stated position)", hi+1, pos+1, pos-expected))
			}
			if tolerant {
				// keep the file's own text for unchanged lines; re-use file lines for context
				kept := make([]string, 0, len(newBlock))
				oi := 0
				for _, pl := range h.lines {
					switch pl.kind {
					case ' ':
						kept = append(kept, lines[pos+oi])
						oi++
					case '-':
						oi++
					case '+':
						kept = append(kept, pl.text)
					}
				}
				newBlock = kept
			}
		}
		atEOF := pos+len(oldBlock) == len(lines)
		lines = append(append(append([]string{}, lines[:pos]...), newBlock...), lines[pos+len(oldBlock):]...)
		delta += len(newBlock) - len(oldBlock)
		prevStart = pos // later hunks may overlap this one's trailing context, but not start before it
		if h.marker && atEOF {
			endsNL = !h.newNoNL
		} else if !endsNL && atEOF && len(newBlock) > len(oldBlock) && !h.marker {
			endsNL = false
		}
		if len(lines) > 0 && !endsNL && content == "" {
			endsNL = true // brand-new file content defaults to a trailing newline
		}
	}
	return joinLines(lines, endsNL), notes, nil
}

type patchPlan struct {
	kind          string // M, A, D, R
	path, newPath string
	before        textFile
	after         string
	added, remov  int
	hunks         int
	notes         []string
}

func applyPatch(_ context.Context, input map[string]any) types.Result {
	text, err := types.GetString(input, "patch")
	if err != nil {
		return types.ErrResult(err)
	}
	dry := types.GetBoolDefault(input, "dry_run", false)
	root := workspace.Root()
	if r := types.GetStringOpt(input, "root"); r != "" {
		root = workspace.Resolve(r)
	}
	strip, hasStrip := types.GetIntOpt(input, "strip")

	patches, err := parsePatch(text)
	if err != nil {
		return types.ErrResult(err)
	}

	resolve := func(p string) string {
		if filepath.IsAbs(p) || root == "" {
			return workspace.Resolve(p)
		}
		return filepath.Join(root, p)
	}

	var plans []patchPlan
	touched := map[string]bool{}
	for _, fp := range patches {
		oldP := stripPatchPath(fp.oldPath, strip, !hasStrip)
		newP := stripPatchPath(fp.newPath, strip, !hasStrip)
		plan := patchPlan{hunks: len(fp.hunks)}
		for _, h := range fp.hunks {
			for _, l := range h.lines {
				switch l.kind {
				case '+':
					plan.added++
				case '-':
					plan.remov++
				}
			}
		}
		switch {
		case fp.create:
			plan.kind, plan.path = "A", resolve(newP)
		case fp.remove:
			plan.kind, plan.path = "D", resolve(oldP)
		case fp.rename && oldP != newP && oldP != "" && newP != "":
			plan.kind, plan.path, plan.newPath = "R", resolve(oldP), resolve(newP)
		default:
			plan.kind, plan.path = "M", resolve(firstNonEmpty(newP, oldP))
		}
		if touched[plan.path] || (plan.newPath != "" && touched[plan.newPath]) {
			return types.Errf("the patch touches %s more than once; merge those sections into one", plan.path)
		}
		touched[plan.path] = true
		if plan.newPath != "" {
			touched[plan.newPath] = true
		}

		tf, err := loadTextFile(plan.path)
		if err != nil {
			return types.Errf("%s: %v", plan.path, err)
		}
		plan.before = tf
		switch plan.kind {
		case "A":
			if tf.Exists && tf.Content != "" {
				return types.Errf("%s: patch creates this file but it already exists", plan.path)
			}
			after, notes, err := applyHunks("", fp.hunks)
			if err != nil {
				return types.Errf("%s: %v", plan.path, err)
			}
			plan.after, plan.notes = after, notes
		case "D":
			if !tf.Exists {
				return types.Errf("%s: patch deletes this file but it does not exist", plan.path)
			}
			after, notes, err := applyHunks(tf.Content, fp.hunks)
			if err != nil {
				return types.Errf("%s: %v", plan.path, err)
			}
			if strings.TrimSpace(after) != "" {
				return types.Errf("%s: patch deletes the file but its hunks leave content behind — the diff does not cover the whole file", plan.path)
			}
			plan.notes = notes
		default:
			if !tf.Exists {
				return types.Errf("%s: file not found (patch modifies it). Use /dev/null as the old path to create a file.", plan.path)
			}
			after, notes, err := applyHunks(tf.Content, fp.hunks)
			if err != nil {
				return types.Errf("%s: %v", plan.path, err)
			}
			plan.after, plan.notes = after, notes
			if plan.kind == "M" && after == tf.Content {
				return types.Errf("%s: patch makes no change to this file", plan.path)
			}
			if plan.kind == "R" {
				if _, err := os.Stat(plan.newPath); err == nil {
					return types.Errf("%s: rename target already exists", plan.newPath)
				}
			}
		}
		plans = append(plans, plan)
	}

	var b strings.Builder
	totalAdd, totalRem, totalHunks := 0, 0, 0
	for _, p := range plans {
		totalAdd += p.added
		totalRem += p.remov
		totalHunks += p.hunks
	}
	if dry {
		fmt.Fprintf(&b, "DRY RUN — patch applies cleanly; nothing was written.\n")
	}
	fmt.Fprintf(&b, "%d file(s), %d hunk(s), +%d -%d lines\n", len(plans), totalHunks, totalAdd, totalRem)
	for _, p := range plans {
		switch p.kind {
		case "R":
			fmt.Fprintf(&b, "R %s -> %s (+%d -%d)\n", p.path, p.newPath, p.added, p.remov)
		case "A":
			fmt.Fprintf(&b, "A %s (+%d)\n", p.path, p.added)
		case "D":
			fmt.Fprintf(&b, "D %s (-%d)\n", p.path, p.remov)
		default:
			fmt.Fprintf(&b, "M %s (+%d -%d)\n", p.path, p.added, p.remov)
		}
		for _, n := range p.notes {
			fmt.Fprintf(&b, "  note: %s\n", n)
		}
	}
	if dry {
		return types.TruncateOutput(strings.TrimRight(b.String(), "\n"))
	}

	var paths []string
	for _, p := range plans {
		paths = append(paths, p.path)
		if p.newPath != "" {
			paths = append(paths, p.newPath)
		}
	}
	unlock := lockPaths(paths...)
	defer unlock()

	type undo func()
	var undos []undo
	rollback := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}
	for _, p := range plans {
		p := p
		switch p.kind {
		case "A":
			if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
				rollback()
				return types.Errf("%s: mkdir error: %v", p.path, err)
			}
			mode := os.FileMode(0o644)
			if strings.HasPrefix(p.after, "#!") {
				mode = 0o755
			}
			if err := writeFileAtomic(p.path, p.before.encode(p.after), mode); err != nil {
				rollback()
				return types.Errf("%s: write error: %v (no changes kept)", p.path, err)
			}
			undos = append(undos, func() { _ = os.Remove(p.path) })
		case "D":
			orig := p.before.encode(p.before.Content)
			if err := os.Remove(p.path); err != nil {
				rollback()
				return types.Errf("%s: delete error: %v (no changes kept)", p.path, err)
			}
			undos = append(undos, func() { _ = writeFileAtomic(p.path, orig, p.before.Mode) })
		case "R":
			orig := p.before.encode(p.before.Content)
			if err := os.MkdirAll(filepath.Dir(p.newPath), 0o755); err != nil {
				rollback()
				return types.Errf("%s: mkdir error: %v", p.newPath, err)
			}
			if err := writeFileAtomic(p.newPath, p.before.encode(p.after), p.before.Mode); err != nil {
				rollback()
				return types.Errf("%s: write error: %v (no changes kept)", p.newPath, err)
			}
			if err := os.Remove(p.path); err != nil {
				_ = os.Remove(p.newPath)
				rollback()
				return types.Errf("%s: could not remove old path: %v (no changes kept)", p.path, err)
			}
			undos = append(undos, func() { _ = os.Remove(p.newPath); _ = writeFileAtomic(p.path, orig, p.before.Mode) })
		default:
			orig := p.before.encode(p.before.Content)
			if err := writeFileAtomic(p.path, p.before.encode(p.after), p.before.Mode); err != nil {
				rollback()
				return types.Errf("%s: write error: %v (no changes kept)", p.path, err)
			}
			undos = append(undos, func() { _ = writeFileAtomic(p.path, orig, p.before.Mode) })
		}
	}
	return types.TruncateOutput(strings.TrimRight(b.String(), "\n"))
}
