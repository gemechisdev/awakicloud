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
			Parameters: map[string]any{},
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
			Required: []string{"name"},
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
			Required: []string{"name"},
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
				"For full packages (references/scripts), the owner should install a skill zip via /skills.",
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
