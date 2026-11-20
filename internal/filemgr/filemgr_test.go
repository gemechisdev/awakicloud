package filemgr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTree(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("main.go", "package main\n\nfunc main() {}\n")
	mk("README.md", "# Hello\n")
	mk("go.mod", "module x\n")
	mk("src/a.go", "package src\n")
	mk("src/deep/b.txt", "bee\n")
	mk(".hidden", "h\n")
	m := New()
	if err := m.SetRoot(root); err != nil {
		t.Fatal(err)
	}
	return m, root
}

func allButtons(s Screen) []Button {
	var out []Button
	for _, r := range s.Rows {
		out = append(out, r...)
	}
	return out
}

func findBtn(t *testing.T, s Screen, contains string) Button {
	t.Helper()
	for _, b := range allButtons(s) {
		if strings.Contains(b.Text, contains) {
			return b
		}
	}
	t.Fatalf("no button containing %q in %+v", contains, allButtons(s))
	return Button{}
}

func act(t *testing.T, data string) Action {
	t.Helper()
	a, err := ParseCallback(data)
	if err != nil {
		t.Fatalf("parse %q: %v", data, err)
	}
	return a
}

// ---------------------------------------------------------------- index/jail

func TestRegisterResolveRoundTrip(t *testing.T) {
	m, root := newTree(t)
	abs := filepath.Join(root, "src", "a.go")
	id := m.touch(abs)
	if id == 0 {
		t.Fatal("child must not get the root id")
	}
	if again := m.touch(abs); again != id {
		t.Fatalf("ids must be stable: %d vs %d", id, again)
	}
	got, err := m.Resolve(id)
	if err != nil || got != abs {
		t.Fatalf("Resolve = %q, %v; want %q", got, err, abs)
	}
	if r, err := m.Resolve(0); err != nil || r != root {
		t.Fatalf("Resolve(0) = %q, %v", r, err)
	}
}

func TestResolveUnknownIDExpired(t *testing.T) {
	m, _ := newTree(t)
	if _, err := m.Resolve(9999); err != ErrExpired {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestPathOutsideRootRejected(t *testing.T) {
	m, root := newTree(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	m.mu.Lock()
	id := m.registerLocked(outside) // simulate a corrupted/forged index entry
	m.mu.Unlock()
	if _, err := m.Resolve(id); err != ErrOutside {
		t.Fatalf("err = %v, want ErrOutside", err)
	}
	// dot-dot traversal that cleans to outside
	m.mu.Lock()
	id2 := m.registerLocked(filepath.Join(root, "..", "x"))
	m.mu.Unlock()
	if _, err := m.Resolve(id2); err != ErrOutside {
		t.Fatalf("err = %v, want ErrOutside", err)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w", "/w/../x", false},
		{"/w", "/wx", false}, // prefix-but-not-child
		{"/w", "/", false},
	}
	for _, c := range cases {
		if got := Within(c.root, c.p); got != c.want {
			t.Errorf("Within(%q,%q) = %v, want %v", c.root, c.p, got, c.want)
		}
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	m, root := newTree(t)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if err := os.Symlink(secretDir, filepath.Join(root, "leakdir")); err != nil {
		t.Fatal(err)
	}
	// Listing shows them but offers no buttons.
	s := m.Dir(0, 0, Options{})
	if !strings.Contains(s.Text, "outside workspace") {
		t.Fatalf("expected outside marker in:\n%s", s.Text)
	}
	for _, b := range allButtons(s) {
		if strings.Contains(b.Text, "leak") {
			t.Fatalf("escaping symlink must not get a button: %+v", b)
		}
	}
	// Even a forged id for the link cannot be read.
	m.mu.Lock()
	id := m.registerLocked(filepath.Join(root, "leak.txt"))
	m.mu.Unlock()
	scr := m.File(id, 0, Options{})
	if strings.Contains(scr.Text, "top secret") {
		t.Fatal("content of file outside the workspace leaked")
	}
	if !strings.Contains(scr.Text, "outside the workspace") {
		t.Fatalf("want outside error, got:\n%s", scr.Text)
	}
	// And it cannot be deleted through the UI.
	if _, _, err := m.Delete(id); err != ErrOutside {
		t.Fatalf("Delete err = %v, want ErrOutside", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("secret file must still exist")
	}
}

func TestInternalSymlinkAllowed(t *testing.T) {
	m, root := newTree(t)
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "alias.go")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	s := m.Dir(0, 0, Options{})
	findBtn(t, s, "alias.go")
}

func TestSetRootResetsButKeepsIDsMonotonic(t *testing.T) {
	m, root := newTree(t)
	id := m.touch(filepath.Join(root, "main.go"))
	if err := m.SetRoot(root); err != nil { // same root: keep index
		t.Fatal(err)
	}
	if _, err := m.Resolve(id); err != nil {
		t.Fatalf("same-root SetRoot must keep ids: %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRoot(other); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(id); err != ErrExpired {
		t.Fatalf("old id after root change: err = %v, want ErrExpired", err)
	}
	newID := m.touch(filepath.Join(other, "main.go"))
	if newID <= id {
		t.Fatalf("ids must never be reused: new %d <= old %d", newID, id)
	}
	if r, _ := m.Resolve(0); r != other {
		t.Fatalf("id 0 must be the new root, got %q", r)
	}
}

func TestIndexCapResetNeverReusesIDs(t *testing.T) {
	m, root := newTree(t)
	first := m.touch(filepath.Join(root, "main.go"))
	for i := 0; i < MaxNodes+10; i++ {
		m.register(filepath.Join(root, fmt.Sprintf("f%d", i)))
	}
	m.touch(filepath.Join(root, "go.mod")) // triggers reset
	if m.Len() > 10 {
		t.Fatalf("index should have been reset, len = %d", m.Len())
	}
	if _, err := m.Resolve(first); err != ErrExpired {
		t.Fatalf("pre-reset id: err = %v, want ErrExpired", err)
	}
	if id := m.touch(filepath.Join(root, "main.go")); id <= first {
		t.Fatalf("id reused after reset: %d <= %d", id, first)
	}
}

func TestSetRootEmpty(t *testing.T) {
	if err := New().SetRoot(" "); err != ErrNoRoot {
		t.Fatalf("err = %v, want ErrNoRoot", err)
	}
}

// ------------------------------------------------------------------ callbacks

func TestParseCallback(t *testing.T) {
	good := map[string]Action{
		"files:d:0":          {Kind: "d", ID: 0},
		"files:d:42":         {Kind: "d", ID: 42},
		"files:d:1:2":        {Kind: "d", ID: 1, Arg: 2},
		"files:f:7":          {Kind: "f", ID: 7},
		"files:f:7:80":       {Kind: "f", ID: 7, Arg: 80},
		"files:copy:7":       {Kind: "copy", ID: 7},
		"files:edit:7":       {Kind: "edit", ID: 7},
		"files:delask:7":     {Kind: "delask", ID: 7},
		"files:del:7":        {Kind: "del", ID: 7},
		"files:d:2147483647": {Kind: "d", ID: 2147483647},
	}
	for in, want := range good {
		got, err := ParseCallback(in)
		if err != nil || got != want {
			t.Errorf("ParseCallback(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := []string{
		"", "files:", "files:d", "files:d:", "files:d:x", "files:d:-1", "files:d:1:2:3",
		"files:zz:1", "files:del:1:2", "files:copy:1:1", "ui:home", "files:d:99999999999",
		"files:d:1:", "files:f:1:abc", "files:d: 1",
	}
	for _, in := range bad {
		if _, err := ParseCallback(in); err == nil {
			t.Errorf("ParseCallback(%q) should fail", in)
		}
	}
}

// ------------------------------------------------------------------- listing

func TestListDirSortsDirsFirstCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"zeta", "Alpha", "beta"} {
		os.Mkdir(filepath.Join(root, d), 0o755)
	}
	for _, f := range []string{"b.txt", "A.txt", "c.txt", ".dot"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	l, err := ListDir(root, root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.Name)
	}
	want := []string{"Alpha", "beta", "zeta", ".dot", "A.txt", "b.txt", "c.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestListDirCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxList+25; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", i)), nil, 0o644)
	}
	l, err := ListDir(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != MaxList || !l.Truncated || l.Total != MaxList+25 {
		t.Fatalf("len=%d trunc=%v total=%d", len(l.Entries), l.Truncated, l.Total)
	}
	m := New()
	m.SetRoot(root)
	s := m.Dir(0, 0, Options{})
	if !strings.Contains(s.Text, "Too many entries") {
		t.Fatal("expected too-many-entries notice")
	}
}

func TestDirScreenRoot(t *testing.T) {
	m, root := newTree(t)
	s := m.Dir(0, 0, Options{})
	if !strings.Contains(s.Text, "📂 `"+filepath.Base(root)+"`") {
		t.Fatalf("header missing:\n%s", s.Text)
	}
	for _, want := range []string{"`src/`", "`main.go` · ", "`.hidden`"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("text missing %q:\n%s", want, s.Text)
		}
	}
	for _, label := range []string{"🔄 Refresh", "⬅ Home", "📁 src/", "📄 main.go"} {
		findBtn(t, s, label)
	}
	for _, b := range allButtons(s) {
		if strings.Contains(b.Text, "Up") || strings.Contains(b.Text, "Root") {
			t.Errorf("root screen must not show %q", b.Text)
		}
	}
}

func TestDirScreenSubdirHasUpAndRoot(t *testing.T) {
	m, _ := newTree(t)
	root := m.Dir(0, 0, Options{})
	src := act(t, findBtn(t, root, "📁 src/").CallbackData)
	s := m.Dir(src.ID, 0, Options{})
	up := findBtn(t, s, "⬆️ Up")
	if a := act(t, up.CallbackData); a.ID != 0 {
		t.Fatalf("Up from src should target root id 0, got %+v", a)
	}
	if a := act(t, findBtn(t, s, "🏠 Root").CallbackData); a.ID != 0 || a.Kind != KindDir {
		t.Fatalf("Root button = %+v", a)
	}
	// nested
	deep := act(t, findBtn(t, s, "📁 deep/").CallbackData)
	ds := m.Dir(deep.ID, 0, Options{})
	upDeep := act(t, findBtn(t, ds, "⬆️ Up").CallbackData)
	if upDeep.ID != src.ID {
		t.Fatalf("Up from deep = %d, want src id %d", upDeep.ID, src.ID)
	}
}

func TestEmptyDirectory(t *testing.T) {
	m, root := newTree(t)
	os.Mkdir(filepath.Join(root, "void"), 0o755)
	s := m.Dir(0, 0, Options{})
	void := act(t, findBtn(t, s, "📁 void/").CallbackData)
	es := m.Dir(void.ID, 0, Options{})
	if !strings.Contains(es.Text, "Empty directory") {
		t.Fatalf("want Empty directory:\n%s", es.Text)
	}
	findBtn(t, es, "⬆️ Up")
	findBtn(t, es, "🏠 Root")
}

func TestPagination(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644)
	}
	m := New()
	m.SetRoot(root)
	p0 := m.Dir(0, 0, Options{})
	if !strings.Contains(p0.Text, "page 1/3") {
		t.Fatalf("page 1 header:\n%s", p0.Text)
	}
	n := 0
	for _, b := range allButtons(p0) {
		if strings.HasPrefix(b.Text, "📄") {
			n++
		}
	}
	if n != PageSize {
		t.Fatalf("page has %d file buttons, want %d", n, PageSize)
	}
	for _, b := range allButtons(p0) {
		if b.Text == "◀️ Prev" {
			t.Fatal("first page must not have Prev")
		}
	}
	next := act(t, findBtn(t, p0, "Next").CallbackData)
	if next.Arg != 1 {
		t.Fatalf("Next arg = %d, want 1", next.Arg)
	}
	p2 := m.Handle(next, Options{}).Screen
	if !strings.Contains(p2.Text, "page 2/3") || !strings.Contains(p2.Text, "f12.txt") {
		t.Fatalf("page 2:\n%s", p2.Text)
	}
	last := m.Dir(0, 99, Options{}) // clamps
	if !strings.Contains(last.Text, "page 3/3") {
		t.Fatalf("clamped page:\n%s", last.Text)
	}
	for _, b := range allButtons(last) {
		if strings.HasPrefix(b.Text, "Next") {
			t.Fatal("last page must not have Next")
		}
	}
	// Refresh keeps the page.
	if a := act(t, findBtn(t, p2, "Refresh").CallbackData); a.Arg != 1 {
		t.Fatalf("Refresh arg = %d, want 1", a.Arg)
	}
}

// ---------------------------------------------------------------------- file

func TestFileViewBasic(t *testing.T) {
	m, _ := newTree(t)
	root := m.Dir(0, 0, Options{AllowDelete: true})
	f := act(t, findBtn(t, root, "📄 main.go").CallbackData)
	s := m.Handle(f, Options{AllowDelete: true}).Screen
	for _, want := range []string{"`main.go`", "lines 1–3 of 3", "```go\npackage main", "func main() {}\n```"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("file view missing %q:\n%s", want, s.Text)
		}
	}
	for _, label := range []string{"Copy path", "Reload", "⬆️ Folder", "Ask agent to edit", "🗑 Delete", "Home"} {
		findBtn(t, s, label)
	}
	cp := findBtn(t, s, "Copy path")
	if cp.CopyText != "main.go" {
		t.Fatalf("CopyText = %q", cp.CopyText)
	}
	if cp.CallbackData == "" {
		t.Fatal("copy button needs a callback fallback")
	}
}

func TestFilePagingByLines(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&sb, "line number %03d: %s\n", i, strings.Repeat("x", 40))
	}
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(sb.String()), 0o644)
	m := New()
	m.SetRoot(root)
	s := m.Open("big.txt", Options{}).Screen
	if !strings.Contains(s.Text, "lines 1–") || !strings.Contains(s.Text, "of 400") {
		t.Fatalf("header:\n%s", firstLines(s.Text, 3))
	}
	more := act(t, findBtn(t, s, "⬇ More").CallbackData)
	if more.Kind != KindFile || more.Arg < 2 {
		t.Fatalf("More = %+v", more)
	}
	// Continuation starts exactly where the first page stopped (no gap/overlap).
	pg1, _ := ReadPage(filepath.Join(root, "big.txt"), 1, PreviewRunes)
	if more.Arg != pg1.Last+1 {
		t.Fatalf("More arg %d != last+1 (%d)", more.Arg, pg1.Last+1)
	}
	s2 := m.Handle(more, Options{}).Screen
	if !strings.Contains(s2.Text, fmt.Sprintf("line number %03d", more.Arg)) {
		t.Fatalf("page 2 should start at line %d:\n%s", more.Arg, firstLines(s2.Text, 6))
	}
	findBtn(t, s2, "⏮ Start")
	// Reload on a continued page stays on that page.
	if a := act(t, findBtn(t, s2, "Reload").CallbackData); a.Arg != more.Arg {
		t.Fatalf("Reload arg = %d, want %d", a.Arg, more.Arg)
	}
	// Walk to the end: no More on last page, every line seen exactly once.
	seen := 0
	cur := Action{Kind: KindFile, ID: more.ID}
	for i := 0; i < 100; i++ {
		pg, err := ReadPage(filepath.Join(root, "big.txt"), cur.Arg, PreviewRunes)
		if err != nil {
			t.Fatal(err)
		}
		seen += pg.Last - pg.First + 1
		if !pg.More {
			break
		}
		cur.Arg = pg.Next
	}
	_ = seen
	total := 0
	off := 1
	for {
		pg, _ := ReadPage(filepath.Join(root, "big.txt"), off, PreviewRunes)
		total += pg.Last - pg.First + 1
		if !pg.More {
			break
		}
		off = pg.Next
	}
	if total != 400 {
		t.Fatalf("paged through %d lines, want 400", total)
	}
}

func firstLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, "\n")
}

func TestOffsetPastEndFallsBack(t *testing.T) {
	m, _ := newTree(t)
	s := m.Open("main.go", Options{}).Screen
	id := act(t, findBtn(t, s, "Reload").CallbackData).ID
	s2 := m.File(id, 5000, Options{})
	if !strings.Contains(s2.Text, "package main") {
		t.Fatalf("expected fallback to start:\n%s", s2.Text)
	}
}

func TestBinaryGuard(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 3, 0, 0xff}, 0o644)
	m := New()
	m.SetRoot(root)
	s := m.Open("blob.bin", Options{AllowDelete: true}).Screen
	if !strings.Contains(s.Text, "Binary file") || strings.Contains(s.Text, "```") {
		t.Fatalf("binary should show metadata only:\n%s", s.Text)
	}
	if strings.Contains(s.Text, "ELF") {
		t.Fatal("raw bytes leaked into the message")
	}
	findBtn(t, s, "Copy path")
	findBtn(t, s, "🗑 Delete")
}

func TestEmptyFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "e.txt"), nil, 0o644)
	m := New()
	m.SetRoot(root)
	s := m.Open("e.txt", Options{}).Screen
	if !strings.Contains(s.Text, "(empty file)") {
		t.Fatalf("got:\n%s", s.Text)
	}
}

func TestFenceInContentIsNeutralised(t *testing.T) {
	root := t.TempDir()
	body := "# doc\n```go\nfmt.Println(1)\n```\ntext ==x== ||y||\n"
	os.WriteFile(filepath.Join(root, "n.md"), []byte(body), 0o644)
	m := New()
	m.SetRoot(root)
	s := m.Open("n.md", Options{}).Screen
	// Only our own opening and closing fence may remain.
	if c := strings.Count(s.Text, "```"); c != 2 {
		t.Fatalf("found %d fences, want exactly 2 (ours):\n%s", c, s.Text)
	}
	if !strings.Contains(s.Text, "```markdown\n") {
		t.Fatalf("lang hint missing:\n%s", s.Text)
	}
}

func TestSanitizeControlCharsCRLFTabsBOMInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	raw := "\uFEFFfirst\r\n\tindented\x01\x1b[31mred\r\nbad\xff\xfeutf8\r\nlast-no-newline"
	os.WriteFile(filepath.Join(root, "w.txt"), []byte(raw), 0o644)
	pg, err := ReadPage(filepath.Join(root, "w.txt"), 1, PreviewRunes)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 4 || pg.Last != 4 || pg.More {
		t.Fatalf("total=%d last=%d more=%v", pg.Total, pg.Last, pg.More)
	}
	lines := strings.Split(pg.Text, "\n")
	if lines[0] != "first" {
		t.Errorf("BOM/CR not stripped: %q", lines[0])
	}
	if lines[1] != "    indented[31mred" {
		t.Errorf("control chars/tab: %q", lines[1])
	}
	if !strings.Contains(lines[2], "\uFFFD") || !strings.HasSuffix(lines[2], "utf8") {
		t.Errorf("invalid utf8: %q", lines[2])
	}
	if lines[3] != "last-no-newline" {
		t.Errorf("unterminated last line: %q", lines[3])
	}
}

func TestLongLineClipped(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "min.js"), []byte(strings.Repeat("a", 200000)+"\nnext\n"), 0o644)
	pg, err := ReadPage(filepath.Join(root, "min.js"), 1, PreviewRunes)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Split(pg.Text, "\n")[0]
	if len([]rune(first)) > MaxLineRunes+len([]rune(clippedNote)) {
		t.Fatalf("line not clipped: %d runes", len([]rune(first)))
	}
	if !strings.HasSuffix(first, "[line clipped]") {
		t.Fatalf("clip marker missing: %q", first[len(first)-30:])
	}
	if pg.Total != 2 {
		t.Fatalf("total = %d, want 2", pg.Total)
	}
}

func TestMultibyteBudgetAndClip(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("አማርኛ", 100) // Ge'ez script, 400 runes
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(line + "\n")
	}
	os.WriteFile(filepath.Join(root, "am.txt"), []byte(sb.String()), 0o644)
	pg, _ := ReadPage(filepath.Join(root, "am.txt"), 1, PreviewRunes)
	if !pg.More {
		t.Fatal("expected paging")
	}
	if n := len([]rune(pg.Text)); n > PreviewRunes {
		t.Fatalf("visible runes %d > budget %d", n, PreviewRunes)
	}
}

func TestSingleHugeLineStillShowsSomething(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "one.txt"), []byte(strings.Repeat("z", 50000)), 0o644)
	pg, _ := ReadPage(filepath.Join(root, "one.txt"), 1, 100)
	if pg.First != 1 || pg.Text == "" {
		t.Fatalf("must emit at least one line: %+v", pg)
	}
}

// -------------------------------------------------------------------- delete

func TestDeleteRequiresConfirmAndWorks(t *testing.T) {
	m, root := newTree(t)
	opts := Options{AllowDelete: true}
	f := act(t, findBtn(t, m.Dir(0, 0, opts), "📄 go.mod").CallbackData)
	fv := m.Handle(f, opts).Screen
	ask := act(t, findBtn(t, fv, "🗑 Delete").CallbackData)
	if ask.Kind != KindDelAsk {
		t.Fatalf("Delete button must open confirmation, got %q", ask.Kind)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("file must survive until confirmed")
	}
	conf := m.Handle(ask, opts).Screen
	if !strings.Contains(conf.Text, "Delete `go.mod`?") || !strings.Contains(conf.Text, "cannot be undone") {
		t.Fatalf("confirm text:\n%s", conf.Text)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("file must survive the confirm screen")
	}
	cancel := act(t, findBtn(t, conf, "Cancel").CallbackData)
	if cancel.Kind != KindFile || cancel.ID != f.ID {
		t.Fatalf("Cancel = %+v", cancel)
	}
	del := act(t, findBtn(t, conf, "✅ Delete").CallbackData)
	if del.Kind != KindDelete {
		t.Fatalf("confirm action = %q", del.Kind)
	}
	res := m.Handle(del, opts)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
	if !strings.Contains(res.Screen.Text, "Deleted `go.mod`") {
		t.Fatalf("post-delete screen:\n%s", res.Screen.Text)
	}
	if strings.Contains(res.Screen.Text, "📄 `go.mod`") {
		t.Fatal("deleted file still listed")
	}
	findBtn(t, res.Screen, "🔄 Refresh") // back on the parent listing
	// Double-tap: second delete reports not found, no panic.
	again := m.Handle(del, opts)
	if !strings.Contains(again.Screen.Text, "⚠️") {
		t.Fatalf("second delete should warn:\n%s", again.Screen.Text)
	}
}

func TestDeleteDisabledInPlanMode(t *testing.T) {
	m, root := newTree(t)
	plan := Options{AllowDelete: false}
	fv := m.Open("go.mod", plan).Screen
	for _, b := range allButtons(fv) {
		if strings.Contains(b.Text, "Delete") {
			t.Fatalf("plan mode must hide Delete: %+v", b)
		}
	}
	id := act(t, findBtn(t, fv, "Reload").CallbackData).ID
	for _, kind := range []string{KindDelAsk, KindDelete} {
		res := m.Handle(Action{Kind: kind, ID: id}, plan)
		if !strings.Contains(res.Screen.Text, "disabled in plan mode") {
			t.Fatalf("%s: want refusal, got:\n%s", kind, res.Screen.Text)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("plan mode must not delete")
	}
}

func TestDeleteRefusesDirsAndRoot(t *testing.T) {
	m, root := newTree(t)
	opts := Options{AllowDelete: true}
	src := act(t, findBtn(t, m.Dir(0, 0, opts), "📁 src/").CallbackData)
	for _, kind := range []string{KindDelAsk, KindDelete} {
		res := m.Handle(Action{Kind: kind, ID: src.ID}, opts)
		if !strings.Contains(res.Screen.Text, "only files") {
			t.Fatalf("%s on dir: %s", kind, res.Screen.Text)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "src", "a.go")); err != nil {
		t.Fatal("directory contents must be untouched")
	}
	if _, _, err := m.Delete(0); err == nil {
		t.Fatal("root delete must fail")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root must exist")
	}
}

// ------------------------------------------------------------ copy/edit/open

func TestCopyAndEditActions(t *testing.T) {
	m, _ := newTree(t)
	opts := Options{AllowDelete: true}
	s := m.Open("src/a.go", opts).Screen
	id := act(t, findBtn(t, s, "Reload").CallbackData).ID

	cp := m.Handle(Action{Kind: KindCopy, ID: id}, opts)
	if !cp.NoScreen || cp.Copy != "src/a.go" {
		t.Fatalf("copy result = %+v", cp)
	}
	ed := m.Handle(Action{Kind: KindEdit, ID: id}, opts)
	if ed.Inject != "Focus file: src/a.go. Wait for my edit instructions." {
		t.Fatalf("inject = %q", ed.Inject)
	}
	if !strings.Contains(ed.Screen.Text, "Agent has this file as context") {
		t.Fatalf("edit card should confirm:\n%s", ed.Screen.Text)
	}
	if !strings.Contains(ed.Screen.Text, "package src") {
		t.Fatal("edit card should still show the file")
	}
	// Edit bridge stays available in plan mode (agent itself is read-only there).
	if r := m.Handle(Action{Kind: KindEdit, ID: id}, Options{}); r.Inject == "" {
		t.Fatal("edit bridge must work with AllowDelete=false")
	}
}

func TestOpenPaths(t *testing.T) {
	m, root := newTree(t)
	if s := m.Open("", Options{}).Screen; !strings.Contains(s.Text, "📂") {
		t.Fatalf("root:\n%s", s.Text)
	}
	if s := m.Open("src", Options{}).Screen; !strings.Contains(s.Text, "`a.go`") {
		t.Fatalf("src:\n%s", s.Text)
	}
	if s := m.Open("src/deep/b.txt", Options{}).Screen; !strings.Contains(s.Text, "bee") {
		t.Fatalf("file:\n%s", s.Text)
	}
	if s := m.Open(filepath.Join(root, "src"), Options{}).Screen; !strings.Contains(s.Text, "`a.go`") {
		t.Fatalf("absolute inside root:\n%s", s.Text)
	}
	for _, bad := range []string{"../", "../../etc/passwd", "/etc/passwd", "src/../../x"} {
		s := m.Open(bad, Options{}).Screen
		if !strings.Contains(s.Text, "⚠️") {
			t.Errorf("Open(%q) must be refused, got:\n%s", bad, s.Text)
		}
	}
	if s := m.Open("nope.txt", Options{}).Screen; !strings.Contains(s.Text, "no longer exists") {
		t.Fatalf("missing:\n%s", s.Text)
	}
}

func TestOpenWithoutRoot(t *testing.T) {
	s := New().Open("", Options{}).Screen
	if !strings.Contains(s.Text, "no workspace") {
		t.Fatalf("got:\n%s", s.Text)
	}
}

func TestStaleButtonShowsExpired(t *testing.T) {
	m, _ := newTree(t)
	s := m.Handle(Action{Kind: KindFile, ID: 4242}, Options{}).Screen
	if !strings.Contains(s.Text, "expired") {
		t.Fatalf("got:\n%s", s.Text)
	}
	findBtn(t, s, "Root")
}

// Every callback_data we emit must be ≤ 64 bytes and parse back.
func TestAllCallbacksValidAndShort(t *testing.T) {
	m, root := newTree(t)
	for i := 0; i < 40; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("n%02d.txt", i)), []byte(strings.Repeat("l\n", 3000)), 0o644)
	}
	opts := Options{AllowDelete: true}
	screens := []Screen{
		m.Dir(0, 0, opts), m.Dir(0, 2, opts), m.Open("n01.txt", opts).Screen,
		m.Open("n01.txt", opts).Screen, m.Open("main.go", opts).Screen,
	}
	id := act(t, findBtn(t, screens[2], "Reload").CallbackData).ID
	screens = append(screens, m.File(id, 2500, opts), m.DeleteConfirm(id), ErrorScreen(ErrExpired))
	for _, s := range screens {
		for _, b := range allButtons(s) {
			if len(b.CallbackData) == 0 || len(b.CallbackData) > 64 {
				t.Errorf("bad callback_data %q on %q", b.CallbackData, b.Text)
			}
			if strings.HasPrefix(b.CallbackData, CallbackPrefix) {
				if _, err := ParseCallback(b.CallbackData); err != nil {
					t.Errorf("unparseable %q: %v", b.CallbackData, err)
				}
			}
			if len(b.Text) == 0 {
				t.Error("empty button label")
			}
		}
	}
}

func TestWeirdFileNamesDoNotBreakMarkdown(t *testing.T) {
	root := t.TempDir()
	name := "we`ird\nname.txt"
	if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o644); err != nil {
		t.Skip("filesystem refuses odd names:", err)
	}
	m := New()
	m.SetRoot(root)
	s := m.Dir(0, 0, Options{})
	if strings.Count(s.Text, "`")%2 != 0 {
		t.Fatalf("unbalanced backticks:\n%s", s.Text)
	}
	if strings.Contains(s.Text, "we`ird") {
		t.Fatal("backtick must be neutralised")
	}
	for _, line := range strings.Split(s.Text, "\n") {
		if strings.Contains(line, "ird") && !strings.HasPrefix(line, "- ") {
			t.Fatalf("newline in name split the entry: %q", line)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 14540: "14.2 KB", 5 << 20: "5.0 MB"}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestLang(t *testing.T) {
	cases := map[string]string{"main.go": "go", "A.PY": "python", "Makefile": "makefile", "noext": "", "x.unknown": "", "t.tsx": "typescript"}
	for in, want := range cases {
		if got := Lang(in); got != want {
			t.Errorf("Lang(%q) = %q, want %q", in, got, want)
		}
	}
}
