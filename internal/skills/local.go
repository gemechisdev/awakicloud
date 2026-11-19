package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/domain"
)

// ResolveLocalSource reports whether source refers to an existing local
// skill location (directory, .zip or .md file) and returns its absolute path.
// "~/x", "./x", "../x" and absolute paths are always treated as local; a bare
// relative path like "path/to/skill" is local only if it exists and looks
// like a skill (dir with SKILL.md somewhere inside, or a .zip/.md file), so
// "owner/repo/path" GitHub sources are not shadowed.
func ResolveLocalSource(source string) (string, bool) {
	s := strings.TrimSpace(source)
	if s == "" || strings.Contains(s, "://") {
		return "", false
	}
	explicit := strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") || s == "." || s == ".." || strings.HasPrefix(s, "~")
	if strings.HasPrefix(s, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			s = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(s, "~"), "/"))
		}
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return "", false
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", false
	}
	if explicit {
		return abs, true
	}
	if st.IsDir() {
		return abs, len(findAllSkillRoots(abs)) > 0
	}
	ext := strings.ToLower(filepath.Ext(abs))
	return abs, ext == ".zip" || ext == ".md"
}

// InstallLocal installs skill(s) from a local path: a skill directory (or a
// directory containing several skills), a .zip package, or a single .md file.
// Directories are staged in a temp dir first, so pointing at a folder that
// already lives inside the skills directory is safe (re-registers it).
func (m *Manager) InstallLocal(path string, opts InstallOptions) ([]domain.Skill, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("local skill path: %w", err)
	}
	if !st.IsDir() {
		return m.InstallPath(path, opts.Skill)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	roots := findAllSkillRoots(abs)
	if opts.Skill != "" {
		want := sanitize(opts.Skill)
		var kept []string
		for _, r := range roots {
			if sanitize(filepath.Base(r)) == want {
				kept = append(kept, r)
				continue
			}
			if body, err := os.ReadFile(filepath.Join(r, "SKILL.md")); err == nil {
				if n, _ := parseFrontmatter(string(body)); sanitize(n) == want {
					kept = append(kept, r)
				}
			}
		}
		roots = kept
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no SKILL.md found under %s", path)
	}
	if len(roots) > maxBulkInstall && !opts.All {
		return nil, fmt.Errorf("%d skills found under %s; pass all=true to install them all", len(roots), path)
	}

	tmp, err := os.MkdirTemp("", "skill-src-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var out []domain.Skill
	for i, r := range roots {
		stage := filepath.Join(tmp, strconv.Itoa(i), filepath.Base(r))
		if err := copyTree(r, stage); err != nil {
			return out, fmt.Errorf("stage %s: %w", r, err)
		}
		sk, err := m.installSkillDir(stage, "")
		if err != nil {
			return out, fmt.Errorf("install %s: %w", r, err)
		}
		out = append(out, sk)
	}
	return out, nil
}
