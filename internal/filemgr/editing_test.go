package filemgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const base = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n\nfunc other() {}\n"

// ------------------------------------------------------------------ patch

func TestPatchGitStyleModify(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "app.go", base)
	patch := "diff --git a/app.go b/app.go\nindex 1..2 100644\n--- a/app.go\n+++ b/app.go\n@@ -3,5 +3,6 @@\n import \"fmt\"\n \n func main() {\n-\tfmt.Println(\"hello\")\n+\tfmt.Println(\"hello, world\")\n+\tfmt.Println(\"bye\")\n }\n"
	res, err := m.ApplyPatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Action != "modified" || res.Files[0].Added != 2 || res.Files[0].Removed != 1 {
		t.Fatalf("result = %+v", res)
	}
	want := strings.Replace(base, "\tfmt.Println(\"hello\")\n", "\tfmt.Println(\"hello, world\")\n\tfmt.Println(\"bye\")\n", 1)
	if got := read(t, root, "app.go"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if res.Seq == 0 {
		t.Fatal("patch should be undoable")
	}
}

func TestPatchCreateDeleteAndNestedDirs(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "old.txt", "bye\n")
	patch := "--- /dev/null\n+++ b/pkg/new/file.txt\n@@ -0,0 +1,2 @@\n+line one\n+line two\n" +
		"--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n"
	res, err := m.ApplyPatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, root, "pkg/new/file.txt") != "line one\nline two\n" {
		t.Fatal("created file wrong")
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old.txt should be deleted")
	}
	acts := res.Files[0].Action + "," + res.Files[1].Action
	if acts != "created,deleted" {
		t.Fatalf("actions = %s", acts)
	}
}

func TestPatchPlainP0AndOffsetAndMultiHunk(t *testing.T) {
	m, root := newTree(t)
	var sb strings.Builder
	for i := 1; i <= 40; i++ {
		sb.WriteString("line " + strings.Repeat("x", i%3) + string(rune('A'+i%26)) + "\n")
	}
	write(t, root, "src/l.txt", "PREFACE\nPREFACE2\n"+sb.String()) // shifts every line by 2
	orig := read(t, root, "src/l.txt")
	lines := strings.Split(strings.TrimSuffix(orig, "\n"), "\n")
	// hunk claims lines 4-6 but real position is shifted; second hunk is far below
	patch := "--- src/l.txt\n+++ src/l.txt\n@@ -2,3 +2,3 @@\n " + lines[3] + "\n-" + lines[4] + "\n+CHANGED-1\n " + lines[5] + "\n" +
		"@@ -30,3 +30,3 @@\n " + lines[29] + "\n-" + lines[30] + "\n+CHANGED-2\n " + lines[31] + "\n"
	if _, err := m.ApplyPatch(patch); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, "src/l.txt")
	if !strings.Contains(got, "CHANGED-1\n") || !strings.Contains(got, "CHANGED-2\n") || strings.Contains(got, lines[4]+"\n") && lines[4] != lines[30] {
		t.Fatalf("hunks not applied:\n%s", got)
	}
}

func TestPatchMismatchChangesNothingAtomic(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")
	write(t, root, "b.txt", "alpha\nbeta\n")
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n" +
		"--- a/b.txt\n+++ b/b.txt\n@@ -1,2 +1,2 @@\n alpha\n-NOT THERE\n+x\n"
	_, err := m.ApplyPatch(patch)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	if read(t, root, "a.txt") != "one\ntwo\nthree\n" || read(t, root, "b.txt") != "alpha\nbeta\n" {
		t.Fatal("atomic: nothing may change when any hunk fails")
	}
}

func TestPatchRejectsUnsafePaths(t *testing.T) {
	m, root := newTree(t)
	outside := filepath.Join(filepath.Dir(root), "pwn.txt")
	for _, p := range []string{"../pwn.txt", "a/../../pwn.txt", "/etc/passwd"} {
		patch := "--- /dev/null\n+++ " + p + "\n@@ -0,0 +1 @@\n+x\n"
		if _, err := m.ApplyPatch(patch); err == nil {
			t.Errorf("path %q must be rejected", p)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file written outside workspace")
	}
	// escaping symlink
	out := t.TempDir()
	if os.Symlink(out, filepath.Join(root, "ln")) == nil {
		if _, err := m.ApplyPatch("--- /dev/null\n+++ ln/x.txt\n@@ -0,0 +1 @@\n+x\n"); err == nil {
			t.Fatal("must not write through an escaping symlink")
		}
		if _, err := os.Stat(filepath.Join(out, "x.txt")); err == nil {
			t.Fatal("wrote outside via symlink")
		}
	}
}

func TestPatchCRLFAndFencedPasteAndBlankContext(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "w.txt", "a\r\n\r\nb\r\nc\r\n")
	// blank context line pasted without its leading space; wrapped in a fence
	patch := "```diff\n--- a/w.txt\n+++ b/w.txt\n@@ -1,4 +1,4 @@\n a\n\n-b\n+B\n c\n```"
	if _, err := m.ApplyPatch(patch); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "w.txt"); got != "a\r\n\r\nB\r\nc\r\n" {
		t.Fatalf("CRLF must be preserved: %q", got)
	}
}

func TestPatchGarbage(t *testing.T) {
	m, _ := newTree(t)
	for _, in := range []string{"", "hello world", "--- a/x\n+++ b/x\nno hunks"} {
		if _, err := m.ApplyPatch(in); err == nil {
			t.Errorf("%q must fail", in)
		}
	}
}

// ---------------------------------------------------------------- replace

func TestReplaceLiteralRegexEmptyAndErrors(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "r.txt", "foo bar foo\nFoo baz\nv1.2 v3.4\n")
	id := m.touch(filepath.Join(root, "r.txt"))
	n, seq, err := m.ReplaceInFile(id, "foo", "qux")
	if err != nil || n != 2 || seq == 0 {
		t.Fatalf("literal: %d %d %v", n, seq, err)
	}
	if read(t, root, "r.txt") != "qux bar qux\nFoo baz\nv1.2 v3.4\n" {
		t.Fatalf("got %q", read(t, root, "r.txt"))
	}
	if n, _, err := m.ReplaceInFile(id, `re:v(\d)\.(\d)`, "V$2-$1"); err != nil || n != 2 {
		t.Fatalf("regex: %d %v", n, err)
	}
	if !strings.Contains(read(t, root, "r.txt"), "V2-1 V4-3") {
		t.Fatalf("regex groups: %q", read(t, root, "r.txt"))
	}
	if _, _, err := m.ReplaceInFile(id, "qux", EmptyReplacement); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, root, "r.txt"), "qux") {
		t.Fatal("matches should be deleted")
	}
	for _, c := range []struct{ f, w string }{{"zzz", "a"}, {"", "a"}, {"re:(", "a"}, {"Foo", "Foo"}} {
		if _, _, err := m.ReplaceInFile(id, c.f, c.w); err == nil {
			t.Errorf("(%q→%q) should error", c.f, c.w)
		}
	}
	bin := filepath.Join(root, "b.bin")
	os.WriteFile(bin, []byte{0, 1, 2, 3}, 0o644)
	if _, _, err := m.ReplaceInFile(m.touch(bin), "a", "b"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary: %v", err)
	}
	if _, _, err := m.ReplaceInFile(m.touch(filepath.Join(root, "src")), "a", "b"); err == nil {
		t.Fatal("folder must fail")
	}
}

// ----------------------------------------------------------------- upload

func TestUploadCreateReplaceUndo(t *testing.T) {
	m, root := newTree(t)
	saved, seq, err := m.SaveUploads(0, []Upload{{"new.txt", []byte("hi")}, {"main.go", []byte("package replaced\n")}})
	if err != nil || len(saved) != 2 || seq == 0 {
		t.Fatalf("%v %d %v", saved, seq, err)
	}
	if read(t, root, "new.txt") != "hi" || read(t, root, "main.go") != "package replaced\n" {
		t.Fatal("upload content wrong")
	}
	restored, err := m.Undo(seq)
	if err != nil || len(restored) != 2 {
		t.Fatalf("undo: %v %v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("created file should be removed by undo")
	}
	if read(t, root, "main.go") != "package main\n\nfunc main() {}\n" {
		t.Fatalf("overwritten file not restored: %q", read(t, root, "main.go"))
	}
	if _, err := m.Undo(seq); err == nil {
		t.Fatal("second undo must fail")
	}
}

func TestUploadRejectsBadNamesSizeAndRollsBack(t *testing.T) {
	m, root := newTree(t)
	// the 2nd file is invalid → the 1st must be rolled back
	if _, _, err := m.SaveUploads(0, []Upload{{"ok.txt", []byte("x")}, {"..", []byte("y")}}); err == nil {
		t.Fatal("invalid name must fail")
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err == nil {
		t.Fatal("batch must roll back")
	}
	// path in the attachment name is reduced to its base name
	saved, _, err := m.SaveUploads(0, []Upload{{"../../evil/x.txt", []byte("y")}})
	if err != nil || saved[0] != "x.txt" {
		t.Fatalf("traversal name: %v %v", saved, err)
	}
	if _, _, err := m.SaveUploads(0, []Upload{{"big", make([]byte, MaxUploadBytes+1)}}); err == nil {
		t.Fatal("oversize must fail")
	}
	if _, _, err := m.SaveUploads(0, nil); err == nil {
		t.Fatal("no files must fail")
	}
	if _, _, err := m.SaveUploads(m.touch(filepath.Join(root, "main.go")), []Upload{{"a", nil}}); err == nil {
		t.Fatal("target must be a folder")
	}
}

// ------------------------------------------------------------------ undo

func TestUndoRefusesAfterLaterChange(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "u.txt", "one\n")
	id := m.touch(filepath.Join(root, "u.txt"))
	_, seq, _ := m.ReplaceInFile(id, "one", "two")
	write(t, root, "u.txt", "two plus my own manual edit\n")
	if _, err := m.Undo(seq); err == nil || !strings.Contains(err.Error(), "modified since") {
		t.Fatalf("err = %v", err)
	}
	if read(t, root, "u.txt") != "two plus my own manual edit\n" {
		t.Fatal("undo must not clobber newer work")
	}
}

func TestUndoPatchGroup(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "a.txt", "1\n2\n")
	res, err := m.ApplyPatch("--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n 1\n-2\n+X\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+new\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Undo(res.Seq); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "a.txt") != "1\n2\n" {
		t.Fatal("a.txt not restored")
	}
	if _, err := os.Stat(filepath.Join(root, "n.txt")); !os.IsNotExist(err) {
		t.Fatal("n.txt should be removed")
	}
}

func TestUndoRingBounded(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "c.txt", "0")
	id := m.touch(filepath.Join(root, "c.txt"))
	var first int
	for i := 0; i < maxUndoGroups+5; i++ {
		_, seq, err := m.ReplaceInFile(id, string(rune('0'+i%10)), string(rune('0'+(i+1)%10)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = seq
		}
	}
	if _, err := m.Undo(first); err == nil {
		t.Fatal("oldest entry should have been evicted")
	}
}

// ------------------------------------------------------------ screens/flow

func TestEditingPromptFlows(t *testing.T) {
	m, root := newTree(t)
	write(t, root, "f.txt", "alpha beta\n")
	fid := act(t, findBtn(t, m.Dir(0, 0, rw), "📄 f.txt").CallbackData).ID

	// replace: two steps, Undo button, then Undo callback
	r1 := m.Handle(Action{Kind: KindReplace, ID: fid}, rw)
	if r1.Prompt == nil || r1.Prompt.Kind != WizardReplaceFind {
		t.Fatalf("step 1: %+v", r1)
	}
	r2 := m.Submit(WizardReplaceFind, fid, "beta", rw)
	if r2.Prompt == nil || r2.Prompt.Kind != WizardReplaceWith || r2.Prompt.Data != "beta" {
		t.Fatalf("step 2: %+v", r2)
	}
	done := m.SubmitReplace(fid, r2.Prompt.Data, "gamma", rw)
	if !strings.Contains(done.Screen.Text, "Replaced 1 occurrence") || read(t, root, "f.txt") != "alpha gamma\n" {
		t.Fatalf("replace result:\n%s", done.Screen.Text)
	}
	undo := act(t, findBtn(t, done.Screen, "↩ Undo").CallbackData)
	if u := m.Handle(undo, rw); !strings.Contains(u.Screen.Text, "Undone") || read(t, root, "f.txt") != "alpha beta\n" {
		t.Fatalf("undo screen:\n%s", u.Screen.Text)
	}
	// replace with no match shows the reason, no Undo
	bad := m.SubmitReplace(fid, "zzz", "q", rw)
	if !strings.Contains(bad.Screen.Text, "no matches") {
		t.Fatalf("got:\n%s", bad.Screen.Text)
	}

	// upload prompt + submit
	up := m.Handle(Action{Kind: KindUpload, ID: 0}, rw)
	if up.Prompt == nil || up.Prompt.Kind != WizardUpload {
		t.Fatalf("upload prompt: %+v", up)
	}
	sr := m.SubmitUpload(0, []Upload{{"u.txt", []byte("1")}}, rw)
	if !strings.Contains(sr.Screen.Text, "Saved `u.txt`") {
		t.Fatalf("got:\n%s", sr.Screen.Text)
	}
	findBtn(t, sr.Screen, "↩ Undo")

	// patch prompt: failure retries, success summarises
	pp := m.Handle(Action{Kind: KindPatch, ID: 0}, rw)
	if pp.Prompt == nil || pp.Prompt.Kind != WizardPatch {
		t.Fatalf("patch prompt: %+v", pp)
	}
	if r := m.Submit(WizardPatch, 0, "not a patch", rw); !r.Retry || !strings.Contains(r.Screen.Text, "unified diff") {
		t.Fatalf("bad patch should retry: %+v", r)
	}
	ok := m.Submit(WizardPatch, 0, "--- /dev/null\n+++ b/p.txt\n@@ -0,0 +1 @@\n+x\n", rw)
	if ok.Retry || !strings.Contains(ok.Screen.Text, "Patched 1 file(s)") || !strings.Contains(ok.Screen.Text, "🆕 `p.txt` +1 −0") {
		t.Fatalf("patch ok:\n%s", ok.Screen.Text)
	}
}

func TestEditingDisabledInPlanMode(t *testing.T) {
	m, root := newTree(t)
	plan := Options{}
	f := m.Open("main.go", plan).Screen
	d := m.Dir(0, 0, plan)
	for _, s := range []Screen{f, d} {
		for _, b := range allButtons(s) {
			for _, bad := range []string{"Replace", "patch", "Upload", "Undo"} {
				if strings.Contains(b.Text, bad) {
					t.Fatalf("plan mode must hide %q", b.Text)
				}
			}
		}
	}
	id := act(t, findBtn(t, f, "Reload").CallbackData).ID
	for _, k := range []string{KindUpload, KindReplace, KindPatch, KindUndo} {
		r := m.Handle(Action{Kind: k, ID: id}, plan)
		if r.Prompt != nil || !strings.Contains(r.Screen.Text, "disabled in plan mode") {
			t.Fatalf("%s: %+v", k, r.Screen.Text)
		}
	}
	m.SubmitReplace(id, "package", "PKG", plan)
	m.SubmitUpload(0, []Upload{{"z", []byte("z")}}, plan)
	m.Submit(WizardPatch, 0, "--- /dev/null\n+++ b/z.txt\n@@ -0,0 +1 @@\n+x\n", plan)
	for _, p := range []string{"z", "z.txt"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Fatalf("%s created in plan mode", p)
		}
	}
	if strings.Contains(read(t, root, "main.go"), "PKG") {
		t.Fatal("replace ran in plan mode")
	}
}

func TestEditButtonsShown(t *testing.T) {
	m, _ := newTree(t)
	findBtn(t, m.Open("main.go", rw).Screen, "🔎 Replace")
	findBtn(t, m.Open("main.go", rw).Screen, "🩹 Apply patch")
	d := m.Dir(0, 0, rw)
	findBtn(t, d, "⬆️ Upload here")
	findBtn(t, d, "🩹 Apply patch")
}
