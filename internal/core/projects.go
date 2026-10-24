package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/project"
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

// syncActiveProject uploads zip if current chat is a project.
func (e *Engine) syncActiveProject(ctx context.Context) {
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
	meta, err := e.projects.Sync(ctx, ch.ProjectSlug)
	if err != nil {
		log.Warn().Err(err).Str("slug", ch.ProjectSlug).Msg("project sync failed")
		return
	}
	ch.ProjectHash = meta.Hash
	_ = ch.Save()
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

func (e *Engine) cmdUpgradeProject(ctx context.Context, msg channel.Message, name string) {
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
			Text: "📁 Upgrade chat → project\n\nSend a name for this project.",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:home"}}},
		})
		return
	}
	meta, err := e.projects.Create(name, ch.ID)
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
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
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("🟢 Upgraded to project «%s» (%s)", meta.Name, meta.Slug))
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
		b.WriteString(fmt.Sprintf("• %s (%s)\n", label, m.Slug))
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
	meta, err := e.projects.Sync(ctx, ch.ProjectSlug)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Sync failed: "+err.Error())
		return
	}
	ch.ProjectHash = meta.Hash
	_ = ch.Save()
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("☁ Synced «%s»\nHash: %s", meta.Name, meta.Hash))
}
