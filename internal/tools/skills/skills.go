package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	skillpkg "github.com/gemechischala/awaki/internal/skills"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/tools/types"
)

func SkillTools(st store.Store) []types.Tool {
	mgr := skillpkg.NewManager(st)
	return []types.Tool{
		{
			Name:  "skill_list",
			Label: "Listing skills",
			Description: "List installed Agent Skills (L1 instructions, L2 +references/assets, L3 +scripts). " +
				"Skills teach workflows; they do not replace tools. Read a matching skill with skill_read before following it.",
			Parameters:   map[string]any{},
			ParallelSafe: true,
			Handler: func(_ context.Context, _ map[string]any) types.Result {
				loaded, err := st.LoadSkills()
				if err != nil {
					return types.Result{Output: "error listing skills: " + err.Error(), IsErr: true}
				}
				if len(loaded) == 0 {
					return types.Result{Output: "(no skills yet — owner can /skills → Add, or use skill_create)"}
				}
				var b strings.Builder
				for _, sk := range loaded {
					fmt.Fprintf(&b, "- %s [%s]: %s\n  path: %s\n", sk.Name, sk.LevelLabel(), sk.Description, sk.FilePath)
				}
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:  "skill_read",
			Label: "Reading skill",
			Description: "Read SKILL.md for a skill. After reading, follow its instructions. " +
				"For L2/L3 packages, use skill_files / skill_read_file for references/scripts, and skill_run for scripts/.",
			Parameters: map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name"},
			},
			Required:     []string{"name"},
			ParallelSafe: true,
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				content, err := st.ReadSkill(name)
				if err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				return types.Result{Output: content}
			},
		},
		{
			Name:        "skill_files",
			Label:       "Listing skill files",
			Description: "List files inside a skill package (SKILL.md, references/, scripts/, assets/).",
			Parameters: map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name"},
			},
			Required:     []string{"name"},
			ParallelSafe: true,
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				tree, err := mgr.Tree(name)
				if err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				if strings.TrimSpace(tree) == "" {
					return types.Result{Output: "(skill has only SKILL.md)"}
				}
				return types.Result{Output: tree}
			},
		},
		{
			Name:        "skill_read_file",
			Label:       "Reading skill file",
			Description: "Read a file relative to a skill root, e.g. references/guide.md or scripts/extract.py.",
			Parameters: map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name"},
				"path": map[string]any{"type": "string", "description": "Relative path inside the skill"},
			},
			Required: []string{"name", "path"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				rel, err := types.GetString(input, "path")
				if err != nil {
					return types.ErrResult(err)
				}
				content, err := mgr.ReadFile(name, rel)
				if err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				return types.Result{Output: content}
			},
		},
		{
			Name:  "skill_run",
			Label: "Running skill script",
			Description: "Execute a script from a skill's scripts/ directory (Level 3). " +
				"Working directory is the skill root. Prefer this over inventing one-off scripts when the skill ships one.",
			Parameters: map[string]any{
				"name":   map[string]any{"type": "string", "description": "Skill name"},
				"script": map[string]any{"type": "string", "description": "Script filename under scripts/ (e.g. extract.py)"},
				"args":   map[string]any{"type": "string", "description": "Optional arguments string"},
			},
			Required: []string{"name", "script"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				script, err := types.GetString(input, "script")
				if err != nil {
					return types.ErrResult(err)
				}
				args, _ := types.GetString(input, "args")
				path, err := mgr.ScriptPath(name, script)
				if err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				sk, ok, _ := mgr.Get(name)
				if !ok {
					return types.Result{Output: "skill not found", IsErr: true}
				}
				ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				defer cancel()
				var cmd *exec.Cmd
				switch strings.ToLower(filepath.Ext(path)) {
				case ".py":
					cmd = exec.CommandContext(ctx, "python3", append([]string{path}, splitArgs(args)...)...)
				case ".js", ".mjs":
					cmd = exec.CommandContext(ctx, "node", append([]string{path}, splitArgs(args)...)...)
				case ".sh":
					cmd = exec.CommandContext(ctx, "bash", append([]string{path}, splitArgs(args)...)...)
				default:
					// try direct execution
					cmd = exec.CommandContext(ctx, path, splitArgs(args)...)
				}
				cmd.Dir = sk.Dir
				cmd.Env = append(os.Environ(), "SKILL_DIR="+sk.Dir)
				out, err := cmd.CombinedOutput()
				text := string(out)
				if len(text) > 50_000 {
					text = text[:50_000] + "\n…(truncated)"
				}
				if err != nil {
					return types.Result{Output: text + "\n[exit: " + err.Error() + "]", IsErr: true}
				}
				if text == "" {
					text = "(script completed with no output)"
				}
				return types.Result{Output: text}
			},
		},
		{
			Name:  "skill_create",
			Label: "Creating skill",
			Description: "Create a Level-1 skill (SKILL.md) for a reusable procedure you want to remember. " +
				"For full packages (references/scripts) or skills from a repo/URL, use skill_install instead.",
			Parameters: map[string]any{
				"name":        map[string]any{"type": "string", "description": "lowercase-hyphen name"},
				"description": map[string]any{"type": "string", "description": "One-line when to use this skill"},
				"body":        map[string]any{"type": "string", "description": "Markdown instructions (procedure, commands, gotchas)"},
			},
			Required: []string{"name", "description", "body"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				desc, err := types.GetString(input, "description")
				if err != nil {
					return types.ErrResult(err)
				}
				body, err := types.GetString(input, "body")
				if err != nil {
					return types.ErrResult(err)
				}
				path, err := st.CreateSkill(name, desc, body)
				if err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				return types.Result{Output: "skill created: " + path}
			},
		},
		{
			Name:  "skill_install",
			Label: "Installing skill",
			Description: "Install Agent Skill package(s) from a source into the skill library, including references/, assets/ and scripts/ " +
				"(L2/L3). Sources: a local path (path/to/skill dir, folder of skills, .zip or .md), owner/repo, owner/repo@skill, owner/repo/path, https://github.com/owner/repo[/tree/ref/path], " +
				"https://skills.sh/..., https://smithery.ai/skills/..., a site with /.well-known/skills, or a direct .zip/.md URL. " +
				"ALWAYS use this to add skills from a repo/URL. NEVER copy files into the skills directory by hand (bash/write_file): " +
				"that bypasses registration and DB sync, so the skill is not persisted and L2/L3 parts are not detected.",
			Parameters: map[string]any{
				"source": map[string]any{"type": "string", "description": "Local path, repo, URL or owner/repo@skill"},
				"skill":  map[string]any{"type": "string", "description": "Optional: install only this skill from a multi-skill source"},
				"all":    map[string]any{"type": "boolean", "description": "Allow installing more than 25 skills from one source"},
			},
			Required: []string{"source"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				source, err := types.GetString(input, "source")
				if err != nil {
					return types.ErrResult(err)
				}
				opts := skillpkg.InstallOptions{}
				if v, ok := input["skill"].(string); ok {
					opts.Skill = strings.TrimSpace(v)
				}
				if v, ok := input["all"].(bool); ok {
					opts.All = v
				}
				var list []store.Skill
				if lp, ok := skillpkg.ResolveLocalSource(source); ok {
					list, err = mgr.InstallLocal(lp, opts)
				} else {
					list, err = mgr.InstallFromSource(ctx, source, opts)
				}
				if err != nil {
					return types.Result{Output: "install failed: " + err.Error(), IsErr: true}
				}
				if len(list) == 0 {
					return types.Result{Output: "install failed: no skills found in " + source, IsErr: true}
				}
				var b strings.Builder
				fmt.Fprintf(&b, "installed %d skill(s) from %s:\n", len(list), source)
				for _, sk := range list {
					got, ok, _ := mgr.Get(sk.Name)
					if !ok {
						return types.Result{Output: "install reported success but skill not registered: " + sk.Name, IsErr: true}
					}
					fmt.Fprintf(&b, "- %s [%s]\n", got.Name, got.LevelLabel())
				}
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:        "skill_update",
			Label:       "Updating skill",
			Description: "Replace the full SKILL.md content of an existing skill.",
			Parameters: map[string]any{
				"name":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string", "description": "Full SKILL.md including optional frontmatter"},
			},
			Required: []string{"name", "content"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				content, err := types.GetString(input, "content")
				if err != nil {
					return types.ErrResult(err)
				}
				if err := st.UpdateSkill(name, content); err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				return types.Result{Output: "skill updated: " + name}
			},
		},
		{
			Name:        "skill_delete",
			Label:       "Deleting skill",
			Description: "Delete a skill package (SKILL.md and any references/scripts/assets).",
			Parameters: map[string]any{
				"name": map[string]any{"type": "string"},
			},
			Required: []string{"name"},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				name, err := types.GetString(input, "name")
				if err != nil {
					return types.ErrResult(err)
				}
				if err := st.DeleteSkill(name); err != nil {
					return types.Result{Output: err.Error(), IsErr: true}
				}
				return types.Result{Output: "skill deleted: " + name}
			},
		},
	}
}

func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}
