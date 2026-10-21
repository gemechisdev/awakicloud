# Awaki

Self-hosted Telegram AI agent (Go). Multi-provider LLM config at runtime, Parallel/DuckDuckGo search, optional Telegram user-account storage.

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
| `/providers` | List / manage providers |
| `/models` | List / select models |
| `/provider_add name\|protocol\|base_url\|api_key` | Add provider |
| `/model_add provider\|model_id\|name` | Add & activate model |
| `/history` `/memory` `/soul` `/user` `/stop` `/help` | Utility |

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

## Web tools

- **Search:** Parallel MCP → DuckDuckGo (no API key)
- **Fetch:** Parallel MCP → Jina.ai → direct HTTP

## Storage

- Default: local disk under `AWAKI_DATA_DIR`
- Optional: Telegrive-style user session + private "Awaki" channel (`AWAKI_TG_API_ID`, `AWAKI_TG_API_HASH`, `AWAKI_TG_PHONE`)
