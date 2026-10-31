# Awaki

Self-hosted Telegram AI agent (Go). Multi-provider LLM config at runtime, Parallel/DuckDuckGo search, MCP server support, optional Telegram user-account storage, and durable project workspaces.

## Quick start

```bash
cp .env.example .env
# set TELEGRAM_BOT_TOKEN and TELEGRAM_OWNER_ID

go build -o awaki ./cmd/awaki
./awaki
```

In Telegram:

```text
/provider_add OpenRouter|openai_chat_completions|https://openrouter.ai/api/v1|sk-or-v1-...
/model_add OpenRouter|anthropic/claude-sonnet-4.6|Claude
```

## Commands

| Command | Description |
|---------|-------------|
| `/new` | New conversation |
| `/plan <task>` | Read-only plan mode |
| `/stop` | Cancel current agent run |
| `/providers` | List / manage LLM providers |
| `/models` | List / select models |
| `/provider_add name\|protocol\|base_url\|api_key` | Add provider |
| `/model_add provider\|model_id\|name` | Add & activate model |
| `/mcp` | List configured MCP servers |
| `/mcp_add` | Add a new MCP server |
| `/skills` | List installed Agent Skills |
| `/skills add <url> [--skill name]` | Install a skill from GitHub, skills.sh, smithery.ai, a `/.well-known/skills` site, or a `.zip`/`SKILL.md` URL |
| `/plugin marketplace add owner/repo` | Register a Claude-style plugin marketplace |
| `/projects` | List projects |
| `/project <name>` | Create a project |
| `/upgrade [name]` | Upgrade chat → durable project |
| `/sync` | Sync active project to Telegram storage |
| `/history` `/memory` `/soul` `/user` `/help` | Utility |
| `/tasks` | Background task board |
| `/task <work>` | Queue a background task |

Protocols: `openai_chat_completions`, `openai_responses`, `anthropic_messages`, `google_gemini`

## Environment

| Variable | Purpose |
|----------|---------|
| `TELEGRAM_BOT_TOKEN` | Bot token (required) |
| `TELEGRAM_OWNER_ID` | Your user id (required) |
| `AWAKI_DATA_DIR` | Data directory (default `~/.awaki`) |
| `AWAKI_WORK_DIR` | Workspace root |
| `AWAKI_BASE_URL` / `AWAKI_API_KEY` / `AWAKI_MODEL` / `AWAKI_PROVIDER` | Optional LLM bootstrap |
| `AWAKI_TG_*` | Optional user-account storage (MTProto) |

`.env` is loaded automatically from the current working directory.

## Skills

Install Agent Skills (a folder with `SKILL.md`) the same way as `npx skills add`:

```text
/skills add anthropics/skills --skill frontend-design
/skills add https://github.com/anthropics/skills/tree/main/skills/frontend-design
/skills add https://skills.sh/anthropics/skills/frontend-design
/skills add https://smithery.ai/skills/anthropics/frontend-design
/skills add https://example.com            # reads /.well-known/skills/index.json
/skills add https://example.com/my-skill.zip
```

Pasting the full `npx -y skills add <url>` command also works. A source that holds several skills installs all of them (up to 25; use `--skill <name>` to pick one or `--all` to lift the cap). Set `GITHUB_TOKEN` to avoid GitHub API rate limits. Skills can include scripts the agent may run, so only install sources you trust.

## MCP Servers

Awaki supports external tools via Model Context Protocol (stdio, Streamable HTTP, SSE). Add a server in Telegram:

```text
/mcp_add
```

Then follow the prompts to name it and provide its transport details. MCP tools become available to the agent after connection. Remote servers may require OAuth — use Settings → MCP → Authorize in the client, then paste the callback back.

## Projects

Durable workspaces under `/data/projects/{slug}/`. Files here survive container restarts and are synced as ZIP archives to Telegram storage.

- `/project <name>` — create a new project and bind the chat
- `/upgrade [name]` — promote current chat to a project
- `/sync` — manually sync the active project ZIP to Telegram
- Auto-sync: project ZIP is synced every 30s (matching the DB snapshot cadence) and after every successful DB upload to Telegram

## Storage

- Default: local disk under `AWAKI_DATA_DIR`
- Optional: Telegrive-style user session + private "Awaki" channel (`AWAKI_TG_API_ID`, `AWAKI_TG_API_HASH`, `AWAKI_TG_PHONE`)
- SQLite DB (`awaki.db`) holds chats, memory, skills, providers, schedules, and project metadata
- Versioned DB snapshots are uploaded to Telegram storage every 30s (debounced)

## Web tools

- **Search:** Parallel MCP → DuckDuckGo (no API key)
- **Fetch:** Parallel MCP → Jina.ai → direct HTTP