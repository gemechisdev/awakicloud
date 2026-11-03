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

// cmdOpenOrCreateProject opens an existing project slug, or creates a new one
// (copying current workspace contents if any). Never overwrites existing files.
//
// Existence is checked against local disk AND the blob store (cloud archives).
// If the project exists only in cloud, ActivateProject restores it locally.
func (e *Engine) cmdOpenOrCreateProject(ctx context.Context, msg channel.Message, name string) {
	if e.projects == nil {
		_ = msg.Reply.SendText(ctx, "Projects not available.")
		return
	}
	if e.refuseIfBusy(ctx, msg) {
		return
	}
	slug := project.Slugify(name)
	if exists, _, _ := e.projects.ProjectExists(ctx, slug); exists {
		// Existing project (local and/or cloud): use it for the current chat.
		// Never creates or overwrites files.
		e.invalidateProjects()
		e.cmdMove(ctx, msg, slug)
		return
	}
	// Project doesn't exist anywhere — create it and copy current workspace contents.
	ch, err := e.chats.Resolve()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	prevRoot := workspace.Root()
	meta, n, err := e.projects.PromoteFromWorkspaceCtx(ctx, name, ch.ID, prevRoot)
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
	if e.refuseIfBusy(ctx, msg) {
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

	meta, n, err := e.projects.PromoteFromWorkspaceCtx(ctx, name, ch.ID, prevRoot)
	if err != nil {
		// If the slug already exists, activate that project instead of failing.
		slug := project.Slugify(name)
		if exists, _, _ := e.projects.ProjectExists(ctx, slug); exists {
			e.cmdMove(ctx, msg, slug)
			return
		}
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
