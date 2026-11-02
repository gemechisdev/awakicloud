package system

import (
	"os"
	"regexp"
	"strings"
)

// Environment hygiene for child processes (bash, git).
//
// The agent process holds every provider key and the Telegram credentials in
// its environment. Commands the model runs — which may be steered by
// instructions hidden in a web page, an issue, or a file — must not inherit
// them: a single `env` would otherwise hand them over.
//
// Policy (AWAKI_BASH_ENV):
//   scrub    (default) drop variables that look like credentials
//   inherit  pass everything through (old behaviour)
// AWAKI_BASH_ENV_PASSTHROUGH="GITHUB_TOKEN,NPM_TOKEN" keeps named variables
// that a workflow genuinely needs.

var (
	secretNameRe = regexp.MustCompile(`(?i)(^|_)(` +
		`api_?key|apikey|(access|secret|private|signing|encryption|auth|master|service)_?key|` +
		`token|secret|passw(or)?d|passphrase|credentials?|cookie|bearer|dsn|api_?hash|session|jwt|salt|otp|signature` +
		`)(_|$)`)

	// Values that are recognisably credentials regardless of the variable's name.
	secretValueRe = regexp.MustCompile(`^(` +
		`sk-[A-Za-z0-9_-]{16,}|sk-ant-|xox[abprs]-|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_|glpat-|AKIA[0-9A-Z]{12,}|` +
		`AIza[0-9A-Za-z_-]{30,}|[0-9]{6,12}:[A-Za-z0-9_-]{30,}|-----BEGIN [A-Z ]*PRIVATE KEY-----` +
		`)`)

	// Names that look sensitive but are harmless handles/paths needed by tools.
	envAlwaysKeep = map[string]bool{
		"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "GPG_AGENT_INFO": true, "GPG_TTY": true,
		"DBUS_SESSION_BUS_ADDRESS": true, "GIT_ASKPASS": true, "SUDO_ASKPASS": true, "KRB5CCNAME": true,
		"AWAKI_WORK_DIR": true, "AWAKI_DATA_DIR": true,
	}
)

// EnvPolicy decides which variables child processes receive.
type EnvPolicy struct {
	Inherit     bool
	Passthrough map[string]bool
}

// EnvPolicyFromOS reads AWAKI_BASH_ENV / AWAKI_BASH_ENV_PASSTHROUGH.
func EnvPolicyFromOS() EnvPolicy {
	p := EnvPolicy{Passthrough: map[string]bool{}}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AWAKI_BASH_ENV")), "inherit") {
		p.Inherit = true
	}
	for _, name := range strings.Split(os.Getenv("AWAKI_BASH_ENV_PASSTHROUGH"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			p.Passthrough[name] = true
		}
	}
	return p
}

// isSecretVar reports whether an environment entry should be withheld.
func isSecretVar(name, value string) bool {
	if envAlwaysKeep[name] || strings.HasPrefix(name, "XDG_") {
		return false
	}
	// Everything the agent itself is configured with, other than the two path hints above.
	if strings.HasPrefix(name, "AWAKI_") || strings.HasPrefix(name, "TELEGRAM_") {
		return true
	}
	if secretNameRe.MatchString(name) {
		return true
	}
	return secretValueRe.MatchString(value)
}

// ScrubEnv returns env (KEY=VALUE entries) without credential-like variables,
// plus the names that were removed (for logging; never the values).
func ScrubEnv(env []string, p EnvPolicy) (kept, removed []string) {
	if p.Inherit {
		return env, nil
	}
	kept = make([]string, 0, len(env))
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if p.Passthrough[name] || !isSecretVar(name, value) {
			kept = append(kept, kv)
			continue
		}
		removed = append(removed, name)
	}
	return kept, removed
}

// safeEnviron is os.Environ() filtered by the configured policy.
func safeEnviron() []string {
	kept, _ := ScrubEnv(os.Environ(), EnvPolicyFromOS())
	return kept
}
