package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Button is one inline button. CopyText, when set, is used by surfaces that
// support tap-to-copy buttons (rich messages); CallbackData is the fallback.
type Button struct {
	Text         string
	CallbackData string
	CopyText     string
}

// Screen is a markdown body plus rows of buttons.
type Screen struct {
	Text string
	Rows [][]Button
}

// Options tunes what a screen offers.
type Options struct {
	// AllowDelete is false in plan mode: the Delete button is not shown and
	// delete callbacks are refused.
	AllowDelete bool
	// Notice is a one-line status shown above the content.
	Notice string
}

// Result is what handling one callback produces.
type Result struct {
	Screen Screen
	// NoScreen means leave the current message alone (e.g. KindCopy).
	NoScreen bool
	// Copy, if set, is a path the UI should send as tap-to-copy text.
	Copy string
	// Inject, if set, is a user message to hand to the agent.
	Inject string
}

// ---------------------------------------------------------------- handling

// Open returns the root browse screen, or — when rel is given — the directory
// or file at that path (relative to the root, or absolute inside it).
func (m *Manager) Open(rel string, opts Options) Result {
	root := m.Root()
	if root == "" {
		return errResult(ErrNoRoot)
	}
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." || rel == "/" {
		return Result{Screen: m.Dir(0, 0, opts)}
	}
	abs := filepath.Clean(rel)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	if err := checkInside(root, abs); err != nil {
		return errResult(err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return errResult(ErrNotFound)
	}
	id := m.touch(abs)
	if st.IsDir() {
		return Result{Screen: m.Dir(id, 0, opts)}
	}
	return Result{Screen: m.File(id, 0, opts)}
}

// Handle executes a parsed callback.
func (m *Manager) Handle(a Action, opts Options) Result {
	switch a.Kind {
	case KindDir:
		return Result{Screen: m.Dir(a.ID, a.Arg, opts)}
	case KindFile:
		return Result{Screen: m.File(a.ID, a.Arg, opts)}
	case KindCopy:
		abs, err := m.Resolve(a.ID)
		if err != nil {
			return errResult(err)
		}
		return Result{NoScreen: true, Copy: m.Rel(abs)}
	case KindEdit:
		abs, err := m.Resolve(a.ID)
		if err != nil {
			return errResult(err)
		}
		opts.Notice = "✏️ Agent has this file as context — send your edit instructions."
		return Result{Screen: m.File(m.touch(abs), 0, opts), Inject: EditFocusText(m.Rel(abs))}
	case KindDelAsk:
		if !opts.AllowDelete {
			opts.Notice = "🔒 Delete is disabled in plan mode."
			return Result{Screen: m.File(a.ID, 0, opts)}
		}
		return Result{Screen: m.DeleteConfirm(a.ID)}
	case KindDelete:
		if !opts.AllowDelete {
			opts.Notice = "🔒 Delete is disabled in plan mode."
			return Result{Screen: m.File(a.ID, 0, opts)}
		}
		parent, rel, err := m.Delete(a.ID)
		if err != nil {
			return errResult(err)
		}
		opts.Notice = "🗑 Deleted " + code(rel)
		return Result{Screen: m.Dir(parent, 0, opts)}
	}
	return errResult(fmt.Errorf("unknown action %q", a.Kind))
}

// EditFocusText is the message handed to the agent by "Ask agent to edit".
func EditFocusText(rel string) string {
	return "Focus file: " + rel + ". Wait for my edit instructions."
}

// ---------------------------------------------------------------- directory

// Dir renders the browse screen for directory id, page (0-based).
func (m *Manager) Dir(id, page int, opts Options) Screen {
	abs, err := m.Resolve(id)
	if err != nil {
		return ErrorScreen(err)
	}
	root := m.Root()
	st, err := os.Stat(abs)
	if err != nil {
		return ErrorScreen(ErrNotFound)
	}
	if !st.IsDir() {
		return m.File(id, 0, opts)
	}
	l, err := ListDir(root, abs)
	if err != nil {
		return ErrorScreen(fmt.Errorf("cannot read directory: %w", err))
	}
	self := m.touch(abs)

	pages := (len(l.Entries) + PageSize - 1) / PageSize
	if pages < 1 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	lo := page * PageSize
	hi := lo + PageSize
	if hi > len(l.Entries) {
		hi = len(l.Entries)
	}
	visible := l.Entries[lo:hi]

	var b strings.Builder
	if opts.Notice != "" {
		b.WriteString(opts.Notice + "\n\n")
	}
	b.WriteString("📂 " + code(m.Display(abs)) + "\n\n")
	if len(l.Entries) == 0 {
		b.WriteString("Empty directory\n")
	} else {
		meta := fmt.Sprintf("%d entries", l.Total)
		if l.Total == 1 {
			meta = "1 entry"
		}
		if pages > 1 {
			meta += fmt.Sprintf(" · page %d/%d", page+1, pages)
		}
		b.WriteString(meta + "\n\n")
		for _, e := range visible {
			b.WriteString("- " + entryLine(e) + "\n")
		}
		if l.Truncated {
			fmt.Fprintf(&b, "\n⚠️ Too many entries — showing the first %d. Ask the agent to search (glob_files) or open a subfolder directly with /files <path>.\n", MaxList)
		}
	}

	var rows [][]Button
	for _, e := range visible {
		if e.Outside {
			continue
		}
		childID := m.register(filepath.Join(abs, e.Name))
		if e.Dir {
			rows = append(rows, []Button{{Text: "📁 " + trunc(e.Name, 30) + "/", CallbackData: cbDir(childID, 0)}})
		} else {
			rows = append(rows, []Button{{Text: "📄 " + trunc(e.Name, 30), CallbackData: cbFile(childID, 0)}})
		}
	}
	if pages > 1 {
		var pager []Button
		if page > 0 {
			pager = append(pager, Button{Text: "◀️ Prev", CallbackData: cbDir(self, page-1)})
		}
		pager = append(pager, Button{Text: fmt.Sprintf("%d/%d", page+1, pages), CallbackData: "ui:noop"})
		if page < pages-1 {
			pager = append(pager, Button{Text: "Next ▶️", CallbackData: cbDir(self, page+1)})
		}
		rows = append(rows, pager)
	}
	var nav []Button
	if abs != root {
		parent := m.touch(filepath.Dir(abs))
		nav = append(nav, Button{Text: "⬆️ Up", CallbackData: cbDir(parent, 0)})
	}
	nav = append(nav, Button{Text: "🔄 Refresh", CallbackData: cbDir(self, page)})
	rows = append(rows, nav)
	var nav2 []Button
	if abs != root {
		nav2 = append(nav2, Button{Text: "🏠 Root", CallbackData: cbDir(0, 0)})
	}
	nav2 = append(nav2, Button{Text: "⬅ Home", CallbackData: "ui:home"})
	rows = append(rows, nav2)

	return Screen{Text: strings.TrimRight(b.String(), "\n"), Rows: rows}
}

func entryLine(e Entry) string {
	switch {
	case e.Outside:
		return "🔗 " + code(e.Name) + " → outside workspace"
	case e.Dir:
		return "📁 " + code(e.Name+"/")
	default:
		return "📄 " + code(e.Name) + " · " + HumanSize(e.Size)
	}
}

// --------------------------------------------------------------------- file

// File renders the file view for id starting at 1-based line `line` (0 = first).
func (m *Manager) File(id, line int, opts Options) Screen {
	abs, err := m.Resolve(id)
	if err != nil {
		return ErrorScreen(err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return ErrorScreen(ErrNotFound)
	}
	if st.IsDir() {
		return m.Dir(id, 0, opts)
	}
	self := m.touch(abs)
	pg, err := ReadPage(abs, line, PreviewRunes)
	if err != nil {
		return ErrorScreen(fmt.Errorf("cannot read file: %w", err))
	}
	if pg.First == 0 && !pg.Empty && !pg.Binary {
		// offset past the end (file shrank since the button was drawn)
		if pg, err = ReadPage(abs, 1, PreviewRunes); err != nil {
			return ErrorScreen(fmt.Errorf("cannot read file: %w", err))
		}
	}

	rel := m.Rel(abs)
	var b strings.Builder
	if opts.Notice != "" {
		b.WriteString(opts.Notice + "\n\n")
	}
	head := "📄 " + code(rel) + " · " + HumanSize(pg.Size)
	switch {
	case pg.Binary:
		head += " · binary"
	case pg.Empty:
		head += " · empty"
	default:
		head += fmt.Sprintf(" · lines %d–%d of %s", pg.First, pg.Last, fmtTotal(pg))
	}
	b.WriteString(head + "\n\n")
	switch {
	case pg.Binary:
		b.WriteString("Binary file — use agent tools to process it (file_info, bash).\n")
	case pg.Empty:
		b.WriteString("(empty file)\n")
	default:
		b.WriteString("```" + Lang(filepath.Base(abs)) + "\n" + pg.Text + "\n```\n")
	}

	var rows [][]Button
	copyBtn := Button{Text: "📋 Copy path", CallbackData: cbSimple(KindCopy, self)}
	if len(rel) <= maxCopyText {
		copyBtn.CopyText = rel
	}
	reload := cbFile(self, 0)
	if pg.First > 1 {
		reload = cbFile(self, pg.First)
	}
	rows = append(rows, []Button{copyBtn, {Text: "🔄 Reload", CallbackData: reload}})
	if !pg.Binary && !pg.Empty && (pg.First > 1 || pg.More) {
		var paging []Button
		if pg.First > 1 {
			paging = append(paging, Button{Text: "⏮ Start", CallbackData: cbFile(self, 0)})
		}
		if pg.More {
			paging = append(paging, Button{Text: "⬇ More", CallbackData: cbFile(self, pg.Next)})
		}
		rows = append(rows, paging)
	}
	parent := m.touch(filepath.Dir(abs))
	rows = append(rows, []Button{
		{Text: "⬆️ Folder", CallbackData: cbDir(parent, 0)},
		{Text: "✏️ Ask agent to edit", CallbackData: cbSimple(KindEdit, self)},
	})
	last := []Button{}
	if opts.AllowDelete {
		last = append(last, Button{Text: "🗑 Delete", CallbackData: cbSimple(KindDelAsk, self)})
	}
	last = append(last, Button{Text: "⬅ Home", CallbackData: "ui:home"})
	rows = append(rows, last)

	return Screen{Text: strings.TrimRight(b.String(), "\n"), Rows: rows}
}

// ------------------------------------------------------------------- delete

// DeleteConfirm renders the "are you sure" card for a file.
func (m *Manager) DeleteConfirm(id int) Screen {
	abs, err := m.Resolve(id)
	if err != nil {
		return ErrorScreen(err)
	}
	if abs == m.Root() {
		return ErrorScreen(errors.New("the workspace root cannot be deleted"))
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return ErrorScreen(ErrNotFound)
	}
	if st.IsDir() {
		return ErrorScreen(errors.New("only files can be deleted from here — ask the agent to remove a directory"))
	}
	self := m.touch(abs)
	return Screen{
		Text: "🗑 Delete " + code(m.Rel(abs)) + "?\n\nThis cannot be undone from chat.",
		Rows: [][]Button{{
			{Text: "✅ Delete", CallbackData: cbSimple(KindDelete, self)},
			{Text: "❌ Cancel", CallbackData: cbFile(self, 0)},
		}},
	}
}

// Delete removes the file behind id. It refuses the root and directories.
// It returns the (re-registered) id of the parent directory and the deleted
// path relative to the root.
func (m *Manager) Delete(id int) (parentID int, rel string, err error) {
	abs, err := m.Resolve(id)
	if err != nil {
		return 0, "", err
	}
	if abs == m.Root() {
		return 0, "", errors.New("the workspace root cannot be deleted")
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return 0, "", ErrNotFound
	}
	if st.IsDir() {
		return 0, "", errors.New("only files can be deleted from here — ask the agent to remove a directory")
	}
	rel = m.Rel(abs)
	if err := os.Remove(abs); err != nil {
		return 0, "", fmt.Errorf("delete failed: %w", err)
	}
	return m.touch(filepath.Dir(abs)), rel, nil
}

// -------------------------------------------------------------------- misc

// ErrorScreen is shown when a button cannot be honoured.
func ErrorScreen(err error) Screen {
	return Screen{
		Text: "⚠️ " + err.Error(),
		Rows: [][]Button{{
			{Text: "🏠 Root", CallbackData: cbDir(0, 0)},
			{Text: "⬅ Home", CallbackData: "ui:home"},
		}},
	}
}

func errResult(err error) Result { return Result{Screen: ErrorScreen(err)} }

// code renders s as an inline code span, neutralising characters that would
// break the span or the line (file names can contain anything).
func code(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	return "`" + s + "`"
}

func trunc(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
