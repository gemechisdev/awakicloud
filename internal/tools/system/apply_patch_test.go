package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func patchIn(p string, extra ...any) map[string]any {
	m := map[string]any{"patch": p}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

const goSrc = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")\n}\n"

func TestPatchModifyWithGitHeaders(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "main.go", goSrc)
	patch := `diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -3,6 +3,7 @@
 import "fmt"
 
 func main() {
-	fmt.Println("hello")
+	fmt.Println("hello, patched")
+	fmt.Println("extra")
 	fmt.Println("world")
 }
`
	r := call(applyPatch, patchIn(patch))
	if r.IsErr {
		t.Fatal(r.Output)
	}
	want := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, patched\")\n\tfmt.Println(\"extra\")\n\tfmt.Println(\"world\")\n}\n"
	if read(t, p) != want {
		t.Fatalf("got %q", read(t, p))
	}
	if !strings.Contains(r.Output, "M "+p) || !strings.Contains(r.Output, "+2 -1") {
		t.Fatalf("%q", r.Output)
	}
}

func TestPatchWrongLineNumbersAndMissingCounts(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "main.go", goSrc)
	// stated line is 40 lines off, and the header has no numbers at all in the second hunk
	patch := "--- main.go\n+++ main.go\n@@ -45,3 +45,3 @@\n func main() {\n-\tfmt.Println(\"hello\")\n+\tfmt.Println(\"HI\")\n \tfmt.Println(\"world\")\n@@\n \tfmt.Println(\"world\")\n+\tfmt.Println(\"bye\")\n }\n"
	r := call(applyPatch, patchIn(patch))
	if r.IsErr {
		t.Fatal(r.Output)
	}
	got := read(t, p)
	if !strings.Contains(got, "\"HI\"") || !strings.Contains(got, "\"bye\"") || strings.Count(got, "world") != 1 {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(r.Output, "offset") {
		t.Fatalf("expected an offset note: %q", r.Output)
	}
}

func TestPatchWhitespaceTolerantContext(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "a.txt", "keep\n\tindented line\nend\n")
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n keep\n-    indented line\n+\tchanged line\n end\n"
	r := call(applyPatch, patchIn(patch))
	if r.IsErr || read(t, p) != "keep\n\tchanged line\nend\n" || !strings.Contains(r.Output, "whitespace") {
		t.Fatalf("%q / %q", r.Output, read(t, p))
	}
}

func TestPatchCreateDeleteRename(t *testing.T) {
	dir := setup(t)
	write(t, dir, "old.txt", "bye\n")
	write(t, dir, "mv.txt", "one\ntwo\n")
	patch := `--- /dev/null
+++ b/new/dir/created.txt
@@ -0,0 +1,2 @@
+hello
+world
--- a/old.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/mv.txt b/renamed.txt
similarity index 90%
rename from mv.txt
rename to renamed.txt
--- a/mv.txt
+++ b/renamed.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
`
	r := call(applyPatch, patchIn(patch))
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if read(t, filepath.Join(dir, "new/dir/created.txt")) != "hello\nworld\n" {
		t.Fatalf("created: %q", read(t, filepath.Join(dir, "new/dir/created.txt")))
	}
	if exists(filepath.Join(dir, "old.txt")) || exists(filepath.Join(dir, "mv.txt")) {
		t.Fatal("old files should be gone")
	}
	if read(t, filepath.Join(dir, "renamed.txt")) != "one\nTWO\n" {
		t.Fatalf("renamed: %q", read(t, filepath.Join(dir, "renamed.txt")))
	}
	for _, w := range []string{"3 file(s)", "A ", "D ", "R "} {
		if !strings.Contains(r.Output, w) {
			t.Errorf("missing %q in %q", w, r.Output)
		}
	}
}

func TestPatchAtomicRollbackOnFailure(t *testing.T) {
	dir := setup(t)
	a := write(t, dir, "a.txt", "one\ntwo\n")
	b := write(t, dir, "b.txt", "x\ny\n")
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n--- a/b.txt\n+++ b/b.txt\n@@ -1,2 +1,2 @@\n x\n-NOT THERE\n+z\n"
	r := call(applyPatch, patchIn(patch))
	if !r.IsErr || !strings.Contains(r.Output, "b.txt") || !strings.Contains(r.Output, "hunk 1 did not apply") {
		t.Fatalf("%q", r.Output)
	}
	if read(t, a) != "one\ntwo\n" || read(t, b) != "x\ny\n" {
		t.Fatal("a failed patch must leave every file untouched")
	}
	if !strings.Contains(r.Output, "Closest match") {
		t.Fatalf("failure should show the closest text: %q", r.Output)
	}
}

func TestPatchDryRunAndAmbiguity(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "d.txt", "x\nsame\ny\nsame\nz\n")
	r := call(applyPatch, patchIn("--- d.txt\n+++ d.txt\n@@ -1,3 +1,3 @@\n x\n-same\n+DIFF\n y\n", "dry_run", true))
	if r.IsErr || !strings.Contains(r.Output, "DRY RUN") || read(t, p) != "x\nsame\ny\nsame\nz\n" {
		t.Fatalf("%q", r.Output)
	}
	// no line numbers + context that matches twice => refuse to guess
	r = call(applyPatch, patchIn("--- d.txt\n+++ d.txt\n@@\n-same\n+DIFF\n"))
	if !r.IsErr || !strings.Contains(r.Output, "2 places") {
		t.Fatalf("%q", r.Output)
	}
}

func TestPatchCRLFAndNoNewlineAtEOF(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "w.txt", "a\r\nb\r\nc\r\n")
	r := call(applyPatch, patchIn("--- w.txt\n+++ w.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"))
	if r.IsErr || read(t, p) != "a\r\nB\r\nc\r\n" {
		t.Fatalf("%q / %q", r.Output, read(t, p))
	}
	q := write(t, dir, "n.txt", "one\ntwo")
	r = call(applyPatch, patchIn("--- n.txt\n+++ n.txt\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+TWO\n\\ No newline at end of file\n"))
	if r.IsErr || read(t, q) != "one\nTWO" {
		t.Fatalf("%q / %q", r.Output, read(t, q))
	}
	// newline gets added when only the old side lacked one
	r = call(applyPatch, patchIn("--- n.txt\n+++ n.txt\n@@ -1,2 +1,2 @@\n one\n-TWO\n\\ No newline at end of file\n+2\n"))
	if r.IsErr || read(t, q) != "one\n2\n" {
		t.Fatalf("%q / %q", r.Output, read(t, q))
	}
}

func TestPatchModeAndErrors(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "run.sh", "echo a\necho b\n")
	os.Chmod(p, 0o755)
	call(applyPatch, patchIn("--- run.sh\n+++ run.sh\n@@ -1,2 +1,2 @@\n echo a\n-echo b\n+echo c\n"))
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode lost: %v", st.Mode().Perm())
	}
	if r := call(applyPatch, patchIn("not a diff at all")); !r.IsErr || !strings.Contains(r.Output, "no file changes") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(applyPatch, patchIn("--- ghost.txt\n+++ ghost.txt\n@@ -1 +1 @@\n-a\n+b\n")); !r.IsErr || !strings.Contains(r.Output, "file not found") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(applyPatch, patchIn("--- /dev/null\n+++ run.sh\n@@ -0,0 +1 @@\n+x\n")); !r.IsErr || !strings.Contains(r.Output, "already exists") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(applyPatch, patchIn("--- run.sh\n+++ run.sh\n@@ -1 +1 @@\n*bad prefix\n")); !r.IsErr || !strings.Contains(r.Output, "unexpected line") {
		t.Fatalf("%q", r.Output)
	}
}

func TestPatchStripAndRoot(t *testing.T) {
	dir := setup(t)
	write(t, dir, "proj/sub/f.txt", "a\nb\n")
	r := call(applyPatch, patchIn("--- x/sub/f.txt\n+++ x/sub/f.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+B\n", "root", "proj", "strip", float64(1)))
	if r.IsErr || read(t, filepath.Join(dir, "proj/sub/f.txt")) != "a\nB\n" {
		t.Fatalf("%q", r.Output)
	}
}

func TestPatchInsertionOnlyHunk(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "i.txt", "1\n2\n3\n")
	// insertion with no context after line 2
	r := call(applyPatch, patchIn("--- i.txt\n+++ i.txt\n@@ -2,0 +3,1 @@\n+two-and-a-half\n"))
	if r.IsErr || read(t, p) != "1\n2\ntwo-and-a-half\n3\n" {
		t.Fatalf("%q / %q", r.Output, read(t, p))
	}
}
