# Awaki

**Self-hosted Telegram AI agent** written in Go.

Runtime multi-provider LLMs, durable projects, Telegram cloud storage, MCP tools, background tasks, scheduling, permission modes, and HTTP hooks — designed to stay **lightweight** enough for free-tier PaaS (Koyeb, Render, Railway) or a small VPS.

---

## Features

| Area | What you get |
|------|----------------|
| **Chat agent** | Tool-use loop over Telegram (files, shell, git, web, memory, skills) |
| **Multi-provider LLM** | OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, Google Gemini — add/select via Telegram (no hardcoded keys) |
| **Permission modes** | Sticky per-chat **auto / ask / plan** (`/mode`); one-shot `/plan <task>` |
| **Projects** | Durable workspaces under `/data/projects/{slug}/`, ZIP-synced to Telegram |
| **Storage** | SQLite + optional Telegrive-style user session / storage channel; DB snapshots to TG |
| **Background tasks** | In-process board with live progress pushes to the owner |
| **Scheduler** | One-shot (`in 5m`), interval, cron; reminder-formatted delivery |
| **MCP** | stdio / HTTP / SSE servers; tools hot-loaded into the agent |
| **Skills** | Agent Skills (`SKILL.md` packages), install from GitHub / URL / zip |
| **Web** | Search (Parallel → DuckDuckGo), fetch (Parallel → Jina → direct) + SSRF guard |
| **Grep** | Flexible, token-efficient `grep` (ripgrep preferred) |
| **HTTP ops** | Health probes + optional authenticated remote hooks |
| **Steer** | While busy: stop-now / queue / dismiss mid-run messages |

---

## Quick start

```bash
cp .env.example .env
# Set TELEGRAM_BOT_TOKEN and TELEGRAM_OWNER_ID

go build -o awaki ./cmd/awaki
./awaki
```

`.env` is loaded automatically from the working directory (shell-exported vars win).

### First-time LLM setup (in Telegram)

```text
/provider_add OpenRouter|openai_chat_completions|https://openrouter.ai/api/v1|sk-or-v1-...
/model_add OpenRouter|anthropic/claude-sonnet-4.6|Claude
/models
```

Protocols: `openai_chat_completions`, `openai_responses`, `anthropic_messages`, `google_gemini`.

Providers and models persist in SQLite and survive redeploys when storage is configured.

---

## Commands

Commands are defined in **one catalog** (`internal/channel/commands.go`) that feeds the Telegram menu, routing, and `/help`.

| Command | Description |
|---------|-------------|
| `/start` | Main menu |
| `/new` | New conversation |
| `/stop` | Cancel current agent run |
| `/help` | Full command list |
| `/mode [auto\|ask\|plan]` | Sticky permission mode (inline buttons if bare `/mode`) |
| `/plan <task>` | One-shot read-only plan turn |
| `/tasks` | Background task board |
| `/task <work>` | Queue a background task |
| `/history` | Past conversations |
| `/memory` | List memory files |
| `/soul` | Agent identity (`soul.md`) |
| `/user` | User profile (`user.md`) |
| `/providers` | Manage LLM providers |
| `/models` | List / select models |
| `/provider_add name\|protocol\|base_url\|api_key` | Quick-add provider |
| `/model_add provider\|model_id\|Display Name` | Quick-add model |
| `/projects` | List projects |
| `/project <name>` | Create project |
| `/upgrade [name]` | Upgrade chat → project |
| `/sync` | Sync active project to Telegram |
| `/mcp` | List MCP servers |
| `/mcp_add` | Add MCP server (wizard) |
| `/skills` | List agent skills |
| `/skill_add` | Add a skill |
| `/plugin` | Skills marketplace helpers |

Inline menus cover the same flows (Settings → providers, MCP, skills, permission mode, tasks, projects).

---

## Permission modes

| Mode | Behavior |
|------|----------|
| **auto** (default) | Full tool use |
| **ask** | Mutating tools pause for **Approve / Deny** |
| **plan** | Read-only tool set; explore and plan only |

- Sticky **per chat** (`permission_modes.json` under data dir)
- `/plan <task>` forces plan **for that turn only**
- Default for new chats: `AWAKI_PERMISSION_MODE` (optional)

---

## Background tasks

```text
/task Research payment APIs and summarize
/tasks
```

- In-process worker pool (default max concurrent **1** on free tier)
- Owner gets **start**, **live progress** (via `report_status` / `send_message`), and **done/fail** Telegram pushes
- Tools: `bg_task_add`, `bg_task_list`, `bg_task_get`, `bg_task_start`, `bg_task_stop`

---

## Scheduler & reminders

Natural schedules:

- `in 5m` / `once:10m` — one-shot (auto-remove after fire)
- `every 10m` / `@every 5m` — interval
- Standard cron expressions

Reminder-style tasks deliver content-first:

```text
📆 Reminder:
Time to call your team.
```

(Not meta text like “I sent the reminder.”)

---

## Projects & workspace

- Normal chats use scratch workspace
- **Projects** live under `{data}/projects/{slug}/` and sync as ZIP archives to Telegram storage
- `/project`, `/upgrade`, `/sync`, or agent `promote_project`
- Auto-sync aligns with DB snapshot cadence (~30s)

---

## Storage

| Layer | Role |
|-------|------|
| **SQLite** (`awaki.db`) | Chats, memory, skills, providers, schedules, tasks, MCP config |
| **Telegram channel** | Optional durable cloud: versioned DB snapshots + project ZIPs |
| **MTProto user session** | Optional Telegrive-style storage (`AWAKI_TG_*`) |

On cold start, configuration rehydrates from SQLite (and TG snapshots when configured).

---

## MCP

```text
/mcp
/mcp_add
```

Supports **stdio**, **Streamable HTTP**, and **SSE**. Connected server tools are registered for the agent. Remote servers may use OAuth (authorize flow in UI, paste callback).

---

## Skills

Install Agent Skills (folder with `SKILL.md`):

```text
/skills
/skill_add
/plugin
```

Supports GitHub trees, skills.sh, smithery-style URLs, zip / `SKILL.md` URLs, and Claude-style marketplace helpers where configured.

---

## Tools (high level)

**Files & shell:** `read_file` (paged, offset/limit), `write_file` (atomic, overwrite diff), `edit_file` (unique-match, multi-hunk, regex, dry-run, diff output), `apply_patch` (multi-file unified diff, atomic), `move_file`, `copy_file`, `delete_file`, `make_dir`, `list_files`, `glob_files` (`**`, .gitignore-aware), `directory_tree`, `read_files`, `file_info`, `bash` (head+tail output, `background=true`, progress heartbeats), `grep` / `file_search`  

**Git:** read-only `git_status`, `git_diff`, `git_log`, `git_show`, `git_blame`, `git_branch`; mutating `git_stage`, `git_commit`, `git_checkout`, `git_restore`, `git_stash`, `git_reset`, `git_branch_update`, `git_sync` (fetch/pull/push), `git_clone` — refs/remotes/URLs are validated, git never prompts  

**Web:** `web_search`, `web_fetch` (paged with `offset`), `web_request` (`json`/`form`/`query`, binary-safe, cookies redacted), `web_download` (SSRF guard on agent-side fetches)  

**Memory / identity:** `memory_list`, `memory_read`, `memory_search`, `memory_write`, `memory_append`, `memory_delete`, `soul_*`, `user_*`  

**Skills:** `skill_list`, `skill_read`, `skill_run`, …  

**Ops:** `schedule_*`, `bg_task_*`, `task_plan`, `report_status`, `send_message`, `ask_user`, `delegate` (sub-agents)

**Host limits (prompt):** on free-tier hosts the agent prefers static checks over full builds/tests unless you explicitly ask.

---

## HTTP: health & remote hooks

Same listen port (`PORT` or `AWAKI_HTTP_PORT`, default `8000`):

| Endpoint | Purpose |
|----------|---------|
| `GET /` | Status JSON (`busy`, `hooks_enabled`, …) |
| `GET /health`, `/healthz`, `/ping` | Liveness |
| `GET /ready` | Ready after Telegram polling starts |

### Remote triggers

Set `AWAKI_HOOK_SECRET`, then:

```bash
# Run agent turn as owner
curl -X POST "https://your-host/hooks/agent" \
  -H "X-Awaki-Secret: $AWAKI_HOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"text":"Summarize deploy status","mode":"auto"}'

# Queue background task
curl -X POST "https://your-host/hooks/task" \
  -H "Authorization: Bearer $AWAKI_HOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"title":"Research X","prompt":"...","auto_start":true}'

# Schedule
curl -X POST "https://your-host/hooks/schedule" \
  -H "X-Awaki-Secret: $AWAKI_HOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"schedule":"in 10m","command":"Time to review PRs"}'
```

Auth: `X-Awaki-Secret` or `Authorization: Bearer`.  
Agent `mode`: `auto` | `queue` | `interrupt`.

---

## Environment

| Variable | Purpose |
|----------|---------|
| `TELEGRAM_BOT_TOKEN` | Bot token (**required**) |
| `TELEGRAM_OWNER_ID` | Your Telegram user id (**required**) |
| `PORT` / `AWAKI_HTTP_PORT` | HTTP listen port (default `8000`) |
| `AWAKI_DATA_DIR` | Data directory (default `~/.awaki`) |
| `AWAKI_WORK_DIR` | Workspace root |
| `AWAKI_HOOK_SECRET` | Enable `/hooks/*` |
| `AWAKI_PERMISSION_MODE` | Default mode: `auto` \| `ask` \| `plan` |
| `AWAKI_PROVIDER` / `AWAKI_MODEL` / `AWAKI_BASE_URL` / `AWAKI_API_KEY` | Optional LLM bootstrap (prefer Telegram CRUD) |
| `AWAKI_STORAGE_CHAT_ID` | Bot-mode storage channel id |
| `AWAKI_TG_API_ID` / `AWAKI_TG_API_HASH` / `AWAKI_TG_PHONE` / … | Optional MTProto user storage |
| `AWAKI_TG_CHANNEL` | Storage channel title (default `Awaki`) |
| `AWAKI_SCHEDULER_TASK_TIMEOUT` | Per scheduled run timeout |
| `AWAKI_SCHEDULER_MAX_CONCURRENT` | Scheduler concurrency |
| `AWAKI_CONTINUATION` | Server-side continuation where supported (default true) |
| `LOG_LEVEL` | e.g. `info`, `debug` |

See `.env.example` for the full annotated list.

### Tool safety settings

| Variable | Default | Effect |
|----------|---------|--------|
| `AWAKI_BASH_GUARD` | `warn` | Command guard for `bash`. `warn`: **critical** commands (wiping `/`, system or home directories, `mkfs`/`dd` on a device, fork bombs) are refused, **risky** ones (`rm -rf`, `git push --force`, `git reset --hard`, `curl \| sh`, `DROP TABLE`, stopping sshd, …) run with a warning appended to the result. `block`: refuse both. `off`: no checks. A typo falls back to `warn`. It is a safety net for accidents, not a sandbox. |
| `AWAKI_BASH_ENV` | `scrub` | `scrub` hides credential-like variables (`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*PASSWORD*`, all `AWAKI_*` / `TELEGRAM_*`, and values that look like keys) from `bash` and `git` child processes. `inherit` passes everything. |
| `AWAKI_BASH_ENV_PASSTHROUGH` | – | Comma-separated variables to keep even when scrubbing, e.g. `GITHUB_TOKEN,NPM_TOKEN`. |
| `AWAKI_SEND_FILE_ROOTS` | – | Extra directories `send_file` may send from (path-list separated). Default: workspace + temp dir; credential-like files are always refused. |
| `AWAKI_SEND_MAX_MB` | `50` | `send_file` size limit. |
| `AWAKI_GIT_NAME` / `AWAKI_GIT_EMAIL` | `Awaki Agent` / `awaki@localhost` | Committer identity used only when git has none configured. |

### Hardening

Scrubbing the environment stops `env`/`printenv` from leaking keys, but a command can still try to read the agent's own `/proc/<pid>/environ`, `.env` files, or the session files in the data directory. The agent marks itself non-dumpable at startup (`PR_SET_DUMPABLE`), which blocks that read for processes running as a **non-root** user. **The stock Dockerfile runs as root, where this is not enough** — for real isolation run the container as an unprivileged user that owns only the data and work directories, and keep `ask` permission mode on when untrusted content (web pages, issues, email) can reach the agent:

```dockerfile
RUN adduser -D -u 10001 awaki && mkdir -p /data /work && chown -R awaki /data /work
USER awaki
```

---

## Deploy

### Docker

```bash
make docker-build
# or
docker build -f deploy/Dockerfile -t awaki .
```

`deploy/docker-compose.yml` is included for local/VPS use.

### Free PaaS (Koyeb / Render / Railway)

1. Build the binary or container from this repo  
2. Set `TELEGRAM_BOT_TOKEN`, `TELEGRAM_OWNER_ID`, and a persistent volume for `AWAKI_DATA_DIR` if the platform offers one  
3. Expose `PORT` (health checks use `/health` or `/`)  
4. Optionally set `AWAKI_HOOK_SECRET`, storage channel, and MTProto vars  

Rolling deploys wait a short poll grace so the previous instance can release `getUpdates`.

### Makefile

```bash
make build    # → ./awaki
make run
make docker-build
```

---

## Architecture (short)

```text
Telegram ──► channel ──► core.Engine ──► agent loop + tools
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
         SQLite store    scheduler      tasks.Manager
              │               │               │
              └────────── optional TG cloud ──┘
                              │
                         HTTP health + hooks
```

- **Single binary**, Go 1.25+  
- Tool registry + optional MCP bridge  
- Permission and plan policies applied at the agent tool loop  

---

## License

See [LICENSE](LICENSE).
