package filemgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileID(t *testing.T, m *Manager, root, rel string) int {
	t.Helper()
	return m.touch(filepath.Join(root, rel))
}

func TestLineEditDiffStyle(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "l.txt", "one\ntwo\nthree\nfour\nfive\n")
	id := fileID(t, m, root, "l.txt")
	spec := "@2\n-two\n+TWO\n+TWO-B\n three\n@5\n+six\n" // second block inserts before line 5 ("five")
	// note: second block is a pure insert before line 5
	res, err := m.ApplyLineEdits(id, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := "one\nTWO\nTWO-B\nthree\nfour\nsix\nfive\n"
	if got := read(t, root, "l.txt"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if res.Blocks != 2 || res.Added != 3 || res.Removed != 1 || res.Seq == 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := m.Undo(res.Seq); err != nil || read(t, root, "l.txt") != "one\ntwo\nthree\nfour\nfive\n" {
		t.Fatalf("undo: %v %q", err, read(t, root, "l.txt"))
	}
}

func TestLineEditAppendAtEndAndBlankKeep(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "e.txt", "a\n\nb\n")
	id := fileID(t, m, root, "e.txt")
	if _, err := m.ApplyLineEdits(id, "@4\n+c\n+d"); err != nil { // @len+1 appends
		t.Fatal(err)
	}
	if read(t, root, "e.txt") != "a\n\nb\nc\nd\n" {
		t.Fatalf("got %q", read(t, root, "e.txt"))
	}
	// blank line mid-body means "keep the blank line"
	if _, err := m.ApplyLineEdits(id, "@1\n a\n\n-b\n+B"); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "e.txt") != "a\n\nB\nc\nd\n" {
		t.Fatalf("got %q", read(t, root, "e.txt"))
	}
}

func TestLineEditRangeReplaceAndDelete(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "r.txt", "1\n2\n3\n4\n5\n")
	id := fileID(t, m, root, "r.txt")
	if _, err := m.ApplyLineEdits(id, "@2-3\nX\nY\nZ"); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "r.txt") != "1\nX\nY\nZ\n4\n5\n" {
		t.Fatalf("got %q", read(t, root, "r.txt"))
	}
	if _, err := m.ApplyLineEdits(id, "@2-4\n"+EmptyReplacement); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "r.txt") != "1\n4\n5\n" {
		t.Fatalf("got %q", read(t, root, "r.txt"))
	}
}

func TestLineEditGuardsNothingChangedOnError(t *testing.T) {
	m, root := newTree(t)
	orig := "a\nb\nc\n"
	write(t, root, "g.txt", orig)
	id := fileID(t, m, root, "g.txt")
	bad := map[string]string{
		"mismatch":      "@2\n-WRONG\n+x",
		"past end":      "@9\n+x",
		"range past":    "@2-9\nx",
		"backwards":     "@3-1\nx",
		"no header":     "+x",
		"empty":         "",
		"no body":       "@2",
		"bad prefix":    "@1\nplain",
		"overlap":       "@1-2\nX\n@2-3\nY",
		"zero":          "@0\n+x",
		"ran off end":   "@3\n-c\n-d",
		"unchanged":     "@1\n a",
		"keep mismatch": "@1\n zz",
	}
	for name, spec := range bad {
		if _, err := m.ApplyLineEdits(id, spec); err == nil {
			t.Errorf("%s: should fail", name)
		}
		if got := read(t, root, "g.txt"); got != orig {
			t.Fatalf("%s: file modified: %q", name, got)
		}
	}
	bin := filepath.Join(root, "b.bin")
	os.WriteFile(bin, []byte{0, 1}, 0o644)
	if _, err := m.ApplyLineEdits(m.touch(bin), "@1\n+x"); err == nil {
		t.Fatal("binary must fail")
	}
}

func TestLineEditCRLFAndFence(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "c.txt", "a\r\nb\r\n")
	id := fileID(t, m, root, "c.txt")
	if _, err := m.ApplyLineEdits(id, "```\n@2\n-b\n+B\n```"); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "c.txt") != "a\r\nB\r\n" {
		t.Fatalf("got %q", read(t, root, "c.txt"))
	}
}

func TestReplaceWholeFileUndo(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "w.txt", "old\n")
	id := fileID(t, m, root, "w.txt")
	seq, err := m.ReplaceWholeFile(id, []byte("brand new\n"))
	if err != nil || read(t, root, "w.txt") != "brand new\n" {
		t.Fatalf("%v %q", err, read(t, root, "w.txt"))
	}
	if _, err := m.Undo(seq); err != nil || read(t, root, "w.txt") != "old\n" {
		t.Fatal("undo failed")
	}
	if _, err := m.ReplaceWholeFile(m.touch(filepath.Join(root, "src")), []byte("x")); err == nil {
		t.Fatal("folder must fail")
	}
	if _, err := m.ReplaceWholeFile(id, make([]byte, MaxUploadBytes+1)); err == nil {
		t.Fatal("oversize must fail")
	}
}

func TestFileBlocks(t *testing.T) {
	m, root := newTree(t)
	text := "File: pkg/a.go\n```go\npackage pkg\n\nfunc A() {}\n```\n\n// path: docs/readme.md\n# Title\nbody line\n\n`File: notes.txt`\nplain notes"
	res, err := m.ApplyPatchOrBlocks(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 3 || res.Seq == 0 {
		t.Fatalf("res = %+v", res)
	}
	if read(t, root, "pkg/a.go") != "package pkg\n\nfunc A() {}\n" {
		t.Fatalf("a.go = %q", read(t, root, "pkg/a.go"))
	}
	if read(t, root, "docs/readme.md") != "# Title\nbody line\n" || read(t, root, "notes.txt") != "plain notes\n" {
		t.Fatal("other blocks wrong")
	}
	// overwrite existing + undo
	res2, err := m.ApplyPatchOrBlocks("File: notes.txt\nreplaced")
	if err != nil || res2.Files[0].Action != "modified" {
		t.Fatalf("%v %+v", err, res2)
	}
	if _, err := m.Undo(res2.Seq); err != nil || read(t, root, "notes.txt") != "plain notes\n" {
		t.Fatal("undo of block overwrite failed")
	}
}

func TestFileBlocksSafetyAndFallback(t *testing.T) {
	m, root := newTree(t)
	for _, bad := range []string{"File: ../evil.txt\nx", "File: /etc/x\nx", "File: a.txt\n1\nFile: a.txt\n2", "just words"} {
		if _, err := m.ApplyPatchOrBlocks(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); err == nil {
		t.Fatal("escaped the workspace")
	}
	// a real diff still goes through the patch path
	write(t, root, "d.txt", "x\n")
	if _, err := m.ApplyPatchOrBlocks("--- a/d.txt\n+++ b/d.txt\n@@ -1 +1 @@\n-x\n+y\n"); err != nil || read(t, root, "d.txt") != "y\n" {
		t.Fatalf("diff path: %v", err)
	}
}

func TestFileBlockContentWithHeaderInsideFence(t *testing.T) {
	m, root := newTree(t)
	text := "File: doc.md\n```markdown\nFile: not-a-header.txt\ntext\n```"
	if _, err := m.ApplyPatchOrBlocks(text); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "doc.md") != "File: not-a-header.txt\ntext\n" {
		t.Fatalf("got %q", read(t, root, "doc.md"))
	}
	if _, err := os.Stat(filepath.Join(root, "not-a-header.txt")); err == nil {
		t.Fatal("header inside a fence must not start a new file")
	}
}

// ------------------------------------------------------------ line numbers

func TestNumberedViewAndToggle(t *testing.T) {
	m, root := newTree(t)
	var sb strings.Builder
	for i := 1; i <= 120; i++ {
		sb.WriteString("row with some padding text to make pages\n")
	}
	write(t, root, "n.txt", sb.String())
	s := m.Open("n.txt", rw).Screen
	toggle := act(t, findBtn(t, s, "🔢 Lines").CallbackData)
	if toggle.Kind != KindFileNum {
		t.Fatalf("toggle = %+v", toggle)
	}
	ns := m.Handle(toggle, rw).Screen
	if !strings.Contains(ns.Text, "1│ row") || strings.Contains(ns.Text, "```text") {
		t.Fatalf("numbered view:\n%s", firstLines(ns.Text, 6))
	}
	findBtn(t, ns, "🔢 Off")
	// paging stays in numbers mode
	more := findBtn(t, ns, "⬇ More")
	if a := act(t, more.CallbackData); a.Kind != KindFileNum || a.Arg < 2 {
		t.Fatalf("More should keep numbers: %+v", a)
	}
	off := act(t, findBtn(t, ns, "🔢 Off").CallbackData)
	if off.Kind != KindFile {
		t.Fatalf("Off = %+v", off)
	}
}

// -------------------------------------------------------------- prompt flows

func TestLinesAndDocPromptFlows(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "p.txt", "a\nb\n")
	id := fileID(t, m, root, "p.txt")

	lp := m.Handle(Action{Kind: KindLines, ID: id}, rw)
	if lp.Prompt == nil || lp.Prompt.Kind != WizardLines || !strings.Contains(lp.Screen.Text, "@12") {
		t.Fatalf("lines prompt: %+v", lp.Screen.Text)
	}
	if a := act(t, findBtn(t, lp.Screen, "Cancel").CallbackData); a.Kind != KindFileNum {
		t.Fatalf("cancel should return to the numbered view: %+v", a)
	}
	bad := m.Submit(WizardLines, id, "@1\n-zzz", rw)
	if !bad.Retry || !strings.Contains(bad.Screen.Text, "not") {
		t.Fatalf("bad spec should retry:\n%s", bad.Screen.Text)
	}
	ok := m.Submit(WizardLines, id, "@2\n-b\n+B", rw)
	if ok.Retry || !strings.Contains(ok.Screen.Text, "Edited 1 block(s): +1 −1") || !strings.Contains(ok.Screen.Text, "2│ B") {
		t.Fatalf("result:\n%s", ok.Screen.Text)
	}
	findBtn(t, ok.Screen, "↩ Undo")

	dp := m.Handle(Action{Kind: KindGetDoc, ID: id}, rw)
	if dp.Prompt == nil || dp.Prompt.Kind != WizardDoc || dp.SendPath != filepath.Join(root, "p.txt") {
		t.Fatalf("doc prompt: %+v", dp)
	}
	dr := m.SubmitDoc(id, []byte("whole\nnew\n"), rw)
	if !strings.Contains(dr.Screen.Text, "replaced with your edited copy") || read(t, root, "p.txt") != "whole\nnew\n" {
		t.Fatalf("doc result:\n%s", dr.Screen.Text)
	}
	// empty file: prompt but nothing to send
	write(t, root, "empty.txt", "")
	ep := m.Handle(Action{Kind: KindGetDoc, ID: fileID(t, m, root, "empty.txt")}, rw)
	if ep.SendPath != "" || !strings.Contains(ep.Screen.Text, "empty") {
		t.Fatalf("empty file: %+v", ep)
	}
	// folder is refused
	if r := m.Handle(Action{Kind: KindLines, ID: 0}, rw); r.Prompt != nil {
		t.Fatal("folder must not open a line-edit prompt")
	}
}

func TestNewEditsHiddenAndRefusedInPlanMode(t *testing.T) {
	m, root := newTree(t)
	plan := Options{}
	s := m.Open("main.go", plan).Screen
	for _, b := range allButtons(s) {
		for _, bad := range []string{"Edit lines", "Edit as file"} {
			if strings.Contains(b.Text, bad) {
				t.Fatalf("plan mode must hide %q", b.Text)
			}
		}
	}
	findBtn(t, s, "🔢 Lines") // reading aids stay available
	id := act(t, findBtn(t, s, "Reload").CallbackData).ID
	for _, k := range []string{KindLines, KindGetDoc} {
		if r := m.Handle(Action{Kind: k, ID: id}, plan); r.Prompt != nil || r.SendPath != "" || !strings.Contains(r.Screen.Text, "disabled in plan mode") {
			t.Fatalf("%s: %+v", k, r.Screen.Text)
		}
	}
	before := read(t, root, "main.go")
	m.SubmitDoc(id, []byte("x"), plan)
	if read(t, root, "main.go") != before {
		t.Fatal("SubmitDoc wrote in plan mode")
	}
}

func TestEditButtonsPresent(t *testing.T) {
	m, _ := newTree(t)
	s := m.Open("main.go", rw).Screen
	for _, l := range []string{"🧮 Edit lines", "📝 Edit as file", "🔎 Replace", "🩹 Apply patch", "🔢 Lines"} {
		findBtn(t, s, l)
	}
	for _, d := range []string{"files:lines:1", "files:getdoc:1", "files:fn:1", "files:fn:1:40"} {
		if _, err := ParseCallback(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
}
