package system

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gemechischala/awaki/internal/tools/types"
)

func gitTool_(t *testing.T, name string) types.Handler {
	t.Helper()
	for _, tl := range GitTools() {
		if tl.Name == name {
			return tl.Handler
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func g(t *testing.T, name string, in map[string]any) types.Result {
	return gitTool_(t, name)(context.Background(), in)
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := setup(t)
	sh(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	sh(t, dir, "add", "-A")
	sh(t, dir, "commit", "-q", "-m", "initial commit")
	return dir
}

func TestGitRunsInWorkspaceNotCwd(t *testing.T) {
	dir := repo(t)
	r := g(t, "git_status", map[string]any{})
	if r.IsErr || !strings.Contains(r.Output, "## main") || !strings.Contains(r.Output, "working tree clean") {
		t.Fatalf("%q", r.Output)
	}
	write(t, dir, "new.txt", "x")
	r = g(t, "git_status", map[string]any{})
	if !strings.Contains(r.Output, "?? new.txt") {
		t.Fatalf("%q", r.Output)
	}
}

func TestGitRefInjectionRejected(t *testing.T) {
	dir := repo(t)
	out := filepath.Join(t.TempDir(), "pwned.txt")
	for _, tc := range []struct {
		tool string
		in   map[string]any
	}{
		{"git_show", map[string]any{"ref": "--output=" + out}},
		{"git_diff", map[string]any{"ref": "--output=" + out}},
		{"git_log", map[string]any{"ref": "--output=" + out}},
		{"git_blame", map[string]any{"file": "a.txt", "ref": "--output=" + out}},
		{"git_checkout", map[string]any{"ref": "--orphan=x"}},
		{"git_reset", map[string]any{"ref": "--hard"}},
		{"git_restore", map[string]any{"files": []any{"a.txt"}, "source": "--foo"}},
		{"git_sync", map[string]any{"action": "push", "remote": "--receive-pack=touch " + out}},
		{"git_branch_update", map[string]any{"action": "create", "name": "-x"}},
		{"git_show", map[string]any{"ref": "HEAD; touch " + out}},
	} {
		r := g(t, tc.tool, tc.in)
		if !r.IsErr {
			t.Errorf("%s %v was not rejected: %q", tc.tool, tc.in, r.Output)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("option injection created a file")
	}
	_ = dir
}

func TestGitDiffVariants(t *testing.T) {
	dir := repo(t)
	write(t, dir, "a.txt", "one\nTWO\nthree\n")
	r := g(t, "git_diff", map[string]any{})
	if !strings.Contains(r.Output, "-two") || !strings.Contains(r.Output, "+TWO") {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_diff", map[string]any{"stat": true}); !strings.Contains(r.Output, "a.txt | 2 +-") {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_diff", map[string]any{"name_only": true}); !strings.Contains(r.Output, "M\ta.txt") {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_diff", map[string]any{"staged": true}); r.Output != "(no differences)" {
		t.Fatalf("%q", r.Output)
	}
	g(t, "git_stage", map[string]any{"files": []any{"a.txt"}})
	if r = g(t, "git_diff", map[string]any{"staged": true}); !strings.Contains(r.Output, "+TWO") {
		t.Fatalf("%q", r.Output)
	}
	g(t, "git_commit", map[string]any{"message": "second"})
	if r = g(t, "git_diff", map[string]any{"ref": "HEAD~1..HEAD", "file": "a.txt"}); !strings.Contains(r.Output, "+TWO") {
		t.Fatalf("range diff: %q", r.Output)
	}
	if r = g(t, "git_diff", map[string]any{"ref": "HEAD~1", "ignore_whitespace": true, "context": float64(0)}); !strings.Contains(r.Output, "@@") {
		t.Fatalf("%q", r.Output)
	}
}

func TestGitLogFiltersAndShowPatch(t *testing.T) {
	dir := repo(t)
	write(t, dir, "b.txt", "bee\n")
	sh(t, dir, "add", "-A")
	sh(t, dir, "commit", "-q", "-m", "add bee", "--author=Zed <z@z>")
	r := g(t, "git_log", map[string]any{})
	if !strings.Contains(r.Output, "add bee") || !strings.Contains(r.Output, "initial commit") {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_log", map[string]any{"author": "Zed"}); strings.Contains(r.Output, "initial") || !strings.Contains(r.Output, "add bee") {
		t.Fatalf("author filter: %q", r.Output)
	}
	if r = g(t, "git_log", map[string]any{"grep": "initial"}); strings.Contains(r.Output, "bee") {
		t.Fatalf("grep filter: %q", r.Output)
	}
	if r = g(t, "git_log", map[string]any{"file": "b.txt"}); strings.Contains(r.Output, "initial") {
		t.Fatalf("file filter: %q", r.Output)
	}
	if r = g(t, "git_log", map[string]any{"grep": "nothing-matches"}); r.Output != "(no matching commits)" {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_log", map[string]any{"patch": true, "n": float64(50)}); !strings.Contains(r.Output, "+bee") {
		t.Fatalf("patch: %q", r.Output)
	}
	r = g(t, "git_show", map[string]any{})
	if !strings.Contains(r.Output, "add bee") || !strings.Contains(r.Output, "+bee") {
		t.Fatalf("show must include the patch: %q", r.Output)
	}
	r = g(t, "git_show", map[string]any{"stat_only": true})
	if !strings.Contains(r.Output, "b.txt") || strings.Contains(r.Output, "+bee") {
		t.Fatalf("stat_only: %q", r.Output)
	}
	r = g(t, "git_show", map[string]any{"ref": "HEAD~1", "file": "a.txt", "content": true})
	if !strings.Contains(r.Output, "one\ntwo\nthree") {
		t.Fatalf("content at ref: %q", r.Output)
	}
}

func TestGitBlame(t *testing.T) {
	repo(t)
	r := g(t, "git_blame", map[string]any{"file": "a.txt", "start": float64(2), "end": float64(2)})
	if r.IsErr || !strings.Contains(r.Output, "two") || strings.Contains(r.Output, "three") {
		t.Fatalf("%q", r.Output)
	}
}

func TestGitBranchCheckoutRestoreStashReset(t *testing.T) {
	dir := repo(t)
	if r := g(t, "git_checkout", map[string]any{"ref": "feature", "create": true}); r.IsErr {
		t.Fatal(r.Output)
	}
	if r := g(t, "git_branch", map[string]any{}); !strings.Contains(r.Output, "* feature") || !strings.Contains(r.Output, "main") {
		t.Fatalf("%q", r.Output)
	}
	write(t, dir, "a.txt", "CHANGED\n")
	// stash round trip
	if r := g(t, "git_stash", map[string]any{"message": "wip"}); r.IsErr {
		t.Fatal(r.Output)
	}
	if read(t, filepath.Join(dir, "a.txt")) != "one\ntwo\nthree\n" {
		t.Fatal("stash did not restore the tree")
	}
	if r := g(t, "git_stash", map[string]any{"action": "list"}); !strings.Contains(r.Output, "wip") {
		t.Fatalf("%q", r.Output)
	}
	if r := g(t, "git_stash", map[string]any{"action": "pop"}); r.IsErr || read(t, filepath.Join(dir, "a.txt")) != "CHANGED\n" {
		t.Fatalf("%q", r.Output)
	}
	// restore discards
	if r := g(t, "git_restore", map[string]any{"files": []any{"a.txt"}}); r.IsErr || read(t, filepath.Join(dir, "a.txt")) != "one\ntwo\nthree\n" {
		t.Fatalf("%q", r.Output)
	}
	// commit on feature, switch back, delete (unmerged => refuse, force => ok)
	write(t, dir, "f.txt", "f")
	g(t, "git_stage", map[string]any{"all": true})
	if r := g(t, "git_commit", map[string]any{"message": "feature work", "author": "Ann <a@a>"}); r.IsErr {
		t.Fatal(r.Output)
	}
	if out := sh(t, dir, "log", "-1", "--format=%an"); strings.TrimSpace(out) != "Ann" {
		t.Fatalf("author override: %q", out)
	}
	g(t, "git_checkout", map[string]any{"ref": "main"})
	if r := g(t, "git_branch_update", map[string]any{"action": "delete", "name": "feature"}); !r.IsErr {
		t.Fatalf("unmerged delete should be refused: %q", r.Output)
	}
	if r := g(t, "git_branch_update", map[string]any{"action": "delete", "name": "feature", "force": true}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	if r := g(t, "git_branch_update", map[string]any{"action": "create", "name": "x1"}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	if r := g(t, "git_branch_update", map[string]any{"action": "rename", "name": "x1", "new_name": "x2"}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	// reset --hard needs confirmation and shows what would be lost
	write(t, dir, "a.txt", "dirty\n")
	r := g(t, "git_reset", map[string]any{"mode": "hard"})
	if !r.IsErr || !strings.Contains(r.Output, "confirm=true") || !strings.Contains(r.Output, "a.txt") || read(t, filepath.Join(dir, "a.txt")) != "dirty\n" {
		t.Fatalf("%q", r.Output)
	}
	if r = g(t, "git_reset", map[string]any{"mode": "hard", "confirm": true}); r.IsErr || read(t, filepath.Join(dir, "a.txt")) != "one\ntwo\nthree\n" {
		t.Fatalf("%q", r.Output)
	}
}

func TestGitCommitAmendAndIdentity(t *testing.T) {
	dir := repo(t)
	t.Setenv("HOME", t.TempDir()) // no global identity
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	cmd := exec.Command("git", "config", "--unset-all", "user.name")
	cmd.Dir = dir
	_ = cmd.Run()
	write(t, dir, "z.txt", "z")
	g(t, "git_stage", map[string]any{"files": []any{"z.txt"}})
	if r := g(t, "git_commit", map[string]any{"message": "needs identity"}); r.IsErr {
		t.Fatalf("commit without configured identity must still work: %q", r.Output)
	}
	if r := g(t, "git_commit", map[string]any{"amend": true}); r.IsErr {
		t.Fatalf("amend without message: %q", r.Output)
	}
	if r := g(t, "git_commit", map[string]any{}); !r.IsErr {
		t.Fatal("message required")
	}
	if r := g(t, "git_commit", map[string]any{"message": "x", "author": "-bad"}); !r.IsErr {
		t.Fatal("bad author must be rejected")
	}
	if r := g(t, "git_stage", map[string]any{"files": []any{"z.txt"}, "unstage": true}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
}

func TestGitSyncPushPullFetch(t *testing.T) {
	dir := repo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	sh(t, dir, "init", "-q", "--bare", "-b", "main", bare)
	sh(t, dir, "remote", "add", "origin", bare)
	if r := g(t, "git_sync", map[string]any{"action": "push", "set_upstream": true, "branch": "main"}); r.IsErr {
		t.Fatalf("push: %q", r.Output)
	}
	clone := filepath.Join(t.TempDir(), "other")
	sh(t, dir, "clone", "-q", bare, clone)
	write(t, clone, "remote.txt", "r")
	sh(t, clone, "add", "-A")
	sh(t, clone, "commit", "-q", "-m", "remote change")
	sh(t, clone, "push", "-q", "origin", "main")
	if r := g(t, "git_sync", map[string]any{"action": "fetch"}); r.IsErr {
		t.Fatalf("fetch: %q", r.Output)
	}
	if r := g(t, "git_sync", map[string]any{"action": "pull", "ff_only": true}); r.IsErr || !exists(filepath.Join(dir, "remote.txt")) {
		t.Fatalf("pull: %q", r.Output)
	}
	// diverged push is refused (no plain --force is offered)
	write(t, dir, "local.txt", "l")
	g(t, "git_stage", map[string]any{"all": true})
	g(t, "git_commit", map[string]any{"message": "local"})
	write(t, clone, "remote2.txt", "r2")
	sh(t, clone, "add", "-A")
	sh(t, clone, "commit", "-q", "-m", "remote 2")
	sh(t, clone, "push", "-q", "origin", "main")
	if r := g(t, "git_sync", map[string]any{"action": "push"}); !r.IsErr {
		t.Fatalf("non-fast-forward push should fail: %q", r.Output)
	}
	if r := g(t, "git_sync", map[string]any{"action": "wat"}); !r.IsErr {
		t.Fatal("unknown action")
	}
}

func TestCloneURLValidation(t *testing.T) {
	good := []string{"https://github.com/a/b.git", "git@github.com:a/b.git", "ssh://git@host.example/a/b", "git://example.org/x.git"}
	bad := []string{"", "-u", "--upload-pack=touch /tmp/x", "/etc", "../repo", "file:///etc", "ext::sh -c id", "http://localhost/x", "https://127.0.0.1/x", "https://10.0.0.5/x", "https://169.254.169.254/latest", "https://printer.local/x", "ftp://example.com/x"}
	for _, u := range good {
		if err := validateCloneURL(u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validateCloneURL(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	setup(t)
	for _, tl := range GitTools() {
		if tl.Name == "git_clone" {
			if r := tl.Handler(context.Background(), map[string]any{"url": "/etc"}); !r.IsErr {
				t.Fatal("local clone must be refused")
			}
		}
	}
}

func TestValidateRef(t *testing.T) {
	for _, ok := range []string{"main", "HEAD~3", "origin/feature-1", "v1.2.3", "a..b", "a...b", "stash@{0}", "abc123:path/file.go", "HEAD^"} {
		if err := validateRef("ref", ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "--output=x", "a b", "a;b", "a$(id)", "a`id`", "a\nb", "a|b", "a'b"} {
		if err := validateRef("ref", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGitNotARepoHint(t *testing.T) {
	setup(t)
	r := g(t, "git_status", map[string]any{})
	if !r.IsErr || !strings.Contains(r.Output, "Hint") {
		t.Fatalf("%q", r.Output)
	}
}
