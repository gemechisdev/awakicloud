# Awaki — Self-hosted AI Agent

Lightweight, multi-provider AI agent for free tier / small VPS.
Based on GoGogot agent loop, expanded with Awaki-style multi-provider support
(OpenAI, Anthropic, Gemini, OpenRouter, custom OpenAI-compatible).

## Providers

| Provider   | Env key(s)                           | Example model                |
|------------|--------------------------------------|------------------------------|
| Anthropic  | ANTHROPIC_API_KEY                    | claude-sonnet-4-6            |
| OpenAI     | OPENAI_API_KEY                       | gpt-4o                       |
| Gemini     | GEMINI_API_KEY or GOOGLE_API_KEY     | gemini-2.5-flash             |
| OpenRouter | OPENROUTER_API_KEY                   | anthropic/claude-sonnet-4.6  |
| Custom     | AWAKI_BASE_URL + AWAKI_API_KEY       | any OpenAI-compatible id     |

```bash
export AWAKI_PROVIDER=gemini
export AWAKI_MODEL=gemini-2.5-flash
export GEMINI_API_KEY=...
export TELEGRAM_BOT_TOKEN=...
export TELEGRAM_OWNER_ID=...
```

Legacy GOGOGOT_* env names still work as fallbacks.

## Docker

```bash
docker run -d --restart unless-stopped --name awaki \
  -e TELEGRAM_BOT_TOKEN=... \
  -e TELEGRAM_OWNER_ID=... \
  -e AWAKI_PROVIDER=anthropic \
  -e ANTHROPIC_API_KEY=... \
  -e AWAKI_MODEL=claude-sonnet-4-6 \
  -v awaki-data:/data -v awaki-work:/work \
  awaki/awaki:latest
```

See deploy/docker-compose.yml.

## Build

```bash
go build -o awaki ./cmd/awaki
./awaki
```

## Status

- [x] Phase 0: Rebrand from GoGogot (module, cmd, Docker, config AWAKI_*)
- [x] Phase 1: Multi-provider (Anthropic, OpenAI, Gemini, OpenRouter, custom)
- [x] Phase 2: Telegram durable store (Telegrive-style hybrid)
- [x] Phase 3 (partial): Plan mode, git tools, batch file tools, destructive bash guard
- [x] Phase 3 remaining: sub-agents / delegate

## License

MIT


## Telegram storage — Telegrive-style user account (recommended)

Awaki can keep durable state in a **private Telegram channel owned by your user
account**, the same idea as [Telegrive](https://daveragos.github.io/telegrive/):

- Sign in once with **phone + login code** (MTProto session file is saved)
- Auto-creates a private channel titled **Awaki** (configurable)
- Every soul / memory / skill / chat snapshot is a channel message with an
  `awaki:<kind>;key=value` details line (Telegrive caption pattern)
- Local disk is only a cache + tiny index — free-tier safe

### Setup

1. Open https://my.telegram.org → API development tools → create an app  
2. Set env:

```bash
export AWAKI_TG_API_ID=12345678
export AWAKI_TG_API_HASH=your_api_hash
export AWAKI_TG_PHONE=+2519xxxxxxxx
# optional 2FA:
# export AWAKI_TG_PASSWORD=your_2fa_password
```

3. First start: Telegram sends a login code. Either:
   - type it on stdin, or
   - `echo 12345 > $AWAKI_DATA_DIR/login_code.txt`, or
   - `export AWAKI_TG_CODE=12345`

4. Session is stored at `$AWAKI_DATA_DIR/telegram.session` — keep this volume.

Agent **chat** still uses the Bot API (`TELEGRAM_BOT_TOKEN` + owner id).  
**Storage** uses the user account when `AWAKI_TG_*` is set.

Legacy bot-only storage (`AWAKI_STORAGE_CHAT_ID`) remains as a fallback.

## Sub-agents (`delegate`)

The main agent can call the `delegate` tool:

```json
{ "task": "Find all auth middleware usages and summarize", "role": "researcher", "scope": "internal/" }
```

| Role | Behavior |
|------|----------|
| `researcher` | Read-only tools; returns findings + plan |
| `implementer` | Full tools except nested chaos; makes changes |
| `general` | Normal tool set; no nested delegate by default |

Depth is capped at 2. Sub-agents start with a clean history; put all context in `task`.


## Providers (Awaki-style runtime config)

Providers are **connection configs** only: name, protocol, base URL, API key.
Nothing is hardcoded for Anthropic/OpenAI/Gemini endpoints.

```text
/providers          — list + delete buttons
/models             — list + select / delete buttons
/provider_add name|protocol|base_url|api_key
/model_add provider|model_id|Display Name
```

Protocols:
- `openai_chat_completions`
- `openai_responses`
- `anthropic_messages`
- `google_gemini`

Examples:

```text
/provider_add OpenRouter|openai_chat_completions|https://openrouter.ai/api/v1|sk-or-v1-...
/model_add OpenRouter|anthropic/claude-sonnet-4.6|Claude Sonnet

/provider_add Gemini|google_gemini|https://generativelanguage.googleapis.com/v1beta|AIza...
/model_add Gemini|gemini-2.5-flash|Flash
```

Config is stored in `$AWAKI_DATA_DIR/providers.json`.
