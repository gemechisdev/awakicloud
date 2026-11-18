# Correctness pass — H-6 … H-9

Working notes for the durability/correctness sweep. Kept in the repo so the reasoning
survives the container restart (see the memory file for why that matters).

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` deliberately skipped

---

## H-6 — "SSH identities aren't durable on the telegram-store backend"

**Verdict: my own misdiagnosis. There is nothing to fix. `[!]`**

### What I originally claimed
`sshStore.SetPersister(sshDBAdapter{ss})` is only wired for `*sqlstore.Store`; the `else`
fallback (`sshStore.RebuildConfig()`, no persister) is "the telegram-store backend", so `/ssh`
data would not survive.

### Why that is wrong — proof, not reasoning
1. `buildStore` has exactly four exits. The concrete types it can return are:
   - `tgstore.StartUserStore(...)` → `*UserStore`
   - `st, nil` where `st, err := sqlstore.Open(cfg.DataDir)` → `*sqlstore.Store`
   - `local.New(cfg.DataDir)` → `*local.Store`
   - `nil, err`
2. `tgstore.New` / `telegram.New(cfg Config)` — the only constructor that produces
   `*tgstore.Store` — **has no callers in the whole repo.** The single `telegram.New(` hit at
   `main.go:526` is the *channel* package (`internal/channel/telegram`), a different package.
3. Therefore `if ss, ok := st.(*sqlstore.Store); ok { … } else if ts, ok := st.(*tgstore.Store); ok { … }`
   can only ever take the **first** branch or neither. All four `*tgstore.Store` branches are
   unreachable.
4. So SSH persistence has exactly two real states: **sqlite-backed (durable)** or
   **disk-only (no DB at all — first install, or sqlite failed to open)**. There is no
   "telegram-store backend that loses SSH identities".

### Confirmed live
The production container has no MTProto vars and no visible `AWAKI_STORAGE_CHAT_ID`, yet
`/data/awaki.db` exists and the `ssh_identities` table holds all five identities with key
material present (3381–3401 bytes each). SQLite is the path; the DB survives.

**Consequence: H-9 alone covers the real trap (disk-only identities).** See below.

---

## H-9 — make the DB-wins rule visible · `[ ]`

### Current behaviour (sshcreds/store.go)
```go
if len(rows) == 0 {
    return s.backfillDBLocked()   // DB empty  → import disk into DB
}
// DB has rows → rebuild disk FROM the DB, silently
```

One-directional and deliberate: the DB wins. That is what stops the post-restart
empty disk from clobbering the DB — the failure the owner already hit. **Keep it.**

The cost is that the overwrite is invisible: no log, no UI signal, and
`Store.Durable()` exists but is called from nowhere except a test.

### Plan
1. `loadLocked()` merges three sources: `identities.json`, `keys/` directory listing, and
   the generated `config` file. A **write-only DB copy** helps here: `backfillDBLocked`
   persists identities found only in `keys/`, but `loadLocked` may not surface them.
2. `HydrateFromDB`, DB branch: compute `diskOnly := identities present on disk but absent
   from the DB rows`. `log.Warn` the count and host ids — **never key material**.
3. Record it on the store (`s.diskOnlyCount`, `s.diskOnlyHosts`) so the UI can read it
   without re-parsing disk on every render.
4. `/ssh` board: when non-empty, show
   *"⚠️ N identity(ies) exist on disk only and will be discarded — the database wins on
   restart. Re-add via /ssh to make them durable."*
5. Drop the dead `Durable()` or start using it (I will use it).

---

## H-8 — ignored errors · `[ ]`

473 `_ =` and 20 `_, _ =` in non-test code. **Most are benign by design** —
`Close()`, `os.Remove`, `SkipDir`, and "hydrate the local cache from cloud" writes.
Churning those adds risk and diff noise for no correctness gain. `[!]` (separate pass if ever asked)

### The 11 sites that matter: user-visible state written, error discarded

| Site | Consequence if it fails |
|---|---|
| `core/projects.go:56,125,135,192,204,230` `_ = ch.Save()` | chat loses `Mode`/`ProjectSlug` → project appears to vanish |
| `core/chat/summarize.go:87` `_ = ch.Save()` | summary silently not persisted |
| `tools/system/project.go:85` `_ = ch.Save()` | same binding loss |
| `mcp/manager.go:120,241` `_ = m.store.Upsert(srv)` | MCP server config lost |
| `core/ui_projects.go:419` `_, _ = e.projects.Sync(...)` | archive never uploaded **and the user is told it synced** |

Aggravating factor: `ui_projects.go` reports success unconditionally. Discarding the
error doesn't just lose data — the UI lies about it. Fix the message first, then the error.

### Plan
- Fix the 11 with `if err := …; err != nil { log.Warn()…; surface to user where the user
  was promised an outcome }`.
- Guard against regression with a **scoped** check in CI, not a whole-repo one.

---

## H-7 — sqlite store tests · `[ ]`

`internal/store/sqlite` has **zero** `_test.go` across 7 source files, and it is the source
of truth for chats, skills, SSH, providers, tasks, MCP and project pointers.

### Why this is the highest-value item
Everything durable in this system funnels through `snapshotBytes()` → Telegram →
`BootstrapFromTelegram` → `sqlstore.Open`. If that round-trip is wrong, *every* feature
silently loses data on the next restart — including the thing the owner cares about most
(his own key material).

### Test surface
- **Fake `TGAPI`** — the interface is only 6 methods (`SendDocument`, `PinMessage`,
  `GetChatPinnedFile`, `DownloadFile`, `SendOrEditText`). Small enough to implement
  faithfully in-memory, which is what makes this testable at all.
- **Round-trip:** write chats + skills + SSH identity + provider + project pointer →
  `snapshotBytes()` → restore into a fresh dir → assert everything is still there.
  This is the durability contract, tested end to end.
- **`LocalDBUsable`:** accept a real DB, reject truncated/zeroed/garbage files.
- **`BootstrapFromTelegram`:** manifest path, pinned-blob path, garbage payload → returns a
  clean error and never leaves a half-written DB at `dbPath`.
- **`MigrateFromDir`:** legacy `ssh/identities.json`, chats and skills all import; a second
  run is a no-op (idempotent).
- **`repairSkillsTable`:** leaves well-formed rows untouched, drops only malformed ones.

---

## H-6b — MTProto durability hole (found while proving H-6) · `[~]`

`*tgstore.Store` is never constructed (see H-6), so:
- the four `*tgstore.Store` branches are **dead code**, and
- under the MTProto config the store is `*UserStore`, which satisfies **none** of the
  `*Cloud` interfaces (`LoadProvidersJSON`, `LoadConfigJSON`, `ListArchives`, …).

So a user who configures MTProto silently gets **no cloud backing for projects, providers,
schedules or MCP** — the oldest data in the system, unprotected, with no warning.

### Options
- **(a)** add `*UserStore` branches + implement the `Cloud` interfaces on it. Correct, but
  touches four subsystems on a path nobody exercises.
- **(b)** delete the dead branches and make `buildStore` log an explicit warning that
  cloud backing is unavailable for the MTProto backend. Minimal, honest, 20 lines.
- **(c)** make `buildStore` refuse to start on MTProto. Safest, but removes a mode that
  builds today (flagged for being too aggressive given the owner's "don't break it").

### Decision: **(b) + (c)-lite**
Take (b), and additionally **route MTProto through `local.New`** as the *base* store so it
keeps its existing durability exactly as today (no behaviour change, no risk), with the dead
branches removed and the limitation stated in one clear startup warning.

Rationale: the owner's standing instruction is "don't break it", and I cannot test MTProto
here (no `gotd` credentials, and `user_store.go` is behind the `!nogotd` build tag, so this
container may not even compile that path). Making a silent degradation **loud** is a strict
improvement with no behavioural risk. Implementing (a) blind and untested would be the
opposite. (a) remains available if MTProto is ever actually used.

---

## Verification protocol (every change, before commit)
1. `gofmt -l` clean on **changed files only** (the repo has pre-existing drift in
   `core/ui_state.go`, `exim/*`, `providers/*`, `sqlite/store.go`, `sshcreds/store_test.go` —
   do not "fix" those, it would bloat the diff).
2. `go build ./...` and `go vet ./...` clean.
3. `go test ./...` — full suite, not just the touched packages.
4. Run on the **remotebuild** runner (this container cannot compile Go in under 2 minutes).
5. Commit locally after each item. **Push once at the end**, after everything is green.
