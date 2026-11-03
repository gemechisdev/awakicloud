package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/store"
)

const (
	pageSizeProjectList = 6
	pageSizePicker      = 6
	pageSizeChatList    = 6
	detailRecentChats   = 4
	cloudCacheTTL       = 30 * time.Second
)

// ---------------------------------------------------------------------------
// Project catalog (local ∪ cloud, enriched with chat stats)
// ---------------------------------------------------------------------------

type projectEntry struct {
	Meta  project.Meta
	Chats int       // visible chats bound to the project
	Last  time.Time // last activity: newest chat or project update
}

func (p projectEntry) Name() string {
	if n := strings.TrimSpace(p.Meta.Name); n != "" {
		return n
	}
	return p.Meta.Slug
}

// cloudCache avoids hitting the blob store (network on Telegram storage) on
// every button tap.
type cloudCache struct {
	mu    sync.Mutex
	at    time.Time
	metas []project.Meta
}

func (e *Engine) invalidateProjects() {
	e.cloud.mu.Lock()
	e.cloud.at = time.Time{}
	e.cloud.mu.Unlock()
}

func (e *Engine) cloudMetas(ctx context.Context) []project.Meta {
	e.cloud.mu.Lock()
	defer e.cloud.mu.Unlock()
	if !e.cloud.at.IsZero() && nowFn().Sub(e.cloud.at) < cloudCacheTTL {
		return e.cloud.metas
	}
	metas, err := e.projects.ListCloud(ctx)
	if err != nil {
		// keep stale data on failure rather than hiding projects
		return e.cloud.metas
	}
	e.cloud.metas = metas
	e.cloud.at = nowFn()
	return metas
}

// visibleChats lists every chat worth showing, newest activity first.
func (e *Engine) visibleChats() ([]store.ChatInfo, string) {
	cur := e.chats.ActiveID()
	infos, err := e.store.ListChats()
	if err != nil {
		return nil, cur
	}
	out := make([]store.ChatInfo, 0, len(infos))
	for _, c := range infos {
		if chatVisible(c, cur) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return chatActivity(out[i]).After(chatActivity(out[j])) })
	return out, cur
}

func (e *Engine) projectCatalog(ctx context.Context) []projectEntry {
	if e.projects == nil {
		return nil
	}
	merged := map[string]project.Meta{}
	cloud := map[string]project.Meta{}
	for _, m := range e.cloudMetas(ctx) {
		if m.Slug != "" {
			cloud[m.Slug] = m
			merged[m.Slug] = m
		}
	}
	if local, err := e.projects.ListLocal(); err == nil {
		for _, m := range local {
			if m.Slug == "" {
				continue
			}
			if m.ID == "" { // dir without meta: prefer richer cloud meta
				if cm, ok := cloud[m.Slug]; ok {
					m = cm
				}
			}
			merged[m.Slug] = m
		}
	}

	chats, _ := e.visibleChats()
	counts := map[string]int{}
	last := map[string]time.Time{}
	for _, c := range chats {
		if c.ProjectSlug == "" {
			continue
		}
		counts[c.ProjectSlug]++
		if a := chatActivity(c); a.After(last[c.ProjectSlug]) {
			last[c.ProjectSlug] = a
		}
	}

	out := make([]projectEntry, 0, len(merged))
	for slug, m := range merged {
		pe := projectEntry{Meta: m, Chats: counts[slug], Last: m.UpdatedAt}
		if l := last[slug]; l.After(pe.Last) {
			pe.Last = l
		}
		out = append(out, pe)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return strings.ToLower(out[i].Name()) < strings.ToLower(out[j].Name())
	})
	return out
}

// findProject resolves user input (slug, name, or unique prefix) to a project.
func (e *Engine) findProject(ctx context.Context, q string) (projectEntry, bool) {
	q = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(q), "📁"))
	if q == "" {
		return projectEntry{}, false
	}
	cat := e.projectCatalog(ctx)
	lq := strings.ToLower(q)
	sq := project.Slugify(q)
	for _, p := range cat {
		if strings.ToLower(p.Meta.Slug) == lq || p.Meta.Slug == sq {
			return p, true
		}
	}
	for _, p := range cat {
		if strings.EqualFold(p.Meta.Name, q) {
			return p, true
		}
	}
	var hits []projectEntry
	for _, p := range cat {
		if strings.HasPrefix(strings.ToLower(p.Meta.Slug), lq) || strings.HasPrefix(strings.ToLower(p.Meta.Name), lq) {
			hits = append(hits, p)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return projectEntry{}, false
}

func projectStats(m project.Meta) string {
	if m.FileCount == 0 && m.TotalSize == 0 {
		return "empty"
	}
	return plural(m.FileCount, "file", "files") + " · " + humanBytes(m.TotalSize)
}

// ---------------------------------------------------------------------------
// /projects — list
// ---------------------------------------------------------------------------

func (e *Engine) uiProjects(ctx context.Context, msg channel.Message, page int) {
	e.ui.clear()
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects are not available.")
		return
	}
	cat := e.projectCatalog(ctx)
	if len(cat) == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "📁 **Projects**\n\nNo projects yet.\n\nA project is a durable workspace — files persist, sync to cloud storage, and can hold many chats.",
			Rows: [][]channel.Button{
				{{Text: "＋ New project", CallbackData: cb("projnew")}},
				{{Text: "🏠 Home", CallbackData: cb("home")}},
			},
		})
		return
	}

	start, end, pages, page := pageBounds(len(cat), pageSizeProjectList, page)
	curSlug := e.currentProjectSlug()
	now := nowFn()

	var b strings.Builder
	fmt.Fprintf(&b, "📁 **Projects** · %d", len(cat))
	if pages > 1 {
		fmt.Fprintf(&b, " · page %d/%d", page+1, pages)
	}
	b.WriteString("\n")
	var rows [][]channel.Button
	for i, p := range cat[start:end] {
		mark := ""
		if p.Meta.Slug == curSlug {
			mark = " ●"
		}
		fmt.Fprintf(&b, "\n%d. **%s**%s %s\n   %s · %s · %s", start+i+1, mdSafe(p.Name()), mark, code(p.Meta.Slug),
			projectStats(p.Meta), plural(p.Chats, "chat", "chats"), ago(p.Last, now))
		rows = append(rows, []channel.Button{{
			Text:         truncRunes(fmt.Sprintf("📂 %d. %s · %d", start+i+1, p.Name(), p.Chats), 40),
			CallbackData: cb("pd", p.Meta.Slug),
		}})
	}
	if nav := pageNav(page, pages, func(p int) string { return cb("projects", fmt.Sprint(p)) }); nav != nil {
		rows = append(rows, nav)
	}
	rows = append(rows,
		[]channel.Button{{Text: "＋ New project", CallbackData: cb("projnew")}, {Text: "💬 New chat", CallbackData: cb("new")}},
		[]channel.Button{{Text: "📜 All chats", CallbackData: cb("chats")}, {Text: "🏠 Home", CallbackData: cb("home")}},
	)
	b.WriteString("\n\n● = project of the current chat. Tap a project to see its chats.")
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

// pageNav renders ◀ n/m ▶ (nil when there is a single page).
func pageNav(page, pages int, data func(int) string) []channel.Button {
	if pages <= 1 {
		return nil
	}
	var nav []channel.Button
	if page > 0 {
		nav = append(nav, channel.Button{Text: "◀ Prev", CallbackData: data(page - 1)})
	}
	nav = append(nav, channel.Button{Text: fmt.Sprintf("%d/%d", page+1, pages), CallbackData: cb("noop")})
	if page < pages-1 {
		nav = append(nav, channel.Button{Text: "Next ▶", CallbackData: data(page + 1)})
	}
	return nav
}

// currentProjectSlug is the project bound to the active chat ("" = none).
func (e *Engine) currentProjectSlug() string {
	id := e.chats.ActiveID()
	if id == "" {
		return ""
	}
	ch, err := e.chats.Get(id)
	if err != nil || ch.Mode != project.ModeProject {
		return ""
	}
	return ch.ProjectSlug
}

// ---------------------------------------------------------------------------
// Project detail
// ---------------------------------------------------------------------------

func (e *Engine) uiProjectDetail(ctx context.Context, msg channel.Message, slug, banner string) {
	e.ui.clear()
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects are not available.")
		return
	}
	var entry projectEntry
	found := false
	for _, p := range e.projectCatalog(ctx) {
		if p.Meta.Slug == slug {
			entry, found = p, true
			break
		}
	}
	if !found {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Project " + code(slug) + " was not found.",
			Rows: [][]channel.Button{{{Text: "⬅ Projects", CallbackData: cb("projects", "0")}, {Text: "🏠 Home", CallbackData: cb("home")}}},
		})
		return
	}

	now := nowFn()
	chats, cur := e.visibleChats()
	var mine []store.ChatInfo
	for _, c := range chats {
		if c.ProjectSlug == slug {
			mine = append(mine, c)
		}
	}

	var b strings.Builder
	if banner != "" {
		b.WriteString(banner + "\n\n")
	}
	fmt.Fprintf(&b, "📁 **%s** %s\n", mdSafe(entry.Name()), code(slug))
	line := projectStats(entry.Meta)
	if !entry.Meta.UpdatedAt.IsZero() {
		line += " · synced " + ago(entry.Meta.UpdatedAt, now)
	}
	b.WriteString(line + "\n")
	if e.currentProjectSlug() == slug {
		b.WriteString("● Active workspace of the current chat\n")
	}
	if le := entry.Meta.LastEditor; le != "" {
		for _, c := range chats {
			if c.ID == le {
				b.WriteString("Last edited from: " + mdSafe(truncRunes(chatTitle(c, c.ID == cur), 40)) + "\n")
				break
			}
		}
	}

	var rows [][]channel.Button
	rows = append(rows, []channel.Button{{Text: "💬 New chat here", CallbackData: cb("nc", slug)}})

	if len(mine) == 0 {
		b.WriteString("\nNo chats in this project yet — start one with **New chat here**.")
	} else {
		fmt.Fprintf(&b, "\n**%s**", plural(len(mine), "chat", "chats"))
		shown := mine
		if len(shown) > detailRecentChats {
			shown = shown[:detailRecentChats]
		}
		lines, chatRows := chatListBlock(shown, 0, cur, now, false)
		b.WriteString("\n" + lines)
		rows = append(rows, chatRows...)
		if len(mine) > len(shown) {
			fmt.Fprintf(&b, "\n…and %d more", len(mine)-len(shown))
		}
	}

	second := []channel.Button{{Text: fmt.Sprintf("📜 All chats (%d)", len(mine)), CallbackData: cb("chats", scopeProject(slug).token(), "0")}}
	if _, err := e.projects.ReadMeta(slug); err == nil {
		second = append(second, channel.Button{Text: "☁ Sync now", CallbackData: cb("psync", slug)})
	}
	rows = append(rows, second)
	rows = append(rows, []channel.Button{
		{Text: "📌 Use for current chat", CallbackData: cb("pmv", slug)},
		{Text: "✏ Rename", CallbackData: cb("pren", slug)},
	})
	rows = append(rows, []channel.Button{
		{Text: "⬅ Projects", CallbackData: cb("projects", "0")},
		{Text: "🏠 Home", CallbackData: cb("home")},
	})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) uiProjectSync(ctx context.Context, msg channel.Message, slug string) {
	if e.projects == nil {
		return
	}
	meta, err := e.projects.Sync(ctx, slug, e.chats.ActiveID())
	e.invalidateProjects()
	if err != nil {
		e.uiProjectDetail(ctx, msg, slug, "⚠️ Sync failed: "+mdSafe(err.Error()))
		return
	}
	e.uiProjectDetail(ctx, msg, slug, fmt.Sprintf("☁ Synced · %s", projectStats(meta)))
}

// ---------------------------------------------------------------------------
// Create / rename project (wizards)
// ---------------------------------------------------------------------------

// startNewProjectWizard asks for a name. then = what to do afterwards:
// "view" (project card), "chat" (start a chat in it), "move" (move chat ref into it).
func (e *Engine) startNewProjectWizard(ctx context.Context, msg channel.Message, then, ref string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects are not available.")
		return
	}
	e.ui.set(&uiWizard{Kind: "new_project", Step: "name", Draft: map[string]string{"then": then, "ref": ref}})
	back := cb("projects", "0")
	switch then {
	case "chat":
		back = cb("new")
	case "move":
		back = cb("cmv", ref, "0")
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "📁 **New project**\n\nSend a name (e.g. `Telebirr Mini App`).",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: back}}},
	})
}

func (e *Engine) createProject(ctx context.Context, msg channel.Message, name, then, ref string) {
	name = strings.TrimSpace(strings.Join(strings.Fields(name), " "))
	if name == "" || len([]rune(name)) > 60 {
		_ = msg.Reply.SendText(ctx, "Please send a name between 1 and 60 characters.")
		e.ui.set(&uiWizard{Kind: "new_project", Step: "name", Draft: map[string]string{"then": then, "ref": ref}})
		return
	}
	if e.refuseIfBusy(ctx, msg) {
		return
	}
	e.navMu.Lock()
	defer e.navMu.Unlock()

	meta, err := e.projects.CreateCtx(ctx, name, "")
	if err != nil {
		e.rebindActiveWorkspace(ctx)
		_ = msg.Reply.SendText(ctx, "Create failed: "+err.Error())
		return
	}
	// Create() points the global workspace at the new project; restore the
	// active chat's workspace unless a follow-up action rebinds explicitly.
	e.rebindActiveWorkspace(ctx)
	_, _ = e.projects.Sync(ctx, meta.Slug)
	e.invalidateProjects()

	switch then {
	case "chat":
		e.startChatLocked(ctx, msg, meta.Slug)
	case "move":
		e.moveChatLocked(ctx, msg, ref, meta.Slug)
	default:
		e.uiProjectDetail(ctx, msg, meta.Slug, "✅ Project created")
	}
}

func (e *Engine) startRenameProjectWizard(ctx context.Context, msg channel.Message, slug string) {
	e.ui.set(&uiWizard{Kind: "rename_project", Step: "name", Draft: map[string]string{"slug": slug}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "✏ **Rename project** " + code(slug) + "\n\nSend the new display name. The slug and files stay the same, so chats keep working.",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: cb("pd", slug)}}},
	})
}

func (e *Engine) renameProject(ctx context.Context, msg channel.Message, slug, name string) {
	meta, err := e.projects.Rename(ctx, slug, name)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Rename failed: "+err.Error())
		return
	}
	e.invalidateProjects()
	e.uiProjectDetail(ctx, msg, slug, "✅ Renamed to **"+mdSafe(meta.Name)+"**")
}
