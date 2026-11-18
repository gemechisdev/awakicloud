package project

import (
	"path/filepath"
	"strings"
)

// DefaultIgnore patterns — never ship these to cloud storage.
//
// Note what is NOT here: ".git". A project's git history is deliberately kept
// in the archive, because it is the cheapest form of backup and rollback the
// agent has on a host whose disk is wiped on every restart. Only .git/index is
// excluded (a cache) — see gitInternalIgnore and gitstate.go.
var DefaultIgnore = []string{
	"node_modules", "node_modules/**",
	"vendor", "vendor/**",
	".awaki", ".awaki/**",
	".awaki-project.json",
	".awaki-uploads", ".awaki-uploads/**",
	"dist", "dist/**",
	"build", "build/**",
	"target", "target/**",
	".env", ".env.*",
	"!.env.example",
	"*.pem", "*.key",
	"*.p12", "*.pfx", "*.ppk",
	"id_rsa", "id_rsa.*", "id_ed25519", "id_ed25519.*",
	"id_ecdsa", "id_ecdsa.*", "id_dsa", "id_dsa.*",
	"ssh/keys", "ssh/keys/**", "ssh/identities.json",
	"known_hosts",
	"credentials*",
	".DS_Store", "Thumbs.db",
	"__pycache__", "__pycache__/**",
	".venv", ".venv/**", "venv", "venv/**",
}

// gitInternalIgnore lists the parts of .git that must not be shipped.
//
//	.git/index — a pure cache; rebuilt from HEAD on restore (EnsureGitIndex).
//	             Shipping it would also make a restored tree look "all deleted".
//
// Everything else in .git — HEAD, refs, packed-refs, config, hooks, logs and
// the whole object database — is archived, so history, branches, remotes and the
// ability to check out any past commit all survive a restore. Objects are large
// but they are the only copy of committed data; dropping them would restore a
// corrupt repository.
var gitInternalIgnore = []string{
	".git/index", ".git/index.lock",
}

// shouldExcludeFromArchive is the single rule used by the zip builder, the
// content hash and the copy helper — so all three always agree.
func shouldExcludeFromArchive(rel string) bool {
	if isGitInternal(rel) {
		return true
	}
	return shouldIgnore(rel)
}

func isGitInternal(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, pat := range gitInternalIgnore {
		pat = filepath.ToSlash(pat)
		if pat == rel {
			return true
		}
		if strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
		}
	}
	return false
}

func shouldIgnore(rel string) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	// Allow-list takes precedence so .env.example (a template, no secrets) ships.
	for _, pat := range DefaultIgnore {
		if strings.HasPrefix(pat, "!") {
			neg := filepath.ToSlash(strings.TrimPrefix(pat, "!"))
			if neg == rel || neg == base {
				return false
			}
		}
	}
	for _, pat := range DefaultIgnore {
		if strings.HasPrefix(pat, "!") {
			continue
		}
		pat = filepath.ToSlash(pat)
		if pat == rel || pat == base {
			return true
		}
		if strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
		}
		if strings.HasPrefix(pat, "*.") && strings.HasSuffix(base, pat[1:]) {
			return true
		}
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
	}
	return false
}
