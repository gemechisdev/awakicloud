package core

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Callback data
//
// Telegram rejects inline buttons whose callback_data exceeds 64 BYTES (not
// characters). Every button in the chats/projects UI is built with cb() so the
// limit is enforced in exactly one place. Chats are referenced by an 8-char id
// prefix ("ref"), projects by their slug (Slugify caps it at 40 bytes).
// ---------------------------------------------------------------------------

const maxCallbackBytes = 64

// cb builds "ui:<part>:<part>…" callback data and guards the 64-byte limit.
// An oversized payload degrades to a harmless no-op instead of making Telegram
// reject the entire keyboard.
func cb(parts ...string) string {
	s := "ui:" + strings.Join(parts, ":")
	if len(s) > maxCallbackBytes {
		log.Warn().Str("data", s).Msg("ui: callback data exceeds 64 bytes; degraded to noop")
		return "ui:noop"
	}
	return s
}

const chatRefLen = 8

// chatRef returns the short callback reference for a chat id.
func chatRef(id string) string {
	if len(id) > chatRefLen {
		return id[:chatRefLen]
	}
	return id
}

// ---------------------------------------------------------------------------
// Chat list scopes: all | gen (no project) | p=<slug>
// ---------------------------------------------------------------------------

type chatScope struct {
	Kind string // "all" | "gen" | "proj"
	Slug string
}

var (
	scopeAll = chatScope{Kind: "all"}
	scopeGen = chatScope{Kind: "gen"}
)

func scopeProject(slug string) chatScope { return chatScope{Kind: "proj", Slug: slug} }

func (s chatScope) token() string {
	switch s.Kind {
	case "gen":
		return "gen"
	case "proj":
		return "p=" + s.Slug
	}
	return "all"
}

func parseScope(tok string) chatScope {
	switch {
	case tok == "gen":
		return scopeGen
	case strings.HasPrefix(tok, "p=") && len(tok) > 2:
		return scopeProject(tok[2:])
	}
	return scopeAll
}

func (s chatScope) matches(c store.ChatInfo) bool {
	switch s.Kind {
	case "gen":
		return c.ProjectSlug == ""
	case "proj":
		return c.ProjectSlug == s.Slug
	}
	return true
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

// mdSafe neutralises Markdown metacharacters in user-controlled text (titles,
// project names). The Telegram formatter does not honour backslash escapes, so
// we swap look-alike characters instead of escaping.
func mdSafe(s string) string {
	r := strings.NewReplacer(
		"*", "∗", "_", "＿", "`", "'", "~", "∼",
		"[", "(", "]", ")", "<", "‹", ">", "›", "\\", "/",
	)
	return strings.TrimSpace(strings.Join(strings.Fields(r.Replace(s)), " "))
}

// code renders text as an inline code span (slugs, ids).
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }

// truncRunes truncates on rune boundaries (titles may be Amharic / Oromo).
func truncRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

// oneLine collapses whitespace/newlines so snippets fit on a single line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var nowFn = time.Now

// ago renders a compact relative time ("just now", "5m ago", "yesterday", "02 Jan").
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 0, d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	if t.Year() != now.Year() {
		return t.Format("02 Jan 2006")
	}
	return t.Format("02 Jan")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// pageBounds clamps page into [0,pages-1] and returns slice bounds.
func pageBounds(total, size, page int) (start, end, pages, clamped int) {
	if size <= 0 {
		size = 1
	}
	pages = (total + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start = page * size
	end = start + size
	if end > total {
		end = total
	}
	return start, end, pages, page
}

// ---------------------------------------------------------------------------
// Chat presentation
// ---------------------------------------------------------------------------

// chatActivity is the best-known "last activity" time of a chat.
func chatActivity(c store.ChatInfo) time.Time {
	for _, t := range []time.Time{c.UpdatedAt, c.EndedAt, c.StartedAt} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// chatVisible hides abandoned empty chats (the engine reuses an untouched
// active chat, but older installs may still have some lying around).
func chatVisible(c store.ChatInfo, currentID string) bool {
	if c.ID == currentID {
		return true
	}
	if c.Status == "active" && c.UserTurns == 0 && strings.TrimSpace(c.Title) == "" && strings.TrimSpace(c.Summary) == "" {
		return false
	}
	return true
}

func chatTitle(c store.ChatInfo, current bool) string {
	if t := strings.TrimSpace(c.Title); t != "" {
		return oneLine(t)
	}
	if current && c.UserTurns == 0 {
		return "New chat"
	}
	if !c.StartedAt.IsZero() {
		return "Untitled · " + c.StartedAt.Format("02 Jan")
	}
	return "Untitled"
}

// chatBadge is the project affiliation shown next to every chat.
func chatBadge(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "💬 General"
	}
	return "📁 " + slug
}
