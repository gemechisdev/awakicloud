package filemgr

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Line-edit syntax (one or more blocks in a single message):
//
//	@12          diff-style block starting at line 12; body lines begin with
//	+text        insert a line here
//	-text        delete the current line (must equal text)
//	 text        keep the current line (must equal text; leading space)
//
//	@40-45       range block: the raw body replaces lines 40..45
//	<empty>      as the only body line, deletes the range
//
// Line numbers always refer to the file as it is now, so several blocks in
// one message do not shift each other.

var lineHeader = regexp.MustCompile(`^@(\d+)(?:-(\d+))?\s*$`)

type lineBlock struct {
	start     int // 1-based
	rangeForm bool
	rangeEnd  int
	body      []string
	hdrLine   int
}

// LineEditResult summarises an applied line edit.
type LineEditResult struct {
	Blocks  int
	Added   int
	Removed int
	Seq     int
}

func parseLineBlocks(spec string) ([]lineBlock, error) {
	spec = strings.ReplaceAll(spec, "\r\n", "\n")
	if t := strings.TrimSpace(spec); strings.HasPrefix(t, "```") {
		ls := strings.Split(t, "\n")[1:]
		if n := len(ls); n > 0 && strings.HasPrefix(strings.TrimSpace(ls[n-1]), "```") {
			ls = ls[:n-1]
		}
		spec = strings.Join(ls, "\n")
	}
	var blocks []lineBlock
	var cur *lineBlock
	for i, l := range strings.Split(spec, "\n") {
		if mt := lineHeader.FindStringSubmatch(l); mt != nil {
			n, _ := strconv.Atoi(mt[1])
			b := lineBlock{start: n, hdrLine: i + 1}
			if mt[2] != "" {
				b.rangeForm = true
				b.rangeEnd, _ = strconv.Atoi(mt[2])
			}
			blocks = append(blocks, b)
			cur = &blocks[len(blocks)-1]
			continue
		}
		if cur == nil {
			if strings.TrimSpace(l) == "" {
				continue
			}
			return nil, errors.New("start with a header such as `@12` (diff-style) or `@40-45` (replace range)")
		}
		cur.body = append(cur.body, l)
	}
	if len(blocks) == 0 {
		return nil, errors.New("no `@N` header found")
	}
	for i := range blocks {
		b := &blocks[i]
		for len(b.body) > 0 && b.body[len(b.body)-1] == "" && !b.rangeForm {
			b.body = b.body[:len(b.body)-1]
		}
		if b.rangeForm {
			for len(b.body) > 0 && b.body[len(b.body)-1] == "" {
				b.body = b.body[:len(b.body)-1]
			}
		}
		if len(b.body) == 0 {
			return nil, fmt.Errorf("block `@%d` has no body", b.start)
		}
		if b.start < 1 {
			return nil, errors.New("line numbers start at 1")
		}
		if !b.rangeForm {
			for _, l := range b.body {
				if l != "" && l[0] != '+' && l[0] != '-' && l[0] != ' ' {
					return nil, fmt.Errorf("in block `@%d`: every line must start with +, - or a space (got %q)", b.start, trunc(l, 30))
				}
			}
		} else if b.rangeEnd < b.start {
			return nil, fmt.Errorf("range `@%d-%d` is backwards", b.start, b.rangeEnd)
		}
	}
	return blocks, nil
}

// ApplyLineEdits applies the spec to the text file behind id.
func (m *Manager) ApplyLineEdits(id int, spec string) (LineEditResult, error) {
	abs, err := m.Resolve(id)
	if err != nil {
		return LineEditResult{}, err
	}
	blocks, err := parseLineBlocks(spec)
	if err != nil {
		return LineEditResult{}, err
	}
	data, err := m.readEditable(abs)
	if err != nil {
		return LineEditResult{}, err
	}
	text := string(data)
	crlf := strings.Contains(text, "\r\n")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	trailing := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}

	type edit struct {
		from, to int // [from,to) in lines
		repl     []string
	}
	var edits []edit
	var res LineEditResult
	for _, b := range blocks {
		from := b.start - 1
		if from > len(lines) {
			return res, fmt.Errorf("`@%d` is past the end of the file (%d lines)", b.start, len(lines))
		}
		if b.rangeForm {
			if b.rangeEnd > len(lines) {
				return res, fmt.Errorf("range `@%d-%d` is past the end of the file (%d lines)", b.start, b.rangeEnd, len(lines))
			}
			repl := b.body
			if len(repl) == 1 && repl[0] == EmptyReplacement {
				repl = nil
			}
			res.Removed += b.rangeEnd - from
			res.Added += len(repl)
			edits = append(edits, edit{from, b.rangeEnd, repl})
			continue
		}
		idx := from
		var repl []string
		for _, l := range b.body {
			op, t := byte(' '), "" // an empty line means "keep this blank line"
			if l != "" {
				op, t = l[0], l[1:]
			}
			switch op {
			case '+':
				repl = append(repl, t)
				res.Added++
			case '-', ' ':
				if idx >= len(lines) {
					return res, fmt.Errorf("block `@%d`: ran past the end of the file", b.start)
				}
				if strings.TrimRight(lines[idx], " \t") != strings.TrimRight(t, " \t") {
					return res, fmt.Errorf("block `@%d`: line %d is %s, not %s", b.start, idx+1, code(trunc(lines[idx], 40)), code(trunc(t, 40)))
				}
				if op == ' ' {
					repl = append(repl, lines[idx])
				} else {
					res.Removed++
				}
				idx++
			}
		}
		edits = append(edits, edit{from, idx, repl})
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].from < edits[j].from })
	for i := 1; i < len(edits); i++ {
		if edits[i].from < edits[i-1].to {
			return res, errors.New("two blocks touch the same lines — merge them into one")
		}
	}
	for i := len(edits) - 1; i >= 0; i-- {
		lines = splice(lines, edits[i].from, edits[i].to-edits[i].from, edits[i].repl)
	}
	out := strings.Join(lines, "\n")
	if len(lines) > 0 && (trailing || text == "") {
		out += "\n"
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if out == string(data) {
		return res, errors.New("nothing changed")
	}
	if err := m.checkWritePath(abs); err != nil {
		return res, err
	}
	op, err := writeTracked(abs, []byte(out))
	if err != nil {
		return res, fmt.Errorf("write failed: %w", err)
	}
	res.Blocks = len(blocks)
	res.Seq = m.pushUndo([]undoOp{op})
	return res, nil
}

// ReplaceWholeFile overwrites the file behind id with data (manual edit
// round-trip). It is undoable.
func (m *Manager) ReplaceWholeFile(id int, data []byte) (seq int, err error) {
	abs, err := m.Resolve(id)
	if err != nil {
		return 0, err
	}
	if len(data) > MaxUploadBytes {
		return 0, fmt.Errorf("file is %s — over the %s limit", HumanSize(int64(len(data))), HumanSize(MaxUploadBytes))
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return 0, errors.New("the target is not a file")
	}
	if err := m.checkWritePath(abs); err != nil {
		return 0, err
	}
	op, err := writeTracked(abs, data)
	if err != nil {
		return 0, fmt.Errorf("write failed: %w", err)
	}
	return m.pushUndo([]undoOp{op}), nil
}

// ---------------------------------------------------- path-header code blocks

var blockHeader = regexp.MustCompile(`(?i)^\s*(?://|#|--|<!--|/\*)?\s*(?:file|path|filename)\s*:\s*` + "`?" + `([^\s` + "`" + `]+)` + "`?" + `\s*(?:-->|\*/)?\s*$`)

type fileBlock struct {
	path    string
	content string
}

// ParseFileBlocks reads "File: path" + (optionally fenced) content blocks.
func ParseFileBlocks(text string) ([]fileBlock, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var blocks []fileBlock
	var cur *fileBlock
	var body []string
	inFence := false
	flush := func() {
		if cur == nil {
			return
		}
		for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
			body = body[1:]
		}
		if len(body) > 0 && strings.HasPrefix(strings.TrimSpace(body[0]), "```") {
			body = body[1:]
			for n := len(body) - 1; n >= 0; n-- {
				if strings.HasPrefix(strings.TrimSpace(body[n]), "```") {
					body = body[:n]
					break
				}
				if strings.TrimSpace(body[n]) != "" {
					break
				}
			}
		}
		cur.content = strings.Join(body, "\n")
		blocks = append(blocks, *cur)
		cur, body = nil, nil
	}
	for _, l := range strings.Split(text, "\n") {
		if !inFence || cur == nil {
			cand := l
			if t := strings.TrimSpace(l); strings.HasPrefix(t, "`") && !strings.HasPrefix(t, "```") {
				cand = strings.Trim(t, "`") // a header wrapped in inline code
			}
			if mt := blockHeader.FindStringSubmatch(cand); mt != nil {
				flush()
				cur = &fileBlock{path: mt[1]}
				continue
			}
		}
		if cur == nil {
			if strings.TrimSpace(l) == "" {
				continue
			}
			return nil, errors.New("not a patch and not a `File: path` code block")
		}
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		body = append(body, l)
	}
	flush()
	if len(blocks) == 0 {
		return nil, errors.New("no `File: path` header found")
	}
	return blocks, nil
}

// ApplyFileBlocks writes whole files from "File: path" blocks (paths relative
// to the root). All-or-nothing, undoable as one step.
func (m *Manager) ApplyFileBlocks(blocks []fileBlock) (PatchResult, error) {
	var ops []undoOp
	var res PatchResult
	rollback := func() {
		for i := len(ops) - 1; i >= 0; i-- {
			_ = restore(ops[i])
		}
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		abs, err := m.resolvePatchPath(b.path, false)
		if err != nil {
			rollback()
			return PatchResult{}, err
		}
		if seen[abs] {
			rollback()
			return PatchResult{}, fmt.Errorf("%s appears twice", code(m.Rel(abs)))
		}
		seen[abs] = true
		if len(b.content) > MaxCreateBytes*8 {
			rollback()
			return PatchResult{}, fmt.Errorf("%s is too large to paste", code(m.Rel(abs)))
		}
		if err := m.checkWritePath(abs); err != nil {
			rollback()
			return PatchResult{}, fmt.Errorf("%s: %w", code(m.Rel(abs)), err)
		}
		_, existed := os.Lstat(abs)
		content := b.content
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.MkdirAll(dirOf(abs), 0o755); err != nil {
			rollback()
			return PatchResult{}, err
		}
		op, err := writeTracked(abs, []byte(content))
		if err != nil {
			rollback()
			return PatchResult{}, fmt.Errorf("write %s failed: %w", code(m.Rel(abs)), err)
		}
		ops = append(ops, op)
		act := "modified"
		if existed != nil {
			act = "created"
		}
		res.Files = append(res.Files, PatchFile{Path: m.Rel(abs), Action: act, Added: strings.Count(content, "\n")})
	}
	res.Seq = m.pushUndo(ops)
	return res, nil
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return "."
}
