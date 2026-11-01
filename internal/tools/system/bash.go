package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 600 * time.Second
	bashStartGrace     = 1500 * time.Millisecond

	bufHeadKeep = 256 << 10 // always keep the first 256 KB of output
	bufTailKeep = 1 << 20   // and a rolling window of the last 1 MB
)

// bashHeartbeat is how often a long-running command reports progress (var for tests).
var bashHeartbeat = 20 * time.Second

func BashTool() types.Tool {
	return types.Tool{
		Name:  "bash",
		Label: "Running command",
		Description: "Execute a shell command (bash -c) and return its output with the exit code. Use for running programs, tests, builds, installing packages, docker, etc. " +
			"Output is cleaned (ANSI colours and progress-bar redraws removed); if it is too long the START and END are kept (errors are usually at the end). " +
			"Default timeout 120s (max 600): on timeout the whole process group is killed. " +
			"For anything that may run longer — dev servers, watchers, big builds, long installs — use background=true: it returns immediately with a PID and a log file to poll with `tail`/read_file. " +
			"stdin is empty, so commands cannot prompt; pass non-interactive flags (-y, --yes). Relative workdir is resolved against the workspace root.",
		DetailFunc: func(input map[string]any) string {
			s, _ := input["command"].(string)
			return s
		},
		Parameters: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Working directory (default: workspace root; relative paths are resolved against it)",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds (default 120, max 600). Ignored for background=true.",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "Start the command detached and return immediately with its PID and log path (for servers, watchers, long jobs).",
			},
			"separate_streams": map[string]any{
				"type":        "boolean",
				"description": "Return stdout and stderr in separate [stdout]/[stderr] sections instead of interleaved (default false).",
			},
		},
		Required: []string{"command"},
		Handler:  executeBash,
	}
}

// boundedBuffer keeps the head and a rolling tail of a command's output so a
// runaway command (cat of a huge file, a chatty build) cannot exhaust memory.
// It also tracks the last output line for progress heartbeats.
type boundedBuffer struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int64
	cur     []byte
	last    string
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	for _, c := range p {
		if c == '\n' || c == '\r' {
			if s := strings.TrimSpace(string(b.cur)); s != "" {
				b.last = s
			}
			b.cur = b.cur[:0]
		} else if len(b.cur) < 300 {
			b.cur = append(b.cur, c)
		}
	}
	if room := bufHeadKeep - len(b.head); room > 0 {
		take := room
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > 2*bufTailKeep {
			drop := len(b.tail) - bufTailKeep
			b.dropped += int64(drop)
			b.tail = append([]byte(nil), b.tail[drop:]...)
		}
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.head)
	if b.dropped > 0 {
		s += fmt.Sprintf("\n… [%d bytes of output dropped] …\n", b.dropped)
	}
	return s + string(b.tail)
}

func (b *boundedBuffer) lastLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := strings.TrimSpace(string(b.cur)); s != "" {
		return s
	}
	return b.last
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// cleanTerminalOutput removes ANSI escapes and collapses carriage-return
// redraws (progress bars) to their final state, which can shrink curl / pip /
// git progress output from hundreds of KB to a few lines.
func cleanTerminalOutput(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.ContainsRune(s, '\r') {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if j := strings.LastIndexByte(l, '\r'); j >= 0 {
				if tail := l[j+1:]; tail != "" {
					lines[i] = tail
				} else if k := strings.LastIndexByte(l[:j], '\r'); k >= 0 {
					lines[i] = l[k+1 : j]
				} else {
					lines[i] = l[:j]
				}
			}
		}
		s = strings.Join(lines, "\n")
	}
	if strings.ContainsRune(s, '\x1b') {
		s = ansiRe.ReplaceAllString(s, "")
	}
	return s
}

func bashEnv() []string {
	return append(os.Environ(),
		"PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "DEBIAN_FRONTEND=noninteractive",
		"NO_COLOR=1", "TERM=dumb", "CI=1",
	)
}

func executeBash(ctx context.Context, input map[string]any) types.Result {
	command, err := types.GetString(input, "command")
	if err != nil {
		return types.ErrResult(err)
	}

	if destructive, reason := CheckDestructive(command); destructive {
		mode := GuardMode(os.Getenv("AWAKI_BASH_GUARD"))
		if mode == "" {
			mode = GuardWarn
		}
		switch mode {
		case GuardBlock:
			return types.Errf("blocked destructive command: %s", reason)
		case GuardWarn, GuardOff:
			if mode == GuardWarn {
				log.Warn().Str("command", command).Str("reason", reason).Msg("bash: destructive command allowed with warning")
			}
		}
	}

	workdir := types.GetStringOpt(input, "workdir")
	if workdir == "" {
		workdir = workspace.Root()
	} else {
		resolved := workspace.Resolve(workdir)
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return types.Errf("workdir is not a directory: %s (resolved from %q)", resolved, workdir)
		}
		workdir = resolved
	}

	if types.GetBoolDefault(input, "background", false) {
		return startBackground(command, workdir)
	}

	timeout := defaultBashTimeout
	if v, ok := types.GetIntOpt(input, "timeout"); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}
	separate := types.GetBoolDefault(input, "separate_streams", false)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-c", command)
	// Unix: own process group so cancel kills grandchildren. Windows: no-op.
	configureProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = bashEnv()
	if workdir != "" {
		cmd.Dir = workdir
	}
	var out, errBuf boundedBuffer
	cmd.Stdout = &out
	if separate {
		cmd.Stderr = &errBuf
	} else {
		cmd.Stderr = &out // same writer => one pipe, original interleaving
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return types.Errf("could not start command: %v", err)
	}
	done := make(chan struct{})
	interval := bashHeartbeat
	go func() { // heartbeat so long commands show life in the chat / task board
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				msg := fmt.Sprintf("bash: still running (%s)", time.Since(start).Round(time.Second))
				if l := out.lastLine(); l != "" {
					msg += " — " + types.CutHead(l, 120)
				}
				reportProgress(ctx, msg)
			}
		}
	}()
	waitErr := cmd.Wait()
	close(done)
	elapsed := time.Since(start)

	body := cleanTerminalOutput(out.String())
	if separate {
		body = fmt.Sprintf("[stdout]\n%s\n[stderr]\n%s", strings.TrimRight(body, "\n"), strings.TrimRight(cleanTerminalOutput(errBuf.String()), "\n"))
	}
	body = types.TruncateHeadTail(body, types.MaxOutputSize-300)
	took := ""
	if elapsed > 15*time.Second {
		took = fmt.Sprintf("\n(took %s)", elapsed.Round(time.Second))
	}

	if waitErr != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			log.Warn().Str("command", command).Dur("timeout", timeout).Dur("elapsed", elapsed).Msg("bash timeout")
			return types.Errf("command timed out after %s and was killed.\n%s\nTip: for long-running work call bash again with background=true and poll the log, or raise timeout (max %d).", timeout, body, int(maxBashTimeout.Seconds()))
		}
		if ctx.Err() != nil {
			return types.Errf("command cancelled\n%s", body)
		}
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				return types.Errf("exit code %d\n%s%s", code, body, took)
			}
			return types.Errf("terminated by signal (%v)\n%s%s", ee, body, took)
		}
		return types.Errf("exit error: %v\n%s%s", waitErr, body, took)
	}
	if strings.TrimSpace(body) == "" {
		body = "(no output)"
	}
	return types.Result{Output: body + took}
}

// startBackground launches command detached (own process group, output to a
// log file) and returns right away. If it dies within a moment — typo, missing
// binary, port already in use — the failure is reported immediately.
func startBackground(command, workdir string) types.Result {
	dir := filepath.Join(os.TempDir(), "awaki-bg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return types.Errf("cannot create log dir: %v", err)
	}
	logPath := filepath.Join(dir, fmt.Sprintf("bg-%d.log", time.Now().UnixNano()))
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return types.Errf("cannot create log file: %v", err)
	}
	// CommandContext (never cancelled) is required for the process-group Cancel hook.
	cmd := exec.CommandContext(context.Background(), "bash", "-c", command)
	configureProcessGroup(cmd)
	cmd.Env = bashEnv()
	cmd.Stdout, cmd.Stderr = lf, lf
	if workdir != "" {
		cmd.Dir = workdir
	}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return types.Errf("could not start command: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		fmt.Fprintf(lf, "\n[process exited with code %d]\n", code)
		lf.Close()
		exited <- err
	}()

	select {
	case err := <-exited:
		data, _ := os.ReadFile(logPath)
		body := types.TruncateHeadTail(cleanTerminalOutput(string(data)), types.MaxOutputSize-300)
		if err != nil {
			return types.Errf("background command exited immediately:\n%s", body)
		}
		return types.Result{Output: "Background command already finished (it was short-lived):\n" + body}
	case <-time.After(bashStartGrace):
	}
	return types.Result{Output: fmt.Sprintf(
		"Started in background (pid %d).\nlog: %s\nPoll: bash `tail -n 40 %s`  |  stop: bash `kill -- -%d`\nThe log ends with \"[process exited with code N]\" when the command finishes.",
		pid, logPath, logPath, pid)}
}
