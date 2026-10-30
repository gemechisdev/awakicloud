package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

// ProjectTools exposes project lifecycle actions the agent can call itself
// (same behavior as /upgrade for promote).
func ProjectTools(m *project.Manager, resolveChat func() (*store.Chat, error)) []types.Tool {
	if m == nil || resolveChat == nil {
		return nil
	}
	return []types.Tool{
		promoteProjectTool(m, resolveChat),
	}
}

func promoteProjectTool(m *project.Manager, resolveChat func() (*store.Chat, error)) types.Tool {
	return types.Tool{
		Name:  "promote_project",
		Label: "Promoting to project",
		Description: "Promote the current chat workspace into a durable project under /data/projects/{slug}/. " +
			"Copies files from the current workspace (scratch) into the project, binds the chat, saves chat metadata to the DB, " +
			"and syncs a zip archive. Use this when work needs more than ~2 files or must survive restarts. " +
			"Same effect as the user running /upgrade [name]. Do not use if already in a project.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Project name (used to derive the slug). Defaults to the chat title if omitted.",
			},
		},
		Required: []string{},
		Handler: func(ctx context.Context, input map[string]any) types.Result {
			ch, err := resolveChat()
			if err != nil {
				return types.Errf("resolve chat: %v", err)
			}
			if ch.Mode == project.ModeProject && ch.ProjectSlug != "" {
				return types.Result{Output: fmt.Sprintf("Already a project: %s (slug %s). No change.", ch.Title, ch.ProjectSlug)}
			}

			name, _ := input["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				name = strings.TrimSpace(ch.Title)
			}
			if name == "" {
				name = "untitled-project"
			}

			prevRoot := workspace.Root()
			meta, n, err := m.PromoteFromWorkspace(name, ch.ID, prevRoot)
			if err != nil {
				return types.Errf("promote failed: %v", err)
			}

			ch.Mode = project.ModeProject
			ch.ProjectID = meta.ID
			ch.ProjectSlug = meta.Slug
			if ch.Title == "" {
				ch.Title = meta.Name
			}
			if err := ch.Save(); err != nil {
				return types.Errf("saved project files but failed to persist chat: %v", err)
			}

			synced, err := m.Sync(ctx, meta.Slug)
			if err != nil {
				return types.Result{
					Output: fmt.Sprintf(
						"Promoted to project «%s» (slug %s). Copied %d files from previous workspace into /data/projects/%s/. "+
							"Chat bound in DB. Sync deferred: %v",
						meta.Name, meta.Slug, n, meta.Slug, err,
					),
				}
			}
			ch.ProjectHash = synced.Hash
			_ = ch.Save()

			return types.Result{Output: fmt.Sprintf(
				"Promoted to project «%s» (slug %s).\n"+
					"Copied %d files into /data/projects/%s/.\n"+
					"Chat bound + synced (hash %s).\n"+
					"File tools now resolve against this durable project directory.",
				meta.Name, meta.Slug, n, meta.Slug, synced.Hash,
			)}
		},
	}
}
