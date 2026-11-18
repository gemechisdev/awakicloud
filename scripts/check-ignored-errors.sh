#!/usr/bin/env bash
# Guard: durable state writes must not discard their error.
#
# Background: the SSH identity store silently swallowed DB write failures
# (`_ = s.db.Upsert...`), so the UI showed "✅ Saved" while the identity was
# lost on the next restart. The same pattern appeared in the project and MCP
# paths. This check exists so it cannot come back.
#
# Scope is deliberately narrow — only calls that write durable state and whose
# failure means real, user-visible data loss. Benign discards (`Close()`,
# `os.Remove`, cache hydration from an upstream source of truth) are NOT
# flagged, because flagging them would train everyone to ignore this check.
#
# A whole-repo `errcheck` run was considered and rejected: it reports hundreds of
# legitimate cleanup discards, which is noise, not signal.
set -uo pipefail

# Files to inspect (non-test only).
roots=(
  internal/core
  internal/mcp
  internal/tools/system
  internal/sshcreds
  internal/skills
)

# Patterns that must never discard their error.
# `\.Save\(\)`    — chat / config persistence
# `\.Upsert\(`    — DB row writes
# `\.Sync\(`      — project archive uploads
# `\.WriteMemory\(|\.WriteSoul\(|\.WriteUser\(` — durable-ish identity/memory writes
pattern='_ = [A-Za-z_][A-Za-z0-9_.]*\.(Save\(\)|Upsert\(|Upsert[A-Za-z]*\(|Sync\(|WriteMemory\(|WriteSoul\(|WriteUser\()'

files=()
for r in "${roots[@]}"; do
  [ -d "$r" ] || continue
  while IFS= read -r f; do
    case "$f" in
      *_test.go) continue ;;
    esac
    files+=("$f")
  done < <(find "$r" -name '*.go' -type f)
done

if [ ${#files[@]} -eq 0 ]; then
  echo "check-ignored-errors: no files found (wrong cwd?)" >&2
  exit 1
fi

hits=$(grep -nE "$pattern" "${files[@]}" 2>/dev/null || true)

# Allow an explicit opt-out for a genuinely non-fatal site, with a reason.
hits=$(printf '%s\n' "$hits" | grep -v 'ignored-error-ok:' || true)

if [ -n "$(printf '%s' "$hits" | tr -d '[:space:]')" ]; then
  echo "::error::a durable state write discards its error (data loss on failure)"
  echo
  echo "These calls persist user-visible state. Handle the error — log it, and tell"
  echo "the user when they were promised an outcome. If the site is genuinely"
  echo "non-fatal, annotate the line with a trailing comment containing"
  echo "'ignored-error-ok: <reason>'."
  echo
  printf '%s\n' "$hits"
  exit 1
fi

echo "✅ no unhandled durable state writes"
