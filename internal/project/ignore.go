package project

import (
	"path/filepath"
	"strings"
)

// DefaultIgnore patterns — never ship these to Telegram.
var DefaultIgnore = []string{
	".git", ".git/**",
	"node_modules", "node_modules/**",
	"vendor", "vendor/**",
	".awaki", ".awaki/**",
	".awaki-project.json",
	".awaki-uploads", ".awaki-uploads/**",
	"dist", "dist/**",
	"build", "build/**",
	"target", "target/**",
	".env", ".env.*",
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

func shouldIgnore(rel string) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	for _, pat := range DefaultIgnore {
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
