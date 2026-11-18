package project

import (
	"os"
	"os/exec"
	"path/filepath"
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

// HasGitRepo reports whether dir contains a usable git repository.
func HasGitRepo(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git", "HEAD"))
	return err == nil && st.Mode().IsRegular()
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
