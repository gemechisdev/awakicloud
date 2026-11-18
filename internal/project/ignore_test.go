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
	keep := []string{
		"internal/sshcreds/store.go", "cmd/awaki/main.go", "go.mod",
		"README.md", ".github/workflows/secret-scan.yml",
	}
	for _, p := range keep {
		if shouldIgnore(p) {
			t.Errorf("shouldIgnore(%q) = true, want false (source must be archived)", p)
		}
	}
}
