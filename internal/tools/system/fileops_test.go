package system

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestMoveFile(t *testing.T) {
	dir := setup(t)
	a := write(t, dir, "a.txt", "A")
	r := call(moveFile, map[string]any{"from": "a.txt", "to": "sub/dir/b.txt"})
	if r.IsErr || exists(a) || read(t, filepath.Join(dir, "sub/dir/b.txt")) != "A" {
		t.Fatalf("%q", r.Output)
	}
	// refuses overwrite by default
	write(t, dir, "c.txt", "C")
	r = call(moveFile, map[string]any{"from": "c.txt", "to": "sub/dir/b.txt"})
	if !r.IsErr || !strings.Contains(r.Output, "exists") || read(t, filepath.Join(dir, "sub/dir/b.txt")) != "A" {
		t.Fatalf("%q", r.Output)
	}
	r = call(moveFile, map[string]any{"from": "c.txt", "to": "sub/dir/b.txt", "overwrite": true})
	if r.IsErr || read(t, filepath.Join(dir, "sub/dir/b.txt")) != "C" {
		t.Fatalf("%q", r.Output)
	}
	// mv semantics: destination directory receives the source
	write(t, dir, "d.txt", "D")
	r = call(moveFile, map[string]any{"from": "d.txt", "to": "sub"})
	if r.IsErr || !exists(filepath.Join(dir, "sub/d.txt")) {
		t.Fatalf("%q", r.Output)
	}
	// directory into itself
	r = call(moveFile, map[string]any{"from": "sub", "to": "sub/dir/inner"})
	if !r.IsErr || !strings.Contains(r.Output, "into itself") {
		t.Fatalf("%q", r.Output)
	}
	if r = call(moveFile, map[string]any{"from": "nope", "to": "x"}); !r.IsErr {
		t.Fatal("missing source should error")
	}
}

func TestCopyFile(t *testing.T) {
	dir := setup(t)
	write(t, dir, "src/a.txt", "A")
	write(t, dir, "src/deep/b.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(dir, "src/deep/b.sh"), 0o755)
	if r := call(copyFileTool, map[string]any{"from": "src", "to": "dst"}); !r.IsErr || !strings.Contains(r.Output, "recursive") {
		t.Fatalf("dir copy without recursive: %q", r.Output)
	}
	r := call(copyFileTool, map[string]any{"from": "src", "to": "dst", "recursive": true})
	if r.IsErr || !strings.Contains(r.Output, "2 files") {
		t.Fatalf("%q", r.Output)
	}
	if read(t, filepath.Join(dir, "dst/a.txt")) != "A" {
		t.Fatal("content")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "dst/deep/b.sh")); st.Mode().Perm() != 0o755 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
		os.Symlink("a.txt", filepath.Join(dir, "src/link"))
		call(copyFileTool, map[string]any{"from": "src", "to": "dst2", "recursive": true})
		if tgt, err := os.Readlink(filepath.Join(dir, "dst2/link")); err != nil || tgt != "a.txt" {
			t.Fatalf("symlink not preserved: %v %q", err, tgt)
		}
	}
	if r := call(copyFileTool, map[string]any{"from": "src/a.txt", "to": "dst/a.txt"}); !r.IsErr {
		t.Fatal("must not overwrite silently")
	}
	if r := call(copyFileTool, map[string]any{"from": "src", "to": "src/inner", "recursive": true}); !r.IsErr {
		t.Fatal("copy into itself must fail")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "dst"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".awaki-copy-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDeleteFile(t *testing.T) {
	dir := setup(t)
	write(t, dir, "f.txt", "x")
	if r := call(deleteFile, map[string]any{"path": "f.txt"}); r.IsErr || exists(filepath.Join(dir, "f.txt")) {
		t.Fatalf("%q", r.Output)
	}
	write(t, dir, "tree/a/b.txt", "x")
	write(t, dir, "tree/c.txt", "x")
	r := call(deleteFile, map[string]any{"path": "tree"})
	if !r.IsErr || !strings.Contains(r.Output, "recursive=true") || !strings.Contains(r.Output, "2 files") || !exists(filepath.Join(dir, "tree")) {
		t.Fatalf("%q", r.Output)
	}
	if r = call(deleteFile, map[string]any{"path": "tree", "recursive": true}); r.IsErr || exists(filepath.Join(dir, "tree")) {
		t.Fatalf("%q", r.Output)
	}
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	if r = call(deleteFile, map[string]any{"path": "empty"}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	if r = call(deleteFile, map[string]any{"path": "ghost"}); !r.IsErr || !strings.Contains(r.Output, "not found") {
		t.Fatalf("%q", r.Output)
	}
}

func TestDeleteProtectedPaths(t *testing.T) {
	dir := setup(t)
	if runtime.GOOS == "windows" {
		t.Skip("unix paths")
	}
	for _, p := range []string{"/", "/etc", dir, filepath.Dir(dir)} {
		r := call(deleteFile, map[string]any{"path": p, "recursive": true})
		if !r.IsErr || !strings.Contains(r.Output, "refusing") {
			t.Errorf("delete %q was not refused: %q", p, r.Output)
		}
	}
	if !exists(dir) {
		t.Fatal("workspace root was deleted")
	}
	if r := call(moveFile, map[string]any{"from": dir, "to": dir + "-moved"}); !r.IsErr {
		t.Fatal("moving the workspace root must be refused")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if r := call(deleteFile, map[string]any{"path": home, "recursive": true}); !r.IsErr {
			t.Fatal("home must be protected")
		}
	}
}

func TestDeleteSymlinkRemovesLinkOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := setup(t)
	real := write(t, dir, "real/keep.txt", "x")
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "lnk"))
	if r := call(deleteFile, map[string]any{"path": "lnk"}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	if !exists(real) || exists(filepath.Join(dir, "lnk")) {
		t.Fatal("symlink delete followed the link")
	}
}

func TestMakeDir(t *testing.T) {
	dir := setup(t)
	if r := call(makeDir, map[string]any{"path": "a/b/c"}); r.IsErr || !strings.Contains(r.Output, "Created") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(makeDir, map[string]any{"path": "a/b/c"}); r.IsErr || !strings.Contains(r.Output, "already exists") {
		t.Fatalf("%q", r.Output)
	}
	write(t, dir, "f", "x")
	if r := call(makeDir, map[string]any{"path": "f"}); !r.IsErr {
		t.Fatal("mkdir over a file must fail")
	}
}
