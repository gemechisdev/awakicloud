package system

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/gemechischala/awaki/internal/workspace"
)

// Command guard: a safety net against catastrophic or surprising shell commands.
//
// It is NOT a sandbox. Anyone who can run arbitrary shell can get around any
// text-level check (encoding, indirection, scripts written then executed). Real
// containment is the permission mode (ask/plan), running the agent as an
// unprivileged user, and container limits. What the guard does well is stop the
// accidental and the obvious: `rm -rf /`, a stray `$VAR/` that expands to
// nothing, mkfs on the wrong device, force-pushes.
//
// Severities:
//   critical  irrecoverable system damage with no legitimate agent use
//             (wipe /, system dirs, home; mkfs; writing to block devices; fork bombs)
//   risky     destructive but sometimes intended (rm -rf of a project dir, git reset
//             --hard, force-push, DROP TABLE, curl | sh, stopping sshd, ...)
//
// AWAKI_BASH_GUARD:
//   warn  (default)  block critical, run risky ones and append a warning to the result
//   block            block both
//   off              no checks at all

type GuardMode string

const (
	GuardOff   GuardMode = "off"
	GuardWarn  GuardMode = "warn"
	GuardBlock GuardMode = "block"
)

// ParseGuardMode maps the environment value to a mode; anything unknown is warn
// (a typo must never silently disable the guard).
func ParseGuardMode(s string) GuardMode {
	switch GuardMode(strings.ToLower(strings.TrimSpace(s))) {
	case GuardOff:
		return GuardOff
	case GuardBlock:
		return GuardBlock
	default:
		return GuardWarn
	}
}

type Severity int

const (
	SevNone Severity = iota
	SevRisky
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "critical"
	case SevRisky:
		return "risky"
	}
	return "none"
}

// CheckDestructive reports whether the command looks destructive (any severity).
func CheckDestructive(command string) (bool, string) {
	sev, reason := Assess(command)
	return sev != SevNone, reason
}

// Assess returns the highest severity found in the command and why.
func Assess(command string) (Severity, string) { return assess(command, 0) }

func assess(command string, depth int) (Severity, string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" || depth > 4 {
		return SevNone, ""
	}
	best, why := SevNone, ""
	consider := func(s Severity, r string) {
		if s > best {
			best, why = s, r
		}
	}
	outside := blankQuoted(cmd)
	sqlClient := -1 // lazily computed: -1 unknown, 0 no, 1 yes
	for _, p := range textPatterns {
		subject := cmd
		if p.outsideOnly {
			subject = outside
		}
		if !p.re.MatchString(subject) {
			continue
		}
		if p.needsSQL {
			if sqlClient < 0 {
				sqlClient = 0
				if mentionsSQLClient(cmd) {
					sqlClient = 1
				}
			}
			if sqlClient == 0 {
				continue
			}
		}
		consider(p.sev, p.reason)
	}
	for _, sub := range extractSubstitutions(cmd) {
		if s, r := assess(sub, depth+1); s != SevNone {
			consider(s, r+" (inside a command substitution)")
		}
	}
	for _, argv := range splitCommands(cmd) {
		if s, r := assessArgv(argv, depth); s != SevNone {
			consider(s, r)
		}
	}
	return best, why
}

// ------------------------------------------------------- text-level patterns

type textPattern struct {
	re          *regexp.Regexp
	sev         Severity
	reason      string
	outsideOnly bool // match against the command with quoted text blanked out (so echo '| sh' is not a pipe)
	needsSQL    bool // only when a SQL client appears in the command
}

var textPatterns = []textPattern{
	{re: regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), sev: SevCritical, reason: "fork bomb"},
	{re: regexp.MustCompile(`>\s*/dev/(sd|hd|vd|xvd|nvme|mmcblk|disk|loop|dm-)[a-z0-9]*`), sev: SevCritical, reason: "redirect that overwrites a block device"},
	{re: regexp.MustCompile(`>>?\s*/etc/(passwd|shadow|group|sudoers|fstab|ssh/|pam)`), sev: SevRisky, reason: "overwrites a critical system file in /etc"},
	{re: regexp.MustCompile(`\|\s*(sudo\s+(-\S+\s+)*)?(ba|z|da|k|a)?sh\b`), sev: SevRisky, reason: "pipes data into a shell (e.g. curl | sh) — the script is run unseen", outsideOnly: true},
	{re: regexp.MustCompile(`(?i)\bdrop\s+(table|database|schema)\s+(if\s+exists\s+)?[\w."` + "`" + `\[\]]+\s*(;|$|'|")`), sev: SevRisky, reason: "SQL DROP statement", needsSQL: true},
	{re: regexp.MustCompile(`(?i)\btruncate\s+table\s+[\w."` + "`" + `\[\]]+`), sev: SevRisky, reason: "SQL TRUNCATE TABLE", needsSQL: true},
	{re: regexp.MustCompile(`/proc/(\d+|\*|\$\$|\$\{?\w+\}?)/(environ|mem)\b`), sev: SevRisky, reason: "reads another process's environment or memory (credential exposure)"},
	{re: regexp.MustCompile(`\.ssh/(id_[a-z0-9_]+|authorized_keys)\b|\.aws/credentials|\.docker/config\.json|\.netrc\b|\.pgpass\b|/etc/shadow\b`), sev: SevRisky, reason: "touches credential files"},
}

var sqlClients = map[string]bool{
	"psql": true, "mysql": true, "mariadb": true, "sqlite3": true, "sqlcmd": true, "mongosh": true,
	"clickhouse-client": true, "cockroach": true, "usql": true, "pgcli": true, "mycli": true,
}

func mentionsSQLClient(cmd string) bool {
	for _, argv := range splitCommands(cmd) {
		for _, tok := range argv {
			if sqlClients[baseCmd(tok)] {
				return true
			}
		}
	}
	return false
}

// blankQuoted replaces the contents of '...' and "..." with spaces, keeping the
// quote characters and the overall offsets.
func blankQuoted(cmd string) string {
	rs := []rune(cmd)
	var quote rune
	for i, c := range rs {
		switch {
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			rs[i] = ' '
		}
	}
	return string(rs)
}

// extractSubstitutions returns the bodies of $(...) and `...` found anywhere,
// including inside double quotes (where the shell still executes them).
func extractSubstitutions(cmd string) []string {
	var out []string
	rs := []rune(cmd)
	inSingle := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\'' {
			inSingle = !inSingle
			continue
		}
		if inSingle {
			continue
		}
		if c == '\\' {
			i++
			continue
		}
		if c == '$' && i+1 < len(rs) && rs[i+1] == '(' {
			depth, j := 1, i+2
			for ; j < len(rs) && depth > 0; j++ {
				switch rs[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			if depth == 0 {
				out = append(out, string(rs[i+2:j-1]))
			}
			continue
		}
		if c == '`' {
			j := i + 1
			for ; j < len(rs) && rs[j] != '`'; j++ {
			}
			if j < len(rs) {
				out = append(out, string(rs[i+1:j]))
				i = j
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- tokenizer

// splitCommands splits a command line into simple commands (argv slices),
// honouring quotes and backslashes. Separators: ; & | ( ) ` newline.
func splitCommands(cmd string) [][]string {
	var cmds [][]string
	var cur []string
	var tok strings.Builder
	has := false
	flushTok := func() {
		if has {
			cur = append(cur, tok.String())
			tok.Reset()
			has = false
		}
	}
	flushCmd := func() {
		flushTok()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
			cur = nil
		}
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			if rs[i] != '\n' {
				tok.WriteRune(rs[i])
				has = true
			}
		case c == '\'':
			has = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				tok.WriteRune(rs[i])
			}
		case c == '"':
			has = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				tok.WriteRune(rs[i])
			}
		case c == ' ' || c == '\t' || c == '\r':
			flushTok()
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '`':
			flushCmd()
		case c == '#' && !has:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i--
		default:
			tok.WriteRune(c)
			has = true
		}
	}
	flushCmd()
	return cmds
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true,
	"!": true, "{": true, "}": true, "time": true, "command": true, "builtin": true, "exec": true,
	"nohup": true, "setsid": true,
}

// wrappers: command name -> options that consume a value
var wrapperValueOpts = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-r": true, "-t": true, "-U": true, "-D": true, "-R": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "-C": true, "-S": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"xargs":   {"-n": true, "-I": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true},
}

// stripWrappers removes env assignments, shell keywords and wrapper commands
// (sudo, env, nice, timeout, xargs, ...) so the real command is first.
func stripWrappers(argv []string) []string {
	for len(argv) > 0 {
		head := baseCmd(argv[0])
		switch {
		case envAssignRe.MatchString(argv[0]):
			argv = argv[1:]
		case shellKeywords[head]:
			argv = argv[1:]
		case wrapperValueOpts[head] != nil:
			valueOpts := wrapperValueOpts[head]
			argv = argv[1:]
			for len(argv) > 0 {
				a := argv[0]
				switch {
				case valueOpts[a]:
					if len(argv) < 2 {
						return nil
					}
					argv = argv[2:]
				case strings.HasPrefix(a, "-") && len(a) > 1:
					argv = argv[1:]
				case head == "env" && envAssignRe.MatchString(a):
					argv = argv[1:]
				default:
					goto doneOpts
				}
			}
		doneOpts:
			if head == "timeout" && len(argv) > 0 { // positional duration
				argv = argv[1:]
			}
		default:
			return argv
		}
	}
	return argv
}

func baseCmd(s string) string {
	s = strings.TrimLeft(s, `\`)
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

var shellNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// ------------------------------------------------------------ command rules

func assessArgv(argv []string, depth int) (Severity, string) {
	argv = stripWrappers(argv)
	if len(argv) == 0 {
		return SevNone, ""
	}
	name := baseCmd(argv[0])
	args := argv[1:]

	switch {
	case shellNames[name]:
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
				return assess(args[i+1], depth+1)
			}
		}
	case name == "eval":
		return assess(strings.Join(args, " "), depth+1)
	case name == "rm":
		return assessRM(args)
	case strings.HasPrefix(name, "mkfs"), name == "wipefs", name == "blkdiscard":
		for _, a := range args {
			if strings.HasPrefix(a, "/dev/") {
				return SevCritical, name + " formats or erases the device " + a
			}
		}
		return SevRisky, name + " erases the target it is pointed at"
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") {
				switch strings.TrimPrefix(a, "of=") {
				case "/dev/null", "/dev/zero", "/dev/stdout", "/dev/stderr", "/dev/tty", "/dev/fd/1", "/dev/fd/2":
				default:
					return SevCritical, "dd writing directly to a device (" + strings.TrimPrefix(a, "of=") + ")"
				}
			}
		}
	case name == "shred":
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if strings.HasPrefix(a, "/dev/") {
				return SevCritical, "shred of the device " + a
			}
			if why, sev := dangerousTarget(a); sev != SevNone {
				return sev, "shred of " + a + " (" + why + ")"
			}
		}
	case name == "chmod" || name == "chown" || name == "chgrp":
		return assessPerm(name, args)
	case name == "find":
		return assessFind(args)
	case name == "shutdown" || name == "reboot" || name == "halt" || name == "poweroff":
		return SevRisky, name + " stops the machine"
	case name == "init" || name == "telinit":
		if len(args) > 0 && (args[0] == "0" || args[0] == "6") {
			return SevRisky, name + " " + args[0] + " halts or reboots the machine"
		}
	case name == "systemctl":
		return assessSystemctl(args)
	case name == "service":
		if len(args) >= 2 && (args[1] == "stop") && lockoutUnit(args[0]) {
			return SevRisky, "stopping " + args[0] + " can lock you out of the machine"
		}
	case name == "iptables" || name == "ip6tables" || name == "nft":
		for i, a := range args {
			if a == "-F" || a == "--flush" || (a == "flush" && i+1 < len(args) && args[i+1] == "ruleset") {
				return SevRisky, "flushing firewall rules"
			}
			if a == "-P" && i+2 < len(args) && strings.EqualFold(args[i+2], "DROP") {
				return SevRisky, "setting a default DROP policy can lock you out"
			}
		}
	case name == "ufw":
		if len(args) > 0 && (args[0] == "disable" || args[0] == "reset") {
			return SevRisky, "ufw " + args[0] + " changes the firewall state"
		}
	case name == "git":
		return assessGit(args)
	case name == "docker" || name == "podman":
		return assessDocker(args)
	case name == "kill" || name == "killall5" || name == "pkill" || name == "killall":
		if name == "killall5" {
			return SevRisky, "killall5 kills every process"
		}
		hard, all := false, false
		for _, a := range args {
			if a == "-9" || strings.EqualFold(a, "-KILL") || strings.EqualFold(a, "-SIGKILL") || a == "-s9" {
				hard = true
			}
			if a == "-1" {
				all = true
			}
		}
		if hard && all {
			return SevRisky, "kill -9 -1 kills every process you can signal"
		}
	case name == "crontab":
		for _, a := range args {
			if a == "-r" {
				return SevRisky, "crontab -r deletes all scheduled jobs"
			}
		}
	}
	return SevNone, ""
}

// ---- rm

var benignDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "target": true, ".cache": true, "__pycache__": true,
	".venv": true, "venv": true, ".next": true, ".nuxt": true, ".pytest_cache": true, "coverage": true,
	".tox": true, ".gradle": true, "out": true, ".parcel-cache": true, ".turbo": true, "tmp": true,
}

func assessRM(args []string) (Severity, string) {
	recursive, force, noPreserve, afterDD := false, false, false, false
	var targets []string
	for _, a := range args {
		switch {
		case !afterDD && a == "--":
			afterDD = true
		case !afterDD && strings.HasPrefix(a, "--"):
			switch a {
			case "--recursive":
				recursive = true
			case "--force":
				force = true
			case "--no-preserve-root":
				noPreserve = true
			}
		case !afterDD && strings.HasPrefix(a, "-") && len(a) > 1:
			for _, c := range a[1:] {
				switch c {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				}
			}
		default:
			targets = append(targets, a)
		}
	}
	if noPreserve {
		return SevCritical, "rm --no-preserve-root"
	}
	for _, t := range targets {
		if why, sev := dangerousTarget(t); sev != SevNone {
			if !recursive && sev == SevCritical && !strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "~") {
				sev = SevRisky
			}
			return sev, fmt.Sprintf("rm of %s (%s)", t, why)
		}
		if recursive && hasUnresolvedVar(t) {
			return SevRisky, fmt.Sprintf("rm -r of %q contains a shell variable that, if empty, would delete the wrong path", t)
		}
	}
	if recursive && force && len(targets) > 0 && !allBenign(targets) {
		return SevRisky, "recursive force delete (rm -rf)"
	}
	return SevNone, ""
}

func allBenign(targets []string) bool {
	for _, t := range targets {
		t = strings.TrimSuffix(t, "/")
		if strings.ContainsAny(t, "*?[") || strings.Contains(t, "..") {
			return false
		}
		if strings.HasPrefix(t, "/tmp/") {
			continue
		}
		if benignDirs[path.Base(t)] {
			continue
		}
		return false
	}
	return true
}

func hasUnresolvedVar(t string) bool {
	return strings.Contains(t, "$") && (strings.HasPrefix(t, "$") || strings.Contains(t, "/$")) &&
		!strings.HasPrefix(t, "$HOME/") && !strings.HasPrefix(t, "${HOME}/") // $HOME-rooted paths are handled by dangerousTarget
}

var protectedTop = map[string]bool{
	"etc": true, "usr": true, "bin": true, "sbin": true, "lib": true, "lib32": true, "lib64": true, "libx32": true,
	"boot": true, "dev": true, "proc": true, "sys": true, "root": true,
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(path.Clean(p), "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// dangerousTarget explains why deleting/changing t could be damaging and how
// bad it is. Critical = the root, a top-level system directory (or something
// directly inside one), a home directory, or the agent's own workspace.
// Risky = a bare wildcard / ".." or a deeper path under a system area.
func dangerousTarget(t string) (string, Severity) {
	switch t {
	case "/", "/*", "/.", "/..":
		return "the filesystem root", SevCritical
	case "~", "~/", "~/*", "$HOME", "${HOME}", "$HOME/", "${HOME}/", "$HOME/*", "${HOME}/*", "~/.*":
		return "the home directory", SevCritical
	case ".", "./", "..", "../", "*", "./*", "../*", ".*", "./.*":
		return "the current/parent directory or a bare wildcard", SevRisky
	}
	if strings.HasPrefix(t, "~") && !strings.Contains(t[1:], "/") { // ~user
		return "a home directory", SevCritical
	}
	if root := workspace.Root(); root != "" && strings.HasPrefix(t, "/") {
		clean := path.Clean(t)
		rc := path.Clean(root)
		if clean == rc || strings.HasPrefix(rc, clean+"/") {
			return "the agent's workspace or one of its parents", SevCritical
		}
	}
	if strings.HasPrefix(t, "/") {
		comps := splitPath(t)
		if len(comps) == 0 {
			return "the filesystem root", SevCritical
		}
		top := comps[0]
		switch {
		case protectedTop[top]:
			if len(comps) <= 2 {
				return "a system directory (/" + strings.Join(comps, "/") + ")", SevCritical
			}
			return "a path inside a system directory (/" + top + ")", SevRisky
		case top == "home" && len(comps) <= 2:
			return "a home directory (/" + strings.Join(comps, "/") + ")", SevCritical
		case (top == "var" || top == "mnt" || top == "media" || top == "opt" || top == "srv") && len(comps) == 1:
			return "a top-level system directory (/" + top + ")", SevCritical
		case (top == "var" || top == "mnt" || top == "media" || top == "opt" || top == "srv") && len(comps) == 2:
			return "a top-level data directory (/" + strings.Join(comps, "/") + ")", SevRisky
		}
	}
	return "", SevNone
}

// ---- chmod / chown / chgrp

func assessPerm(name string, args []string) (Severity, string) {
	recursive := false
	var rest []string
	for _, a := range args {
		switch {
		case a == "--recursive":
			recursive = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a[1:], "R") {
				recursive = true
			}
		default:
			rest = append(rest, a)
		}
	}
	if !recursive || len(rest) < 2 {
		return SevNone, ""
	}
	spec, targets := rest[0], rest[1:]
	for _, t := range targets {
		if why, sev := dangerousTarget(t); sev != SevNone && !strings.Contains(why, "current/parent") {
			return sev, fmt.Sprintf("recursive %s on %s (%s)", name, t, why)
		}
	}
	if name == "chmod" && (spec == "777" || spec == "0777" || spec == "a+rwx" || spec == "ugo+rwx" || spec == "a=rwx") {
		return SevRisky, "chmod -R 777 makes everything world-writable"
	}
	return SevNone, ""
}

// ---- find

func assessFind(args []string) (Severity, string) {
	var starts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		starts = append(starts, a)
	}
	deleting := false
	for i, a := range args {
		if a == "-delete" {
			deleting = true
		}
		if a == "-exec" || a == "-execdir" || a == "-ok" {
			if i+1 < len(args) {
				c := baseCmd(args[i+1])
				if c == "rm" || c == "shred" || c == "unlink" {
					deleting = true
				}
			}
		}
	}
	if !deleting {
		return SevNone, ""
	}
	for _, st := range starts {
		if why, sev := dangerousTarget(st); sev != SevNone && !strings.Contains(why, "current/parent") {
			return sev, fmt.Sprintf("find %s … -delete/-exec rm (%s)", st, why)
		}
	}
	return SevNone, ""
}

// ---- systemctl / services

var lockoutRe = regexp.MustCompile(`^(ssh|sshd|networking|network|networkmanager|systemd-networkd|firewalld|ufw|docker)(\.service)?$`)

func lockoutUnit(u string) bool { return lockoutRe.MatchString(strings.ToLower(u)) }

func assessSystemctl(args []string) (Severity, string) {
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return SevNone, ""
	}
	switch rest[0] {
	case "poweroff", "reboot", "halt", "kexec", "suspend", "hibernate":
		return SevRisky, "systemctl " + rest[0] + " stops the machine"
	case "stop", "disable", "mask":
		for _, u := range rest[1:] {
			if lockoutUnit(u) {
				return SevRisky, "systemctl " + rest[0] + " " + u + " can lock you out of the machine"
			}
		}
	}
	return SevNone, ""
}

// ---- git

var shortForceRe = regexp.MustCompile(`^-[a-zA-Z]*f[a-zA-Z]*$`)

func assessGit(args []string) (Severity, string) {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i += 2
		case strings.HasPrefix(a, "-"):
			i++
		default:
			goto found
		}
	}
	return SevNone, ""
found:
	sub, rest := args[i], args[i+1:]
	has := func(vals ...string) bool {
		for _, a := range rest {
			for _, v := range vals {
				if a == v {
					return true
				}
			}
		}
		return false
	}
	switch sub {
	case "push":
		for _, a := range rest {
			if a == "--force" || a == "--delete" || a == "--mirror" || (!strings.HasPrefix(a, "--") && shortForceRe.MatchString(a)) || (strings.HasPrefix(a, "+") && len(a) > 1) {
				return SevRisky, "git push that overwrites or deletes remote history"
			}
		}
	case "reset":
		if has("--hard") {
			return SevRisky, "git reset --hard discards uncommitted changes"
		}
	case "clean":
		for _, a := range rest {
			if a == "--dry-run" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "n")) {
				return SevNone, "" // dry run: lists what would be removed, deletes nothing
			}
		}
		for _, a := range rest {
			if a == "--force" || (!strings.HasPrefix(a, "--") && shortForceRe.MatchString(a)) {
				return SevRisky, "git clean -f deletes untracked files permanently"
			}
		}
	case "checkout", "restore", "switch":
		if has("-f", "--force", "--discard-changes") || (sub != "switch" && has(".")) {
			return SevRisky, "git " + sub + " discards uncommitted changes"
		}
	case "reflog":
		if has("expire") {
			return SevRisky, "git reflog expire removes the safety net for recovering commits"
		}
	case "gc":
		for _, a := range rest {
			if strings.HasPrefix(a, "--prune=now") {
				return SevRisky, "git gc --prune=now permanently drops unreachable commits"
			}
		}
	}
	return SevNone, ""
}

// ---- docker

func assessDocker(args []string) (Severity, string) {
	var words []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	if len(words) >= 2 && words[0] == "system" && words[1] == "prune" {
		return SevRisky, "docker system prune removes unused containers, networks and images"
	}
	if len(words) >= 2 && words[0] == "volume" && (words[1] == "rm" || words[1] == "prune") {
		return SevRisky, "removing docker volumes deletes their data"
	}
	if len(words) >= 1 && (words[0] == "compose" || len(words) >= 2 && words[1] == "down") {
		hasDown, hasV := false, false
		for _, a := range args {
			if a == "down" {
				hasDown = true
			}
			if a == "-v" || a == "--volumes" {
				hasV = true
			}
		}
		if hasDown && hasV {
			return SevRisky, "docker compose down -v deletes the stack's volumes"
		}
	}
	return SevNone, ""
}
