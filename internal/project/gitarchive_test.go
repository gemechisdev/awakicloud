package project

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// zipNames lists the entry names inside a zip blob.
func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// objectNames returns the relative paths of loose objects in a repository.
func objectNames(t *testing.T, repo string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(filepath.Join(repo, ".git", "objects"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(repo, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

// The whole point: a project's git history must be inside the archive, so it can
// be restored on a host whose disk is wiped on every restart.
func TestGitDirectoryIsArchived(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "first")
	gitRun(t, dir, "branch", "feature")

	data, _, _, _, err := ZipTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := zipNames(t, data)

	for _, want := range []string{".git/HEAD", ".git/config", "a.txt"} {
		if !hasName(names, want) {
			t.Errorf("archive is missing %s (git state must be archived)", want)
		}
	}
	hasObject := false
	for _, n := range names {
		if strings.HasPrefix(n, ".git/objects/") {
			hasObject = true
			break
		}
	}
	if !hasObject {
		t.Error("archive is missing .git/objects (restoring it would corrupt the repository)")
	}
	if hasName(names, ".git/index") {
		t.Error(".git/index must not be archived (it is a rebuildable cache)")
	}
}

// A restored tree must keep its executable bits, otherwise shell scripts shipped
// with a project come back non-runnable.
func TestArchivePreservesFileMode(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _, _, _, err := ZipTree(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := ExtractZip(data, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh restored as %v, want the executable bit preserved", st.Mode().Perm())
	}
	pl, err := os.Stat(filepath.Join(dst, "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Mode().Perm()&0o111 != 0 {
		t.Errorf("plain.txt restored as %v, want non-executable", pl.Mode().Perm())
	}
}

// The common shape: a project promoted from a workspace that already held a
// checkout, so the repository sits one level DOWN from the project root. The
// archive must carry it and a restore must repair its index.
func TestNestedRepositorySurvivesRestore(t *testing.T) {
	src := t.TempDir()
	repo := filepath.Join(src, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("project\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, _, _, _, err := ZipTree(src)
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(zipNames(t, data), "myrepo/.git/HEAD") {
		t.Fatal("nested repository was not archived")
	}

	dst := t.TempDir()
	if err := ExtractZip(data, dst); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dst, "myrepo")
	if status := gitRun(t, restored, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("nested repo restored dirty:\n%s", status)
	}
	if log := gitRun(t, restored, "log", "--oneline"); !strings.Contains(log, "initial") {
		t.Errorf("nested repo lost its history:\n%s", log)
	}
	heads := GitHeads(dst)
	if len(heads) != 1 || !strings.HasPrefix(heads[0], "myrepo=") {
		t.Errorf("GitHeads(%q) = %v, want one entry %q", dst, heads, "myrepo=<sha>")
	}
}

// Restoring an archive must yield a clean working tree, not "everything deleted".
func TestRestoreProducesCleanWorkTree(t *testing.T) {
	src := t.TempDir()
	gitRun(t, src, "init", "-q")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, src, "add", "-A")
	gitRun(t, src, "commit", "-qm", "first")

	data, _, _, _, err := ZipTree(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := ExtractZip(data, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if status := gitRun(t, dst, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("restored work tree is dirty:\n%s", status)
	}
	if log := gitRun(t, dst, "log", "--oneline"); !strings.Contains(log, "first") {
		t.Errorf("history did not survive restore:\n%s", log)
	}
}

// A bare commit rewrites HEAD without touching any working-tree file. The
// content hash covers .git/HEAD, so it must be picked up — this is what protects
// history between archive uploads.
func TestCommitWithoutFileEditIsDetected(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, newMemBlobs())
	slug := "demo"
	if _, err := m.Create(slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	repo := m.ProjectDir(slug)
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "first")
	if _, err := m.Sync(context.Background(), slug, "chat1"); err != nil {
		t.Fatal(err)
	}

	// Amending rewrites the commit without touching any working-tree file.
	gitRun(t, repo, "commit", "--amend", "-qm", "first (amended)")

	synced, _, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Error("a rewritten commit was not detected as a tree change")
	}
}

// git housekeeping (gc folding loose objects into a packfile) rewrites the
// object paths, so the content hash moves and the project is re-uploaded once.
// This pins that behaviour deliberately: change detection must never lean on a
// git-aware heuristic, because such a heuristic cannot tell "objects pruned"
// from "objects added" and would silently drop real changes.
func TestGitGcTriggersOneResync(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, newMemBlobs())
	slug := "demo"
	if _, err := m.Create(slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	repo := m.ProjectDir(slug)
	gitRun(t, repo, "init", "-q")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "first")
	if _, err := m.Sync(context.Background(), slug, "chat1"); err != nil {
		t.Fatal(err)
	}

	before := objectNames(t, repo)
	gitRun(t, repo, "gc", "--aggressive", "--prune=now")
	after := objectNames(t, repo)
	if len(after) == len(before) && len(before) > 0 && before[0] == after[0] {
		t.Skip("gc did not change the object layout; nothing to assert")
	}

	synced, meta, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Error("a repack changed the archive contents but was not re-uploaded")
	}
	// …and it settles: the next tick sees no further change.
	again, _, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("repack did not settle — project re-uploads on every tick")
	}
	if meta.GitHead == "" {
		t.Error("GitHead was not recorded on the synced meta")
	}
}

// Committed data must be recoverable from the archive even after the file is
// removed from the working tree — the reason history is worth archiving.
func TestArchivedHistoryAllowsRecovery(t *testing.T) {
	src := t.TempDir()
	gitRun(t, src, "init", "-q")
	target := filepath.Join(src, "precious.txt")
	if err := os.WriteFile(target, []byte("do not lose me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", "-A")
	gitRun(t, src, "commit", "-qm", "add precious")

	data, _, _, _, err := ZipTree(src)
	if err != nil {
		t.Fatal(err)
	}
	// The file is then deleted and the deletion committed.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", "-A")
	gitRun(t, src, "commit", "-qm", "remove precious")

	dst := t.TempDir()
	if err := ExtractZip(data, dst); err != nil {
		t.Fatal(err)
	}
	if show := gitRun(t, dst, "show", "HEAD:precious.txt"); !strings.Contains(show, "do not lose me") {
		t.Errorf("archived history lost committed content:\n%s", show)
	}
}
