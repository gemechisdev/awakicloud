package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gemechischala/awaki/internal/domain"
	"github.com/rs/zerolog/log"
)

// This file implements `/skills add <source>`, the equivalent of
//
//	npx skills add <source> [--skill name]
//
// Supported sources:
//
//	owner/repo, owner/repo@skill, owner/repo/path/to/skill
//	https://github.com/owner/repo[/tree/<ref>/<path>]
//	https://skills.sh/<owner>/<repo>/<skill>
//	https://smithery.ai/skills/<owner>/<slug>
//	https://example.com            (/.well-known/skills/index.json discovery)
//	https://example.com/x.zip|.md  (direct skill archive / SKILL.md)

const (
	maxRemoteBytes  = 64 << 20 // total download cap for one archive
	maxRemoteFileSz = 10 << 20 // cap per individual file
	maxRemoteFiles  = 500      // cap on files per skill
	maxBulkInstall  = 25       // refuse "install everything" above this unless --all

	// smitheryAPI is the public JSON endpoint that returns a skill's gitUrl
	// (and metadata). Example:
	//   GET https://api.smithery.ai/skills/anthropics/frontend-design
	//   → {"gitUrl":"https://github.com/anthropics/claude-code/tree/main/..."}
	smitheryAPI = "https://api.smithery.ai/skills/"
)

// InstallOptions mirrors the `skills` CLI flags we support.
type InstallOptions struct {
	Skill string // --skill <name>: install only this skill from a multi-skill source
	All   bool   // --all: allow installing more than maxBulkInstall skills
}

var (
	errNoWellKnown = errors.New("no /.well-known/skills index found")
	errNotSkill    = errors.New("not a skill archive or SKILL.md")
)

// ParseAddArgs parses the arguments of `/skills add ...`. It also accepts the
// full pasted CLI command (`npx -y skills add <url> --skill x`) by ignoring
// the launcher words.
func ParseAddArgs(fields []string) (string, InstallOptions, error) {
	var opts InstallOptions
	var src string
	noise := map[string]bool{
		"npx": true, "bunx": true, "pnpm": true, "dlx": true, "yarn": true,
		"-y": true, "--yes": true, "-g": true, "--global": true,
		"skills": true, "add": true, "install": true,
	}
	for i := 0; i < len(fields); i++ {
		f := strings.Trim(strings.TrimSpace(fields[i]), "\"'`<>")
		switch {
		case f == "":
			continue
		case f == "--skill" || f == "-s":
			if i+1 >= len(fields) {
				return "", opts, fmt.Errorf("%s needs a skill name", f)
			}
			i++
			opts.Skill = strings.Trim(strings.TrimSpace(fields[i]), "\"'`<>")
		case strings.HasPrefix(f, "--skill="):
			opts.Skill = strings.Trim(strings.TrimPrefix(f, "--skill="), "\"'`<>")
		case f == "--all":
			opts.All = true
		case strings.HasPrefix(f, "-"):
			continue // other CLI flags (agent targets, etc.) don't apply here
		case src == "" && noise[strings.ToLower(f)]:
			continue
		case src == "":
			src = f
		}
	}
	if src == "" {
		return "", opts, fmt.Errorf("missing skill source")
	}
	return src, opts, nil
}

// LooksLikeSource reports whether s plausibly names an installable source
// (used so `/skills <url>` works without the `add` keyword).
func LooksLikeSource(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return true
	}
	if _, ok := parseGitHubRef(s); ok {
		return true
	}
	first := strings.SplitN(s, "/", 2)[0]
	return strings.Contains(first, ".") && strings.Contains(s, "/")
}

// InstallFromSource installs skill(s) from any supported source.
func (m *Manager) InstallFromSource(ctx context.Context, source string, opts InstallOptions) ([]domain.Skill, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("missing skill source")
	}
	c := newRemoteClient()

	if g, ok := parseSkillsSh(source); ok {
		return m.installGitHub(ctx, c, g, opts)
	}
	if g, ok := parseGitHubRef(source); ok {
		return m.installGitHub(ctx, c, g, opts)
	}

	u, err := normalizeURL(source)
	if err != nil {
		return nil, err
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "smithery.ai" {
		return m.installSmithery(ctx, c, u, opts)
	}

	ext := strings.ToLower(path.Ext(u.Path))
	if ext == ".zip" || ext == ".md" {
		return m.installDirect(ctx, c, u)
	}

	skills, err := m.installWellKnown(ctx, c, u, opts.Skill, opts)
	if err == nil {
		return skills, nil
	}
	if !errors.Is(err, errNoWellKnown) {
		return nil, err
	}
	skills, derr := m.installDirect(ctx, c, u)
	if derr == nil {
		return skills, nil
	}
	if errors.Is(derr, errNotSkill) {
		return nil, fmt.Errorf("%s: no /.well-known/skills/index.json and the URL is not a skill archive or SKILL.md", source)
	}
	return nil, derr
}

// ---------------------------------------------------------------------------
// Source parsing
// ---------------------------------------------------------------------------

type ghRef struct {
	Owner, Repo string
	Ref         string // branch/tag/sha; empty = default branch
	Subpath     string // restrict to skills under this repo path
	Skill       string // skill (directory) name filter
}

var (
	ghOwnerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	ghRepoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func parseGitHubRef(src string) (ghRef, bool) {
	var g ghRef
	s := strings.TrimSpace(src)
	hadScheme := strings.Contains(s, "://")

	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	// owner/repo@skill (skills CLI shorthand)
	if !hadScheme {
		if i := strings.LastIndex(s, "@"); i > 0 && !strings.Contains(s[i:], "/") {
			g.Skill = s[i+1:]
			s = s[:i]
		}
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	hadHost := false
	if strings.HasPrefix(s, "github.com/") {
		s = strings.TrimPrefix(s, "github.com/")
		hadHost = true
	}
	if hadScheme && !hadHost {
		return ghRef{}, false
	}
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ghRef{}, false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if !ghOwnerRe.MatchString(parts[0]) || !ghRepoRe.MatchString(repo) || repo == "" {
		return ghRef{}, false
	}
	g.Owner, g.Repo = parts[0], repo

	rest := parts[2:]
	if len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "blob") {
		isBlob := rest[0] == "blob"
		g.Ref = rest[1]
		rest = rest[2:]
		if isBlob && len(rest) > 0 && strings.EqualFold(rest[len(rest)-1], "SKILL.md") {
			rest = rest[:len(rest)-1]
		}
	}
	g.Subpath = strings.Join(rest, "/")
	return g, true
}

// parseSkillsSh handles https://skills.sh/<owner>/<repo>[/<skill>].
func parseSkillsSh(src string) (ghRef, bool) {
	if !strings.Contains(src, "://") {
		src = "https://" + src
	}
	u, err := url.Parse(src)
	if err != nil {
		return ghRef{}, false
	}
	if strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") != "skills.sh" {
		return ghRef{}, false
	}
	parts := splitPath(u.Path)
	if len(parts) < 2 || parts[0] == "site" || !ghOwnerRe.MatchString(parts[0]) {
		return ghRef{}, false
	}
	g := ghRef{Owner: parts[0], Repo: parts[1]}
	if len(parts) >= 3 {
		g.Skill = parts[2]
	}
	return g, true
}

func normalizeURL(src string) (*url.URL, error) {
	s := strings.TrimSpace(src)
	if !strings.Contains(s, "://") {
		first := strings.SplitN(s, "/", 2)[0]
		if !strings.Contains(first, ".") {
			return nil, fmt.Errorf("unsupported source %q — use a URL, owner/repo, or owner/repo@skill", src)
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", src, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("unsupported URL scheme %q (use http or https)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid URL %q", src)
	}
	return u, nil
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP client (blocks loopback / private / link-local targets)
// ---------------------------------------------------------------------------

type remoteClient struct {
	http *http.Client
}

func newRemoteClient() *remoteClient {
	dialer := &net.Dialer{
		Timeout: 20 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return fmt.Errorf("blocked non-public address %s", host)
			}
			return nil
		},
	}
	return &remoteClient{http: &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 20 * time.Second,
		},
	}}
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// get downloads rawURL (max limit bytes) and returns body + Content-Type.
func (c *remoteClient) get(ctx context.Context, rawURL string, limit int64, headers map[string]string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "awaki-skills")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("GET %s: response larger than %d MB", rawURL, limit>>20)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func githubHeaders(accept string) map[string]string {
	h := map[string]string{}
	if accept != "" {
		h["Accept"] = accept
	}
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

// ---------------------------------------------------------------------------
// GitHub (tree API: downloads only the selected skill folders)
// ---------------------------------------------------------------------------

type ghEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	Size int64  `json:"size"`
}

type ghTree struct {
	Tree      []ghEntry `json:"tree"`
	Truncated bool      `json:"truncated"`
}

func (m *Manager) installGitHub(ctx context.Context, c *remoteClient, g ghRef, opts InstallOptions) ([]domain.Skill, error) {
	want := opts.Skill
	if want == "" {
		want = g.Skill
	}
	ref := g.Ref
	if ref == "" {
		ref = "HEAD"
	}

	treeURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", g.Owner, g.Repo, url.PathEscape(ref))
	body, _, err := c.get(ctx, treeURL, 64<<20, githubHeaders("application/vnd.github+json"))
	var tree ghTree
	if err == nil {
		err = json.Unmarshal(body, &tree)
	}
	if err != nil || tree.Truncated {
		// API rate-limited / repo too large for one tree response: fall back to
		// the repo zip (fine for normal-size repos).
		log.Warn().Err(err).Str("repo", g.Owner+"/"+g.Repo).Msg("skills: github tree API unavailable, using zip download")
		return m.installGitHubZip(ctx, c, g, want, opts)
	}

	files := map[string]ghEntry{}
	var dirs []string
	for _, e := range tree.Tree {
		if e.Type != "blob" {
			continue
		}
		files[e.Path] = e
		if strings.EqualFold(path.Base(e.Path), "SKILL.md") {
			dirs = append(dirs, path.Dir(e.Path))
		}
	}
	picked, err := selectSkillDirs(dirs, g.Subpath, want, opts.All)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", g.Owner, g.Repo, err)
	}

	tmp, err := os.MkdirTemp("", "awaki-skill-gh-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var installed []domain.Skill
	for i, d := range picked {
		prefix := d + "/"
		localName := path.Base(d)
		if d == "." {
			prefix = ""
			localName = g.Repo
		}
		var rels []string
		for p := range files {
			if strings.HasPrefix(p, ".git/") {
				continue
			}
			if prefix == "" || strings.HasPrefix(p, prefix) {
				rels = append(rels, p)
			}
		}
		sort.Strings(rels)
		if len(rels) > maxRemoteFiles {
			return installed, fmt.Errorf("skill %q has %d files (limit %d)", d, len(rels), maxRemoteFiles)
		}
		local := filepath.Join(tmp, fmt.Sprintf("%d", i), localName)
		for _, p := range rels {
			e := files[p]
			if e.Size > maxRemoteFileSz {
				log.Warn().Str("file", p).Int64("size", e.Size).Msg("skills: skipping oversized file")
				continue
			}
			data, _, err := c.get(ctx, rawGitHubURL(g, ref, p), maxRemoteFileSz, githubHeaders(""))
			if err != nil {
				return installed, err
			}
			if err := writeSafe(local, strings.TrimPrefix(p, prefix), data, e.Mode == "100755"); err != nil {
				return installed, err
			}
		}
		sk, err := m.installSkillDir(local, "")
		if err != nil {
			return installed, err
		}
		installed = append(installed, sk)
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("%s/%s: nothing installed", g.Owner, g.Repo)
	}
	return installed, nil
}

func rawGitHubURL(g ghRef, ref, p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", g.Owner, g.Repo, url.PathEscape(ref), strings.Join(segs, "/"))
}

// installGitHubZip is the fallback path: download the repo archive, then apply
// the same selection rules on the extracted tree.
func (m *Manager) installGitHubZip(ctx context.Context, c *remoteClient, g ghRef, want string, opts InstallOptions) ([]domain.Skill, error) {
	tmp, err := os.MkdirTemp("", "awaki-skill-zip-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	urls := []string{
		fmt.Sprintf("https://github.com/%s/%s/archive/refs/heads/main.zip", g.Owner, g.Repo),
		fmt.Sprintf("https://github.com/%s/%s/archive/refs/heads/master.zip", g.Owner, g.Repo),
	}
	if g.Ref != "" {
		urls = []string{fmt.Sprintf("https://github.com/%s/%s/archive/%s.zip", g.Owner, g.Repo, url.PathEscape(g.Ref))}
	}
	var data []byte
	var lastErr error
	for _, u := range urls {
		data, _, lastErr = c.get(ctx, u, maxRemoteBytes, githubHeaders(""))
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	zipPath := filepath.Join(tmp, "repo.zip")
	if err := os.WriteFile(zipPath, data, 0o600); err != nil {
		return nil, err
	}
	extract := filepath.Join(tmp, "extract")
	if err := unzipTo(zipPath, extract); err != nil {
		return nil, err
	}
	root := extract
	if entries, _ := os.ReadDir(extract); len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(extract, entries[0].Name())
	}

	var dirs []string
	for _, r := range findAllSkillRoots(root) {
		rel, err := filepath.Rel(root, r)
		if err != nil {
			continue
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	picked, err := selectSkillDirs(dirs, g.Subpath, want, opts.All)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", g.Owner, g.Repo, err)
	}
	var installed []domain.Skill
	for _, d := range picked {
		sk, err := m.installSkillDir(filepath.Join(root, filepath.FromSlash(d)), "")
		if err != nil {
			return installed, err
		}
		installed = append(installed, sk)
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("%s/%s: nothing installed", g.Owner, g.Repo)
	}
	return installed, nil
}

// selectSkillDirs picks skill directories (repo-relative, slash-separated;
// "." = repo root) matching the optional subpath and skill-name filters.
func selectSkillDirs(dirs []string, subpath, name string, all bool) ([]string, error) {
	uniq := map[string]bool{}
	var list []string
	for _, d := range dirs {
		if !uniq[d] {
			uniq[d] = true
			list = append(list, d)
		}
	}
	sort.Strings(list)

	// Drop skill folders nested inside another skill folder (bundled examples).
	var top []string
	for _, d := range list {
		nested := false
		for _, o := range list {
			if o != d && o != "." && strings.HasPrefix(d, o+"/") {
				nested = true
				break
			}
		}
		if !nested {
			top = append(top, d)
		}
	}

	if sub := strings.Trim(subpath, "/"); sub != "" {
		var f []string
		for _, d := range top {
			if d == sub || strings.HasPrefix(d, sub+"/") {
				f = append(f, d)
			}
		}
		top = f
	}

	available := make([]string, 0, len(top))
	for _, d := range top {
		available = append(available, path.Base(d))
	}

	if name != "" {
		target := sanitize(name)
		var f []string
		for _, d := range top {
			b := path.Base(d)
			if strings.EqualFold(b, name) || sanitize(b) == target {
				f = append(f, d)
			}
		}
		top = f
	}

	if len(top) == 0 {
		if name != "" {
			hint := available
			if len(hint) > 20 {
				hint = append(hint[:20:20], "…")
			}
			return nil, fmt.Errorf("no skill named %q found (available: %s)", name, strings.Join(hint, ", "))
		}
		return nil, fmt.Errorf("no SKILL.md found")
	}
	if name == "" && len(top) > maxBulkInstall && !all {
		return nil, fmt.Errorf("source contains %d skills — pick one with --skill <name> or pass --all", len(top))
	}
	return top, nil
}

// ---------------------------------------------------------------------------
// /.well-known/skills discovery (npx skills add https://example.com)
// ---------------------------------------------------------------------------

type wellKnownIndex struct {
	Skills []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Files       []string `json:"files"`
		URL         string   `json:"url"`
	} `json:"skills"`
}

func wellKnownCandidates(base *url.URL) []string {
	var out []string
	seen := map[string]bool{}
	add := func(prefix string) {
		for _, d := range []string{".well-known/skills", ".well-known/agent-skills"} {
			u := prefix + "/" + d + "/index.json"
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	origin := base.Scheme + "://" + base.Host
	if p := strings.TrimRight(base.Path, "/"); p != "" {
		add(origin + p)
	}
	add(origin)
	return out
}

func (m *Manager) installWellKnown(ctx context.Context, c *remoteClient, base *url.URL, want string, opts InstallOptions) ([]domain.Skill, error) {
	var idx wellKnownIndex
	var idxURL string
	for _, cand := range wellKnownCandidates(base) {
		data, _, err := c.get(ctx, cand, 8<<20, nil)
		if err != nil {
			continue
		}
		var parsed wellKnownIndex
		if json.Unmarshal(data, &parsed) != nil || len(parsed.Skills) == 0 {
			continue
		}
		idx, idxURL = parsed, cand
		break
	}
	if idxURL == "" {
		return nil, errNoWellKnown
	}
	idxBase, err := url.Parse(idxURL)
	if err != nil {
		return nil, err
	}

	target := sanitize(want)
	var picked []int
	var names []string
	for i, s := range idx.Skills {
		names = append(names, s.Name)
		if want == "" || strings.EqualFold(s.Name, want) || sanitize(s.Name) == target {
			picked = append(picked, i)
		}
	}
	if len(picked) == 0 {
		if len(names) > 20 {
			names = append(names[:20:20], "…")
		}
		return nil, fmt.Errorf("no skill named %q in %s (available: %s)", want, idxURL, strings.Join(names, ", "))
	}
	if want == "" && len(picked) > maxBulkInstall && !opts.All {
		return nil, fmt.Errorf("%s lists %d skills — pick one with --skill <name> or pass --all", idxURL, len(picked))
	}

	tmp, err := os.MkdirTemp("", "awaki-skill-wk-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var installed []domain.Skill
	for n, i := range picked {
		s := idx.Skills[i]
		if sanitize(s.Name) == "" {
			continue
		}
		// Entry points at an archive / single file instead of a file list.
		if len(s.Files) == 0 && s.URL != "" {
			ref, err := idxBase.Parse(s.URL)
			if err != nil {
				return installed, err
			}
			got, err := m.installDirect(ctx, c, ref)
			if err != nil {
				return installed, err
			}
			installed = append(installed, got...)
			continue
		}
		files := s.Files
		if len(files) == 0 {
			files = []string{"SKILL.md"}
		}
		if len(files) > maxRemoteFiles {
			return installed, fmt.Errorf("skill %q lists %d files (limit %d)", s.Name, len(files), maxRemoteFiles)
		}
		local := filepath.Join(tmp, fmt.Sprintf("%d", n), sanitize(s.Name))
		for _, rel := range files {
			rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
			data, err := fetchWellKnownFile(ctx, c, idxBase, base, s.Name, rel)
			if err != nil {
				return installed, err
			}
			if err := writeSafe(local, rel, data, false); err != nil {
				return installed, err
			}
		}
		sk, err := m.installSkillDir(local, "")
		if err != nil {
			return installed, err
		}
		installed = append(installed, sk)
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("%s: nothing installed", idxURL)
	}
	return installed, nil
}

// fetchWellKnownFile tries several relative locations for a file listed in a
// well-known skills index. Sites disagree on whether files live next to the
// index, under a skill-name folder, or at the skill page path.
func fetchWellKnownFile(ctx context.Context, c *remoteClient, idxBase, page *url.URL, skillName, rel string) ([]byte, error) {
	segs := strings.Split(rel, "/")
	for j := range segs {
		segs[j] = url.PathEscape(segs[j])
	}
	escaped := strings.Join(segs, "/")
	nameEsc := url.PathEscape(skillName)

	var candidates []string
	add := func(u *url.URL, ref string) {
		if u == nil {
			return
		}
		r, err := u.Parse(ref)
		if err != nil {
			return
		}
		candidates = append(candidates, r.String())
	}
	// Common layouts relative to the index.json location.
	add(idxBase, nameEsc+"/"+escaped)
	add(idxBase, escaped)
	// Relative to the skill page URL the user pasted.
	add(page, escaped)
	add(page, nameEsc+"/"+escaped)
	// Origin-rooted guesses.
	if page != nil {
		origin := &url.URL{Scheme: page.Scheme, Host: page.Host}
		add(origin, nameEsc+"/"+escaped)
		add(origin, "skills/"+nameEsc+"/"+escaped)
	}

	var lastErr error
	seen := map[string]bool{}
	for _, raw := range candidates {
		if seen[raw] {
			continue
		}
		seen[raw] = true
		data, _, err := c.get(ctx, raw, maxRemoteFileSz, nil)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate URL for %s/%s", skillName, rel)
	}
	return nil, lastErr
}

// ---------------------------------------------------------------------------
// Direct .zip / SKILL.md URL
// ---------------------------------------------------------------------------

func (m *Manager) installDirect(ctx context.Context, c *remoteClient, u *url.URL) ([]domain.Skill, error) {
	data, _, err := c.get(ctx, u.String(), maxRemoteBytes, nil)
	if err != nil {
		return nil, err
	}
	isZip := len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 3 && data[3] == 4
	if isZip {
		f, err := os.CreateTemp("", "awaki-skill-dl-*.zip")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		_, werr := f.Write(data)
		_ = f.Close()
		if werr != nil {
			return nil, werr
		}
		return m.InstallZip(f.Name(), "")
	}

	text := strings.TrimPrefix(string(data), "\ufeff")
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "---") && strings.ToLower(path.Ext(u.Path)) != ".md" {
		return nil, errNotSkill
	}
	fmName, _ := parseFrontmatter(trimmed)
	hint := ""
	if fmName == "" {
		segs := splitPath(u.Path)
		if n := len(segs); n > 0 {
			last := segs[n-1]
			if strings.EqualFold(last, "SKILL.md") && n > 1 {
				hint = segs[n-2]
			} else {
				hint = strings.TrimSuffix(last, path.Ext(last))
			}
		}
	}
	f, err := os.CreateTemp("", "awaki-skill-dl-*.md")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(text)
	_ = f.Close()
	if werr != nil {
		return nil, werr
	}
	sk, err := m.InstallMarkdownFile(f.Name(), hint)
	if err != nil {
		return nil, err
	}
	return []domain.Skill{sk}, nil
}

// ---------------------------------------------------------------------------
// Smithery
// ---------------------------------------------------------------------------

// installSmithery handles https://smithery.ai/skills/<owner>/<slug>.
// Strategy (in order):
//  1. api.smithery.ai → gitUrl → GitHub install (path from API)
//  2. Same gitUrl but ignore Subpath (stale path under a still-valid repo)
//  3. Common mirror repos: {owner}/skills, anthropics/skills with skill=slug
//  4. Well-known index on the skill page
func (m *Manager) installSmithery(ctx context.Context, c *remoteClient, u *url.URL, opts InstallOptions) ([]domain.Skill, error) {
	parts := splitPath(u.Path)
	var owner, slug string
	switch {
	case len(parts) >= 3 && parts[0] == "skills":
		owner, slug = parts[1], parts[2]
	case len(parts) >= 2 && parts[0] != "skills":
		owner, slug = parts[0], parts[1]
	}
	if owner == "" || slug == "" {
		return nil, fmt.Errorf("unrecognised Smithery URL %q — expected https://smithery.ai/skills/<owner>/<skill>", u.String())
	}

	skillName := opts.Skill
	if skillName == "" {
		skillName = slug
	}

	var errs []string
	tryGH := func(label string, g ghRef, o InstallOptions) ([]domain.Skill, bool) {
		sk, err := m.installGitHub(ctx, c, g, o)
		if err == nil {
			return sk, true
		}
		errs = append(errs, label+": "+err.Error())
		return nil, false
	}

	// 1–2) Official API gitUrl, then same repo without the (often stale) subpath.
	if g, metaSkill, err := fetchSmitheryGitRef(ctx, c, owner, slug); err != nil {
		errs = append(errs, "api: "+err.Error())
	} else {
		o := opts
		if o.Skill == "" {
			if metaSkill != "" {
				o.Skill = metaSkill
			} else if base := path.Base(g.Subpath); base != "" && base != "." {
				o.Skill = base
			} else {
				o.Skill = skillName
			}
		}
		if sk, ok := tryGH("api-gitUrl", g, o); ok {
			return sk, nil
		}
		// Stale subpath (Smithery often links into claude-code paths that no longer exist).
		if g.Subpath != "" {
			g2 := g
			g2.Subpath = ""
			if sk, ok := tryGH("api-repo", g2, o); ok {
				return sk, nil
			}
		}
	}

	// 3) Conventional skill registries / Claude plugin monorepos.
	//    Smithery's gitUrl for Anthropic skills is often stale (points into
	//    claude-code paths that no longer hold SKILL.md). The real content
	//    lives in anthropics/skills or anthropics/claude-plugins-official.
	guesses := []ghRef{
		{Owner: owner, Repo: "skills"},
		{Owner: owner, Repo: "agent-skills"},
		{Owner: owner, Repo: "claude-plugins-official", Subpath: "plugins/" + slug},
		{Owner: "anthropics", Repo: "skills"},
		{Owner: "anthropics", Repo: "claude-plugins-official", Subpath: "plugins/" + slug},
		{Owner: "anthropics", Repo: "claude-code"}, // last resort; marketplace may list plugins here
	}
	seen := map[string]bool{}
	for _, g := range guesses {
		key := g.Owner + "/" + g.Repo
		if g.Subpath != "" {
			key += "@" + g.Subpath
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		o := InstallOptions{Skill: skillName, All: opts.All}
		if sk, ok := tryGH(key, g, o); ok {
			return sk, nil
		}
	}

	// 3b) Marketplace / plugin path (same as /plugin install <slug>@…).
	//     Auto-add the owner/claude-code marketplace when present and install
	//     the matching plugin — this is how Claude Code itself ships skills.
	if sk, err := m.trySmitheryAsPlugin(ctx, owner, slug, skillName); err == nil {
		return sk, nil
	} else if err != nil {
		errs = append(errs, "plugin: "+err.Error())
	}

	// 4) Well-known index (name is usually "<owner>-<slug>").
	candidates := []string{owner + "-" + slug, slug, skillName}
	if opts.Skill != "" {
		candidates = append([]string{opts.Skill}, candidates...)
	}
	tried := map[string]bool{}
	for _, name := range candidates {
		if name == "" || tried[name] {
			continue
		}
		tried[name] = true
		sk, err := m.installWellKnown(ctx, c, u, name, opts)
		if err == nil {
			return sk, nil
		}
		if !errors.Is(err, errNoWellKnown) {
			errs = append(errs, "well-known: "+err.Error())
		}
	}

	return nil, fmt.Errorf("could not install %s from Smithery. "+
		"Try the source repo directly, e.g. /skills add %s/skills --skill %s. Details: %s",
		u.String(), owner, skillName, strings.Join(errs, "; "))
}

// trySmitheryAsPlugin installs via the Claude-compatible marketplace flow:
// register a known marketplace for the namespace, then install the plugin/skill
// by name. Content for Anthropic plugins lives in claude-plugins-official;
// the claude-code repo only holds the marketplace index.
func (m *Manager) trySmitheryAsPlugin(ctx context.Context, owner, slug, skillName string) ([]domain.Skill, error) {
	candidates := []string{
		owner + "/claude-plugins-official",
		owner + "/skills",
		owner + "/claude-code",
		"anthropics/claude-plugins-official",
		"anthropics/skills",
		"anthropics/claude-code",
	}
	var lastErr error
	seen := map[string]bool{}
	for _, src := range candidates {
		if seen[src] {
			continue
		}
		seen[src] = true
		mk, err := m.AddMarketplace(ctx, src)
		if err != nil {
			lastErr = err
			continue
		}
		for _, name := range []string{slug, skillName} {
			if name == "" {
				continue
			}
			sk, err := m.installMarketPlugins(ctx, mk, name)
			if err == nil && len(sk) > 0 {
				return sk, nil
			}
			if err != nil {
				lastErr = err
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no matching plugin for %s/%s", owner, slug)
	}
	return nil, lastErr
}

// fetchSmitheryGitRef calls the Smithery skill API and returns a parsed GitHub
// ref plus a preferred skill directory name.
func fetchSmitheryGitRef(ctx context.Context, c *remoteClient, owner, slug string) (ghRef, string, error) {
	apiURL := smitheryAPI + url.PathEscape(owner) + "/" + url.PathEscape(slug)
	data, _, err := c.get(ctx, apiURL, 1<<20, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return ghRef{}, "", err
	}
	var meta struct {
		GitURL      string `json:"gitUrl"`
		DisplayName string `json:"displayName"`
		Slug        string `json:"slug"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ghRef{}, "", fmt.Errorf("decode smithery api: %w", err)
	}
	if meta.GitURL == "" {
		return ghRef{}, "", fmt.Errorf("smithery api returned no gitUrl for %s/%s", owner, slug)
	}
	g, ok := parseGitHubRef(meta.GitURL)
	if !ok {
		return ghRef{}, "", fmt.Errorf("smithery gitUrl is not a GitHub path: %s", meta.GitURL)
	}
	name := meta.DisplayName
	if name == "" {
		name = meta.Slug
	}
	if name == "" {
		name = slug
	}
	return g, name, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeSafe writes data to root/rel, refusing absolute paths and ".." escapes.
func writeSafe(root, rel string, data []byte, exec bool) error {
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("unsafe file path %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "" {
			return fmt.Errorf("unsafe file path %q", rel)
		}
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	return os.WriteFile(target, data, mode)
}
