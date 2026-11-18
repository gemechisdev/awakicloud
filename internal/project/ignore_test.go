package project

import "testing"

// TestSecretFilesAreIgnored guards that key material never enters a project
// archive (which is uploaded to storage) — only real source should ship.
func TestSecretFilesAreIgnored(t *testing.T) {
	must := []string{
		"deploy.key", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"foo.pem", "a.p12", "b.pfx", "c.ppk",
		"ssh/keys/github.com-gemechisdev", "ssh/identities.json",
		".env", ".env.local", ".awaki-project.json", ".awaki-uploads/x.tar.gz",
		"known_hosts", "credentials.json",
	}
	for _, p := range must {
		if !shouldIgnore(p) {
			t.Errorf("shouldIgnore(%q) = false, want true (secret must not be archived)", p)
		}
	}
	// .git ships (history is the point) but its index cache does not.
	mustGitInternal := []string{".git/index", ".git/index.lock"}
	for _, p := range mustGitInternal {
		if !shouldExcludeFromArchive(p) {
			t.Errorf("shouldExcludeFromArchive(%q) = false, want true (cache must not be archived)", p)
		}
	}
	keep := []string{
		"internal/sshcreds/store.go", "cmd/awaki/main.go", "go.mod",
		"README.md", ".github/workflows/secret-scan.yml",
		// git history is deliberately archived
		".git/HEAD", ".git/config", ".git/refs/heads/main",
		".git/objects/ab/cdef0123456789", ".git/packed-refs",
		".env.example",
	}
	for _, p := range keep {
		if shouldExcludeFromArchive(p) {
			t.Errorf("shouldExcludeFromArchive(%q) = true, want false (must be archived)", p)
		}
	}
	// Regression: the archive rule and the ignore rule must agree on secrets.
	for _, p := range must {
		if !shouldIgnore(p) && !isGitInternal(p) {
			t.Errorf("shouldIgnore(%q) = false, want true (secret must not be archived)", p)
		}
	}
}
