package skills

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/store"
)

// Manager installs and inspects Agent Skill packages on disk under Store.SkillsDir().
type Manager struct {
	st store.Store
}

func NewManager(st store.Store) *Manager {
	return &Manager{st: st}
}

func (m *Manager) Dir() string { return m.st.SkillsDir() }

func (m *Manager) List() ([]domain.Skill, error) {
	return m.st.LoadSkills()
}

func (m *Manager) Get(name string) (domain.Skill, bool, error) {
	list, err := m.List()
	if err != nil {
		return domain.Skill{}, false, err
	}
	name = sanitize(name)
	for _, s := range list {
		if strings.EqualFold(s.Name, name) || sanitize(s.Name) == name {
			return enrich(s), true, nil
		}
	}
	return domain.Skill{}, false, nil
}

func (m *Manager) Remove(name string) error {
	return m.st.DeleteSkill(name)
}

// InstallZip extracts an Agent Skill zip into SkillsDir.
// Accepts either:
//   - zip root = skill contents (must contain SKILL.md), name from arg or frontmatter
//   - zip root = single top-level folder containing SKILL.md
func (m *Manager) InstallZip(zipPath, preferredName string) (domain.Skill, error) {
	tmp, err := os.MkdirTemp("", "awaki-skill-*")
	if err != nil {
		return domain.Skill{}, err
	}
	defer os.RemoveAll(tmp)

	if err := unzipTo(zipPath, tmp); err != nil {
		return domain.Skill{}, err
	}

	root, err := findSkillRoot(tmp)
	if err != nil {
		return domain.Skill{}, err
	}

	body, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		return domain.Skill{}, err
	}
	fmName, fmDesc := parseFrontmatter(string(body))
	name := preferredName
	if name == "" {
		name = fmName
	}
	if name == "" {
		name = filepath.Base(root)
	}
	name = sanitize(name)
	if name == "" {
		return domain.Skill{}, fmt.Errorf("could not determine skill name")
	}

	dest := filepath.Join(m.Dir(), name)
	_ = os.RemoveAll(dest)
	if err := copyTree(root, dest); err != nil {
		return domain.Skill{}, err
	}

	// Sync SKILL.md into store backends that track body in DB.
	content := string(body)
	if _, err := m.st.CreateSkill(name, fmDesc, stripFrontmatterBody(content)); err != nil {
		// already exists or sqlite unique — update instead
		_ = m.st.UpdateSkill(name, content)
		// Ensure full tree still on disk after UpdateSkill (sqlite only writes md)
		if err2 := copyTree(root, dest); err2 != nil {
			return domain.Skill{}, err2
		}
		_ = os.WriteFile(filepath.Join(dest, "SKILL.md"), body, 0o644)
	}

	sk, ok, err := m.Get(name)
	if err != nil {
		return domain.Skill{}, err
	}
	if !ok {
		return domain.Skill{Name: name, Description: fmDesc, Dir: dest, FilePath: filepath.Join(dest, "SKILL.md")}, nil
	}
	return sk, nil
}

// InstallMarkdown creates a Level-1 skill from name/description/body.
func (m *Manager) InstallMarkdown(name, description, body string) (domain.Skill, error) {
	path, err := m.st.CreateSkill(name, description, body)
	if err != nil {
		return domain.Skill{}, err
	}
	return domain.Skill{
		Name:        sanitize(name),
		Description: description,
		FilePath:    path,
		Dir:         filepath.Dir(path),
	}, nil
}

// Tree returns a short listing of files under the skill (max 80 entries).
func (m *Manager) Tree(name string) (string, error) {
	sk, ok, err := m.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	var b strings.Builder
	n := 0
	_ = filepath.Walk(sk.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, _ := filepath.Rel(sk.Dir, path)
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			fmt.Fprintf(&b, "%s/\n", rel)
		} else {
			fmt.Fprintf(&b, "%s (%d bytes)\n", rel, info.Size())
		}
		n++
		if n >= 80 {
			b.WriteString("…\n")
			return io.EOF
		}
		return nil
	})
	return b.String(), nil
}

// ReadFile reads a path relative to the skill root (no .. escape).
func (m *Manager) ReadFile(name, rel string) (string, error) {
	sk, ok, err := m.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid path")
	}
	full := filepath.Join(sk.Dir, rel)
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if len(data) > 200_000 {
		return string(data[:200_000]) + "\n…(truncated)", nil
	}
	return string(data), nil
}

// ScriptPath resolves scripts/<name> under the skill if it exists.
func (m *Manager) ScriptPath(skillName, script string) (string, error) {
	sk, ok, err := m.Get(skillName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("skill %q not found", skillName)
	}
	script = filepath.Clean(script)
	if strings.Contains(script, "..") {
		return "", fmt.Errorf("invalid script path")
	}
	// Allow "extract.py" or "scripts/extract.py"
	candidates := []string{
		filepath.Join(sk.Dir, "scripts", script),
		filepath.Join(sk.Dir, script),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("script %q not found in skill %q", script, skillName)
}

func enrich(s domain.Skill) domain.Skill {
	if s.Dir == "" && s.FilePath != "" {
		s.Dir = filepath.Dir(s.FilePath)
	}
	if s.Dir == "" {
		return s
	}
	if st, err := os.Stat(filepath.Join(s.Dir, "references")); err == nil && st.IsDir() {
		s.HasReferences = true
	}
	if st, err := os.Stat(filepath.Join(s.Dir, "scripts")); err == nil && st.IsDir() {
		s.HasScripts = true
	}
	if st, err := os.Stat(filepath.Join(s.Dir, "assets")); err == nil && st.IsDir() {
		s.HasAssets = true
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".disabled")); err == nil {
		s.Disabled = true
	}
	return s
}

func findSkillRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	if len(dirs) == 1 {
		if _, err := os.Stat(filepath.Join(dirs[0], "SKILL.md")); err == nil {
			return dirs[0], nil
		}
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "SKILL.md")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("zip does not contain SKILL.md (expected Agent Skill package)")
}

func unzipTo(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := f.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			continue
		}
		target := filepath.Join(dest, name)
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		out.Close()
		return err
	})
}

func sanitize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ', r == '_':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func parseFrontmatter(content string) (name, description string) {
	if !strings.HasPrefix(content, "---") {
		return "", ""
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return "", ""
	}
	block := content[3 : 3+end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, ":"); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			v = strings.Trim(v, `"'`)
			switch k {
			case "name":
				name = v
			case "description":
				description = v
			}
		}
	}
	return name, description
}

func stripFrontmatterBody(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return content
	}
	return strings.TrimSpace(content[3+end+3:])
}
