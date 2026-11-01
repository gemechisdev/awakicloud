package system

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"
)

func bash(in map[string]any) types.Result { return executeBash(context.Background(), in) }

func skipWin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs bash/unix")
	}
}

func TestBashBasicsExitCodeAndWorkdir(t *testing.T) {
	skipWin(t)
	dir := setup(t)
	r := bash(map[string]any{"command": "echo hi; pwd"})
	if r.IsErr || !strings.Contains(r.Output, "hi") || !strings.Contains(r.Output, dir) {
		t.Fatalf("%q", r.Output)
	}
	r = bash(map[string]any{"command": "echo oops >&2; exit 3"})
	if !r.IsErr || !strings.HasPrefix(r.Output, "exit code 3") || !strings.Contains(r.Output, "oops") {
		t.Fatalf("%q", r.Output)
	}
	if r = bash(map[string]any{"command": "true"}); r.Output != "(no output)" {
		t.Fatalf("%q", r.Output)
	}
	// relative workdir resolves against the workspace root, not the process cwd
	os.MkdirAll(filepath.Join(dir, "sub/deep"), 0o755)
	r = bash(map[string]any{"command": "pwd", "workdir": "sub/deep"})
	if r.IsErr || !strings.Contains(r.Output, filepath.Join(dir, "sub/deep")) {
		t.Fatalf("%q", r.Output)
	}
	if r = bash(map[string]any{"command": "pwd", "workdir": "nope"}); !r.IsErr || !strings.Contains(r.Output, "not a directory") {
		t.Fatalf("%q", r.Output)
	}
	// timeout may arrive as a string
	if r = bash(map[string]any{"command": "echo ok", "timeout": "5"}); r.IsErr {
		t.Fatalf("%q", r.Output)
	}
}

func TestBashTimeoutKillsGroupAndSuggestsBackground(t *testing.T) {
	skipWin(t)
	dir := setup(t)
	marker := filepath.Join(dir, "child-survived")
	start := time.Now()
	r := bash(map[string]any{"command": "echo started; (sleep 3; touch " + marker + ") & sleep 30", "timeout": float64(1)})
	if !r.IsErr || !strings.Contains(r.Output, "timed out") || !strings.Contains(r.Output, "background=true") || !strings.Contains(r.Output, "started") {
		t.Fatalf("%q", r.Output)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout not enforced: %v", time.Since(start))
	}
	time.Sleep(3500 * time.Millisecond)
	if exists(marker) {
		t.Fatal("grandchild process survived the timeout kill")
	}
}

func TestBashSeparateStreams(t *testing.T) {
	skipWin(t)
	setup(t)
	r := bash(map[string]any{"command": "echo out; echo err >&2", "separate_streams": true})
	if !strings.Contains(r.Output, "[stdout]\nout") || !strings.Contains(r.Output, "[stderr]\nerr") {
		t.Fatalf("%q", r.Output)
	}
	// interleaving is preserved by default
	r = bash(map[string]any{"command": "echo 1; echo 2 >&2; echo 3"})
	if r.Output != "1\n2\n3\n" {
		t.Fatalf("%q", r.Output)
	}
}

func TestBashKeepsHeadAndTailOfHugeOutput(t *testing.T) {
	skipWin(t)
	setup(t)
	r := bash(map[string]any{"command": `seq 1 300000; echo FATAL-ERROR-AT-THE-END`})
	if len(r.Output) > types.MaxOutputSize || !strings.Contains(r.Output, "FATAL-ERROR-AT-THE-END") || !strings.HasPrefix(r.Output, "1\n2\n") || !strings.Contains(r.Output, "omitted from the middle") {
		t.Fatalf("len=%d head=%q", len(r.Output), r.Output[:20])
	}
	if !utf8.ValidString(r.Output) {
		t.Fatal("invalid UTF-8")
	}
}

func TestBashBoundedMemoryOnRunawayOutput(t *testing.T) {
	skipWin(t)
	setup(t)
	// ~60 MB of output must not be buffered in full
	r := bash(map[string]any{"command": `yes "some noisy log line that repeats" | head -c 60000000; echo DONE-MARK`})
	if !strings.Contains(r.Output, "DONE-MARK") || len(r.Output) > types.MaxOutputSize {
		t.Fatalf("len=%d", len(r.Output))
	}
}

func TestCleanTerminalOutput(t *testing.T) {
	in := "\x1b[31mred\x1b[0m text\n 10%\r 50%\r100% done\nplain\r\nbad\xffutf8\n"
	got := cleanTerminalOutput(in)
	if got != "red text\n100% done\nplain\nbad�utf8\n" {
		t.Fatalf("%q", got)
	}
	if cleanTerminalOutput("a\r\n") != "a\n" {
		t.Fatal("CRLF")
	}
}

func TestBashBackground(t *testing.T) {
	skipWin(t)
	dir := setup(t)
	r := bash(map[string]any{"command": "echo booting; sleep 4; echo finished", "background": true})
	if r.IsErr || !strings.Contains(r.Output, "Started in background") || !strings.Contains(r.Output, "pid") {
		t.Fatalf("%q", r.Output)
	}
	logPath := strings.TrimSpace(strings.TrimPrefix(strings.Split(strings.Split(r.Output, "log: ")[1], "\n")[0], ""))
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "booting") {
		t.Fatalf("log: %q", data)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(logPath)
		if strings.Contains(string(data), "process exited with code 0") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(string(data), "finished") || !strings.Contains(string(data), "exited with code 0") {
		t.Fatalf("log never completed: %q", data)
	}
	// immediate failure is reported right away
	r = bash(map[string]any{"command": "definitely-not-a-command-xyz", "background": true})
	if !r.IsErr || !strings.Contains(r.Output, "exited immediately") || !strings.Contains(r.Output, "not found") {
		t.Fatalf("%q", r.Output)
	}
	_ = dir
}

type captureSink struct {
	mu     sync.Mutex
	status []string
}

func (c *captureSink) Emit(ev transport.Event) {
	if p, ok := ev.(transport.ProgressEvent); ok {
		c.mu.Lock()
		c.status = append(c.status, p.Status)
		c.mu.Unlock()
	}
}

func TestBashHeartbeatProgress(t *testing.T) {
	skipWin(t)
	setup(t)
	old := bashHeartbeat
	bashHeartbeat = 300 * time.Millisecond
	defer func() { bashHeartbeat = old }()
	sink := &captureSink{}
	ctx := transport.WithSink(context.Background(), sink)
	r := executeBash(ctx, map[string]any{"command": "echo step-one; sleep 1.2; echo step-two"})
	if r.IsErr {
		t.Fatalf("%q", r.Output)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.status) < 2 || !strings.Contains(sink.status[0], "still running") || !strings.Contains(strings.Join(sink.status, "|"), "step-one") {
		t.Fatalf("heartbeats: %v", sink.status)
	}
}

func TestBashCancelledContext(t *testing.T) {
	skipWin(t)
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	r := executeBash(ctx, map[string]any{"command": "sleep 20"})
	if !r.IsErr || !strings.Contains(r.Output, "cancelled") || time.Since(start) > 8*time.Second {
		t.Fatalf("%q after %v", r.Output, time.Since(start))
	}
}

func TestBashEnvIsNonInteractive(t *testing.T) {
	skipWin(t)
	setup(t)
	r := bash(map[string]any{"command": `echo "$PAGER|$DEBIAN_FRONTEND|$NO_COLOR"; read x </dev/stdin; echo "stdin-empty:$?"`})
	if !strings.Contains(r.Output, "cat|noninteractive|1") || !strings.Contains(r.Output, "stdin-empty:1") {
		t.Fatalf("%q", r.Output)
	}
}
