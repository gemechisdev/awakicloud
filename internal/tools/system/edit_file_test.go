package system

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := workspace.SetRoot(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func run(in map[string]any) types.Result {
	return editFile(context.Background(), in)
}

func TestUniqueReplaceReturnsDiff(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "a.go", "package a\n\nfunc One() int {\n\treturn 1\n}\n")
	r := run(map[string]any{"path": "a.go", "old_string": "return 1", "new_string": "return 2"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if got := read(t, p); !strings.Contains(got, "return 2") || strings.Contains(got, "return 1") {
		t.Fatalf("not edited: %q", got)
	}
	for _, want := range []string{"+1 -1", "-\treturn 1", "+\treturn 2", "@@"} {
		if !strings.Contains(r.Output, want) {
			t.Errorf("output missing %q:\n%s", want, r.Output)
		}
	}
}

func TestAmbiguousRefusedAndFileUntouched(t *testing.T) {
	dir := setup(t)
	orig := "x := 1\nfoo()\ny := 2\nfoo()\nz := 3\n"
	p := write(t, dir, "b.go", orig)
	r := run(map[string]any{"path": "b.go", "old_string": "foo()", "new_string": "bar()"})
	if !r.IsErr {
		t.Fatal("expected error")
	}
	if !strings.Contains(r.Output, "2 locations") || !strings.Contains(r.Output, "line 2") || !strings.Contains(r.Output, "line 4") {
		t.Errorf("ambiguity report missing details:\n%s", r.Output)
	}
	if read(t, p) != orig {
		t.Fatal("file modified despite error")
	}
	// extra context disambiguates
	r = run(map[string]any{"path": "b.go", "old_string": "foo()\ny := 2", "new_string": "bar()\ny := 2"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if read(t, p) != "x := 1\nbar()\ny := 2\nfoo()\nz := 3\n" {
		t.Fatalf("wrong occurrence edited: %q", read(t, p))
	}
}

func TestReplaceAllAndExpected(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "c.txt", "a b a b a\n")
	r := run(map[string]any{"path": "c.txt", "old_string": "a", "new_string": "X", "expected_replacements": float64(2)})
	if !r.IsErr || !strings.Contains(r.Output, "found 3") {
		t.Fatalf("expected mismatch error, got %v %s", r.IsErr, r.Output)
	}
	if read(t, p) != "a b a b a\n" {
		t.Fatal("modified on mismatch")
	}
	r = run(map[string]any{"path": "c.txt", "old_string": "a", "new_string": "X", "replace_all": true})
	if r.IsErr || read(t, p) != "X b X b X\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
}

func TestEmptyNewStringDeletes(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "d.txt", "keep\ndrop me\nkeep2\n")
	r := run(map[string]any{"path": "d.txt", "old_string": "drop me\n", "new_string": ""})
	if r.IsErr || read(t, p) != "keep\nkeep2\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
}

func TestCRLFAndBOMPreserved(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "e.txt", "\xef\xbb\xbfline1\r\nline2\r\nline3\r\n")
	r := run(map[string]any{"path": "e.txt", "old_string": "line1\nline2", "new_string": "A\nB\nC"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if got := read(t, p); got != "\xef\xbb\xbfA\r\nB\r\nC\r\nline3\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestIndentationFallbackReindents(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "f.go", "func f() {\n\tif ok {\n\t\tdo()\n\t}\n}\n")
	// model sends spaces / no indentation
	r := run(map[string]any{
		"path":       "f.go",
		"old_string": "if ok {\n    do()\n}",
		"new_string": "if ok {\n    do()\n    more()\n}",
	})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if !strings.Contains(r.Output, "Note:") {
		t.Errorf("expected fallback note:\n%s", r.Output)
	}
	want := "func f() {\n\tif ok {\n    do()\n    more()\n\t}\n}\n"
	_ = want
	got := read(t, p)
	if !strings.Contains(got, "\tif ok {") || !strings.Contains(got, "more()") || strings.Count(got, "do()") != 1 {
		t.Fatalf("bad result %q", got)
	}
	// new lines for a block sent with matching 0-indent get the file indent
	p2 := write(t, dir, "g.go", "\t\tx := 1\n\t\ty := 2\n")
	r = run(map[string]any{"path": "g.go", "old_string": "x := 1\ny := 2", "new_string": "x := 10\ny := 20\nz := 30"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if read(t, p2) != "\t\tx := 10\n\t\ty := 20\n\t\tz := 30\n" {
		t.Fatalf("got %q", read(t, p2))
	}
}

func TestSmartQuotesFallback(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "q.md", "He said “hello” – twice\nnext\n")
	r := run(map[string]any{"path": "q.md", "old_string": "He said \"hello\" - twice", "new_string": "He said bye"})
	if r.IsErr || read(t, p) != "He said bye\nnext\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
}

func TestNotFoundSuggestsClosest(t *testing.T) {
	dir := setup(t)
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "line number %d here\n", i)
	}
	sb.WriteString("func compute(a int, b int) int {\n\treturn a + b\n}\n")
	write(t, dir, "h.go", sb.String())
	r := run(map[string]any{
		"path":       "h.go",
		"old_string": "func compute(a int, c int) int {\n\treturn a + b\n}",
		"new_string": "x",
	})
	if !r.IsErr {
		t.Fatal("expected error")
	}
	for _, want := range []string{"Closest match", "lines 51-53", "First difference at line 51", "b int", "c int"} {
		if !strings.Contains(r.Output, want) {
			t.Errorf("missing %q:\n%s", want, r.Output)
		}
	}
	r = run(map[string]any{"path": "h.go", "old_string": "zzzzzz qqqqqq wwwww", "new_string": "x"})
	if !r.IsErr || !strings.Contains(r.Output, "No similar text") {
		t.Fatalf("expected no-similar message:\n%s", r.Output)
	}
}

func TestCaseHint(t *testing.T) {
	dir := setup(t)
	write(t, dir, "i.txt", "Hello World\n")
	r := run(map[string]any{"path": "i.txt", "old_string": "hello world", "new_string": "x"})
	if !r.IsErr || !strings.Contains(r.Output, "capitalisation") {
		t.Fatalf("%s", r.Output)
	}
}

func TestMultiEditSequentialAndAtomic(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "m.txt", "alpha\nbeta\ngamma\n")
	r := run(map[string]any{"path": "m.txt", "edits": []any{
		map[string]any{"old_string": "alpha", "new_string": "ALPHA"},
		map[string]any{"old_string": "ALPHA\nbeta", "new_string": "AB"}, // sees earlier result
		map[string]any{"old_string": "gamma", "new_string": "G"},
	}})
	if r.IsErr || read(t, p) != "AB\nG\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
	before := read(t, p)
	r = run(map[string]any{"path": "m.txt", "edits": []any{
		map[string]any{"old_string": "AB", "new_string": "ab"},
		map[string]any{"old_string": "does not exist", "new_string": "x"},
	}})
	if !r.IsErr || !strings.Contains(r.Output, "edits[2] of 2") || !strings.Contains(r.Output, "No changes were written") {
		t.Fatalf("%s", r.Output)
	}
	if read(t, p) != before {
		t.Fatal("partial write on failed multi-edit")
	}
}

func TestEditsAsJSONString(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "j.txt", "one two\n")
	arr, _ := json.Marshal([]map[string]any{{"old_string": "one", "new_string": "1"}})
	r := run(map[string]any{"path": "j.txt", "edits": string(arr)})
	if r.IsErr || read(t, p) != "1 two\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
}

func TestRegex(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "r.go", "v1 := getUser(1)\nv2 := getUser(22)\n")
	r := run(map[string]any{"path": "r.go", "old_string": `getUser\((\d+)\)`, "new_string": "fetchUser(ctx, $1)", "regex": true, "replace_all": true})
	if r.IsErr || read(t, p) != "v1 := fetchUser(ctx, 1)\nv2 := fetchUser(ctx, 22)\n" {
		t.Fatalf("%s / %q", r.Output, read(t, p))
	}
	// ambiguity applies to regex too
	r = run(map[string]any{"path": "r.go", "old_string": `fetchUser`, "new_string": "x", "regex": true})
	if !r.IsErr || !strings.Contains(r.Output, "pattern matches 2") {
		t.Fatalf("%s", r.Output)
	}
	// ^ anchors per line by default
	r = run(map[string]any{"path": "r.go", "old_string": `^v2`, "new_string": "w2", "regex": true})
	if r.IsErr || !strings.Contains(read(t, p), "w2 :=") {
		t.Fatalf("%s", r.Output)
	}
	r = run(map[string]any{"path": "r.go", "old_string": `(`, "new_string": "x", "regex": true})
	if !r.IsErr || !strings.Contains(r.Output, "invalid regex") {
		t.Fatalf("%s", r.Output)
	}
	r = run(map[string]any{"path": "r.go", "old_string": `x*`, "new_string": "y", "regex": true})
	if !r.IsErr || !strings.Contains(r.Output, "empty string") {
		t.Fatalf("%s", r.Output)
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	dir := setup(t)
	p := write(t, dir, "n.txt", "abc\n")
	r := run(map[string]any{"path": "n.txt", "old_string": "abc", "new_string": "xyz", "dry_run": true})
	if r.IsErr || !strings.Contains(r.Output, "DRY RUN") || !strings.Contains(r.Output, "+xyz") {
		t.Fatalf("%s", r.Output)
	}
	if read(t, p) != "abc\n" {
		t.Fatal("dry run wrote")
	}
}

func TestModePreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := setup(t)
	p := write(t, dir, "run.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	r := run(map[string]any{"path": "run.sh", "old_string": "echo hi", "new_string": "echo bye"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	// no stray temp files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".awaki-edit-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestSymlinkTargetEdited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := setup(t)
	real := write(t, dir, "real.txt", "one\n")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unsupported")
	}
	r := run(map[string]any{"path": "link.txt", "old_string": "one", "new_string": "two"})
	if r.IsErr {
		t.Fatal(r.Output)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	if read(t, real) != "two\n" {
		t.Fatalf("target = %q", read(t, real))
	}
}

func TestCreateViaEmptyOldString(t *testing.T) {
	dir := setup(t)
	r := run(map[string]any{"path": "sub/dir/new.txt", "old_string": "", "new_string": "hello\n"})
	if r.IsErr || !strings.Contains(r.Output, "Created") {
		t.Fatalf("%s", r.Output)
	}
	if read(t, filepath.Join(dir, "sub/dir/new.txt")) != "hello\n" {
		t.Fatal("bad content")
	}
	// existing non-empty file + empty old_string must not clobber
	r = run(map[string]any{"path": "sub/dir/new.txt", "old_string": "", "new_string": "x"})
	if !r.IsErr {
		t.Fatal("should refuse")
	}
	// missing file + real old_string
	r = run(map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"})
	if !r.IsErr || !strings.Contains(r.Output, "file not found") {
		t.Fatalf("%s", r.Output)
	}
}

func TestGuards(t *testing.T) {
	dir := setup(t)
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := run(map[string]any{"path": "d", "old_string": "a", "new_string": "b"}); !r.IsErr || !strings.Contains(r.Output, "directory") {
		t.Fatalf("%s", r.Output)
	}
	write(t, dir, "bin.dat", "ab\x00cd")
	if r := run(map[string]any{"path": "bin.dat", "old_string": "a", "new_string": "b"}); !r.IsErr || !strings.Contains(r.Output, "binary") {
		t.Fatalf("%s", r.Output)
	}
	write(t, dir, "same.txt", "abc\n")
	if r := run(map[string]any{"path": "same.txt", "old_string": "abc", "new_string": "abc"}); !r.IsErr {
		t.Fatal("identical replace should error")
	}
	if r := run(map[string]any{"path": "same.txt", "old_string": "abc"}); !r.IsErr || !strings.Contains(r.Output, "new_string is required") {
		t.Fatalf("%s", r.Output)
	}
	if r := run(map[string]any{"path": "same.txt", "old_string": "abc", "new_string": "x", "edits": []any{map[string]any{"old_string": "a", "new_string": "b"}}}); !r.IsErr {
		t.Fatal("both forms should error")
	}
}

func TestJSONWarning(t *testing.T) {
	dir := setup(t)
	write(t, dir, "c.json", "{\"a\": 1,\n \"b\": 2}\n")
	r := run(map[string]any{"path": "c.json", "old_string": "\"a\": 1,", "new_string": "\"a\": 1"})
	if r.IsErr || !strings.Contains(r.Output, "WARNING") {
		t.Fatalf("%s", r.Output)
	}
}

func TestConcurrentEditsSameFile(t *testing.T) {
	dir := setup(t)
	var sb strings.Builder
	const n = 30
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "token%d\n", i)
	}
	p := write(t, dir, "z.txt", sb.String())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := run(map[string]any{"path": "z.txt", "old_string": fmt.Sprintf("token%d\n", i), "new_string": fmt.Sprintf("done%d\n", i)})
			if r.IsErr {
				t.Errorf("edit %d: %s", i, r.Output)
			}
		}(i)
	}
	wg.Wait()
	got := read(t, p)
	for i := 0; i < n; i++ {
		if !strings.Contains(got, fmt.Sprintf("done%d\n", i)) {
			t.Fatalf("lost update %d", i)
		}
	}
}

func TestRelativePathUsesWorkspaceRoot(t *testing.T) {
	dir := setup(t)
	write(t, dir, "proj/x.txt", "k\n")
	cwd, _ := os.Getwd()
	if cwd == dir {
		t.Skip("cwd equals root")
	}
	r := run(map[string]any{"path": "proj/x.txt", "old_string": "k", "new_string": "v"})
	if r.IsErr || read(t, filepath.Join(dir, "proj/x.txt")) != "v\n" {
		t.Fatalf("%s", r.Output)
	}
}

// Property test: applying diff ops to old must reproduce new.
func TestMyersReconstructs(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "b", "c", "d", "e"}
	gen := func() []string {
		n := rng.Intn(25)
		out := make([]string, n)
		for i := range out {
			out[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return out
	}
	for iter := 0; iter < 2000; iter++ {
		a, b := gen(), gen()
		ops := myersOps(a, b)
		var rebuilt []string
		ai := 0
		for _, op := range ops {
			switch op.kind {
			case ' ':
				if a[op.a] != b[op.b] || op.a != ai {
					t.Fatalf("bad equal op %+v a=%v b=%v", op, a, b)
				}
				rebuilt = append(rebuilt, a[op.a])
				ai++
			case '-':
				if op.a != ai {
					t.Fatalf("bad delete pos %+v", op)
				}
				ai++
			case '+':
				rebuilt = append(rebuilt, b[op.b])
			}
		}
		if ai != len(a) || strings.Join(rebuilt, ",") != strings.Join(b, ",") {
			t.Fatalf("reconstruct failed\na=%v\nb=%v\nrebuilt=%v", a, b, rebuilt)
		}
		// minimality sanity: never worse than delete-all+insert-all
		d := 0
		for _, op := range ops {
			if op.kind != ' ' {
				d++
			}
		}
		if d > len(a)+len(b) {
			t.Fatal("non-minimal")
		}
	}
}

func TestDiffMultiHunkLineNumbers(t *testing.T) {
	var a, b []string
	for i := 1; i <= 40; i++ {
		a = append(a, fmt.Sprintf("l%d", i))
		b = append(b, fmt.Sprintf("l%d", i))
	}
	b[1] = "CHANGED2"
	b[30] = "CHANGED31"
	d, add, rem := renderDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n")
	if add != 2 || rem != 2 {
		t.Fatalf("+%d -%d\n%s", add, rem, d)
	}
	if strings.Count(d, "\n@@ ")+1 != 2 { // two hunk headers
		t.Fatalf("expected two hunks:\n%s", d)
	}
	if !strings.Contains(d, "-l2") || !strings.Contains(d, "+CHANGED31") || !strings.Contains(d, "@@ -28,7 +28,7 @@") {
		t.Fatalf("diff wrong:\n%s", d)
	}
}

func mustTime(year int) time.Time { return time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC) }
