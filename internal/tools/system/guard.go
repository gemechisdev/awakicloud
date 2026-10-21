package system

import (
	"regexp"
	"strings"
)

// Destructive patterns that should be flagged before bash execution.
// Port of Awaki Android DestructiveCommandGuard concepts for a server agent.
var destructivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r|--recursive.*--force|--force.*--recursive)\b`),
	regexp.MustCompile(`(?i)\brm\s+-rf\b`),
	regexp.MustCompile(`(?i)\brm\s+-fr\b`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\s+if=`),
	regexp.MustCompile(`(?i)\b>\s*/dev/sd`),
	regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\b`),
	regexp.MustCompile(`(?i)\bchown\s+-R\s+`),
	regexp.MustCompile(`(?i)\bgit\s+push\s+.*--force\b`),
	regexp.MustCompile(`(?i)\bgit\s+push\s+-f\b`),
	regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\b`),
	regexp.MustCompile(`(?i)\bgit\s+clean\s+-fd\b`),
	regexp.MustCompile(`(?i)\bgit\s+clean\s+-f\b`),
	regexp.MustCompile(`(?i)\bdrop\s+(table|database)\b`),
	regexp.MustCompile(`(?i)\btruncate\s+table\b`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\breboot\b`),
	regexp.MustCompile(`(?i)\bkill\s+-9\s+-1\b`),
	regexp.MustCompile(`(?i)\b:(){ :\|:& };:`), // fork bomb
	regexp.MustCompile(`(?i)\bcurl\s+[^\n]*\|\s*(ba)?sh\b`),
	regexp.MustCompile(`(?i)\bwget\s+[^\n]*\|\s*(ba)?sh\b`),
}

// GuardMode controls how destructive commands are handled.
type GuardMode string

const (
	GuardOff        GuardMode = "off"        // never block
	GuardWarn       GuardMode = "warn"       // annotate but allow (default)
	GuardBlock      GuardMode = "block"      // refuse destructive commands
)

// CheckDestructive returns a human-readable warning if the command looks destructive.
func CheckDestructive(command string) (isDestructive bool, reason string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false, ""
	}
	for _, re := range destructivePatterns {
		if re.MatchString(cmd) {
			return true, "command matches a known destructive pattern (" + re.String() + ")"
		}
	}
	// Broad heuristic: rm of absolute paths under /
	if strings.Contains(cmd, "rm ") && (strings.Contains(cmd, " /") || strings.Contains(cmd, " /*")) {
		if !strings.Contains(cmd, "/tmp") && !strings.Contains(cmd, "/home") {
			return true, "rm targeting a system path"
		}
	}
	return false, ""
}
