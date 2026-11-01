package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/tools/types"
)

func call(h types.Handler, in map[string]any) types.Result { return h(context.Background(), in) }

func numbered(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return sb.String()
}

func TestReadFileWholeSmallFileIsRaw(t *testing.T) {
	dir := setup(t)
	write(t, dir, "s.txt", "a\nb\n")
	r := call(readFile, map[string]any{"path": "s.txt"})
	if r.IsErr || r.Output != "a\nb\n" {
		t.Fatalf("%v %q", r.IsErr, r.Output)
	}
}

func TestReadFilePagingReachesTail(t *testing.T) {
	dir := setup(t)
	write(t, dir, "big.txt", numbered(5000))
	r := call(readFile, map[string]any{"path": "big.txt"})
	if r.IsErr || !strings.Contains(r.Output, "lines 1-2000 of 5000") || !strings.Contains(r.Output, "continue with offset=2001") {
		t.Fatalf("%q", r.Output[:min(200, len(r.Output))]+" … "+r.Output[max(0, len(r.Output)-120):])
	}
	r = call(readFile, map[string]any{"path": "big.txt", "offset": float64(4990), "limit": float64(100)})
	if !strings.Contains(r.Output, "line 5000") || strings.Contains(r.Output, "more line") {
		t.Fatalf("tail read wrong: %q", r.Output)
	}
	r = call(readFile, map[string]any{"path": "big.txt", "offset": float64(10), "limit": float64(2), "line_numbers": true})
	if !strings.Contains(r.Output, "    10  line 10") || !strings.Contains(r.Output, "    11  line 11") || strings.Contains(r.Output, "line 12") {
		t.Fatalf("%q", r.Output)
	}
	r = call(readFile, map[string]any{"path": "big.txt", "offset": float64(9999)})
	if !r.IsErr || !strings.Contains(r.Output, "past the end") {
		t.Fatalf("%q", r.Output)
	}
}

func TestReadFileByteBudgetAndLongLines(t *testing.T) {
	dir := setup(t)
	long := strings.Repeat("x", 100000)
	write(t, dir, "wide.txt", long+"\nsecond\n")
	r := call(readFile, map[string]any{"path": "wide.txt"})
	if r.IsErr || len(r.Output) > types.MaxOutputSize || !strings.Contains(r.Output, "line clipped") || !strings.Contains(r.Output, "second") {
		t.Fatalf("len=%d %q", len(r.Output), r.Output[:80])
	}
	// many medium lines: the byte budget must cut at a line boundary with a correct continuation offset
	var sb strings.Builder
	for i := 1; i <= 1500; i++ {
		fmt.Fprintf(&sb, "%04d %s\n", i, strings.Repeat("é", 40))
	}
	write(t, dir, "mid.txt", sb.String())
	r = call(readFile, map[string]any{"path": "mid.txt"})
	if !utf8.ValidString(r.Output) || len(r.Output) > types.MaxOutputSize || !strings.Contains(r.Output, "continue with offset=") {
		t.Fatalf("bad budget cut: len=%d", len(r.Output))
	}
	var next int
	fmt.Sscanf(r.Output[strings.LastIndex(r.Output, "offset=")+7:], "%d", &next)
	r2 := call(readFile, map[string]any{"path": "mid.txt", "offset": float64(next), "limit": float64(1)})
	if !strings.Contains(r2.Output, fmt.Sprintf("%04d ", next)) {
		t.Fatalf("continuation offset %d does not start at the next unread line: %q", next, r2.Output)
	}
}

func TestReadFileRejectsBinaryAndDir(t *testing.T) {
	dir := setup(t)
	write(t, dir, "b.bin", "ab\x00cd")
	if r := call(readFile, map[string]any{"path": "b.bin"}); !r.IsErr || !strings.Contains(r.Output, "binary") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(readFile, map[string]any{"path": "."}); !r.IsErr || !strings.Contains(r.Output, "directory") {
		t.Fatalf("%q", r.Output)
	}
	write(t, dir, "e.txt", "")
	if r := call(readFile, map[string]any{"path": "e.txt"}); r.Output != "(empty file)" {
		t.Fatalf("%q", r.Output)
	}
}

func TestReadFileCRLFAndBOM(t *testing.T) {
	dir := setup(t)
	write(t, dir, "w.txt", "\xef\xbb\xbfone\r\ntwo\r\n")
	r := call(readFile, map[string]any{"path": "w.txt"})
	if r.Output != "one\ntwo\n" {
		t.Fatalf("%q", r.Output)
	}
}

func TestWriteFileCreateOverwriteAndMode(t *testing.T) {
	dir := setup(t)
	r := call(writeFile, map[string]any{"path": "n/x.sh", "content": "#!/bin/sh\necho 1\n"})
	if r.IsErr || !strings.HasPrefix(r.Output, "Created") {
		t.Fatalf("%q", r.Output)
	}
	p := filepath.Join(dir, "n/x.sh")
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
			t.Fatalf("shebang file mode %v", st.Mode().Perm())
		}
	}
	r = call(writeFile, map[string]any{"path": "n/x.sh", "content": "#!/bin/sh\necho 2\n"})
	if r.IsErr || !strings.Contains(r.Output, "OVERWROTE") || !strings.Contains(r.Output, "-echo 1") || !strings.Contains(r.Output, "+echo 2") {
		t.Fatalf("%q", r.Output)
	}
	r = call(writeFile, map[string]any{"path": "n/x.sh", "content": "z", "create_only": true})
	if !r.IsErr || !strings.Contains(r.Output, "already exists") {
		t.Fatalf("%q", r.Output)
	}
	if read(t, p) != "#!/bin/sh\necho 2\n" {
		t.Fatal("create_only clobbered the file")
	}
	// existing mode is kept
	q := write(t, dir, "keep.txt", "a")
	os.Chmod(q, 0o600)
	call(writeFile, map[string]any{"path": "keep.txt", "content": "b"})
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(q); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode lost: %v", st.Mode().Perm())
		}
	}
	// missing content is an error, empty content is allowed
	if r := call(writeFile, map[string]any{"path": "k.txt"}); !r.IsErr {
		t.Fatal("missing content should error")
	}
	if r := call(writeFile, map[string]any{"path": "k.txt", "content": ""}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
}

func TestListFiles(t *testing.T) {
	dir := setup(t)
	write(t, dir, "b.txt", "12345")
	write(t, dir, ".hidden", "x")
	write(t, dir, "sub/c.txt", "x")
	r := call(listFiles, map[string]any{})
	lines := strings.Split(strings.TrimSpace(r.Output), "\n")
	if lines[0] != "sub/" || !strings.Contains(r.Output, "b.txt  (5 B)") || !strings.Contains(r.Output, ".hidden") {
		t.Fatalf("%q", r.Output)
	}
	r = call(listFiles, map[string]any{"hidden": false})
	if strings.Contains(r.Output, ".hidden") {
		t.Fatalf("hidden shown: %q", r.Output)
	}
	r = call(listFiles, map[string]any{"details": true, "path": "sub"})
	if !strings.Contains(r.Output, "c.txt") || !strings.Contains(r.Output, "-rw") {
		t.Fatalf("%q", r.Output)
	}
	r = call(listFiles, map[string]any{"max": float64(1)})
	if !strings.Contains(r.Output, "more entries") {
		t.Fatalf("%q", r.Output)
	}
}

func TestGlobDoubleStarAndIgnore(t *testing.T) {
	dir := setup(t)
	for _, f := range []string{"src/a.ts", "src/x/b.ts", "src/x/y/c.ts", "other/d.ts", "node_modules/p/e.ts", "dist/f.ts", "top.ts"} {
		write(t, dir, f, "x")
	}
	write(t, dir, ".gitignore", "dist/\n*.log\n")
	write(t, dir, "src/x/keep.log", "x")
	got := func(in map[string]any) string { return strings.TrimSpace(call(globFiles, in).Output) }

	if g := got(map[string]any{"pattern": "src/**/*.ts"}); g != "src/a.ts\nsrc/x/b.ts\nsrc/x/y/c.ts" {
		t.Fatalf("src/**/*.ts => %q", g)
	}
	if g := got(map[string]any{"pattern": "**/*.ts"}); g != "other/d.ts\nsrc/a.ts\nsrc/x/b.ts\nsrc/x/y/c.ts\ntop.ts" {
		t.Fatalf("**/*.ts => %q (node_modules and gitignored dist must be skipped)", g)
	}
	if g := got(map[string]any{"pattern": "*.ts"}); !strings.Contains(g, "src/x/y/c.ts") || !strings.Contains(g, "top.ts") {
		t.Fatalf("bare pattern should match at any depth: %q", g)
	}
	if g := got(map[string]any{"pattern": "*.ts", "include_ignored": true}); !strings.Contains(g, "node_modules/p/e.ts") || !strings.Contains(g, "dist/f.ts") {
		t.Fatalf("include_ignored: %q", g)
	}
	if g := got(map[string]any{"pattern": "*.log"}); !strings.HasPrefix(g, "No files match") {
		t.Fatalf("gitignored *.log should be skipped: %q", g)
	}
	if g := got(map[string]any{"pattern": "src/**/*.{ts,tsx}"}); !strings.Contains(g, "src/a.ts") {
		t.Fatalf("brace expansion: %q", g)
	}
	if g := got(map[string]any{"pattern": "src/x/?.ts"}); g != "src/x/b.ts" {
		t.Fatalf("? : %q", g)
	}
	if g := got(map[string]any{"pattern": "**/*.ts", "max": float64(2)}); !strings.Contains(g, "showing 2 of 5") {
		t.Fatalf("cap message: %q", g)
	}
	if g := got(map[string]any{"pattern": "src/**", "include_dirs": true}); !strings.Contains(g, "src/x") {
		t.Fatalf("dirs: %q", g)
	}
}

func TestGlobNestedGitignoreAndNegation(t *testing.T) {
	dir := setup(t)
	write(t, dir, "pkg/.gitignore", "gen_*.go\n!gen_keep.go\n")
	write(t, dir, "pkg/gen_a.go", "x")
	write(t, dir, "pkg/gen_keep.go", "x")
	write(t, dir, "pkg/real.go", "x")
	write(t, dir, "gen_root.go", "x") // nested rule must not apply outside pkg/
	g := call(globFiles, map[string]any{"pattern": "**/*.go"}).Output
	if strings.Contains(g, "pkg/gen_a.go") || !strings.Contains(g, "pkg/gen_keep.go") || !strings.Contains(g, "pkg/real.go") || !strings.Contains(g, "gen_root.go") {
		t.Fatalf("%q", g)
	}
}

func TestGlobMtimeSort(t *testing.T) {
	dir := setup(t)
	a := write(t, dir, "a.txt", "x")
	b := write(t, dir, "b.txt", "x")
	os.Chtimes(a, mustTime(2024), mustTime(2024))
	os.Chtimes(b, mustTime(2026), mustTime(2026))
	g := strings.Fields(call(globFiles, map[string]any{"pattern": "*.txt", "sort": "mtime"}).Output)
	if len(g) != 2 || g[0] != "b.txt" {
		t.Fatalf("%v", g)
	}
}

func TestDirectoryTree(t *testing.T) {
	dir := setup(t)
	write(t, dir, "cmd/main.go", "x")
	write(t, dir, "internal/a/b/c/deep.go", "x")
	write(t, dir, "node_modules/x/y.js", "x")
	r := call(directoryTree, map[string]any{"max_depth": float64(2)})
	if strings.Contains(r.Output, "node_modules") || !strings.Contains(r.Output, "main.go") {
		t.Fatalf("%q", r.Output)
	}
	if !strings.Contains(r.Output, "a/  [1 entries]") {
		t.Fatalf("depth-cut dir should show entry count:\n%s", r.Output)
	}
	r = call(directoryTree, map[string]any{"include_ignored": true})
	if !strings.Contains(r.Output, "node_modules") {
		t.Fatalf("%q", r.Output)
	}
}

func TestReadFilesBudgetAndHints(t *testing.T) {
	dir := setup(t)
	write(t, dir, "a.txt", "alpha\n")
	write(t, dir, "big.txt", numbered(900))
	write(t, dir, "bin.dat", "a\x00b")
	r := call(readFiles, map[string]any{"paths": []any{"a.txt", "big.txt", "bin.dat", "missing.txt", "."}, "max_lines": float64(50)})
	for _, want := range []string{"===== a.txt =====\nalpha", "big.txt (lines 1-50 of 900)", "read_file path=big.txt offset=51", "binary file", "missing.txt =====\nerror", "not a regular file"} {
		if !strings.Contains(r.Output, want) {
			t.Errorf("missing %q in:\n%s", want, r.Output)
		}
	}
	if len(r.Output) > types.MaxOutputSize+100 {
		t.Fatalf("output too big: %d", len(r.Output))
	}
	if r := call(readFiles, map[string]any{"paths": "a.txt"}); r.IsErr || !strings.Contains(r.Output, "alpha") {
		t.Fatalf("single string paths: %q", r.Output)
	}
}

func TestFileInfo(t *testing.T) {
	dir := setup(t)
	write(t, dir, "t.txt", "1\n2\n3")
	r := call(fileInfo, map[string]any{"path": "t.txt"})
	if !strings.Contains(r.Output, "lines: 3") || !strings.Contains(r.Output, "binary: false") || !strings.Contains(r.Output, "kind: file") {
		t.Fatalf("%q", r.Output)
	}
}

func TestTruncateHelpersAreUTF8Safe(t *testing.T) {
	s := strings.Repeat("a", types.MaxOutputSize-1) + "é€€€"
	if out := types.TruncateOutput(s).Output; !utf8.ValidString(out) {
		t.Fatal("TruncateOutput split a rune")
	}
	s = strings.Repeat("ሰላም\n", 20000)
	out := types.TruncateHeadTail(s, 4000)
	if !utf8.ValidString(out) || len(out) > 4200 || !strings.Contains(out, "omitted from the middle") {
		t.Fatalf("head/tail: valid=%v len=%d", utf8.ValidString(out), len(out))
	}
	log := strings.Repeat("noise\n", 50000) + "FATAL: the real error\n"
	if out := types.TruncateHeadTail(log, 8000); !strings.HasSuffix(out, "FATAL: the real error\n") {
		t.Fatal("tail was lost")
	}
}

func TestGrepRelativePathResolvesAgainstWorkspace(t *testing.T) {
	dir := setup(t)
	write(t, dir, "proj/f.txt", "the-needle\n")
	r := call(grepHandler, map[string]any{"pattern": "the-needle", "path": "proj"})
	if r.IsErr || !strings.Contains(r.Output, "f.txt") {
		t.Fatalf("%q", r.Output)
	}
}

func TestGNUGrepFallbackUsesExtendedRegex(t *testing.T) {
	args := buildGNUGrepArgs(grepOpts{Pattern: "a|b+", Path: "."})
	hasE := false
	for _, a := range args {
		hasE = hasE || a == "-E"
	}
	if !hasE {
		t.Fatalf("fallback grep must use -E so patterns mean the same as with ripgrep: %v", args)
	}
	for _, a := range buildGNUGrepArgs(grepOpts{Pattern: "a.b", Path: ".", FixedString: true}) {
		if a == "-E" {
			t.Fatal("-E must not be combined with -F")
		}
	}
}
