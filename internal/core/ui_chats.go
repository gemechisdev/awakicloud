package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/core/chat"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/store"
)

// ---------------------------------------------------------------------------
// Guards & small helpers
// ---------------------------------------------------------------------------

// refuseIfBusy blocks anything that would swap the active chat / workspace
// underneath a running agent.
func (e *Engine) refuseIfBusy(ctx context.Context, msg channel.Message) bool {
	if e.Busy() {
		_ = msg.Reply.SendText(ctx, "⏳ The agent is still working in this chat. Send /stop first (or wait for it to finish), then try again.")
		return true
	}
	return false
}

// rebindActiveWorkspace re-points the global workspace at the active chat's
// own workspace (scratch or project).
func (e *Engine) rebindActiveWorkspace(ctx context.Context) {
	id := e.chats.ActiveID()
	if id == "" {
		return
	}
	if ch, err := e.chats.Get(id); err == nil {
		e.bindWorkspaceForChat(ctx, ch.ID, ch.Mode, ch.ProjectSlug)
	}
}

// resolveChatRef maps a callback ref (8-char prefix) or full id to a chat.
func (e *Engine) resolveChatRef(ref string) (store.ChatInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.ChatInfo{}, chat.ErrNotFound
	}
	infos, err := e.store.ListChats()
	if err != nil {
		return store.ChatInfo{}, err
	}
	var hit []store.ChatInfo
	for _, c := range infos {
		if c.ID == ref {
			return c, nil
		}
		if strings.HasPrefix(c.ID, ref) {
			hit = append(hit, c)
		}
	}
	switch len(hit) {
	case 0:
		return store.ChatInfo{}, chat.ErrNotFound
	case 1:
		return hit[0], nil
	}
	return store.ChatInfo{}, fmt.Errorf("ambiguous chat reference %q", ref)
}

// chatListBlock renders numbered chat lines plus [▶ open][⋯ actions] button rows.
func chatListBlock(list []store.ChatInfo, offset int, curID string, now time.Time, showBadge bool) (string, [][]channel.Button) {
	var b strings.Builder
	var rows [][]channel.Button
	for i, c := range list {
		isCur := c.ID == curID
		title := chatTitle(c, isCur)
		mark := ""
		if isCur {
			mark = "● "
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s**%s**\n   ", offset+i+1, mark, mdSafe(truncRunes(title, 60)))
		meta := []string{}
		if showBadge {
			meta = append(meta, mdSafeBadge(c.ProjectSlug))
		}
		if c.UserTurns > 0 {
			meta = append(meta, plural(c.UserTurns, "msg", "msgs"))
		}
		meta = append(meta, ago(chatActivity(c), now))
		b.WriteString(strings.Join(meta, " · "))

		icon := "▶"
		if isCur {
			icon = "●"
		}
		label := fmt.Sprintf("%s %d. %s", icon, offset+i+1, truncRunes(title, 26))
		rows = append(rows, []channel.Button{
			{Text: label, CallbackData: cb("co", chatRef(c.ID))},
			{Text: "⋯", CallbackData: cb("cm", chatRef(c.ID))},
		})
	}
	return b.String(), rows
}

func mdSafeBadge(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "💬 General"
	}
	return "📁 " + code(slug)
}

// ---------------------------------------------------------------------------
// New chat
// ---------------------------------------------------------------------------

// cmdNewChat handles /new, /new <project>, /new - and the "New Chat" buttons.
func (e *Engine) cmdNewChat(ctx context.Context, msg channel.Message, raw string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		e.uiNewChatPicker(ctx, msg, 0)
		return
	case "-", "none", "general", "scratch", "0":
		e.startChat(ctx, msg, "")
		return
	}
	if e.projects != nil {
		if p, ok := e.findProject(ctx, raw); ok {
			e.startChat(ctx, msg, p.Meta.Slug)
			return
		}
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("No project matches “%s”. Pick one below.", raw))
	e.uiNewChatPicker(ctx, msg, 0)
}

// uiNewChatPicker asks where the new chat should live: no project or one of
// the projects. With no projects at all there is nothing to choose, so a
// general chat starts immediately.
func (e *Engine) uiNewChatPicker(ctx context.Context, msg channel.Message, page int) {
	e.ui.clear()
	cat := e.projectCatalog(ctx)
	if len(cat) == 0 {
		e.startChat(ctx, msg, "")
		return
	}
	start, end, pages, page := pageBounds(len(cat), pageSizePicker, page)
	curSlug := e.currentProjectSlug()

	var b strings.Builder
	b.WriteString("✨ **New chat**\n\nWhere should it live?\n\n")
	b.WriteString("• 💬 **General** — no project, disposable scratch workspace\n")
	b.WriteString("• 📁 **Project** — shared files that persist and sync; the agent works inside it")
	if pages > 1 {
		fmt.Fprintf(&b, "\n\nProjects · page %d/%d", page+1, pages)
	}

	rows := [][]channel.Button{{{Text: "💬 General (no project)", CallbackData: cb("nc", "-")}}}
	for _, p := range cat[start:end] {
		label := "📁 " + p.Name()
		if p.Meta.Slug == curSlug {
			label = "★ " + p.Name()
		}
		label = truncRunes(label, 30) + " · " + plural(p.Chats, "chat", "chats")
		rows = append(rows, []channel.Button{{Text: label, CallbackData: cb("nc", p.Meta.Slug)}})
	}
	if nav := pageNav(page, pages, func(p int) string { return cb("newp", fmt.Sprint(p)) }); nav != nil {
		rows = append(rows, nav)
	}
	rows = append(rows,
		[]channel.Button{{Text: "＋ New project & start", CallbackData: cb("ncp")}},
		[]channel.Button{{Text: "✖ Cancel", CallbackData: cb("home")}},
	)
	if curSlug != "" {
		b.WriteString("\n\n★ = project of the current chat")
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) startChat(ctx context.Context, msg channel.Message, slug string) {
	if e.refuseIfBusy(ctx, msg) {
		return
	}
	e.navMu.Lock()
	defer e.navMu.Unlock()
	e.startChatLocked(ctx, msg, slug)
}

// startChatLocked requires navMu. slug "" = general chat.
func (e *Engine) startChatLocked(ctx context.Context, msg channel.Message, slug string) {
	e.ui.clear()
	var bind chat.Project
	var entry projectEntry
	if slug != "" {
		if e.projects == nil {
			_ = msg.Reply.SendText(ctx, "Projects are not available.")
			return
		}
		p, ok := e.findProject(ctx, slug)
		if !ok {
			_ = msg.Reply.SendText(ctx, fmt.Sprintf("Project %s was not found.", code(slug)))
			return
		}
		entry = p
		// Restore from cloud if needed so the workspace exists before we bind.
		if err := e.projects.ActivateProject(ctx, p.Meta.Slug); err != nil {
			_ = msg.Reply.SendText(ctx, "Could not open project: "+err.Error())
			e.rebindActiveWorkspace(ctx)
			return
		}
		if m, err := e.projects.ReadMeta(p.Meta.Slug); err == nil {
			entry.Meta = m
		}
		bind = chat.Project{ID: entry.Meta.ID, Slug: entry.Meta.Slug, Hash: entry.Meta.Hash}
	}

	e.syncActiveProjectIfDirty(ctx) // flush the project we are leaving
	ch, _, err := e.chats.Start(ctx, bind)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Could not start a new chat: "+err.Error())
		e.rebindActiveWorkspace(ctx)
		return
	}
	e.bindWorkspaceForChat(ctx, ch.ID, ch.Mode, ch.ProjectSlug)
	e.invalidateProjects()

	var b strings.Builder
	var rows [][]channel.Button
	if slug == "" {
		b.WriteString("✨ **New chat** · 💬 General\n\nNo project — a disposable scratch workspace. Just send a message to begin.")
		rows = append(rows, []channel.Button{{Text: "📁 Use a project…", CallbackData: cb("cmv", chatRef(ch.ID), "0")}})
	} else {
		fmt.Fprintf(&b, "✨ **New chat** · 📁 **%s** %s\n%s\n\nFiles persist and sync. Just send a message to begin.",
			mdSafe(entry.Name()), code(entry.Meta.Slug), projectStats(entry.Meta))
		rows = append(rows, []channel.Button{
			{Text: "🔀 Change project", CallbackData: cb("cmv", chatRef(ch.ID), "0")},
			{Text: "📂 Project", CallbackData: cb("pd", entry.Meta.Slug)},
		})
	}
	rows = append(rows, []channel.Button{{Text: "📜 Chats", CallbackData: cb("chats")}, {Text: "🏠 Home", CallbackData: cb("home")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

// ---------------------------------------------------------------------------
// Chats hub
// ---------------------------------------------------------------------------

func (e *Engine) scopeLabel(ctx context.Context, s chatScope) string {
	switch s.Kind {
	case "gen":
		return "💬 General"
	case "proj":
		if e.projects != nil {
			if p, ok := e.findProject(ctx, s.Slug); ok {
				return "📁 " + mdSafe(p.Name())
			}
		}
		return "📁 " + code(s.Slug)
	}
	return "All"
}

func (e *Engine) uiChats(ctx context.Context, msg channel.Message, scope chatScope, page int) {
	e.ui.clear()
	all, cur := e.visibleChats()
	var list []store.ChatInfo
	for _, c := range all {
		if scope.matches(c) {
			list = append(list, c)
		}
	}
	now := nowFn()
	start, end, pages, page := pageBounds(len(list), pageSizeChatList, page)

	var b strings.Builder
	fmt.Fprintf(&b, "📜 **Chats** · %s · %d", e.scopeLabel(ctx, scope), len(list))
	if pages > 1 {
		fmt.Fprintf(&b, " · page %d/%d", page+1, pages)
	}
	b.WriteString("\n\n")

	var rows [][]channel.Button
	rows = append(rows, e.scopeChips(scope))
	if len(list) == 0 {
		switch scope.Kind {
		case "proj":
			b.WriteString("No chats in this project yet.")
		case "gen":
			b.WriteString("No general chats yet.")
		default:
			b.WriteString("No conversations yet.")
		}
	} else {
		lines, chatRows := chatListBlock(list[start:end], start, cur, now, scope.Kind != "proj")
		b.WriteString(lines)
		rows = append(rows, chatRows...)
		b.WriteString("\n\n▶ opens a chat · ⋯ rename / move / details")
	}
	if nav := pageNav(page, pages, func(p int) string { return cb("chats", scope.token(), fmt.Sprint(p)) }); nav != nil {
		rows = append(rows, nav)
	}
	if scope.Kind == "proj" {
		rows = append(rows, []channel.Button{
			{Text: "💬 New chat here", CallbackData: cb("nc", scope.Slug)},
			{Text: "📂 Project", CallbackData: cb("pd", scope.Slug)},
		})
	} else {
		rows = append(rows, []channel.Button{{Text: "💬 New chat", CallbackData: cb("new")}, {Text: "📁 Projects", CallbackData: cb("projects", "0")}})
	}
	rows = append(rows, []channel.Button{{Text: "🏠 Home", CallbackData: cb("home")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) scopeChips(s chatScope) []channel.Button {
	chip := func(label string, on bool, data string) channel.Button {
		if on {
			label = "• " + label
		}
		return channel.Button{Text: label, CallbackData: data}
	}
	third := channel.Button{Text: "📁 By project ▾", CallbackData: cb("cfp", "0")}
	if s.Kind == "proj" {
		third = channel.Button{Text: "• 📁 " + truncRunes(s.Slug, 16), CallbackData: cb("cfp", "0")}
	}
	return []channel.Button{
		chip("All", s.Kind == "all", cb("chats", "all", "0")),
		chip("General", s.Kind == "gen", cb("chats", "gen", "0")),
		third,
	}
}

// uiChatFilterProjects lets the user pick a project to filter the hub by.
func (e *Engine) uiChatFilterProjects(ctx context.Context, msg channel.Message, page int) {
	cat := e.projectCatalog(ctx)
	if len(cat) == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "No projects yet.",
			Rows: [][]channel.Button{
				{{Text: "＋ New project", CallbackData: cb("projnew")}},
				{{Text: "⬅ Chats", CallbackData: cb("chats")}},
			},
		})
		return
	}
	start, end, pages, page := pageBounds(len(cat), pageSizePicker, page)
	var rows [][]channel.Button
	for _, p := range cat[start:end] {
		rows = append(rows, []channel.Button{{
			Text:         truncRunes("📁 "+p.Name(), 30) + " · " + plural(p.Chats, "chat", "chats"),
			CallbackData: cb("chats", scopeProject(p.Meta.Slug).token(), "0"),
		}})
	}
	if nav := pageNav(page, pages, func(p int) string { return cb("cfp", fmt.Sprint(p)) }); nav != nil {
		rows = append(rows, nav)
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Chats", CallbackData: cb("chats")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: "📁 **Filter chats by project**", Rows: rows})
}

// cmdChats handles /chats, /history, /chats <project>, /chats <search words>.
func (e *Engine) cmdChats(ctx context.Context, msg channel.Message, raw string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "", "all":
		e.uiChats(ctx, msg, scopeAll, 0)
		return
	case "-", "none", "general", "scratch":
		e.uiChats(ctx, msg, scopeGen, 0)
		return
	}
	if e.projects != nil {
		if p, ok := e.findProject(ctx, raw); ok {
			e.uiChats(ctx, msg, scopeProject(p.Meta.Slug), 0)
			return
		}
	}
	e.uiChatSearch(ctx, msg, raw)
}

func (e *Engine) uiChatSearch(ctx context.Context, msg channel.Message, q string) {
	e.ui.clear()
	words := strings.Fields(strings.ToLower(q))
	all, cur := e.visibleChats()
	var hits []store.ChatInfo
	for _, c := range all {
		hay := strings.ToLower(c.Title + " " + c.Summary + " " + strings.Join(c.Tags, " ") + " " + c.ProjectSlug)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, c)
		}
	}
	rows := [][]channel.Button{}
	var b strings.Builder
	if len(hits) == 0 {
		fmt.Fprintf(&b, "🔎 No chats match “%s”.", mdSafe(q))
	} else {
		const maxHits = 8
		more := len(hits) - maxHits
		if more > 0 {
			hits = hits[:maxHits]
		}
		fmt.Fprintf(&b, "🔎 **Chats matching** “%s”\n\n", mdSafe(q))
		lines, chatRows := chatListBlock(hits, 0, cur, nowFn(), true)
		b.WriteString(lines)
		rows = append(rows, chatRows...)
		if more > 0 {
			fmt.Fprintf(&b, "\n\n…and %d more — refine your search.", more)
		}
	}
	rows = append(rows, []channel.Button{{Text: "📜 All chats", CallbackData: cb("chats")}, {Text: "🏠 Home", CallbackData: cb("home")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

// ---------------------------------------------------------------------------
// Open / resume a chat
// ---------------------------------------------------------------------------

func (e *Engine) openChat(ctx context.Context, msg channel.Message, ref string) {
	info, err := e.resolveChatRef(ref)
	if err != nil {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "That chat is no longer available.",
			Rows: [][]channel.Button{{{Text: "📜 Chats", CallbackData: cb("chats")}}},
		})
		return
	}
	if info.ID == e.chats.ActiveID() {
		e.uiChatCard(ctx, msg, info.ID, "● This is already your current chat.")
		return
	}
	if e.refuseIfBusy(ctx, msg) {
		return
	}
	e.navMu.Lock()
	defer e.navMu.Unlock()
	e.ui.clear()

	e.syncActiveProjectIfDirty(ctx) // flush the project we are leaving
	ch, err := e.chats.Switch(ctx, info.ID)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Could not open chat: "+err.Error())
		return
	}
	e.bindWorkspaceForChat(ctx, ch.ID, ch.Mode, ch.ProjectSlug)

	var b strings.Builder
	fmt.Fprintf(&b, "▶️ **Resumed** · %s\n", mdSafe(truncRunes(chatTitle(chatInfoOf(ch), true), 70)))
	b.WriteString(mdSafeBadge(chatSlug(ch)))
	if ch.UserTurns > 0 {
		b.WriteString(" · " + plural(ch.UserTurns, "msg", "msgs"))
	}
	b.WriteString(" · " + ago(chatActivity(chatInfoOf(ch)), nowFn()) + "\n")
	if recap := recapOf(ch); recap != "" {
		b.WriteString("\n" + recap + "\n")
	}
	b.WriteString("\nContinue where you left off — just send a message.")

	rows := [][]channel.Button{}
	if slug := chatSlug(ch); slug != "" {
		rows = append(rows, []channel.Button{{Text: "💬 New chat here", CallbackData: cb("nc", slug)}, {Text: "⋯ Actions", CallbackData: cb("cm", chatRef(ch.ID))}})
	} else {
		rows = append(rows, []channel.Button{{Text: "💬 New chat", CallbackData: cb("new")}, {Text: "⋯ Actions", CallbackData: cb("cm", chatRef(ch.ID))}})
	}
	rows = append(rows, []channel.Button{{Text: "📜 Chats", CallbackData: cb("chats")}, {Text: "🏠 Home", CallbackData: cb("home")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func chatSlug(ch *store.Chat) string {
	if ch.Mode == project.ModeProject {
		return ch.ProjectSlug
	}
	return ""
}

func chatInfoOf(ch *store.Chat) store.ChatInfo {
	return store.ChatInfo{
		ID: ch.ID, Title: ch.Title, Summary: ch.Summary, Tags: ch.Tags, Mode: ch.Mode,
		ProjectSlug: chatSlug(ch), Status: ch.Status, StartedAt: ch.StartedAt, EndedAt: ch.EndedAt,
		UpdatedAt: ch.UpdatedAt, UserTurns: ch.UserTurns,
	}
}

// recapOf shows the last user/assistant exchange so a resumed chat has context.
func recapOf(ch *store.Chat) string {
	msgs, err := ch.TextMessages()
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var lastUser, lastAsst string
	for i := len(msgs) - 1; i >= 0 && (lastUser == "" || lastAsst == ""); i-- {
		switch msgs[i].Role {
		case "user":
			if lastUser == "" {
				lastUser = msgs[i].Content
			}
		case "assistant":
			if lastAsst == "" {
				lastAsst = msgs[i].Content
			}
		}
	}
	var parts []string
	if lastUser != "" {
		parts = append(parts, "**You:** "+mdSafe(truncRunes(oneLine(lastUser), 160)))
	}
	if lastAsst != "" {
		parts = append(parts, "**Assistant:** "+mdSafe(truncRunes(oneLine(lastAsst), 220)))
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// Chat card (details + actions); /chat shows it for the current chat
// ---------------------------------------------------------------------------

func (e *Engine) uiCurrentChat(ctx context.Context, msg channel.Message) {
	id := e.chats.ActiveID()
	if id == "" {
		ch, err := e.chats.Resolve()
		if err != nil {
			_ = msg.Reply.SendText(ctx, err.Error())
			return
		}
		id = ch.ID
	}
	e.uiChatCard(ctx, msg, id, "")
}

func (e *Engine) uiChatCard(ctx context.Context, msg channel.Message, fullID, banner string) {
	e.ui.clear()
	ch, err := e.chats.Get(fullID)
	if err != nil {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "That chat is no longer available.",
			Rows: [][]channel.Button{{{Text: "📜 Chats", CallbackData: cb("chats")}}},
		})
		return
	}
	isCur := ch.ID == e.chats.ActiveID()
	info := chatInfoOf(ch)
	slug := chatSlug(ch)

	var b strings.Builder
	if banner != "" {
		b.WriteString(banner + "\n\n")
	}
	fmt.Fprintf(&b, "💬 **%s**\n", mdSafe(truncRunes(chatTitle(info, isCur), 80)))
	b.WriteString(mdSafeBadge(slug))
	if ch.UserTurns > 0 {
		b.WriteString(" · " + plural(ch.UserTurns, "msg", "msgs"))
	}
	b.WriteString(" · " + ago(chatActivity(info), nowFn()) + "\n")
	if isCur {
		b.WriteString("● Current chat")
		if e.permStore != nil {
			m := e.permStore.Get(ch.ID)
			fmt.Fprintf(&b, " · %s %s mode", m.Emoji(), m.Title())
		}
		b.WriteString("\n")
	}
	if s := strings.TrimSpace(ch.Summary); s != "" {
		b.WriteString("\n" + mdSafe(truncRunes(oneLine(s), 300)) + "\n")
	}
	if len(ch.Tags) > 0 {
		b.WriteString("\nTags: " + mdSafe(strings.Join(ch.Tags, ", ")) + "\n")
	}

	ref := chatRef(ch.ID)
	var rows [][]channel.Button
	if isCur {
		rows = append(rows, []channel.Button{{Text: "✏ Rename", CallbackData: cb("cren", ref)}, {Text: "🛡 Mode", CallbackData: cb("mode")}})
	} else {
		rows = append(rows, []channel.Button{{Text: "▶ Resume", CallbackData: cb("co", ref)}, {Text: "✏ Rename", CallbackData: cb("cren", ref)}})
	}
	mv := []channel.Button{{Text: "🔀 Move to project…", CallbackData: cb("cmv", ref, "0")}}
	if slug != "" {
		mv = append(mv, channel.Button{Text: "🚫 Remove from project", CallbackData: cb("cmt", ref, "-")})
	}
	rows = append(rows, mv)
	if slug != "" {
		rows = append(rows, []channel.Button{
			{Text: "💬 New chat in " + truncRunes(slug, 18), CallbackData: cb("nc", slug)},
			{Text: "📂 Project", CallbackData: cb("pd", slug)},
		})
	} else {
		rows = append(rows, []channel.Button{{Text: "💬 New chat", CallbackData: cb("new")}})
	}
	back := scopeAll
	if slug != "" {
		back = scopeProject(slug)
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Chats", CallbackData: cb("chats", back.token(), "0")}, {Text: "🏠 Home", CallbackData: cb("home")}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

// ---------------------------------------------------------------------------
// Move chat ↔ project
// ---------------------------------------------------------------------------

func (e *Engine) uiMovePicker(ctx context.Context, msg channel.Message, ref string, page int) {
	e.ui.clear()
	info, err := e.resolveChatRef(ref)
	if err != nil {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "That chat is no longer available.",
			Rows: [][]channel.Button{{{Text: "📜 Chats", CallbackData: cb("chats")}}},
		})
		return
	}
	cat := e.projectCatalog(ctx)
	start, end, pages, page := pageBounds(len(cat), pageSizePicker, page)
	isCur := info.ID == e.chats.ActiveID()

	var b strings.Builder
	fmt.Fprintf(&b, "🔀 **Move chat** · %s\n", mdSafe(truncRunes(chatTitle(info, isCur), 60)))
	fmt.Fprintf(&b, "Now in: %s\n\n", mdSafeBadge(info.ProjectSlug))
	b.WriteString("Pick a project. The history stays with the chat; the agent works in the new project's files from the next message.")
	if pages > 1 {
		fmt.Fprintf(&b, "\n\nProjects · page %d/%d", page+1, pages)
	}

	var rows [][]channel.Button
	if info.ProjectSlug != "" {
		rows = append(rows, []channel.Button{{Text: "🚫 No project (general)", CallbackData: cb("cmt", ref, "-")}})
	}
	for _, p := range cat[start:end] {
		label := "📁 " + p.Name()
		if p.Meta.Slug == info.ProjectSlug {
			label = "✓ " + p.Name()
		}
		rows = append(rows, []channel.Button{{
			Text:         truncRunes(label, 30) + " · " + plural(p.Chats, "chat", "chats"),
			CallbackData: cb("cmt", ref, p.Meta.Slug),
		}})
	}
	if nav := pageNav(page, pages, func(p int) string { return cb("cmv", ref, fmt.Sprint(p)) }); nav != nil {
		rows = append(rows, nav)
	}
	rows = append(rows,
		[]channel.Button{{Text: "＋ New project & move here", CallbackData: cb("pnm", ref)}},
		[]channel.Button{{Text: "⬅ Back", CallbackData: cb("cm", ref)}},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

// cmdMove handles /move and /chatlink for the CURRENT chat.
func (e *Engine) cmdMove(ctx context.Context, msg channel.Message, raw string) {
	raw = strings.TrimSpace(raw)
	id := e.chats.ActiveID()
	if id == "" {
		ch, err := e.chats.Resolve()
		if err != nil {
			_ = msg.Reply.SendText(ctx, err.Error())
			return
		}
		id = ch.ID
	}
	switch strings.ToLower(raw) {
	case "":
		e.uiMovePicker(ctx, msg, chatRef(id), 0)
	case "-", "none", "general", "scratch":
		e.moveChat(ctx, msg, chatRef(id), "-")
	default:
		p, ok := e.findProject(ctx, raw)
		if !ok {
			_ = msg.Reply.SendText(ctx, fmt.Sprintf("No project matches “%s”. Pick one below.", raw))
			e.uiMovePicker(ctx, msg, chatRef(id), 0)
			return
		}
		e.moveChat(ctx, msg, chatRef(id), p.Meta.Slug)
	}
}

// moveChat binds chat ref to project slug ("-" = no project).
func (e *Engine) moveChat(ctx context.Context, msg channel.Message, ref, slug string) {
	info, err := e.resolveChatRef(ref)
	if err == nil && info.ID == e.chats.ActiveID() && e.refuseIfBusy(ctx, msg) {
		return
	}
	e.navMu.Lock()
	defer e.navMu.Unlock()
	e.moveChatLocked(ctx, msg, ref, slug)
}

func (e *Engine) moveChatLocked(ctx context.Context, msg channel.Message, ref, slug string) {
	e.ui.clear()
	info, err := e.resolveChatRef(ref)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "That chat is no longer available.")
		return
	}
	isCur := info.ID == e.chats.ActiveID()
	target := chat.Project{}
	label := "💬 General"
	if slug != "-" && slug != "" {
		if e.projects == nil {
			_ = msg.Reply.SendText(ctx, "Projects are not available.")
			return
		}
		p, ok := e.findProject(ctx, slug)
		if !ok {
			_ = msg.Reply.SendText(ctx, fmt.Sprintf("Project %s was not found.", code(slug)))
			return
		}
		if isCur {
			if err := e.projects.ActivateProject(ctx, p.Meta.Slug); err != nil {
				_ = msg.Reply.SendText(ctx, "Could not open project: "+err.Error())
				e.rebindActiveWorkspace(ctx)
				return
			}
		}
		meta := p.Meta
		if m, err := e.projects.ReadMeta(p.Meta.Slug); err == nil {
			meta = m
		}
		target = chat.Project{ID: meta.ID, Slug: meta.Slug, Hash: meta.Hash}
		label = "📁 " + mdSafe(p.Name())
	}
	if info.ProjectSlug == target.Slug {
		e.uiChatCard(ctx, msg, info.ID, "Already there — nothing to move.")
		return
	}
	if isCur {
		e.syncActiveProjectIfDirty(ctx) // flush the project being left
	}
	ch, err := e.chats.Bind(info.ID, target)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Move failed: "+err.Error())
		if isCur {
			e.rebindActiveWorkspace(ctx)
		}
		return
	}
	if isCur {
		e.bindWorkspaceForChat(ctx, ch.ID, ch.Mode, ch.ProjectSlug)
	}
	e.invalidateProjects()
	note := "Moved to " + label + "."
	if isCur && target.Slug != "" {
		note += " The agent now works in this project's files."
	} else if isCur {
		note += " The agent now uses a scratch workspace."
	}
	e.uiChatCard(ctx, msg, ch.ID, "🔀 "+note)
}

// ---------------------------------------------------------------------------
// Rename chat
// ---------------------------------------------------------------------------

func (e *Engine) startRenameChatWizard(ctx context.Context, msg channel.Message, ref string) {
	info, err := e.resolveChatRef(ref)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "That chat is no longer available.")
		return
	}
	e.ui.set(&uiWizard{Kind: "rename_chat", Step: "title", Draft: map[string]string{"ref": chatRef(info.ID)}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "✏ **Rename chat**\n\nCurrent: " + mdSafe(chatTitle(info, info.ID == e.chats.ActiveID())) + "\n\nSend the new title.",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: cb("cm", chatRef(info.ID))}}},
	})
}

func (e *Engine) renameChat(ctx context.Context, msg channel.Message, ref, title string) {
	info, err := e.resolveChatRef(ref)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "That chat is no longer available.")
		return
	}
	if info.ID == e.chats.ActiveID() && e.refuseIfBusy(ctx, msg) {
		return
	}
	e.navMu.Lock()
	defer e.navMu.Unlock()
	ch, err := e.chats.Rename(info.ID, title)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Rename failed: "+err.Error())
		return
	}
	e.uiChatCard(ctx, msg, ch.ID, "✅ Renamed")
}

// cmdRename handles /rename <title> for the current chat.
func (e *Engine) cmdRename(ctx context.Context, msg channel.Message, raw string) {
	id := e.chats.ActiveID()
	if id == "" {
		ch, err := e.chats.Resolve()
		if err != nil {
			_ = msg.Reply.SendText(ctx, err.Error())
			return
		}
		id = ch.ID
	}
	if strings.TrimSpace(raw) == "" {
		e.startRenameChatWizard(ctx, msg, chatRef(id))
		return
	}
	e.renameChat(ctx, msg, chatRef(id), raw)
}

// sortedSlugs is a tiny helper used by tests and diagnostics.
func sortedSlugs(cat []projectEntry) []string {
	out := make([]string, 0, len(cat))
	for _, p := range cat {
		out = append(out, p.Meta.Slug)
	}
	sort.Strings(out)
	return out
}
