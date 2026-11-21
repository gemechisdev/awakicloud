package filemgr

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func searchTree(t *testing.T) (*Manager, string) {
	m, root := newTree(t)
	write(t, root, "src/util/helper.go", "package util\n\n// TODO: refactor the Helper\nfunc Helper() {}\n")
	write(t, root, "docs/Guide.md", "# Guide\nThe helper is documented here.\nsecond todo item\n")
	write(t, root, ".git/config", "helper = secret\n")
	write(t, root, "node_modules/lib/index.js", "helper()\n")
	os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0, 'h', 'e', 'l', 'p', 'e', 'r'}, 0o644)
	return m, root
}

func rels(hs []Hit) string {
	var out []string
	for _, h := range hs {
		s := h.Rel
		if h.Line > 0 {
			s += fmt.Sprintf(":%d", h.Line)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestSearchNamesAndContentsCaseInsensitive(t *testing.T) {
	m, _ := searchTree(t)
	hits, trunc, err := m.Search(0, "helper")
	if err != nil || trunc {
		t.Fatal(err, trunc)
	}
	got := rels(hits)
	for _, want := range []string{"src/util/helper.go", "src/util/helper.go:3", "src/util/helper.go:4", "docs/Guide.md:2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, bad := range []string{".git", "node_modules", "blob.bin"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s must be skipped: %s", bad, got)
		}
	}
}

func TestSearchFolderNamesRegexAndScope(t *testing.T) {
	m, root := searchTree(t)
	hits, _, _ := m.Search(0, "util")
	if !strings.Contains(rels(hits), "src/util") {
		t.Fatalf("folder name match: %s", rels(hits))
	}
	hits, _, err := m.Search(0, `re:^func \w+\(\)`)
	if err != nil || !strings.Contains(rels(hits), "src/util/helper.go:4") {
		t.Fatalf("regex: %v %s", err, rels(hits))
	}
	if _, _, err := m.Search(0, "re:("); err == nil {
		t.Fatal("bad regex must error")
	}
	if _, _, err := m.Search(0, "  "); err == nil {
		t.Fatal("empty query must error")
	}
	// scoped to a subfolder id
	docs := m.touch(filepath.Join(root, "docs"))
	hits, _, _ = m.Search(docs, "todo")
	if rels(hits) != "docs/Guide.md:3" {
		t.Fatalf("scoped: %s", rels(hits))
	}
	if _, _, err := m.Search(m.touch(filepath.Join(root, "main.go")), "x"); err == nil {
		t.Fatal("search needs a folder")
	}
}

func TestSearchDoesNotFollowSymlinksAndCaps(t *testing.T) {
	m, root := searchTree(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "o.txt"), []byte("helper outside\n"), 0o644)
	if os.Symlink(outside, filepath.Join(root, "ln")) == nil {
		hits, _, _ := m.Search(0, "outside")
		if len(hits) != 0 {
			t.Fatalf("symlink followed: %s", rels(hits))
		}
	}
	for i := 0; i < 60; i++ {
		write(t, root, fmt.Sprintf("many/f%02d.txt", i), "needle here\n")
	}
	hits, truncated, _ := m.Search(0, "needle")
	if len(hits) != MaxSearchHits || !truncated {
		t.Fatalf("cap: %d %v", len(hits), truncated)
	}
}

func TestSearchScreen(t *testing.T) {
	m, _ := searchTree(t)
	s := m.SearchScreen(0, "helper", rw)
	if !strings.Contains(s.Text, "🔍 `helper`") || !strings.Contains(s.Text, "`src/util/helper.go:3`") {
		t.Fatalf("text:\n%s", s.Text)
	}
	// a content hit opens the numbered view at that line
	b := findBtn(t, s, "helper.go:3")
	if a := act(t, b.CallbackData); a.Kind != KindFileNum || a.Arg != 3 {
		t.Fatalf("hit button = %+v", a)
	}
	opened := m.Handle(act(t, b.CallbackData), rw).Screen
	if !strings.Contains(opened.Text, "3│ // TODO") {
		t.Fatalf("opened at hit:\n%s", opened.Text)
	}
	if none := m.SearchScreen(0, "zzzzzz", rw); !strings.Contains(none.Text, "No matches") {
		t.Fatalf("none:\n%s", none.Text)
	}
	if bad := m.SearchScreen(0, "re:(", rw); !strings.Contains(bad.Text, "invalid regular expression") {
		t.Fatalf("bad:\n%s", bad.Text)
	}
	for _, b := range allButtons(s) {
		if len(b.CallbackData) > 64 {
			t.Fatalf("callback too long: %q", b.CallbackData)
		}
	}
}

func TestSearchPromptWorksInPlanMode(t *testing.T) {
	m, _ := searchTree(t)
	plan := Options{}
	d := m.Dir(0, 0, plan)
	for _, l := range []string{"🔍 Search", "📦 ZIP", "🕘 Recent"} {
		findBtn(t, d, l)
	}
	r := m.Handle(Action{Kind: KindSearch, ID: 0}, plan)
	if r.Prompt == nil || r.Prompt.Kind != WizardSearch {
		t.Fatalf("prompt: %+v", r)
	}
	out := m.Submit(WizardSearch, r.Prompt.ID, "helper", plan)
	if !strings.Contains(out.Screen.Text, "helper.go") {
		t.Fatalf("plan-mode search:\n%s", out.Screen.Text)
	}
	if a := act(t, findBtn(t, r.Screen, "Cancel").CallbackData); a.Kind != KindDir {
		t.Fatal("cancel returns to folder")
	}
}

func TestRecentFiles(t *testing.T) {
	m, root := searchTree(t)
	if s := m.RecentScreen(rw); !strings.Contains(s.Text, "Nothing opened") {
		t.Fatalf("empty:\n%s", s.Text)
	}
	for _, f := range []string{"main.go", "go.mod", "README.md", "main.go"} {
		m.Open(f, rw)
	}
	s := m.RecentScreen(rw)
	i1, i2 := strings.Index(s.Text, "`main.go`"), strings.Index(s.Text, "`README.md`")
	if i1 < 0 || i2 < 0 || i1 > i2 || strings.Count(s.Text, "main.go") != 1 {
		t.Fatalf("order/dedupe wrong:\n%s", s.Text)
	}
	os.Remove(filepath.Join(root, "go.mod"))
	if strings.Contains(m.RecentScreen(rw).Text, "go.mod") {
		t.Fatal("deleted files must disappear")
	}
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("r%d.txt", i), "x")
		m.Open(fmt.Sprintf("r%d.txt", i), rw)
	}
	if n := strings.Count(m.RecentScreen(rw).Text, "\n- "); n != MaxRecent {
		t.Fatalf("recent cap: %d", n+1)
	}
	b := findBtn(t, m.RecentScreen(rw), "r19.txt")
	if act(t, b.CallbackData).Kind != KindFile {
		t.Fatal("recent button opens file")
	}
}

func TestZipFolder(t *testing.T) {
	m, root := searchTree(t)
	os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "alias.go"))
	zp, name, err := m.ZipFolder(0)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zp)
	if name != filepath.Base(root)+".zip" {
		t.Fatalf("name = %q", name)
	}
	zr, err := zip.OpenReader(zp)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	all := strings.Join(names, "\n")
	for _, want := range []string{"/main.go", "/src/util/helper.go", "/docs/Guide.md"} {
		if !strings.Contains(all, want) {
			t.Errorf("zip lacks %s:\n%s", want, all)
		}
	}
	for _, bad := range []string{".git/", "node_modules", "alias.go"} {
		if strings.Contains(all, bad) {
			t.Errorf("zip must exclude %s:\n%s", bad, all)
		}
	}
	for _, n := range names {
		if !strings.HasPrefix(n, filepath.Base(root)+"/") || strings.Contains(n, "..") {
			t.Errorf("bad entry name %q", n)
		}
	}
	// subfolder + handler result
	src := m.touch(filepath.Join(root, "src"))
	r := m.Handle(Action{Kind: KindZip, ID: src}, Options{})
	if !r.SendTemp || !r.NoScreen || r.SendCaption != "src.zip" {
		t.Fatalf("handle: %+v", r)
	}
	os.Remove(r.SendPath)
	// errors
	os.MkdirAll(filepath.Join(root, "void"), 0o755)
	if _, _, err := m.ZipFolder(m.touch(filepath.Join(root, "void"))); err == nil {
		t.Fatal("empty folder must fail")
	}
	if _, _, err := m.ZipFolder(m.touch(filepath.Join(root, "main.go"))); err == nil {
		t.Fatal("file must fail")
	}
	leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), "awaki-*.zip"))
	for _, l := range leftovers {
		if l != zp {
			t.Errorf("temp zip leaked on error: %s", l)
		}
	}
}

func TestNewReadOnlyCallbacksParse(t *testing.T) {
	for _, d := range []string{"files:find:3", "files:zip:3", "files:rec:0"} {
		if _, err := ParseCallback(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
}
