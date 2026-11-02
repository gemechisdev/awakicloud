package system

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func envMap(kept []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kept {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestScrubEnvDropsCredentialsKeepsToolchain(t *testing.T) {
	in := []string{
		// must go
		"ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnop", "OPENAI_API_KEY=x", "GEMINI_API_KEY=x", "OPENROUTER_API_KEY=x", "BRAVE_API_KEY=x",
		"TELEGRAM_BOT_TOKEN=123456:ABC", "AWAKI_API_KEY=x", "AWAKI_TG_API_HASH=x", "AWAKI_TG_PASSWORD=x", "AWAKI_TG_PHONE=+251900000000",
		"AWAKI_TG_SESSION=/data/s", "AWAKI_PROVIDER=anthropic", "GITHUB_TOKEN=ghp_x", "NPM_TOKEN=x", "AWS_SECRET_ACCESS_KEY=x", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"DATABASE_PASSWORD=x", "DB_PASSWD=x", "STRIPE_SECRET=x", "MY_PRIVATE_KEY=x", "SESSION_COOKIE=x", "SENTRY_DSN=https://k@o.ingest.sentry.io/1",
		"INNOCENT_NAME=sk-proj-aaaaaaaaaaaaaaaaaaaaaaaa", "BOT=1234567890:AAH-abcdefghijklmnopqrstuvwxyz012345", "WEBHOOK=ghp_abcdefghijklmnopqrstuvwxyz0123",
		// must stay
		"PATH=/usr/bin", "HOME=/root", "LANG=C.UTF-8", "PWD=/work", "OLDPWD=/", "USER=root", "SHELL=/bin/bash", "TERM=xterm", "TMPDIR=/tmp",
		"GOPATH=/root/go", "GOCACHE=/data/.cache", "GOPRIVATE=github.com/me/*", "NODE_ENV=production", "PYTHONPATH=/x", "HTTP_PROXY=http://p:3128", "NO_PROXY=localhost",
		"SSH_AUTH_SOCK=/tmp/agent.sock", "GIT_ASKPASS=/bin/true", "XDG_SESSION_ID=3", "XDG_RUNTIME_DIR=/run/user/0", "AWAKI_WORK_DIR=/work", "AWAKI_DATA_DIR=/data",
		"DEBIAN_FRONTEND=noninteractive", "KEYBOARD_LAYOUT=us", "MONKEY=1", "TOKENIZERS_PARALLELISM=false",
	}
	kept, removed := ScrubEnv(in, EnvPolicy{Passthrough: map[string]bool{}})
	m := envMap(kept)
	for _, gone := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "BRAVE_API_KEY", "TELEGRAM_BOT_TOKEN", "AWAKI_API_KEY", "AWAKI_TG_API_HASH",
		"AWAKI_TG_PASSWORD", "AWAKI_TG_PHONE", "AWAKI_TG_SESSION", "AWAKI_PROVIDER", "GITHUB_TOKEN", "NPM_TOKEN", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID",
		"DATABASE_PASSWORD", "DB_PASSWD", "STRIPE_SECRET", "MY_PRIVATE_KEY", "SESSION_COOKIE", "SENTRY_DSN", "INNOCENT_NAME", "BOT", "WEBHOOK",
	} {
		if _, ok := m[gone]; ok {
			t.Errorf("%s leaked into the child environment", gone)
		}
	}
	for _, stay := range []string{
		"PATH", "HOME", "LANG", "PWD", "OLDPWD", "USER", "SHELL", "TERM", "TMPDIR", "GOPATH", "GOCACHE", "GOPRIVATE", "NODE_ENV", "PYTHONPATH", "HTTP_PROXY", "NO_PROXY",
		"SSH_AUTH_SOCK", "GIT_ASKPASS", "XDG_SESSION_ID", "XDG_RUNTIME_DIR", "AWAKI_WORK_DIR", "AWAKI_DATA_DIR", "DEBIAN_FRONTEND", "KEYBOARD_LAYOUT", "MONKEY", "TOKENIZERS_PARALLELISM",
	} {
		if _, ok := m[stay]; !ok {
			t.Errorf("%s was scrubbed but is needed by ordinary tooling", stay)
		}
	}
	for _, name := range removed {
		if strings.Contains(name, "=") {
			t.Fatalf("removed list must hold names only, got %q", name)
		}
	}
}

func TestScrubEnvPassthroughAndInherit(t *testing.T) {
	in := []string{"GITHUB_TOKEN=ghp_x", "OPENAI_API_KEY=y", "PATH=/bin"}
	kept, _ := ScrubEnv(in, EnvPolicy{Passthrough: map[string]bool{"GITHUB_TOKEN": true}})
	m := envMap(kept)
	if m["GITHUB_TOKEN"] != "ghp_x" || m["OPENAI_API_KEY"] != "" {
		t.Fatalf("passthrough must keep only the named variable: %v", m)
	}
	kept, removed := ScrubEnv(in, EnvPolicy{Inherit: true})
	if len(kept) != 3 || removed != nil {
		t.Fatalf("inherit mode must pass everything: %v", kept)
	}
	t.Setenv("AWAKI_BASH_ENV", "inherit")
	if !EnvPolicyFromOS().Inherit {
		t.Fatal("AWAKI_BASH_ENV=inherit not honoured")
	}
	t.Setenv("AWAKI_BASH_ENV", "")
	t.Setenv("AWAKI_BASH_ENV_PASSTHROUGH", " GITHUB_TOKEN , NPM_TOKEN,")
	p := EnvPolicyFromOS()
	if p.Inherit || !p.Passthrough["GITHUB_TOKEN"] || !p.Passthrough["NPM_TOKEN"] || len(p.Passthrough) != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestBashDoesNotSeeAgentSecrets(t *testing.T) {
	skipWin(t)
	setup(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SECRET-VALUE-123456")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456789:AAH-SECRET-TOKEN-abcdefghijklmnop")
	t.Setenv("AWAKI_TG_PASSWORD", "hunter2-SECRET")
	t.Setenv("HARMLESS_SETTING", "visible")
	for _, cmd := range []string{"env", "printenv", "cat /proc/self/environ | tr '\\0' '\\n'", "bash -c env", "sh -c 'echo $ANTHROPIC_API_KEY'"} {
		r := bash(map[string]any{"command": cmd})
		if strings.Contains(r.Output, "SECRET") {
			t.Fatalf("%q leaked a secret:\n%s", cmd, r.Output)
		}
	}
	if r := bash(map[string]any{"command": "echo $HARMLESS_SETTING"}); !strings.Contains(r.Output, "visible") {
		t.Fatalf("ordinary variables must still be inherited: %q", r.Output)
	}
	// opt-in passthrough
	t.Setenv("AWAKI_BASH_ENV_PASSTHROUGH", "ANTHROPIC_API_KEY")
	if r := bash(map[string]any{"command": "echo $ANTHROPIC_API_KEY"}); !strings.Contains(r.Output, "SECRET") {
		t.Fatalf("passthrough did not work: %q", r.Output)
	}
}

func TestGitChildrenDoNotSeeAgentSecrets(t *testing.T) {
	dir := repo(t)
	t.Setenv("OPENAI_API_KEY", "sk-SECRET-should-not-leak-0123456789")
	sh(t, dir, "config", "alias.leak", "!env")
	for _, tl := range GitTools() {
		if tl.Name == "git_status" {
			_ = tl
		}
	}
	for _, kv := range gitEnv(dir) {
		if strings.Contains(kv, "SECRET") {
			t.Fatalf("git environment carries a secret: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// HardenProcess must leave the flag cleared while the agent keeps working normally.
// (That the flag blocks same-uid, non-root readers of /proc/<pid>/environ was verified
// by experiment on Linux 6.x; it needs a second uid, so it is not asserted here.)
func TestHardenProcessMarksNonDumpableAndAgentKeepsWorking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	setup(t)
	if err := HardenProcess(); err != nil {
		t.Fatalf("HardenProcess: %v", err)
	}
	if d, err := processDumpable(); err != nil || d != 0 {
		t.Fatalf("expected dumpable=0 after HardenProcess, got %d (%v)", d, err)
	}
	// the agent itself must keep working
	if _, err := os.ReadFile("/proc/self/status"); err != nil {
		t.Fatalf("agent lost access to its own /proc: %v", err)
	}
	if _, err := os.Executable(); err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// and children must run normally and be dumpable again (execve resets the flag)
	out, err := exec.Command("sh", "-c", "echo ok").Output()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("spawning children broke: %v %q", err, out)
	}
	if r := bash(map[string]any{"command": "echo still-works"}); !strings.Contains(r.Output, "still-works") {
		t.Fatalf("bash tool broke after hardening: %q", r.Output)
	}
}

func TestBashMissingWorkdirGivesClearError(t *testing.T) {
	skipWin(t)
	dir := setup(t)
	os.RemoveAll(dir) // workspace vanished underneath the agent
	r := bash(map[string]any{"command": "echo hi"})
	if !r.IsErr || !strings.Contains(r.Output, "working directory does not exist") || strings.Contains(r.Output, "fork/exec") {
		t.Fatalf("%q", r.Output)
	}
	r = bash(map[string]any{"command": "echo hi", "background": true})
	if !r.IsErr || !strings.Contains(r.Output, "working directory does not exist") {
		t.Fatalf("%q", r.Output)
	}
}
