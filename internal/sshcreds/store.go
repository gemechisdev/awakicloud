// Package sshcreds manages SSH identities for git (multi-account GitHub, etc.).
//
// Layout under {DataDir}/ssh/:
//
//	config                 — generated ssh_config (Host blocks)
//	identities.json        — metadata
//	keys/{id}              — private key (mode 0600)
//	keys/{id}.pub          — public key (optional)
//
// Also keeps ~/.ssh/config in sync inside a managed marker section so the
// process user's normal ssh client picks up the same Host aliases.
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
	ID           string    `json:"id"`                      // safe filename / Host alias key
	Host         string    `json:"host"`                    // Host alias in ssh config, e.g. github.com-gemechisdev
	HostName     string    `json:"host_name"`               // real hostname, e.g. github.com
	User         string    `json:"user"`                    // usually "git"
	IdentityFile string    `json:"identity_file"`           // absolute path to private key
	PublicKey    string    `json:"public_key,omitempty"`    // content of .pub (for display)
	GitName      string    `json:"git_name,omitempty"`      // optional commit author name
	GitEmail     string    `json:"git_email,omitempty"`     // optional commit author email
	GitHubUser   string    `json:"github_user,omitempty"`   // e.g. gemechisdev (informational)
	CreatedAt    time.Time `json:"created_at"`
}

// Store persists identities under dataDir/ssh.
type Store struct {
	mu      sync.Mutex
	dataDir string // parent data dir (e.g. ~/.awaki)
	sshDir  string // dataDir/ssh
}

func New(dataDir string) *Store {
	return &Store{
		dataDir: dataDir,
		sshDir:  filepath.Join(dataDir, "ssh"),
	}
}

func (s *Store) Dir() string { return s.sshDir }

func (s *Store) ConfigPath() string { return filepath.Join(s.sshDir, "config") }

func (s *Store) ensure() error {
	if err := os.MkdirAll(filepath.Join(s.sshDir, "keys"), 0o700); err != nil {
		return err
	}
	return nil
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
	return Identity{}, false, nil
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
	Host       string // required — Host alias (e.g. github.com-gemechisdev)
	HostName   string // default github.com
	User       string // default git
	PrivateKey string // required — PEM / OpenSSH private key body
	PublicKey  string // optional
	GitName    string
	GitEmail   string
	GitHubUser string
	ID         string // optional override for file id; default derived from Host
}

// Add writes the key files, updates identities.json, regenerates ssh config,
// and syncs the managed section into ~/.ssh/config.
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
	// Replace existing same id or same host
	var next []Identity
	for _, existing := range list {
		if existing.ID == id || strings.EqualFold(existing.Host, host) {
			// remove old key files
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
		GitHubUser:   strings.TrimSpace(in.GitHubUser),
		CreatedAt:    time.Now().UTC(),
	}
	next = append(next, ident)
	if err := s.saveLocked(next); err != nil {
		return Identity{}, err
	}
	if err := s.writeConfigsLocked(next); err != nil {
		return Identity{}, err
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
	found := false
	for _, id := range list {
		if strings.EqualFold(id.ID, idOrHost) || strings.EqualFold(id.Host, idOrHost) {
			_ = os.Remove(id.IdentityFile)
			_ = os.Remove(id.IdentityFile + ".pub")
			found = true
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
	return s.writeConfigsLocked(next)
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

// syncUserSSHConfig writes/updates the managed section in ~/.ssh/config.
func syncUserSSHConfig(managedBody string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil // best-effort
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
			// orphan begin — drop from begin to EOF
			return strings.TrimRight(s[:start], "\n") + "\n"
		}
		end = start + end + len(managedEnd)
		// swallow trailing newline
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

// SSHCommand returns a GIT_SSH_COMMAND value that uses the managed config when
// at least one identity exists. Empty string means "use default".
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
	// -F forces our config; BatchMode avoids interactive prompts.
	return fmt.Sprintf("ssh -F %s -o BatchMode=yes -o StrictHostKeyChecking=accept-new", cfg)
}

// CloneURLForHost rewrites git@github.com:owner/repo to git@alias:owner/repo
// when the user wants a specific identity host.
func CloneURLForHost(hostAlias, ownerRepo string) string {
	ownerRepo = strings.TrimPrefix(ownerRepo, "/")
	return fmt.Sprintf("git@%s:%s", hostAlias, ownerRepo)
}
