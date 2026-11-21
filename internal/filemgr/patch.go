package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PatchFile summarises one file touched by a patch.
type PatchFile struct {
	Path    string
	Action  string // modified | created | deleted
	Added   int
	Removed int
}

// PatchResult is the outcome of ApplyPatch.
type PatchResult struct {
	Files []PatchFile
	Seq   int // undo step, 0 if not undoable
}

type hunkLine struct {
	op   byte // ' ', '+', '-'
	text string
}

type hunk struct {
	oldStart int
	lines    []hunkLine
}

type fileDiff struct {
	oldPath, newPath string // "" for /dev/null
	hunks            []hunk
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParsePatch parses a unified diff (git style or plain `diff -u`).
func ParsePatch(text string) ([]fileDiff, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	// Chat clients often wrap pasted diffs in a code fence.
	if t := strings.TrimSpace(text); strings.HasPrefix(t, "```") {
		lines := strings.Split(t, "\n")
		lines = lines[1:]
		if n := len(lines); n > 0 && strings.HasPrefix(strings.TrimSpace(lines[n-1]), "```") {
			lines = lines[:n-1]
		}
		text = strings.Join(lines, "\n")
	}
	lines := strings.Split(text, "\n")
	var diffs []fileDiff
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "--- ") || i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			i++
			continue
		}
		fd := fileDiff{oldPath: diffPath(lines[i][4:]), newPath: diffPath(lines[i+1][4:])}
		i += 2
		for i < len(lines) {
			m := hunkHeader.FindStringSubmatch(lines[i])
			if m == nil {
				break
			}
			oldStart, _ := strconv.Atoi(m[1])
			oldCnt, newCnt := 1, 1
			if m[2] != "" {
				oldCnt, _ = strconv.Atoi(m[2])
			}
			if m[4] != "" {
				newCnt, _ = strconv.Atoi(m[4])
			}
			h := hunk{oldStart: oldStart}
			i++
			for i < len(lines) && (oldCnt > 0 || newCnt > 0) {
				l := lines[i]
				switch {
				case strings.HasPrefix(l, "\\"):
					i++
					continue
				case l == "":
					// a context line whose leading space was stripped
					h.lines = append(h.lines, hunkLine{' ', ""})
					oldCnt--
					newCnt--
				case l[0] == ' ':
					h.lines = append(h.lines, hunkLine{' ', l[1:]})
					oldCnt--
					newCnt--
				case l[0] == '-':
					h.lines = append(h.lines, hunkLine{'-', l[1:]})
					oldCnt--
				case l[0] == '+':
					h.lines = append(h.lines, hunkLine{'+', l[1:]})
					newCnt--
				default:
					oldCnt, newCnt = 0, 0
					continue
				}
				i++
			}
			for i < len(lines) && strings.HasPrefix(lines[i], "\\") {
				i++
			}
			fd.hunks = append(fd.hunks, h)
		}
		if len(fd.hunks) == 0 {
			return nil, fmt.Errorf("no hunks found for %s", firstNonEmpty(fd.newPath, fd.oldPath))
		}
		diffs = append(diffs, fd)
	}
	if len(diffs) == 0 {
		return nil, errors.New("this does not look like a unified diff (expected `--- a/file`, `+++ b/file` and `@@` hunks)")
	}
	return diffs, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func diffPath(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if s == "/dev/null" {
		return ""
	}
	return s
}

// resolvePatchPath maps a diff path onto the workspace (git -p1 when the
// paths carry a/ b/ prefixes, otherwise as written).
func (m *Manager) resolvePatchPath(p string, stripPrefix bool) (string, error) {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if stripPrefix {
		if i := strings.IndexByte(p, '/'); i > 0 {
			p = p[i+1:]
		}
	}
	if p == "." || p == "" || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("unsafe path in patch: %q", p)
	}
	abs := filepath.Join(m.Root(), filepath.FromSlash(p))
	if !Within(m.Root(), abs) {
		return "", fmt.Errorf("unsafe path in patch: %q", p)
	}
	return abs, nil
}

type pendingWrite struct {
	abs     string
	rel     string
	content []byte
	del     bool
	pf      PatchFile
}

// ApplyPatch applies a unified diff to the workspace, paths relative to the
// root. It is all-or-nothing: every hunk must match before anything is written.
func (m *Manager) ApplyPatch(text string) (PatchResult, error) {
	if m.Root() == "" {
		return PatchResult{}, ErrNoRoot
	}
	diffs, err := ParsePatch(text)
	if err != nil {
		return PatchResult{}, err
	}
	var pend []pendingWrite
	seen := map[string]bool{}
	for _, fd := range diffs {
		strip := strings.HasPrefix(fd.oldPath, "a/") && strings.HasPrefix(fd.newPath, "b/") ||
			fd.oldPath == "" && strings.HasPrefix(fd.newPath, "b/") ||
			fd.newPath == "" && strings.HasPrefix(fd.oldPath, "a/")
		target := firstNonEmpty(fd.newPath, fd.oldPath)
		abs, err := m.resolvePatchPath(target, strip)
		if err != nil {
			return PatchResult{}, err
		}
		if seen[abs] {
			return PatchResult{}, fmt.Errorf("%s appears twice in the patch", code(m.Rel(abs)))
		}
		seen[abs] = true
		if err := m.checkWritePath(abs); err != nil {
			return PatchResult{}, fmt.Errorf("%s: %w", code(m.Rel(abs)), err)
		}
		pw := pendingWrite{abs: abs, rel: m.Rel(abs)}
		pw.pf.Path = pw.rel
		switch {
		case fd.oldPath == "": // new file
			if _, err := os.Lstat(abs); err == nil {
				return PatchResult{}, fmt.Errorf("%s already exists but the patch creates it", code(pw.rel))
			}
			var b strings.Builder
			for _, h := range fd.hunks {
				for _, l := range h.lines {
					if l.op == '+' {
						b.WriteString(l.text + "\n")
						pw.pf.Added++
					}
				}
			}
			pw.content = []byte(b.String())
			pw.pf.Action = "created"
		default:
			data, err := m.readEditable(abs)
			if err != nil {
				return PatchResult{}, fmt.Errorf("%s: %w", code(pw.rel), err)
			}
			out, added, removed, err := applyHunks(string(data), fd.hunks)
			if err != nil {
				return PatchResult{}, fmt.Errorf("%s: %w", code(pw.rel), err)
			}
			pw.pf.Added, pw.pf.Removed = added, removed
			if fd.newPath == "" {
				pw.del = true
				pw.pf.Action = "deleted"
			} else {
				pw.content = []byte(out)
				pw.pf.Action = "modified"
			}
		}
		pend = append(pend, pw)
	}

	var ops []undoOp
	rollback := func() {
		for i := len(ops) - 1; i >= 0; i-- {
			_ = restore(ops[i])
		}
	}
	res := PatchResult{}
	for _, pw := range pend {
		var op undoOp
		var err error
		if pw.del {
			op, err = deleteTracked(pw.abs)
		} else {
			if err := os.MkdirAll(filepath.Dir(pw.abs), 0o755); err != nil {
				rollback()
				return PatchResult{}, err
			}
			op, err = writeTracked(pw.abs, pw.content)
		}
		if err != nil {
			rollback()
			return PatchResult{}, fmt.Errorf("write %s failed: %w", code(pw.rel), err)
		}
		ops = append(ops, op)
		res.Files = append(res.Files, pw.pf)
	}
	res.Seq = m.pushUndo(ops)
	return res, nil
}

// applyHunks applies hunks to text. CRLF files stay CRLF.
func applyHunks(text string, hunks []hunk) (out string, added, removed int, err error) {
	crlf := strings.Contains(text, "\r\n")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	trailing := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	cursor := 0 // earliest line the next hunk may start at
	shift := 0  // net line delta from earlier hunks
	for n, h := range hunks {
		var oldBlock, newBlock []string
		for _, l := range h.lines {
			if l.op != '+' {
				oldBlock = append(oldBlock, l.text)
			}
			if l.op != '-' {
				newBlock = append(newBlock, l.text)
				if l.op == '+' {
					added++
				}
			} else {
				removed++
			}
		}
		expected := h.oldStart - 1 + shift
		if len(oldBlock) == 0 {
			// pure insertion: after line oldStart
			expected = h.oldStart + shift
			if expected < cursor || expected > len(lines) {
				expected = len(lines)
			}
			lines = splice(lines, expected, 0, newBlock)
			shift += len(newBlock)
			cursor = expected + len(newBlock)
			continue
		}
		pos := findBlock(lines, oldBlock, cursor, expected, false)
		if pos < 0 {
			pos = findBlock(lines, oldBlock, cursor, expected, true)
		}
		if pos < 0 {
			return "", 0, 0, fmt.Errorf("hunk %d does not match (context not found near line %d)", n+1, h.oldStart)
		}
		lines = splice(lines, pos, len(oldBlock), newBlock)
		shift += len(newBlock) - len(oldBlock)
		cursor = pos + len(newBlock)
	}
	out = strings.Join(lines, "\n")
	if len(lines) > 0 && (trailing || text == "") {
		out += "\n"
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, added, removed, nil
}

// findBlock returns the index in lines where block matches, nearest to
// expected, at or after min; -1 if none. loose ignores trailing whitespace.
func findBlock(lines, block []string, min, expected int, loose bool) int {
	eq := func(a, b string) bool {
		if loose {
			return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
		}
		return a == b
	}
	var cands []int
	for i := min; i+len(block) <= len(lines); i++ {
		ok := true
		for j, b := range block {
			if !eq(lines[i+j], b) {
				ok = false
				break
			}
		}
		if ok {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return -1
	}
	sort.SliceStable(cands, func(a, b int) bool { return abs(cands[a]-expected) < abs(cands[b]-expected) })
	return cands[0]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func splice(lines []string, at, del int, ins []string) []string {
	out := make([]string, 0, len(lines)-del+len(ins))
	out = append(out, lines[:at]...)
	out = append(out, ins...)
	return append(out, lines[at+del:]...)
}
