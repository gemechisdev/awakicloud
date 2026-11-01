package system

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

// Git tools. Every command runs inside the workspace (or an explicit `path`),
// never inherits an interactive terminal, and every ref / remote / URL taken
// from the model is validated so it cannot be parsed by git as an option.
//
// Read-only:  git_status git_diff git_log git_show git_blame git_branch
// Mutating:   git_stage git_commit git_checkout git_restore git_stash git_reset
//             git_branch_update git_sync git_clone   (see permission.PlanDenylist)

const (
	gitDefaultTimeout = 60 * time.Second
	gitSlowTimeout    = 5 * time.Minute
	gitCloneTimeout   = 10 * time.Minute
)

var (
	safeRefRe  = regexp.MustCompile(`^[A-Za-z0-9._/@^~{}:+,\-]+$`)
	scpURLRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/~+-]+$`)
	cloneURLOK = map[string]bool{"https": true, "http": true, "ssh": true, "git": true}
)

// validateRef rejects anything git could read as an option or that has
// characters with no business in a ref / revision expression.
func validateRef(what, v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s %q must not start with '-'", what, v)
	}
	if !safeRefRe.MatchString(v) {
		return fmt.Errorf("%s %q contains characters that are not allowed in a git ref", what, v)
	}
	return nil
}

func validateCloneURL(raw string) error {
	u := strings.TrimSpace(raw)
	if u == "" {
		return errors.New("url is required")
	}
	if strings.HasPrefix(u, "-") {
		return errors.New("url must not start with '-'")
	}
	if scpURLRe.MatchString(u) {
		return nil
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || !cloneURLOK[strings.ToLower(parsed.Scheme)] {
		return errors.New("url must be https://, http://, ssh://, git:// or user@host:path (local paths, file:// and ext:: transports are not allowed)")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errors.New("url has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return errors.New("url points at a local host")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return errors.New("url points at a private or loopback address")
	}
	return nil
}

func gitDir(input map[string]any) (string, error) {
	dir := types.GetStringOpt(input, "path")
	if dir == "" {
		dir = workspace.Root()
	}
	if dir == "" {
		dir = "."
	} else {
		dir = workspace.Resolve(dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", dir)
	}
	return dir, nil
}

func gitEnv(dir string) []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true",
		"GCM_INTERACTIVE=never", "LC_ALL=C",
	)
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	}
	return env
}

// identityEnv supplies a committer identity only when git has none configured.
func identityEnv(ctx context.Context, dir string) []string {
	get := func(key string) string {
		c := exec.CommandContext(ctx, "git", "config", "--get", key)
		c.Dir = dir
		out, _ := c.Output()
		return strings.TrimSpace(string(out))
	}
	name, email := get("user.name"), get("user.email")
	if name != "" && email != "" {
		return nil
	}
	if name == "" {
		name = firstNonEmpty(os.Getenv("AWAKI_GIT_NAME"), "Awaki Agent")
	}
	if email == "" {
		email = firstNonEmpty(os.Getenv("AWAKI_GIT_EMAIL"), "awaki@localhost")
	}
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
}

type gitRun struct {
	dir     string
	timeout time.Duration
	env     []string
	hint    string // appended when output is truncated
}

// run executes git and returns (output, error). Output is stdout+stderr,
// trimmed; error is non-nil on a non-zero exit.
func (g gitRun) run(ctx context.Context, args ...string) (string, error) {
	timeout := g.timeout
	if timeout == 0 {
		timeout = gitDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"--no-pager", "-c", "core.quotepath=false", "-c", "color.ui=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = g.dir
	cmd.Env = append(gitEnv(g.dir), g.env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return s, fmt.Errorf("timed out after %s", timeout)
		}
		if s == "" {
			s = err.Error()
		}
		if strings.Contains(s, "not a git repository") {
			s += "\nHint: pass path=<repo directory>, or create one with bash (git init) / git_clone."
		}
		return s, errors.New(s)
	}
	return s, nil
}

func finishGit(out string, hint string) types.Result {
	if out == "" {
		out = "(no output)"
	}
	if len(out) > types.MaxOutputSize && hint != "" {
		r := types.TruncateOutput(out)
		r.Output += "\n" + hint
		return r
	}
	return types.TruncateOutput(out)
}

func filesArg(input map[string]any, keys ...string) []string {
	var out []string
	for _, k := range keys {
		out = append(out, stringList(input[k])...)
	}
	return out
}

var gitPathProp = map[string]any{"type": "string", "description": "Repo directory (default: workspace root)"}

func gitTool(name, label, desc string, params map[string]any, required []string, h types.Handler) types.Tool {
	p := map[string]any{"path": gitPathProp}
	for k, v := range params {
		p[k] = v
	}
	return types.Tool{Name: name, Label: label, Description: desc, Parameters: p, Required: required, Handler: h}
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// withRepo resolves the repo dir and builds the runner for a handler.
func withRepo(fn func(ctx context.Context, g gitRun, input map[string]any) types.Result) types.Handler {
	return func(ctx context.Context, input map[string]any) types.Result {
		dir, err := gitDir(input)
		if err != nil {
			return types.ErrResult(err)
		}
		return fn(ctx, gitRun{dir: dir}, input)
	}
}

func gitFail(op string, err error) types.Result { return types.Errf("git %s: %v", op, err) }

func GitTools() []types.Tool {
	return []types.Tool{
		gitTool("git_status", "Git status", "Show branch, ahead/behind and changed/untracked files (short format).", nil, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				out, err := g.run(ctx, "status", "--short", "--branch")
				if err != nil {
					return gitFail("status", err)
				}
				if !strings.Contains(out, "\n") {
					out += "\n(working tree clean)"
				}
				return finishGit(out, "")
			})),

		gitTool("git_diff", "Git diff",
			"Show changes. Default: unstaged changes. staged=true for the index; ref for a commit or range (HEAD~3, main..feature, A...B); "+
				"file/files to limit paths; stat=true or name_only=true for a summary of big diffs.",
			map[string]any{
				"staged":            prop("boolean", "Diff the staged changes (--cached)"),
				"ref":               prop("string", "Commit or range, e.g. HEAD~1, main..feature, abc123"),
				"file":              prop("string", "Limit to this path"),
				"files":             strList("Limit to these paths"),
				"stat":              prop("boolean", "Only a per-file +/- summary"),
				"name_only":         prop("boolean", "Only changed file names with status letters"),
				"context":           prop("integer", "Lines of context (default 3)"),
				"ignore_whitespace": prop("boolean", "Ignore whitespace changes (-w)"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				args := []string{"diff", "--no-ext-diff"}
				if types.GetBool(in, "staged") {
					args = append(args, "--cached")
				}
				if n, ok := types.GetIntOpt(in, "context"); ok && n >= 0 && n <= 200 {
					args = append(args, fmt.Sprintf("-U%d", n))
				}
				switch {
				case types.GetBool(in, "stat"):
					args = append(args, "--stat")
				case types.GetBool(in, "name_only"):
					args = append(args, "--name-status")
				}
				if types.GetBool(in, "ignore_whitespace") {
					args = append(args, "-w")
				}
				if ref := types.GetStringOpt(in, "ref"); ref != "" {
					if err := validateRef("ref", ref); err != nil {
						return types.ErrResult(err)
					}
					args = append(args, ref)
				}
				if files := filesArg(in, "file", "files"); len(files) > 0 {
					args = append(append(args, "--"), files...)
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("diff", err)
				}
				if out == "" {
					return types.Result{Output: "(no differences)"}
				}
				return finishGit(out, "Diff is large: rerun with stat=true, name_only=true, or file=<path> to look at one file at a time.")
			})),

		gitTool("git_log", "Git log",
			"Commit history (one line each: hash date author subject). Filter with file, author, grep (message), since/until (e.g. '2 weeks ago'), ref (branch or range). "+
				"patch=true includes diffs (max 5 commits); stat=true adds per-commit file stats.",
			map[string]any{
				"n":            prop("integer", "Number of commits (default 15, max 200)"),
				"file":         prop("string", "Only commits touching this path"),
				"author":       prop("string", "Filter by author name/email"),
				"grep":         prop("string", "Filter by commit message text"),
				"since":        prop("string", "Only commits after this date"),
				"until":        prop("string", "Only commits before this date"),
				"ref":          prop("string", "Branch, tag or range to log (default HEAD)"),
				"all_branches": prop("boolean", "Include all branches"),
				"patch":        prop("boolean", "Show the diff for each commit (n capped at 5)"),
				"stat":         prop("boolean", "Show file stats for each commit"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				n, ok := types.GetIntOpt(in, "n")
				if !ok || n < 1 {
					n = 15
				}
				if n > 200 {
					n = 200
				}
				patch := types.GetBool(in, "patch")
				if patch && n > 5 {
					n = 5
				}
				args := []string{"log", fmt.Sprintf("-n%d", n), "--date=short"}
				if patch {
					args = append(args, "--pretty=format:%n%h %ad %an%n%s%n%b", "-p")
				} else {
					args = append(args, "--pretty=format:%h %ad %an  %s%d")
				}
				if types.GetBool(in, "stat") && !patch {
					args = append(args, "--stat")
				}
				if types.GetBool(in, "all_branches") {
					args = append(args, "--all")
				}
				for flag, key := range map[string]string{"--author=": "author", "--grep=": "grep", "--since=": "since", "--until=": "until"} {
					if v := types.GetStringOpt(in, key); v != "" {
						args = append(args, flag+v)
					}
				}
				if ref := types.GetStringOpt(in, "ref"); ref != "" {
					if err := validateRef("ref", ref); err != nil {
						return types.ErrResult(err)
					}
					args = append(args, ref)
				}
				if files := filesArg(in, "file"); len(files) > 0 {
					args = append(append(args, "--"), files...)
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("log", err)
				}
				if out == "" {
					return types.Result{Output: "(no matching commits)"}
				}
				return finishGit(out, "Log is large: lower n or add filters (file, author, since).")
			})),

		gitTool("git_show", "Git show",
			"Show a commit: message plus the full patch (stat_only=true for just the file list). "+
				"With file=<path> and content=true it prints that file as it was at ref.",
			map[string]any{
				"ref":       prop("string", "Commit SHA / ref (default HEAD)"),
				"file":      prop("string", "Limit the patch to this path, or the file to print with content=true"),
				"stat_only": prop("boolean", "Only the commit message and per-file stats"),
				"content":   prop("boolean", "With file: print the file's content at ref instead of a patch"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				ref := types.GetStringOpt(in, "ref")
				if ref == "" {
					ref = "HEAD"
				}
				if err := validateRef("ref", ref); err != nil {
					return types.ErrResult(err)
				}
				file := types.GetStringOpt(in, "file")
				var args []string
				switch {
				case types.GetBool(in, "content") && file != "":
					args = []string{"show", ref + ":" + strings.TrimPrefix(filepath.ToSlash(file), "./")}
				case types.GetBool(in, "stat_only"):
					args = []string{"show", "--stat", ref}
				default:
					args = []string{"show", "--stat", "--patch", "--no-ext-diff", ref}
				}
				if file != "" && !types.GetBool(in, "content") {
					args = append(args, "--", file)
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("show", err)
				}
				return finishGit(out, "Output is large: use stat_only=true, or file=<path> for one file.")
			})),

		gitTool("git_blame", "Git blame", "Who last changed each line of a file (hash, author, date). Limit with start/end line numbers.",
			map[string]any{
				"file":  prop("string", "File to blame"),
				"start": prop("integer", "First line (1-based)"),
				"end":   prop("integer", "Last line (default start+39)"),
				"ref":   prop("string", "Blame as of this revision (default: working tree)"),
			}, []string{"file"},
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				file, err := types.GetString(in, "file")
				if err != nil {
					return types.ErrResult(err)
				}
				args := []string{"blame", "--date=short", "-w"}
				if start, ok := types.GetIntOpt(in, "start"); ok && start >= 1 {
					end, ok := types.GetIntOpt(in, "end")
					if !ok || end < start {
						end = start + 39
					}
					args = append(args, fmt.Sprintf("-L%d,%d", start, end))
				}
				if ref := types.GetStringOpt(in, "ref"); ref != "" {
					if err := validateRef("ref", ref); err != nil {
						return types.ErrResult(err)
					}
					args = append(args, ref)
				}
				args = append(args, "--", file)
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("blame", err)
				}
				return finishGit(out, "Blame output is large: pass start/end to look at a range.")
			})),

		gitTool("git_branch", "Git branches", "List branches with their last commit and upstream (all=true includes remote-tracking branches). Read-only; use git_branch_update to create/delete/rename.",
			map[string]any{"all": prop("boolean", "Include remote-tracking branches")}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				args := []string{"branch", "-vv"}
				if types.GetBool(in, "all") {
					args = append(args, "--all")
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("branch", err)
				}
				return finishGit(out, "")
			})),

		gitTool("git_stage", "Git stage", "Stage files (git add), or unstage them with unstage=true. all=true stages every change including untracked files.",
			map[string]any{
				"files":   strList("Paths to stage / unstage"),
				"all":     prop("boolean", "Stage everything (git add -A)"),
				"unstage": prop("boolean", "Remove the paths from the index instead (keeps working-tree changes)"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				files := filesArg(in, "files", "file")
				all := types.GetBool(in, "all")
				unstage := types.GetBool(in, "unstage")
				if len(files) == 0 && !all {
					return types.Errf("files is required (or all=true)")
				}
				var args []string
				switch {
				case unstage && all:
					args = []string{"restore", "--staged", "--", "."}
				case unstage:
					args = append([]string{"restore", "--staged", "--"}, files...)
				case all:
					args = []string{"add", "-A"}
				default:
					args = append([]string{"add", "--"}, files...)
				}
				if out, err := g.run(ctx, args...); err != nil {
					return gitFail(args[0], err)
				} else if out != "" {
					return types.Result{Output: out}
				}
				if unstage {
					return types.Result{Output: "Unstaged."}
				}
				return types.Result{Output: "Staged."}
			})),

		gitTool("git_commit", "Git commit",
			"Commit staged changes. all=true also commits tracked modifications (-a). amend=true rewrites the last commit (omit message to keep it). "+
				"A committer identity is supplied automatically when git has none.",
			map[string]any{
				"message":     prop("string", "Commit message"),
				"all":         prop("boolean", "Commit all tracked changes (-a)"),
				"amend":       prop("boolean", "Amend the previous commit"),
				"author":      prop("string", "Override author as 'Name <email>'"),
				"allow_empty": prop("boolean", "Allow a commit with no changes"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				msg := types.GetStringOpt(in, "message")
				amend := types.GetBool(in, "amend")
				if msg == "" && !amend {
					return types.Errf("message is required")
				}
				args := []string{"commit"}
				if types.GetBool(in, "all") {
					args = append(args, "-a")
				}
				if types.GetBool(in, "allow_empty") {
					args = append(args, "--allow-empty")
				}
				if a := types.GetStringOpt(in, "author"); a != "" {
					if strings.HasPrefix(a, "-") || !regexp.MustCompile(`^[^<>]+ <[^<>\s]+>$`).MatchString(a) {
						return types.Errf("author must look like 'Name <email>'")
					}
					args = append(args, "--author="+a)
				}
				if amend {
					args = append(args, "--amend")
					if msg == "" {
						args = append(args, "--no-edit")
					}
				}
				if msg != "" {
					args = append(args, "-m", msg)
				}
				g.timeout = 3 * time.Minute // pre-commit hooks can be slow
				g.env = identityEnv(ctx, g.dir)
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("commit", err)
				}
				return finishGit(out, "")
			})),

		gitTool("git_checkout", "Git checkout", "Switch branches (git switch). create=true makes a new branch (from `from`, default HEAD). detach=true checks out a commit/tag without a branch. Fails if local changes would be overwritten.",
			map[string]any{
				"ref":    prop("string", "Branch, tag or commit to switch to; with create=true, the NEW branch name"),
				"create": prop("boolean", "Create the branch"),
				"from":   prop("string", "Start point for create (default HEAD)"),
				"detach": prop("boolean", "Detach HEAD at ref"),
			}, []string{"ref"},
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				ref, err := types.GetString(in, "ref")
				if err != nil {
					return types.ErrResult(err)
				}
				if err := validateRef("ref", ref); err != nil {
					return types.ErrResult(err)
				}
				args := []string{"switch"}
				switch {
				case types.GetBool(in, "create"):
					args = append(args, "-c", ref)
					if from := types.GetStringOpt(in, "from"); from != "" {
						if err := validateRef("from", from); err != nil {
							return types.ErrResult(err)
						}
						args = append(args, from)
					}
				case types.GetBool(in, "detach"):
					args = append(args, "--detach", ref)
				default:
					args = append(args, ref)
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("switch", err)
				}
				return finishGit(out, "")
			})),

		gitTool("git_branch_update", "Git branch", "Create, delete or rename a local branch (action: create | delete | rename). delete refuses unmerged branches unless force=true.",
			map[string]any{
				"action":   map[string]any{"type": "string", "enum": []string{"create", "delete", "rename"}, "description": "What to do"},
				"name":     prop("string", "Branch name"),
				"new_name": prop("string", "New name (rename)"),
				"from":     prop("string", "Start point (create; default HEAD)"),
				"force":    prop("boolean", "Force delete of an unmerged branch (-D)"),
			}, []string{"action", "name"},
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				name := types.GetStringOpt(in, "name")
				if err := validateRef("name", name); err != nil {
					return types.ErrResult(err)
				}
				var args []string
				switch types.GetStringOpt(in, "action") {
				case "create":
					args = []string{"branch", name}
					if from := types.GetStringOpt(in, "from"); from != "" {
						if err := validateRef("from", from); err != nil {
							return types.ErrResult(err)
						}
						args = append(args, from)
					}
				case "delete":
					flag := "-d"
					if types.GetBool(in, "force") {
						flag = "-D"
					}
					args = []string{"branch", flag, name}
				case "rename":
					nn := types.GetStringOpt(in, "new_name")
					if err := validateRef("new_name", nn); err != nil {
						return types.ErrResult(err)
					}
					args = []string{"branch", "-m", name, nn}
				default:
					return types.Errf("action must be create, delete or rename")
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("branch", err)
				}
				return finishGit(firstNonEmpty(out, "Done."), "")
			})),

		gitTool("git_restore", "Git restore",
			"DISCARD changes to files: by default restores the working-tree copy from the index (losing uncommitted edits). staged=true only unstages; source=<ref> restores from a commit. Cannot be undone.",
			map[string]any{
				"files":    strList("Paths to restore"),
				"staged":   prop("boolean", "Restore the index (unstage) instead of the working tree"),
				"worktree": prop("boolean", "With staged=true, also restore the working tree"),
				"source":   prop("string", "Restore content from this commit/ref"),
			}, []string{"files"},
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				files := filesArg(in, "files", "file")
				if len(files) == 0 {
					return types.Errf("files is required")
				}
				args := []string{"restore"}
				if types.GetBool(in, "staged") {
					args = append(args, "--staged")
					if types.GetBool(in, "worktree") {
						args = append(args, "--worktree")
					}
				}
				if src := types.GetStringOpt(in, "source"); src != "" {
					if err := validateRef("source", src); err != nil {
						return types.ErrResult(err)
					}
					args = append(args, "--source="+src)
				}
				args = append(append(args, "--"), files...)
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("restore", err)
				}
				return types.Result{Output: firstNonEmpty(out, fmt.Sprintf("Restored %d path(s).", len(files)))}
			})),

		gitTool("git_stash", "Git stash", "Shelve and restore local changes. action: push (default) | pop | apply | list | drop | show. include_untracked=true also stashes new files.",
			map[string]any{
				"action":            map[string]any{"type": "string", "enum": []string{"push", "pop", "apply", "list", "drop", "show"}, "description": "Operation (default push)"},
				"message":           prop("string", "Stash message (push)"),
				"include_untracked": prop("boolean", "Also stash untracked files (push)"),
				"index":             prop("integer", "Which stash entry for pop/apply/drop/show (default 0 = newest)"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				idx, _ := types.GetIntOpt(in, "index")
				if idx < 0 {
					idx = 0
				}
				entry := fmt.Sprintf("stash@{%d}", idx)
				var args []string
				switch a := firstNonEmpty(types.GetStringOpt(in, "action"), "push"); a {
				case "push":
					args = []string{"stash", "push"}
					if types.GetBool(in, "include_untracked") {
						args = append(args, "--include-untracked")
					}
					if m := types.GetStringOpt(in, "message"); m != "" {
						args = append(args, "-m", m)
					}
				case "pop", "apply", "drop":
					args = []string{"stash", a, entry}
				case "show":
					args = []string{"stash", "show", "--stat", "-p", entry}
				case "list":
					args = []string{"stash", "list"}
				default:
					return types.Errf("unknown action %q", a)
				}
				g.env = identityEnv(ctx, g.dir)
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("stash", err)
				}
				if out == "" && args[1] == "list" {
					out = "(no stashes)"
				}
				return finishGit(out, "")
			})),

		gitTool("git_reset", "Git reset",
			"Move HEAD to ref. mode soft keeps changes staged, mixed (default) keeps them unstaged, hard DISCARDS all uncommitted changes and requires confirm=true.",
			map[string]any{
				"ref":     prop("string", "Target commit (default HEAD)"),
				"mode":    map[string]any{"type": "string", "enum": []string{"soft", "mixed", "hard"}, "description": "Reset mode (default mixed)"},
				"confirm": prop("boolean", "Required for mode=hard"),
			}, nil,
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				mode := firstNonEmpty(types.GetStringOpt(in, "mode"), "mixed")
				if mode != "soft" && mode != "mixed" && mode != "hard" {
					return types.Errf("mode must be soft, mixed or hard")
				}
				ref := firstNonEmpty(types.GetStringOpt(in, "ref"), "HEAD")
				if err := validateRef("ref", ref); err != nil {
					return types.ErrResult(err)
				}
				if mode == "hard" && !types.GetBool(in, "confirm") {
					st, _ := g.run(ctx, "status", "--short")
					return types.Errf("reset --hard to %s would permanently discard all uncommitted changes:\n%s\nRepeat with confirm=true if that is really intended.", ref, firstNonEmpty(st, "(nothing uncommitted)"))
				}
				out, err := g.run(ctx, "reset", "--"+mode, ref, "--")
				if err != nil {
					return gitFail("reset", err)
				}
				return types.Result{Output: firstNonEmpty(out, fmt.Sprintf("Reset (%s) to %s.", mode, ref))}
			})),

		gitTool("git_sync", "Git sync",
			"Talk to a remote. action: fetch | pull | push. pull supports rebase=true / ff_only=true; push supports set_upstream=true, tags=true and force_with_lease=true (plain --force is not offered). Network commands time out after 5 minutes and never prompt for credentials.",
			map[string]any{
				"action":           map[string]any{"type": "string", "enum": []string{"fetch", "pull", "push"}, "description": "fetch, pull or push"},
				"remote":           prop("string", "Remote name (default origin)"),
				"branch":           prop("string", "Branch (default: current branch's upstream)"),
				"rebase":           prop("boolean", "pull: rebase instead of merge"),
				"ff_only":          prop("boolean", "pull: refuse non-fast-forward merges"),
				"set_upstream":     prop("boolean", "push: set the upstream (-u)"),
				"tags":             prop("boolean", "push/fetch: include tags"),
				"force_with_lease": prop("boolean", "push: overwrite remote only if it is unchanged since your last fetch"),
			}, []string{"action"},
			withRepo(func(ctx context.Context, g gitRun, in map[string]any) types.Result {
				g.timeout = gitSlowTimeout
				remote := firstNonEmpty(types.GetStringOpt(in, "remote"), "origin")
				if err := validateRef("remote", remote); err != nil {
					return types.ErrResult(err)
				}
				branch := types.GetStringOpt(in, "branch")
				if branch != "" {
					if err := validateRef("branch", branch); err != nil {
						return types.ErrResult(err)
					}
				}
				args := []string{}
				switch a := types.GetStringOpt(in, "action"); a {
				case "fetch":
					args = append(args, "fetch", "--prune")
					if types.GetBool(in, "tags") {
						args = append(args, "--tags")
					}
					args = append(args, remote)
					if branch != "" {
						args = append(args, branch)
					}
				case "pull":
					args = append(args, "pull")
					switch {
					case types.GetBool(in, "rebase"):
						args = append(args, "--rebase")
					case types.GetBool(in, "ff_only"):
						args = append(args, "--ff-only")
					}
					args = append(args, remote)
					if branch != "" {
						args = append(args, branch)
					}
					g.env = identityEnv(ctx, g.dir)
				case "push":
					args = append(args, "push")
					if types.GetBool(in, "set_upstream") {
						args = append(args, "-u")
					}
					if types.GetBool(in, "tags") {
						args = append(args, "--tags")
					}
					if types.GetBool(in, "force_with_lease") {
						args = append(args, "--force-with-lease")
					}
					args = append(args, remote)
					if branch != "" {
						args = append(args, branch)
					}
				default:
					return types.Errf("action must be fetch, pull or push")
				}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail(args[0], err)
				}
				return finishGit(firstNonEmpty(out, "Done."), "")
			})),

		{
			Name:  "git_clone",
			Label: "Git clone",
			Description: "Clone a repository (https, ssh or git:// URL) into dest (default: ./<repo name> in the workspace). depth for a shallow clone; branch to check out. " +
				"Local paths, file:// and private/loopback hosts are refused.",
			Parameters: map[string]any{
				"url":    prop("string", "Repository URL"),
				"dest":   prop("string", "Target directory (default: repo name under the workspace root)"),
				"depth":  prop("integer", "Shallow clone depth"),
				"branch": prop("string", "Branch or tag to check out"),
			},
			Required: []string{"url"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				raw := strings.TrimSpace(types.GetStringOpt(input, "url"))
				if err := validateCloneURL(raw); err != nil {
					return types.ErrResult(err)
				}
				dest := types.GetStringOpt(input, "dest")
				if dest == "" {
					base := strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git")
					if i := strings.LastIndexAny(base, "/:"); i >= 0 {
						base = base[i+1:]
					}
					if base == "" || strings.HasPrefix(base, "-") {
						return types.Errf("cannot derive a directory name from the url; pass dest")
					}
					dest = base
				}
				destAbs := workspace.Resolve(dest)
				if _, err := os.Stat(destAbs); err == nil {
					if entries, _ := os.ReadDir(destAbs); len(entries) > 0 {
						return types.Errf("destination already exists and is not empty: %s", destAbs)
					}
				}
				if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
					return types.Errf("mkdir error: %v", err)
				}
				args := []string{"clone", "--progress"}
				if d, ok := types.GetIntOpt(input, "depth"); ok && d > 0 {
					args = append(args, fmt.Sprintf("--depth=%d", d))
				}
				if b := types.GetStringOpt(input, "branch"); b != "" {
					if err := validateRef("branch", b); err != nil {
						return types.ErrResult(err)
					}
					args = append(args, "--branch="+b)
				}
				args = append(args, "--", raw, destAbs)
				g := gitRun{dir: filepath.Dir(destAbs), timeout: gitCloneTimeout}
				out, err := g.run(ctx, args...)
				if err != nil {
					return gitFail("clone", err)
				}
				return types.Result{Output: fmt.Sprintf("Cloned into %s\n%s", destAbs, types.CutTail(out, 1500))}
			},
		},
	}
}
