package filemgr

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxSearchHits     = 40
	MaxSearchFiles    = 20000
	MaxSearchFileSize = 1 << 20
	searchBudget      = 8 * time.Second
	maxHitsPerFile    = 3
	MaxRecent         = 8

	MaxZipSource = 200 << 20 // uncompressed bytes we are willing to read
)

// skipDirs are never searched or zipped (huge, generated, or private).
var skipDirs = map[string]bool{".git": true, "node_modules": true}

// Hit is one search result.
type Hit struct {
	Path    string // absolute
	Rel     string
	Line    int // 0 = file/folder name match
	Snippet string
	Dir     bool
}

type matcher func(string) bool

func newMatcher(q string) (matcher, error) {
	if strings.HasPrefix(q, "re:") {
		re, err := regexp.Compile("(?i)" + strings.TrimPrefix(q, "re:"))
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", err)
		}
		return re.MatchString, nil
	}
	lq := strings.ToLower(q)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), lq) }, nil
}

// Search looks for query in file/folder names and in text file contents under
// folder dirID. Plain queries are case-insensitive substrings; "re:" starts a
// regular expression. Symlinks are never followed; .git and node_modules are skipped.
func (m *Manager) Search(dirID int, query string) (hits []Hit, truncated bool, err error) {
	dir, err := m.Resolve(dirID)
	if err != nil {
		return nil, false, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, false, errors.New("search starts from a folder")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("type something to search for")
	}
	match, err := newMatcher(query)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(searchBudget)
	scanned := 0
	errStop := errors.New("stop")
	add := func(h Hit) error {
		hits = append(hits, h)
		if len(hits) >= MaxSearchHits {
			truncated = true
			return errStop
		}
		return nil
	}
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if time.Now().After(deadline) || scanned >= MaxSearchFiles {
			truncated = true
			return errStop
		}
		if p == dir {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if match(d.Name()) {
				return add(Hit{Path: p, Rel: m.Rel(p), Dir: true})
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, …
		}
		scanned++
		if match(d.Name()) {
			if err := add(Hit{Path: p, Rel: m.Rel(p)}); err != nil {
				return err
			}
		}
		if info, err := d.Info(); err != nil || info.Size() == 0 || info.Size() > MaxSearchFileSize {
			return nil
		}
		return grepFile(p, m.Rel(p), match, add)
	})
	if walkErr != nil && walkErr != errStop {
		return hits, truncated, walkErr
	}
	return hits, truncated, nil
}

func grepFile(abs, rel string, match matcher, add func(Hit) error) error {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, binarySniff)
	n, _ := io.ReadFull(f, head)
	if looksBinary(head[:n]) {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 256<<10)
	found := 0
	for line := 1; sc.Scan(); line++ {
		t := sc.Text()
		if !utf8.ValidString(t) {
			t = strings.ToValidUTF8(t, "\uFFFD")
		}
		if match(t) {
			if err := add(Hit{Path: abs, Rel: rel, Line: line, Snippet: strings.TrimSpace(t)}); err != nil {
				return err
			}
			if found++; found >= maxHitsPerFile {
				return nil
			}
		}
	}
	return nil
}

// SearchScreen renders results with a grid of buttons that open the hit.
func (m *Manager) SearchScreen(dirID int, query string, opts Options) Screen {
	dir, err := m.Resolve(dirID)
	if err != nil {
		return ErrorScreen(err)
	}
	hits, truncated, err := m.Search(dirID, query)
	self := m.touch(dir)
	back := []Button{
		{Text: "⬆️ Folder", CallbackData: cbDir(self, 0)},
		{Text: "🔍 New search", CallbackData: cbSimple(KindSearch, self)},
		{Text: "⬅ Home", CallbackData: "ui:home"},
	}
	if err != nil {
		return Screen{Text: "⚠️ " + err.Error(), Rows: [][]Button{back}}
	}
	var b strings.Builder
	if opts.Notice != "" {
		b.WriteString(opts.Notice + "\n\n")
	}
	fmt.Fprintf(&b, "🔍 %s in %s\n\n", code(trunc(query, 50)), code(m.Display(dir)))
	if len(hits) == 0 {
		b.WriteString("No matches.")
		return Screen{Text: b.String(), Rows: [][]Button{back}}
	}
	for _, h := range hits {
		switch {
		case h.Dir:
			b.WriteString("- 📁 " + code(h.Rel+"/") + "\n")
		case h.Line == 0:
			b.WriteString("- 📄 " + code(h.Rel) + "\n")
		default:
			fmt.Fprintf(&b, "- 📄 %s — %s\n", code(fmt.Sprintf("%s:%d", h.Rel, h.Line)), code(trunc(h.Snippet, 60)))
		}
	}
	if truncated {
		b.WriteString("\n⚠️ Showing the first results only — narrow the query or search a subfolder.")
	}
	var cells []Button
	for i, h := range hits {
		if i >= PageSize {
			break
		}
		id := m.register(h.Path)
		switch {
		case h.Dir:
			cells = append(cells, Button{Text: "📁 " + trunc(filepath.Base(h.Path), GridLabelRunes), CallbackData: cbDir(id, 0)})
		case h.Line > 0:
			cells = append(cells, Button{Text: "📄 " + trunc(fmt.Sprintf("%s:%d", filepath.Base(h.Path), h.Line), GridLabelRunes), CallbackData: cbFileN(id, h.Line)})
		default:
			cells = append(cells, Button{Text: "📄 " + trunc(filepath.Base(h.Path), GridLabelRunes), CallbackData: cbFile(id, 0)})
		}
	}
	rows := append(gridRows(cells), back)
	return Screen{Text: strings.TrimRight(b.String(), "\n"), Rows: rows}
}

// ------------------------------------------------------------------- recent

func (m *Manager) noteRecent(abs string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{abs}
	for _, r := range m.recent {
		if r != abs {
			out = append(out, r)
		}
	}
	if len(out) > MaxRecent {
		out = out[:MaxRecent]
	}
	m.recent = out
}

// RecentScreen lists the most recently opened files of this session.
func (m *Manager) RecentScreen(opts Options) Screen {
	m.mu.Lock()
	recent := append([]string(nil), m.recent...)
	m.mu.Unlock()
	nav := []Button{{Text: "🏠 Root", CallbackData: cbDir(0, 0)}, {Text: "⬅ Home", CallbackData: "ui:home"}}
	var b strings.Builder
	b.WriteString("🕘 **Recent files**\n\n")
	var cells []Button
	for _, p := range recent {
		if _, err := os.Stat(p); err != nil || checkInside(m.Root(), p) != nil {
			continue
		}
		b.WriteString("- 📄 " + code(m.Rel(p)) + "\n")
		cells = append(cells, Button{Text: "📄 " + trunc(filepath.Base(p), GridLabelRunes), CallbackData: cbFile(m.register(p), 0)})
	}
	if len(cells) == 0 {
		return Screen{Text: "🕘 **Recent files**\n\nNothing opened yet in this session.", Rows: [][]Button{nav}}
	}
	return Screen{Text: strings.TrimRight(b.String(), "\n"), Rows: append(gridRows(cells), nav)}
}

// ---------------------------------------------------------------------- zip

// ZipFolder writes folder dirID (without .git / node_modules / symlinks) to a
// temporary .zip and returns its path; the caller deletes it after sending.
func (m *Manager) ZipFolder(dirID int) (zipPath, name string, err error) {
	dir, err := m.Resolve(dirID)
	if err != nil {
		return "", "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", "", errors.New("only folders can be zipped")
	}
	base := filepath.Base(dir)
	tmp, err := os.CreateTemp("", "awaki-*.zip")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, string, error) {
		tmp.Close()
		os.Remove(tmpName)
		return "", "", err
	}
	zw := zip.NewWriter(tmp)
	var total int64
	files := 0
	werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || p == dir {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if total > MaxZipSource {
			return fmt.Errorf("folder is over %s — zip a subfolder instead", HumanSize(MaxZipSource))
		}
		rel, _ := filepath.Rel(dir, p)
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(filepath.Join(base, rel))
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		_, cerr := io.Copy(w, f)
		f.Close()
		if cerr != nil {
			return cerr
		}
		files++
		return nil
	})
	if werr != nil {
		return fail(werr)
	}
	if files == 0 {
		return fail(errors.New("the folder has no files to zip"))
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	st, err := tmp.Stat()
	if err != nil {
		return fail(err)
	}
	tmp.Close()
	if st.Size() > MaxSendBytes {
		os.Remove(tmpName)
		return "", "", fmt.Errorf("the zip is %s — over Telegram's %s limit; zip a subfolder instead", HumanSize(st.Size()), HumanSize(MaxSendBytes))
	}
	return tmpName, base + ".zip", nil
}
