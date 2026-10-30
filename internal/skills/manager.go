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

// SkillPackager is implemented by sqlite store to persist full packages in DB.
type SkillPackager interface {
	SyncSkillPackage(name string) (hash string, changed bool, err error)
}

// Manager installs and inspects Agent Skill packages under Store.SkillsDir().
//
// On-disk layout (one skill = one directory):
//
//	{SkillsDir}/
//	  pdf-processing/          ← one skill package
//	    SKILL.md               ← required (Agent Skills / open standard style)
//	    references/            ← optional
//	    scripts/               ← optional
//	    assets/                ← optional
//	  github-workflow/         ← another skill
//	    SKILL.md
//
// This matches the common Agent Skills layout used by Anthropic/OpenAI-style
// skill packages: each skill is its own folder with SKILL.md at the root of
// that folder — not a single shared folder of mixed files.
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

func (m *Manager) syncPackage(name string) {
	if p, ok := m.st.(SkillPackager); ok {
		if _, _, err := p.SyncSkillPackage(name); err != nil {
			// non-fatal; disk still has the files
			_ = err
		}
	}
}

// InstallPath installs from a file path: .zip package(s) or a single .md skill file.
// Returns all skills installed (zip may contain multiple).
func (m *Manager) InstallPath(path, preferredName string) ([]domain.Skill, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".zip":
		return m.InstallZip(path, preferredName)
	case ".md":
		sk, err := m.InstallMarkdownFile(path, preferredName)
		if err != nil {
			return nil, err
		}
		return []domain.Skill{sk}, nil
	default:
		return nil, fmt.Errorf("unsupported skill file type %q (use .zip or .md)", ext)
	}
}

// InstallMarkdownFile installs a Level-1 skill from a single markdown file.
// Accepts SKILL.md or {name}.md (name taken from filename if needed).
func (m *Manager) InstallMarkdownFile(mdPath, preferredName string) (domain.Skill, error) {
	body, err := os.ReadFile(mdPath)
	if err != nil {
		return domain.Skill{}, err
	}
	fmName, fmDesc := parseFrontmatter(string(body))
	name := preferredName
	if name == "" {
		name = fmName
	}
	if name == "" {
		base := strings.TrimSuffix(filepath.Base(mdPath), filepath.Ext(mdPath))
		if strings.EqualFold(base, "skill") {
			return domain.Skill{}, fmt.Errorf("name required when file is SKILL.md without frontmatter name")
		}
		name = base
	}
	name = sanitize(name)
	content := string(body)
	// CreateSkill formats frontmatter if missing description
	path, err := m.st.CreateSkill(name, fmDesc, stripFrontmatterBody(content))
	if err != nil {
		_ = m.st.UpdateSkill(name, content)
		path = filepath.Join(m.Dir(), name, "SKILL.md")
		_ = os.WriteFile(path, body, 0o644)
	}
	m.syncPackage(name)
	return domain.Skill{Name: name, Description: fmDesc, FilePath: path, Dir: filepath.Dir(path)}, nil
}

// InstallZip extracts skill package(s). Accepted layouts:
//
//  1. Single skill at zip root: SKILL.md + optional references/scripts/assets
//  2. Single skill in one folder: my-skill/SKILL.md + …
//  3. Multiple skills: skills/foo/SKILL.md, skills/bar/SKILL.md
//     or foo/SKILL.md and bar/SKILL.md as sibling folders
//
// preferredName applies only when exactly one skill is found.
func (m *Manager) InstallZip(zipPath, preferredName string) ([]domain.Skill, error) {
	tmp, err := os.MkdirTemp("", "awaki-skill-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if err := unzipTo(zipPath, tmp); err != nil {
		return nil, err
	}

	roots := findAllSkillRoots(tmp)
	if len(roots) == 0 {
		return nil, fmt.Errorf("no SKILL.md found in zip — see /skills format help")
	}

	var out []domain.Skill
	for _, root := range roots {
		body, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
		if err != nil {
			return out, err
		}
		fmName, fmDesc := parseFrontmatter(string(body))
		name := fmName
		if name == "" {
			name = filepath.Base(root)
		}
		if len(roots) == 1 && preferredName != "" {
			name = preferredName
		}
		name = sanitize(name)
		if name == "" || name == "skills" || name == "tmp" {
			return out, fmt.Errorf("could not determine skill name for %s", root)
		}

		dest := filepath.Join(m.Dir(), name)
		_ = os.RemoveAll(dest)
		if err := copyTree(root, dest); err != nil {
			return out, err
		}
		content := string(body)
		if _, err := m.st.CreateSkill(name, fmDesc, stripFrontmatterBody(content)); err != nil {
			_ = m.st.UpdateSkill(name, content)
			_ = copyTree(root, dest)
			_ = os.WriteFile(filepath.Join(dest, "SKILL.md"), body, 0o644)
		}
		m.syncPackage(name)
		sk, ok, _ := m.Get(name)
		if !ok {
			sk = domain.Skill{Name: name, Description: fmDesc, Dir: dest, FilePath: filepath.Join(dest, "SKILL.md")}
		}
		out = append(out, sk)
	}
	return out, nil
}

// InstallMarkdown creates a Level-1 skill from fields.
func (m *Manager) InstallMarkdown(name, description, body string) (domain.Skill, error) {
	path, err := m.st.CreateSkill(name, description, body)
	if err != nil {
		return domain.Skill{}, err
	}
	name = sanitize(name)
	m.syncPackage(name)
	return domain.Skill{
		Name:        name,
		Description: description,
		FilePath:    path,
		Dir:         filepath.Dir(path),
	}, nil
}

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
		if rel == "." || rel == ".package_hash" {
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

// findAllSkillRoots returns directories that contain SKILL.md.
func findAllSkillRoots(dir string) []string {
	var roots []string
	// Direct SKILL.md at dir
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return []string{dir}
	}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(info.Name(), "SKILL.md") {
			roots = append(roots, filepath.Dir(path))
		}
		return nil
	})
	// De-dupe and drop nested roots inside another skill root
	var filtered []string
	for _, r := range roots {
		nested := false
		for _, o := range roots {
			if r != o && strings.HasPrefix(r, o+string(os.PathSeparator)) {
				nested = true
				break
			}
		}
		if !nested {
			filtered = append(filtered, r)
		}
	}
	return filtered
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
