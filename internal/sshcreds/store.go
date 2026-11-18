// Package sshcreds manages SSH identities for git and generic SSH hosts.
//
// Layout under {DataDir}/ssh/:
//
//	config                 — generated ssh_config (Host blocks)
//	identities.json        — metadata (disk cache)
//	keys/{id}              — private key (mode 0600)
//	keys/{id}.pub          — public key (optional)
//
// When a Persister is attached (SQLite), identities also live in the DB so they
// survive redeploys / Telegram DB snapshots. Disk files are materialised on
// hydrate so ssh/git can use them.
//
// Also keeps ~/.ssh/config in sync inside a managed marker section.
package sshcreds

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	managedBegin = "# BEGIN AWAKI MANAGED SSH"
	managedEnd   = "# END AWAKI MANAGED SSH"
)

// Identity is one Host entry + key pair.
type Identity struct {
	ID           string    `json:"id"`
	Host         string    `json:"host"`          // Host alias, e.g. github.com-gemechisdev
	HostName     string    `json:"host_name"`     // real hostname, e.g. github.com
	User         string    `json:"user"`          // usually "git"
	IdentityFile string    `json:"identity_file"` // absolute path to private key on disk
	PublicKey    string    `json:"public_key,omitempty"`
	GitName      string    `json:"git_name,omitempty"`
	GitEmail     string    `json:"git_email,omitempty"`
	GitHubUser   string    `json:"github_user,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// DurableRow is what the DB layer stores (includes private key bytes).
type DurableRow struct {
	ID         string
	Host       string
	HostName   string
	User       string
	PrivateKey []byte
	PublicKey  string
	GitName    string
	GitEmail   string
	GitHubUser string
	CreatedAt  string
}

// Persister is implemented by the SQLite store so identities survive redeploy.
type Persister interface {
	ListSSHIdentities() ([]DurableRow, error)
	UpsertSSHIdentity(DurableRow) error
	DeleteSSHIdentity(id string) error
}

// Store manages identities on disk (+ optional durable backend).
type Store struct {
	mu      sync.Mutex
	dataDir string
	sshDir  string
	db      Persister // optional
}

func New(dataDir string) *Store {
	return &Store{
		dataDir: dataDir,
		sshDir:  filepath.Join(dataDir, "ssh"),
	}
}

// SetPersister attaches the DB backend and hydrates disk from it.
func (s *Store) SetPersister(p Persister) error {
	s.mu.Lock()
	s.db = p
	s.mu.Unlock()
	return s.HydrateFromDB()
}

func (s *Store) Dir() string        { return s.sshDir }
func (s *Store) ConfigPath() string { return filepath.Join(s.sshDir, "config") }

func (s *Store) ensure() error {
	return os.MkdirAll(filepath.Join(s.sshDir, "keys"), 0o700)
}

func (s *Store) metaPath() string { return filepath.Join(s.sshDir, "identities.json") }

func (s *Store) loadLocked() ([]Identity, error) {
	data, err := os.ReadFile(s.metaPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []Identity
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) saveLocked(list []Identity) error {
	if err := s.ensure(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(), data, 0o600)
}

// List returns all identities (copy).
func (s *Store) List() ([]Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Get returns one identity by id or host alias.
func (s *Store) Get(idOrHost string) (Identity, bool, error) {
	list, err := s.List()
	if err != nil {
		return Identity{}, false, err
	}
	for _, id := range list {
		if strings.EqualFold(id.ID, idOrHost) || strings.EqualFold(id.Host, idOrHost) {
			return id, true, nil
		}
	}
	// Also match by GitHub username → github.com-{user}
	for _, id := range list {
		if id.GitHubUser != "" && strings.EqualFold(id.GitHubUser, idOrHost) {
			return id, true, nil
		}
	}
	return Identity{}, false, nil
}

// ResolveHostForGitHubUser returns the Host alias to use for a GitHub username,
// or empty if none is configured.
func (s *Store) ResolveHostForGitHubUser(username string) string {
	username = strings.TrimSpace(strings.TrimPrefix(username, "@"))
	if username == "" {
		return ""
	}
	list, err := s.List()
	if err != nil {
		return ""
	}
	// Prefer exact github_user match
	for _, id := range list {
		if strings.EqualFold(id.GitHubUser, username) {
			return id.Host
		}
	}
	// Convention: github.com-{username}
	want := "github.com-" + username
	for _, id := range list {
		if strings.EqualFold(id.Host, want) {
			return id.Host
		}
	}
	// Single identity whose Host is plain github.com — treat as default only
	// when the username matches nothing else (caller decides).
	return ""
}

var safeIDRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	s = safeIDRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "key"
	}
	return s
}

// AddInput is the payload for creating an identity.
type AddInput struct {
	Host       string
	HostName   string
	User       string
	PrivateKey string
	PublicKey  string
	GitName    string
	GitEmail   string
	GitHubUser string
	ID         string
}

// Add writes key files, updates metadata, regenerates ssh config, and persists to DB.
func (s *Store) Add(in AddInput) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensure(); err != nil {
		return Identity{}, err
	}
	host := strings.TrimSpace(in.Host)
	if host == "" {
		return Identity{}, fmt.Errorf("host alias is required")
	}
	priv := strings.TrimSpace(in.PrivateKey)
	if priv == "" {
		return Identity{}, fmt.Errorf("private key is required")
	}
	if !looksLikePrivateKey(priv) {
		return Identity{}, fmt.Errorf("private key does not look like an OpenSSH/PEM key (missing BEGIN header)")
	}
	if !strings.HasSuffix(priv, "\n") {
		priv += "\n"
	}

	hostName := strings.TrimSpace(in.HostName)
	if hostName == "" {
		hostName = "github.com"
	}
	user := strings.TrimSpace(in.User)
	if user == "" {
		user = "git"
	}

	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = sanitizeID(host)
	} else {
		id = sanitizeID(id)
	}

	list, err := s.loadLocked()
	if err != nil {
		return Identity{}, err
	}
	var next []Identity
	for _, existing := range list {
		if existing.ID == id || strings.EqualFold(existing.Host, host) {
			_ = os.Remove(existing.IdentityFile)
			_ = os.Remove(existing.IdentityFile + ".pub")
			continue
		}
		next = append(next, existing)
	}

	keyPath := filepath.Join(s.sshDir, "keys", id)
	if err := os.WriteFile(keyPath, []byte(priv), 0o600); err != nil {
		return Identity{}, fmt.Errorf("write private key: %w", err)
	}

	pub := strings.TrimSpace(in.PublicKey)
	if pub != "" {
		if !strings.HasSuffix(pub, "\n") {
			pub += "\n"
		}
		if err := os.WriteFile(keyPath+".pub", []byte(pub), 0o644); err != nil {
			return Identity{}, fmt.Errorf("write public key: %w", err)
		}
	}

	ident := Identity{
		ID:           id,
		Host:         host,
		HostName:     hostName,
		User:         user,
		IdentityFile: keyPath,
		PublicKey:    strings.TrimSpace(pub),
		GitName:      strings.TrimSpace(in.GitName),
		GitEmail:     strings.TrimSpace(in.GitEmail),
		GitHubUser:   strings.TrimSpace(strings.TrimPrefix(in.GitHubUser, "@")),
		CreatedAt:    time.Now().UTC(),
	}
	next = append(next, ident)
	if err := s.saveLocked(next); err != nil {
		return Identity{}, err
	}
	if err := s.writeConfigsLocked(next); err != nil {
		return Identity{}, err
	}
	// Durable persist
	if s.db != nil {
		_ = s.db.UpsertSSHIdentity(DurableRow{
			ID:         ident.ID,
			Host:       ident.Host,
			HostName:   ident.HostName,
			User:       ident.User,
			PrivateKey: []byte(priv),
			PublicKey:  ident.PublicKey,
			GitName:    ident.GitName,
			GitEmail:   ident.GitEmail,
			GitHubUser: ident.GitHubUser,
			CreatedAt:  ident.CreatedAt.Format(time.RFC3339Nano),
		})
	}
	return ident, nil
}

// Remove deletes an identity by id or host alias.
func (s *Store) Remove(idOrHost string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked()
	if err != nil {
		return err
	}
	var next []Identity
	var removedID string
	found := false
	for _, id := range list {
		if strings.EqualFold(id.ID, idOrHost) || strings.EqualFold(id.Host, idOrHost) {
			_ = os.Remove(id.IdentityFile)
			_ = os.Remove(id.IdentityFile + ".pub")
			found = true
			removedID = id.ID
			continue
		}
		next = append(next, id)
	}
	if !found {
		return fmt.Errorf("ssh identity %q not found", idOrHost)
	}
	if err := s.saveLocked(next); err != nil {
		return err
	}
	if err := s.writeConfigsLocked(next); err != nil {
		return err
	}
	if s.db != nil && removedID != "" {
		_ = s.db.DeleteSSHIdentity(removedID)
	}
	return nil
}

// RebuildConfig regenerates managed config files from current identities.
func (s *Store) RebuildConfig() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked()
	if err != nil {
		return err
	}
	return s.writeConfigsLocked(list)
}

// HydrateFromDB materialises key files + config from the durable store.
// Call after SetPersister / on process start so redeploys restore keys to disk.
func (s *Store) HydrateFromDB() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	rows, err := s.db.ListSSHIdentities()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.ensure(); err != nil {
		return err
	}
	var list []Identity
	for _, r := range rows {
		keyPath := filepath.Join(s.sshDir, "keys", r.ID)
		if len(r.PrivateKey) > 0 {
			if err := os.WriteFile(keyPath, r.PrivateKey, 0o600); err != nil {
				return err
			}
		}
		if strings.TrimSpace(r.PublicKey) != "" {
			pub := r.PublicKey
			if !strings.HasSuffix(pub, "\n") {
				pub += "\n"
			}
			_ = os.WriteFile(keyPath+".pub", []byte(pub), 0o644)
		}
		created, _ := time.Parse(time.RFC3339Nano, r.CreatedAt)
		list = append(list, Identity{
			ID:           r.ID,
			Host:         r.Host,
			HostName:     r.HostName,
			User:         r.User,
			IdentityFile: keyPath,
			PublicKey:    strings.TrimSpace(r.PublicKey),
			GitName:      r.GitName,
			GitEmail:     r.GitEmail,
			GitHubUser:   r.GitHubUser,
			CreatedAt:    created,
		})
	}
	if err := s.saveLocked(list); err != nil {
		return err
	}
	return s.writeConfigsLocked(list)
}

func (s *Store) writeConfigsLocked(list []Identity) error {
	if err := s.ensure(); err != nil {
		return err
	}
	body := renderConfig(list)
	if err := os.WriteFile(s.ConfigPath(), []byte(body), 0o600); err != nil {
		return err
	}
	return syncUserSSHConfig(body)
}

func renderConfig(list []Identity) string {
	var b strings.Builder
	b.WriteString("# Generated by Awaki — do not edit by hand; use /ssh\n")
	b.WriteString("Host *\n")
	b.WriteString("    IdentitiesOnly yes\n")
	b.WriteString("    AddKeysToAgent no\n\n")
	for _, id := range list {
		fmt.Fprintf(&b, "Host %s\n", id.Host)
		fmt.Fprintf(&b, "    HostName %s\n", id.HostName)
		fmt.Fprintf(&b, "    User %s\n", id.User)
		fmt.Fprintf(&b, "    IdentityFile %s\n", id.IdentityFile)
		b.WriteString("    IdentitiesOnly yes\n\n")
	}
	return b.String()
}

func syncUserSSHConfig(managedBody string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	sshDir := filepath.Join(home, ".ssh")
	_ = os.MkdirAll(sshDir, 0o700)
	cfgPath := filepath.Join(sshDir, "config")

	existing, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	outside := stripManagedSection(string(existing))
	var out strings.Builder
	if strings.TrimSpace(outside) != "" {
		out.WriteString(strings.TrimRight(outside, "\n"))
		out.WriteString("\n\n")
	}
	out.WriteString(managedBegin)
	out.WriteByte('\n')
	out.WriteString(strings.TrimRight(managedBody, "\n"))
	out.WriteByte('\n')
	out.WriteString(managedEnd)
	out.WriteByte('\n')
	return os.WriteFile(cfgPath, []byte(out.String()), 0o600)
}

func stripManagedSection(s string) string {
	for {
		start := strings.Index(s, managedBegin)
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], managedEnd)
		if end < 0 {
			return strings.TrimRight(s[:start], "\n") + "\n"
		}
		end = start + end + len(managedEnd)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		s = s[:start] + s[end:]
	}
}

func looksLikePrivateKey(s string) bool {
	return strings.Contains(s, "BEGIN OPENSSH PRIVATE KEY") ||
		strings.Contains(s, "BEGIN RSA PRIVATE KEY") ||
		strings.Contains(s, "BEGIN EC PRIVATE KEY") ||
		strings.Contains(s, "BEGIN PRIVATE KEY") ||
		strings.Contains(s, "BEGIN DSA PRIVATE KEY")
}

// SSHCommand returns GIT_SSH_COMMAND using the managed config when identities exist.
func (s *Store) SSHCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked()
	if err != nil || len(list) == 0 {
		return ""
	}
	cfg := s.ConfigPath()
	if _, err := os.Stat(cfg); err != nil {
		return ""
	}
	return fmt.Sprintf("ssh -F %s -o BatchMode=yes -o StrictHostKeyChecking=accept-new", cfg)
}

// RewriteGitSSHURL rewrites github.com URLs to use a Host alias.
//
//	git@github.com:owner/repo.git + host=github.com-owner
//	  → git@github.com-owner:owner/repo.git
//
// Also accepts https://github.com/owner/repo → ssh form with alias.
func RewriteGitSSHURL(raw, hostAlias string) string {
	hostAlias = strings.TrimSpace(hostAlias)
	if hostAlias == "" {
		return raw
	}
	raw = strings.TrimSpace(raw)

	// Already using an alias Host
	if strings.HasPrefix(raw, "git@"+hostAlias+":") {
		return raw
	}

	// git@github.com:owner/repo[.git]
	if strings.HasPrefix(raw, "git@") {
		// git@host:path
		rest := strings.TrimPrefix(raw, "git@")
		if i := strings.Index(rest, ":"); i > 0 {
			path := rest[i+1:]
			return "git@" + hostAlias + ":" + path
		}
	}

	// ssh://git@github.com/owner/repo
	if strings.HasPrefix(raw, "ssh://") {
		u := strings.TrimPrefix(raw, "ssh://")
		// strip user@
		if i := strings.Index(u, "@"); i >= 0 {
			u = u[i+1:]
		}
		// host/path
		if i := strings.Index(u, "/"); i > 0 {
			path := strings.TrimPrefix(u[i:], "/")
			return "git@" + hostAlias + ":" + path
		}
	}

	// https://github.com/owner/repo[.git]
	for _, prefix := range []string{
		"https://github.com/",
		"http://github.com/",
		"https://www.github.com/",
	} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			path := raw[len(prefix):]
			path = strings.TrimSuffix(path, ".git")
			path = strings.Trim(path, "/")
			return "git@" + hostAlias + ":" + path + ".git"
		}
	}

	return raw
}

// PromptBlock returns a system-prompt section describing configured identities.
func (s *Store) PromptBlock() string {
	list, err := s.List()
	if err != nil || len(list) == 0 {
		return `SSH / GIT IDENTITIES: None configured yet.
Owner can add keys via /ssh (Telegram). Until then prefer HTTPS remotes, or ask the owner to add an SSH identity.
For multi-account GitHub, the convention is Host alias github.com-{username} and clone URL:
  git@github.com-{username}:{username}/repo.git`
	}
	var b strings.Builder
	b.WriteString("SSH / GIT IDENTITIES (configured — use these Host aliases):\n")
	b.WriteString("Before any git clone/push/fetch over SSH, call ssh_list (or read this list) and pick the matching Host alias.\n")
	b.WriteString("GitHub multi-account rule: for @username repos use\n")
	b.WriteString("  git@github.com-username:username/repo.git\n")
	b.WriteString("or pass ssh_host=\"github.com-username\" to git_clone / git tools.\n")
	b.WriteString("Never use plain git@github.com: when a username-specific identity exists — the wrong key will be offered.\n\n")
	for _, id := range list {
		fmt.Fprintf(&b, "• Host `%s` → %s (user %s)", id.Host, id.HostName, id.User)
		if id.GitHubUser != "" {
			fmt.Fprintf(&b, "  GitHub @%s", id.GitHubUser)
			fmt.Fprintf(&b, "  → clone: git@%s:%s/REPO.git", id.Host, id.GitHubUser)
		}
		if id.GitName != "" || id.GitEmail != "" {
			fmt.Fprintf(&b, "  author: %s <%s>", id.GitName, id.GitEmail)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nNon-GitHub SSH hosts work the same way: use the Host alias in ssh/scp/git URLs.\n")
	b.WriteString("Managed config path is used automatically by git tools (GIT_SSH_COMMAND).\n")
	b.WriteString("Tools: ssh_list, ssh_resolve. git_clone accepts optional ssh_host.")
	return b.String()
}
