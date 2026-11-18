package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// Making an embedded .git directory survive an archive → upload → restore cycle.
//
// The object database, refs and HEAD are archived like any other file, so
// history, branches, remotes and the ability to check out any past commit all
// survive. One thing still needs explicit handling, because a zip only captures
// a file's *content*: .git/index.
//
// .git/index is a cache of the last checkout. If it is restored from an older
// archive than the objects it describes (which is exactly what happens, since
// index is rewritten on nearly every git invocation while refs change rarely),
// git compares HEAD against a stale index and reports the whole tree as
// "deleted and untracked". It is therefore excluded from the archive and
// rebuilt from HEAD on restore — one read-tree, always correct.
//
// The cost of archiving the object database is that git's own housekeeping
// (gc, repack) rewrites the object paths, so the content hash moves and the
// project is re-uploaded once. That is accepted: the alternative is a second
// change-detection signal that cannot distinguish "objects pruned" from
// "objects added" (both move the paths) and would therefore suppress real
// changes. Re-uploading is the safe failure mode; silently dropping a change is
// not.

// HasGitRepo reports whether dir is a git repository root. .git may be a
// directory (normal clone) or a file (linked worktree / submodule).
func HasGitRepo(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	if st.Mode().IsRegular() {
		return true // .git file → worktree or submodule
	}
	if !st.IsDir() {
		return false
	}
	head, err := os.Stat(filepath.Join(dir, ".git", "HEAD"))
	return err == nil && head.Mode().IsRegular()
}

// EnsureGitIndex rebuilds .git/index from HEAD when it is missing.
//
// .git/index is deliberately not shipped in project archives: it is a cache,
// and a stale one produces a wrong "everything changed" diff after restore. It
// can always be regenerated from HEAD with a single read-tree.
func EnsureGitIndex(dir string) error {
	if !HasGitRepo(dir) {
		return nil
	}
	idx := filepath.Join(dir, ".git", "index")
	if _, err := os.Stat(idx); err == nil {
		return nil // already present
	}
	// An unborn HEAD (no commits yet) has no tree to read — nothing to do.
	cmd := exec.Command("git", "-C", dir, "read-tree", "HEAD")
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Debug().Err(err).Str("out", strings.TrimSpace(string(out))).
			Str("dir", dir).Msg("git index rebuild skipped")
		return nil
	}
	log.Info().Str("dir", dir).Msg("git index rebuilt from HEAD after restore")
	return nil
}

// GitHead returns the current HEAD commit (short form), or "" when the tree is
// not a repository or has no commits yet. Recorded in Meta for diagnostics so a
// restored project can be compared against the commit it was archived from.
func GitHead(dir string) string {
	if !HasGitRepo(dir) {
		return ""
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return sha
}

// gitObjectRel reports whether rel is a path inside the git object database.
func gitObjectRel(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), ".git/objects/")
}

// gitRepoRoots returns the directories inside root that hold a .git entry —
// either a repository itself (a .git directory) or a linked worktree/submodule
// (a .git file). Projects are frequently promoted from a scratch workspace that
// already contained a checkout, so the repository is usually one level DOWN
// from the project root (…/projects/<slug>/<repo>/.git) rather than at it.
// Bounded depth keeps the walk cheap on large trees.
func gitRepoRoots(dir string) []string {
	const maxDepth = 3
	var out []string
	root := filepath.Clean(dir)
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(filepath.ToSlash(rel), "/") + 1
		}
		if depth > maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			// Do not descend into a repository looking for more repositories.
			if _, serr := os.Stat(filepath.Join(p, ".git")); serr == nil {
				out = append(out, p)
				return filepath.SkipDir
			}
			switch info.Name() {
			case "node_modules", "vendor", ".venv", "venv", "__pycache__":
				return filepath.SkipDir
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// GitHeads returns "relpath=head" for every repository inside root, so a project
// that merely contains a checkout still gets its revision recorded. Returns nil
// when there is no repository at all.
func GitHeads(dir string) []string {
	roots := gitRepoRoots(dir)
	if len(roots) == 0 {
		return nil
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		head := GitHead(r)
		if head == "" {
			continue
		}
		rel, err := filepath.Rel(dir, r)
		if err != nil {
			rel = r
		}
		out = append(out, filepath.ToSlash(rel)+"="+head)
	}
	return out
}

// EnsureGitIndexes repairs every repository inside root that is missing its
// index (project archives deliberately omit .git/index). Called after a restore.
func EnsureGitIndexes(root string) error {
	for _, r := range gitRepoRoots(root) {
		_ = EnsureGitIndex(r)
	}
	return nil
}
