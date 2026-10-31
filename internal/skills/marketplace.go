package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/domain"
	"github.com/rs/zerolog/log"
)

// Marketplace is a registered GitHub skills/plugin source
// (Claude Code compatible: owner/repo with .claude-plugin/marketplace.json).
type Marketplace struct {
	ID          string    `json:"id"`          // marketplace.json "name", e.g. anthropic-agent-skills
	Source      string    `json:"source"`      // owner/repo, e.g. anthropics/skills
	Description string    `json:"description"` // from metadata
	AddedAt     time.Time `json:"added_at"`
	Plugins     []Plugin  `json:"plugins"`
}

type Plugin struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Skills      []string `json:"skills"` // relative paths like ./skills/pdf
	// Source is the plugin root inside the marketplace repo, e.g. "./",
	// "./plugins/frontend-design", or a git-subdir path. Empty means repo root.
	Source string `json:"source,omitempty"`
}

type marketplaceFile struct {
	Name     string `json:"name"`
	Metadata struct {
		Description string `json:"description"`
		Version     string `json:"version"`
	} `json:"metadata"`
	Plugins []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Skills      []string        `json:"skills"`
		Source      json.RawMessage `json:"source"` // string "./" or {source,url,path,ref}
	} `json:"plugins"`
}

type marketRegistry struct {
	Markets []Marketplace `json:"markets"`
}

func (m *Manager) marketPath() string {
	return filepath.Join(filepath.Dir(m.Dir()), "marketplaces.json")
}

func (m *Manager) loadMarkets() marketRegistry {
	var reg marketRegistry
	data, err := os.ReadFile(m.marketPath())
	if err != nil {
		return reg
	}
	_ = json.Unmarshal(data, &reg)
	return reg
}

func (m *Manager) saveMarkets(reg marketRegistry) error {
	path := m.marketPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ListMarketplaces returns registered market sources.
func (m *Manager) ListMarketplaces() []Marketplace {
	return m.loadMarkets().Markets
}

// AddMarketplace registers owner/repo (or full github URL), downloads the
// repo archive, and parses .claude-plugin/marketplace.json when present.
// Equivalent to: /plugin marketplace add anthropics/skills
func (m *Manager) AddMarketplace(ctx context.Context, source string) (Marketplace, error) {
	owner, repo, err := parseGitHubSource(source)
	if err != nil {
		return Marketplace{}, err
	}
	source = owner + "/" + repo

	tmp, err := os.MkdirTemp("", "awaki-market-*")
	if err != nil {
		return Marketplace{}, err
	}
	defer os.RemoveAll(tmp)

	root, err := downloadGitHubRepo(ctx, owner, repo, tmp)
	if err != nil {
		return Marketplace{}, err
	}

	mf, plugins, err := readMarketplaceJSON(root)
	if err != nil {
		// No marketplace.json — treat whole repo as one implicit plugin of all skills
		skills := findAllSkillRoots(root)
		if len(skills) == 0 {
			return Marketplace{}, fmt.Errorf("%s: no marketplace.json and no SKILL.md found", source)
		}
		var rels []string
		for _, s := range skills {
			rel, _ := filepath.Rel(root, s)
			rels = append(rels, "./"+filepath.ToSlash(rel))
		}
		mf = marketplaceFile{Name: repo}
		mf.Metadata.Description = "GitHub skills repo " + source
		plugins = []Plugin{{
			Name:        repo,
			Description: "All skills in " + source,
			Skills:      rels,
		}}
	}

	id := mf.Name
	if id == "" {
		id = repo
	}
	market := Marketplace{
		ID:          id,
		Source:      source,
		Description: mf.Metadata.Description,
		AddedAt:     time.Now().UTC(),
		Plugins:     plugins,
	}

	reg := m.loadMarkets()
	found := false
	for i, existing := range reg.Markets {
		if strings.EqualFold(existing.ID, market.ID) || strings.EqualFold(existing.Source, market.Source) {
			reg.Markets[i] = market
			found = true
			break
		}
	}
	if !found {
		reg.Markets = append(reg.Markets, market)
	}
	if err := m.saveMarkets(reg); err != nil {
		return market, err
	}
	log.Info().Str("id", market.ID).Str("source", source).Int("plugins", len(market.Plugins)).Msg("skills: marketplace added")
	return market, nil
}

// RemoveMarketplace drops a registered market by id or source.
func (m *Manager) RemoveMarketplace(idOrSource string) error {
	reg := m.loadMarkets()
	var out []Marketplace
	removed := false
	for _, mk := range reg.Markets {
		if strings.EqualFold(mk.ID, idOrSource) || strings.EqualFold(mk.Source, idOrSource) {
			removed = true
			continue
		}
		out = append(out, mk)
	}
	if !removed {
		return fmt.Errorf("marketplace %q not found", idOrSource)
	}
	reg.Markets = out
	return m.saveMarkets(reg)
}

// InstallPlugin installs skills for plugin@marketplace
// e.g. document-skills@anthropic-agent-skills or document-skills@anthropics/skills
// If plugin is "*" or "all", installs every plugin in that marketplace.
func (m *Manager) InstallPlugin(ctx context.Context, spec string) ([]domain.Skill, error) {
	pluginName, marketRef, ok := splitPluginSpec(spec)
	if !ok {
		// bare owner/repo → add market + install all
		if strings.Contains(spec, "/") && !strings.Contains(spec, "@") {
			mk, err := m.AddMarketplace(ctx, spec)
			if err != nil {
				return nil, err
			}
			return m.installMarketPlugins(ctx, mk, "*")
		}
		return nil, fmt.Errorf("expected plugin@marketplace (e.g. document-skills@anthropic-agent-skills)")
	}

	mk, err := m.resolveMarket(ctx, marketRef)
	if err != nil {
		return nil, err
	}
	return m.installMarketPlugins(ctx, mk, pluginName)
}

func (m *Manager) resolveMarket(ctx context.Context, ref string) (Marketplace, error) {
	reg := m.loadMarkets()
	for _, mk := range reg.Markets {
		if strings.EqualFold(mk.ID, ref) || strings.EqualFold(mk.Source, ref) {
			return mk, nil
		}
	}
	// Not registered yet — if it looks like owner/repo, add it
	if strings.Contains(ref, "/") {
		return m.AddMarketplace(ctx, ref)
	}
	return Marketplace{}, fmt.Errorf("marketplace %q not registered — run: /plugin marketplace add owner/repo", ref)
}

func (m *Manager) installMarketPlugins(ctx context.Context, mk Marketplace, pluginName string) ([]domain.Skill, error) {
	owner, repo, err := parseGitHubSource(mk.Source)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "awaki-plugin-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	root, err := downloadGitHubRepo(ctx, owner, repo, tmp)
	if err != nil {
		return nil, err
	}

	// Prefer a fresh parse of marketplace.json from the zip so fields like
	// source:"./" are always available (older registry entries may lack them).
	plugins := mk.Plugins
	if _, fresh, err := readMarketplaceJSON(root); err == nil && len(fresh) > 0 {
		plugins = fresh
	}

	var targets []string
	installAll := pluginName == "*" || pluginName == "all" || pluginName == ""
	matchedPlugin := false
	for _, p := range plugins {
		if !installAll && !strings.EqualFold(p.Name, pluginName) {
			continue
		}
		matchedPlugin = true
		targets = append(targets, resolvePluginSkillDirs(root, p)...)
	}
	if !installAll && !matchedPlugin {
		return nil, fmt.Errorf("plugin %q not found in marketplace %s", pluginName, mk.ID)
	}
	if len(targets) == 0 {
		// Whole-repo fallback (marketplace with no usable plugin paths).
		targets = findAllSkillRoots(root)
	}

	// Deduplicate while preserving order.
	seen := map[string]bool{}
	var uniq []string
	for _, d := range targets {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		uniq = append(uniq, d)
	}

	var installed []domain.Skill
	for _, dir := range uniq {
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			continue
		}
		sk, err := m.installSkillDir(dir, "")
		if err != nil {
			log.Warn().Err(err).Str("dir", dir).Msg("skills: plugin skill install skipped")
			continue
		}
		installed = append(installed, sk)
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("no skills installed from %s (plugin %q) — marketplace listed the plugin but no SKILL.md was found under its source path", mk.ID, pluginName)
	}
	return installed, nil
}

// resolvePluginSkillDirs finds skill package directories for one marketplace
// plugin entry. Handles:
//   - explicit skills[] paths
//   - source: "./" or "./plugins/foo" (scan that tree for SKILL.md)
//   - Claude-style name-only plugins (plugins/<name>, skills/<name>, …)
func resolvePluginSkillDirs(root string, p Plugin) []string {
	var out []string
	for _, rel := range p.Skills {
		rel = strings.TrimPrefix(strings.TrimSpace(rel), "./")
		if rel == "" {
			continue
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}
	if len(out) > 0 {
		return out
	}

	// Resolve plugin root from source ("./" → repo root).
	src := strings.TrimSpace(p.Source)
	src = strings.TrimPrefix(src, "./")
	src = strings.Trim(src, "/")
	pluginRoot := root
	if src != "" && src != "." {
		pluginRoot = filepath.Join(root, filepath.FromSlash(src))
	}

	// Prefer every SKILL.md under the plugin root (jakubkrehel/skills style:
	// source "./" + skills/*/SKILL.md with no skills[] array).
	if found := findAllSkillRoots(pluginRoot); len(found) > 0 {
		return found
	}

	// Name-based guesses when source is empty/missing.
	if p.Name != "" {
		for _, cand := range []string{
			filepath.Join(root, "plugins", p.Name),
			filepath.Join(root, "skills", p.Name),
			filepath.Join(root, p.Name),
			filepath.Join(root, "plugins", p.Name, "skills", p.Name),
			filepath.Join(root, "skills", p.Name, "skills", p.Name),
		} {
			if _, err := os.Stat(filepath.Join(cand, "SKILL.md")); err == nil {
				out = append(out, cand)
				continue
			}
			// Plugin folder that itself contains nested skill packages.
			out = append(out, findAllSkillRoots(cand)...)
		}
	}
	return out
}

func (m *Manager) installSkillDir(root, preferredName string) (domain.Skill, error) {
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
	dest := filepath.Join(m.Dir(), name)
	_ = os.RemoveAll(dest)
	if err := copyTree(root, dest); err != nil {
		return domain.Skill{}, err
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
		return domain.Skill{Name: name, Description: fmDesc, Dir: dest, FilePath: filepath.Join(dest, "SKILL.md")}, nil
	}
	return sk, nil
}

func parseGitHubSource(source string) (owner, repo string, err error) {
	source = strings.TrimSpace(source)
	source = strings.TrimSuffix(source, ".git")
	source = strings.TrimPrefix(source, "https://github.com/")
	source = strings.TrimPrefix(source, "http://github.com/")
	source = strings.TrimPrefix(source, "github.com/")
	source = strings.Trim(source, "/")
	parts := strings.Split(source, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid source %q — use owner/repo (e.g. anthropics/skills)", source)
	}
	return parts[0], parts[1], nil
}

func splitPluginSpec(spec string) (plugin, market string, ok bool) {
	spec = strings.TrimSpace(spec)
	i := strings.LastIndex(spec, "@")
	if i <= 0 || i == len(spec)-1 {
		return "", "", false
	}
	return strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:]), true
}

func readMarketplaceJSON(root string) (marketplaceFile, []Plugin, error) {
	candidates := []string{
		filepath.Join(root, ".claude-plugin", "marketplace.json"),
		filepath.Join(root, "marketplace.json"),
	}
	var data []byte
	var err error
	for _, c := range candidates {
		data, err = os.ReadFile(c)
		if err == nil {
			break
		}
	}
	if err != nil {
		return marketplaceFile{}, nil, err
	}
	var mf marketplaceFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return marketplaceFile{}, nil, err
	}
	var plugins []Plugin
	for _, p := range mf.Plugins {
		plugins = append(plugins, Plugin{
			Name:        p.Name,
			Description: p.Description,
			Skills:      p.Skills,
			Source:      parsePluginSource(p.Source),
		})
	}
	return mf, plugins, nil
}

// parsePluginSource accepts either a string ("./plugins/foo") or Claude's
// object form {"source":"git-subdir","path":"plugins/foo",...}.
func parsePluginSource(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Source string `json:"source"`
		Path   string `json:"path"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if obj.Path != "" {
		return obj.Path
	}
	return strings.TrimSpace(obj.Source)
}

func downloadGitHubRepo(ctx context.Context, owner, repo, dest string) (root string, err error) {
	client := &http.Client{Timeout: 120 * time.Second}
	var lastErr error
	for _, branch := range []string{"main", "master"} {
		url := fmt.Sprintf("https://github.com/%s/%s/archive/refs/heads/%s.zip", owner, repo, branch)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "awaki-skills")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("github %s: HTTP %d", url, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		zipPath := filepath.Join(dest, "repo.zip")
		f, err := os.Create(zipPath)
		if err != nil {
			resp.Body.Close()
			return "", err
		}
		_, err = io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		extractDir := filepath.Join(dest, "extract")
		if err := unzipTo(zipPath, extractDir); err != nil {
			return "", err
		}
		// GitHub zipball has a single top-level folder owner-repo-hash or repo-branch
		entries, _ := os.ReadDir(extractDir)
		if len(entries) == 1 && entries[0].IsDir() {
			return filepath.Join(extractDir, entries[0].Name()), nil
		}
		return extractDir, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("could not download %s/%s", owner, repo)
	}
	return "", lastErr
}
