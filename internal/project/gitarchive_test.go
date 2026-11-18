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

// The whole point of the change: a project's git history must be inside the
// archive, so it can be restored on a host whose disk is wiped.
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

	for _, want := range []string{
		".git/HEAD",
		".git/config",
		"a.txt",
	} {
		if !hasName(names, want) {
			t.Errorf("archive is missing %s (git state must be archived)", want)
		}
	}
	// A committed object must ship, otherwise a restore is a corrupt repo.
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
	// The index is a cache and is deliberately not shipped.
	if hasName(names, ".git/index") {
		t.Error(".git/index must not be archived (it is a rebuildable cache)")
	}
}

// Restoring an archive must yield a clean working tree, not "everything deleted".
func TestRestoreProducesCleanWorkTree(t *testing.T) {
	src := t.TempDir()
	gitRun(t, src, "init", "-q")
	for i, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = i
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
	status := gitRun(t, dst, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("restored work tree is dirty:\n%s", status)
	}
	if log := gitRun(t, dst, "log", "--oneline"); !strings.Contains(log, "first") {
		t.Errorf("history did not survive restore:\n%s", log)
	}
	// Branches survive too.
	if br := gitRun(t, dst, "branch", "--list"); !strings.Contains(br, "master") && !strings.Contains(br, "main") {
		t.Errorf("default branch did not survive restore:\n%s", br)
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
// This test pins that behaviour deliberately: change detection must never lean
// on a git-aware heuristic, because such a heuristic cannot tell "objects
// pruned" from "objects added" and would silently drop real changes.
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

	namesBefore := objectNames(t, repo)
	gitRun(t, repo, "gc", "--aggressive", "--prune=now")
	if len(objectNames(t, repo)) == len(namesBefore) && len(namesBefore) > 0 && namesBefore[0] == objectNames(t, repo)[0] {
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
	// The archived HEAD still contains the file.
	show := gitRun(t, dst, "show", "HEAD:precious.txt")
	if !strings.Contains(show, "do not lose me") {
		t.Errorf("archived history lost committed content:\n%s", show)
	}
}

// objectNames returns the relative paths of the loose objects in a repo.
func objectNames(t *testing.T, repo string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(repo, ".git", "objects")
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(repo, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}
