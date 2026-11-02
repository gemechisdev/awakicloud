package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/workspace"
	"github.com/rs/zerolog/log"
)

func (e *Engine) ensureProjects() *project.Manager {
	return e.projects
}

// bindWorkspaceForChat activates scratch or project dir for the active chat.
func (e *Engine) bindWorkspaceForChat(ctx context.Context, chatID, mode, slug string) {
	if e.projects == nil {
		return
	}
	if err := e.projects.EnsureSessionWorkspace(ctx, mode, slug, chatID); err != nil {
		log.Warn().Err(err).Str("chat", chatID).Msg("workspace bind failed")
	}
}

// SyncActiveProject uploads zip if current chat is a project.
func (e *Engine) SyncActiveProject(ctx context.Context) {
	e.syncActiveProjectIfDirty(ctx)
}

// syncActiveProjectIfDirty uploads the project zip only if it has been marked
// dirty since the last sync AND the content hash actually changed.
func (e *Engine) syncActiveProjectIfDirty(ctx context.Context) {
	if e.projects == nil {
		return
	}
	ch, err := e.chats.Resolve()
	if err != nil || ch == nil {
		return
	}
	if ch.Mode != project.ModeProject || ch.ProjectSlug == "" {
		return
	}
	synced, meta, err := e.projects.SyncIfDirty(ctx, ch.ProjectSlug, ch.ID)
	if err != nil {
		log.Warn().Err(err).Str("slug", ch.ProjectSlug).Msg("project sync-if-dirty failed")
		return
	}
	if synced {
		ch.ProjectHash = meta.Hash
		_ = ch.Save()
		log.Debug().Str("slug", ch.ProjectSlug).Str("hash", meta.Hash).Msg("project auto-synced (dirty)")
	}
}

// startProjectAutoSync launches a background ticker that auto-syncs the active
// project whenever the project manager reports dirty (file changes, DB updates,
// etc.). Interval is configurable (default 30s).
func (e *Engine) startProjectAutoSync(ctx context.Context) {
	if e.projects == nil {
		return
	}
	interval := e.projectSyncInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.syncActiveProjectIfDirty(context.WithoutCancel(ctx))
			}
		}
	}()
}

func (e *Engine) cmdNewProject(ctx context.Context, msg channel.Message, name string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		e.ui.set(&uiWizard{Kind: "new_project", Step: "name", Draft: map[string]string{}})
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "📁 New Project\n\nSend a project name (e.g. Telebirr Mini App).",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:home"}}},
		})
		return
	}
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	meta, err := e.projects.Create(name, ch.ID)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Create failed: "+err.Error())
		return
	}
	ch.Mode = project.ModeProject
	ch.ProjectID = meta.ID
	ch.ProjectSlug = meta.Slug
	if ch.Title == "" {
		ch.Title = meta.Name
	}
	_ = ch.Save()
	_, _ = e.projects.Sync(ctx, meta.Slug)
	_ = msg.Reply.SendText(ctx, fmt.Sprintf(
		"✅ Project «%s» ready\nSlug: %s\nWorkspace bound. Agent file tools will use this folder.\nSynced to durable storage.",
		meta.Name, meta.Slug,
	))
}

// cmdOpenOrCreateProject opens an existing project slug, or creates a new one
// (copying current workspace contents if any). Never overwrites existing files.
func (e *Engine) cmdOpenOrCreateProject(ctx context.Context, msg channel.Message, name string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	slug := project.Slugify(name)
	if _, err := e.projects.ReadMeta(slug); err == nil {
		// Project exists — activate it without overwriting anything.
		if err := e.projects.ActivateProject(ctx, slug); err != nil {
			_ = msg.Reply.SendText(ctx, "Activate failed: "+err.Error())
			return
		}
		ch, err := e.chats.Resolve()
		if err == nil && ch != nil {
			ch.Mode = project.ModeProject
			ch.ProjectSlug = slug
			if meta, err := e.projects.ReadMeta(slug); err == nil {
				ch.ProjectID = meta.ID
				ch.ProjectHash = meta.Hash
				if ch.Title == "" {
					ch.Title = meta.Name
				}
			}
			_ = ch.Save()
		}
		meta, _ := e.projects.ReadMeta(slug)
		_ = msg.Reply.SendText(ctx, fmt.Sprintf(
			"📂 Project «%s» activated\nSlug: %s\nFiles: %d\nWorkspace bound.",
			meta.Name, meta.Slug, meta.FileCount,
		))
		return
	}
	// Project doesn't exist — create it and copy current workspace contents.
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	prevRoot := workspace.Root()
	meta, n, err := e.projects.PromoteFromWorkspace(name, ch.ID, prevRoot)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Create failed: "+err.Error())
		return
	}
	ch.Mode = project.ModeProject
	ch.ProjectID = meta.ID
	ch.ProjectSlug = meta.Slug
	if ch.Title == "" {
		ch.Title = meta.Name
	}
	_ = ch.Save()
	synced, err := e.projects.Sync(ctx, meta.Slug, ch.ID)
	if err != nil {
		_ = msg.Reply.SendText(ctx, fmt.Sprintf(
			"🟢 Created «%s» (%s)\nCopied %d files.\nSync deferred: %v",
			meta.Name, meta.Slug, n, err,
		))
		return
	}
	ch.ProjectHash = synced.Hash
	_ = ch.Save()
	_ = msg.Reply.SendText(ctx, fmt.Sprintf(
		"🟢 Created «%s» (%s)\nCopied %d files from current workspace.\nChat bound + synced (hash %s).",
		meta.Name, meta.Slug, n, synced.Hash,
	))
}

func (e *Engine) cmdUpgradeProject(ctx context.Context, msg channel.Message, name string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	if ch.Mode == project.ModeProject {
		_ = msg.Reply.SendText(ctx, "Already a project: "+ch.ProjectSlug)
		return
	}
	if strings.TrimSpace(name) == "" {
		name = ch.Title
	}
	if strings.TrimSpace(name) == "" {
		e.ui.set(&uiWizard{Kind: "upgrade_project", Step: "name", Draft: map[string]string{}})
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "📁 Upgrade chat → project\n\nSend a name for this project.\nCurrent workspace files will be copied into the project and synced.",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:home"}}},
		})
		return
	}

	// Capture current workspace (usually scratch) before Create rebinds it.
	prevRoot := workspace.Root()

	meta, n, err := e.projects.PromoteFromWorkspace(name, ch.ID, prevRoot)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Promote failed: "+err.Error())
		return
	}

	ch.Mode = project.ModeProject
	ch.ProjectID = meta.ID
	ch.ProjectSlug = meta.Slug
	if ch.Title == "" {
		ch.Title = meta.Name
	}
	_ = ch.Save() // persists mode/slug into chat DB

	synced, err := e.projects.Sync(ctx, meta.Slug)
	if err != nil {
		log.Warn().Err(err).Str("slug", meta.Slug).Msg("project sync after promote failed")
		_ = msg.Reply.SendText(ctx, fmt.Sprintf(
			"🟢 Promoted to project «%s» (%s)\nCopied %d files from previous workspace.\nSync deferred: %v",
			meta.Name, meta.Slug, n, err,
		))
		return
	}
	ch.ProjectHash = synced.Hash
	_ = ch.Save()

	_ = msg.Reply.SendText(ctx, fmt.Sprintf(
		"🟢 Promoted to project «%s» (%s)\n"+
			"Copied %d files from previous workspace into /data/projects/%s/\n"+
			"Chat bound + synced (hash %s).\n"+
			"Agent file tools now use this durable project directory.",
		meta.Name, meta.Slug, n, meta.Slug, synced.Hash,
	))
}

func (e *Engine) cmdListProjects(ctx context.Context, msg channel.Message) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	local, _ := e.projects.ListLocal()
	cloud, _ := e.projects.ListCloud(ctx)
	seen := map[string]project.Meta{}
	for _, m := range cloud {
		seen[m.Slug] = m
	}
	for _, m := range local {
		seen[m.Slug] = m
	}
	if len(seen) == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "📁 No projects yet.\nCreate one with /project or 📁 New Project.",
			Rows: [][]channel.Button{
				{{Text: "📁 New Project", CallbackData: "ui:projnew"}},
				{{Text: "⬅ Home", CallbackData: "ui:home"}},
			},
		})
		return
	}
	var b strings.Builder
	b.WriteString("📁 Projects\n\n")
	var rows [][]channel.Button
	for _, m := range seen {
		label := m.Name
		if label == "" {
			label = m.Slug
		}
		stats := ""
		if m.FileCount > 0 {
			stats = fmt.Sprintf(" — %d files, %s", m.FileCount, humanBytes(m.TotalSize))
		}
		if m.LastEditor != "" {
			stats += fmt.Sprintf(" (last: %s)", truncateUI(m.LastEditor, 20))
		}
		b.WriteString(fmt.Sprintf("• %s (%s)%s\n", label, m.Slug, stats))
		rows = append(rows, []channel.Button{
			{Text: "▶ " + truncateUI(label, 28), CallbackData: "ui:projopen:" + m.Slug},
		})
	}
	rows = append(rows,
		[]channel.Button{{Text: "📁 New Project", CallbackData: "ui:projnew"}},
		[]channel.Button{{Text: "⬅ Home", CallbackData: "ui:home"}},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) openProjectBySlug(ctx context.Context, msg channel.Message, slug string) {
	if e.projects == nil {
		return
	}
	if err := e.projects.ActivateProject(ctx, slug); err != nil {
		_ = msg.Reply.SendText(ctx, "Open failed: "+err.Error())
		return
	}
	// bind active chat if it matches, else still set workspace
	ch, err := e.chats.Resolve()
	if err == nil && ch != nil {
		ch.Mode = project.ModeProject
		ch.ProjectSlug = slug
		if meta, err := e.projects.ReadMeta(slug); err == nil {
			ch.ProjectID = meta.ID
			ch.ProjectHash = meta.Hash
			if ch.Title == "" {
				ch.Title = meta.Name
			}
		}
		_ = ch.Save()
	}
	_ = msg.Reply.SendText(ctx, "📂 Project workspace active: "+slug)
}

func (e *Engine) cmdSyncProject(ctx context.Context, msg channel.Message) {
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	if ch.Mode != project.ModeProject || ch.ProjectSlug == "" {
		_ = msg.Reply.SendText(ctx, "Current chat is not a project. Use /project or upgrade first.")
		return
	}
	meta, err := e.projects.Sync(ctx, ch.ProjectSlug, ch.ID)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Sync failed: "+err.Error())
		return
	}
	ch.ProjectHash = meta.Hash
	_ = ch.Save()
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("☁ Synced «%s»\nHash: %s", meta.Name, meta.Hash))
}

// humanBytes formats bytes into a human-readable string.
func humanBytes(b int64) string {
	if b < 0 {
		return "N/A"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(b)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.0f %s", f, units[i])
}

// cmdChatLink links the current chat to a project (multi-chat per project).
func (e *Engine) cmdChatLink(ctx context.Context, msg channel.Message, slug string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	if slug == "" {
		_ = msg.Reply.SendText(ctx, "Usage: /chatlink <slug>")
		return
	}
	if _, err := e.projects.ReadMeta(slug); err != nil {
		_ = msg.Reply.SendText(ctx, "Project not found: "+slug)
		return
	}
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	ch.Mode = project.ModeProject
	ch.ProjectSlug = slug
	var meta *project.Meta
	if m, err := e.projects.ReadMeta(slug); err == nil {
		meta = &m
		ch.ProjectID = m.ID
		ch.ProjectHash = m.Hash
		if ch.Title == "" {
			ch.Title = m.Name
		}
	}
	if err := e.projects.ActivateProject(ctx, slug); err != nil {
		_ = msg.Reply.SendText(ctx, "Link failed: "+err.Error())
		return
	}
	_ = ch.Save()
	name := slug
	if meta != nil {
		name = meta.Name
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("🔗 Chat linked to project «%s» (%s)", name, slug))
}

// cmdChatsByProject lists chats bound to a project.
func (e *Engine) cmdChatsByProject(ctx context.Context, msg channel.Message, slug string) {
	if e.store == nil {
		_ = msg.Reply.SendText(ctx, "Store not available.")
		return
	}
	if slug == "" {
		_ = msg.Reply.SendText(ctx, "Usage: /chats <slug>")
		return
	}
	chats, err := e.store.ListChatsByProject(slug)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Failed to list chats: "+err.Error())
		return
	}
	if len(chats) == 0 {
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("No chats linked to project «%s».", slug))
		return
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("💬 Chats in «%s»\n\n", slug))
	var rows [][]channel.Button
	for _, c := range chats {
		label := c.Title
		if label == "" {
			label = c.ID
		}
		b.WriteString(fmt.Sprintf("• %s (%s)\n", truncateUI(label, 28), c.ID))
		rows = append(rows, []channel.Button{
			{Text: "▶ " + truncateUI(label, 20), CallbackData: "ui:chatopen:" + c.ID},
		})
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Home", CallbackData: "ui:home"}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}
