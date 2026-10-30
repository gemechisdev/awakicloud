package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Cloud is optional durable storage (e.g. Telegram storage chat or SQLite kv).
type Cloud interface {
	LoadMCPJSON() ([]byte, error)
	SaveMCPJSON(data []byte) error
}

// Store persists MCP server configs as JSON under dataDir/mcp-servers.json.
type Store struct {
	mu   sync.Mutex
	path string
	cloud Cloud
}

func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "mcp-servers.json")}
}

func NewStoreWithCloud(dataDir string, cloud Cloud) *Store {
	return &Store{path: filepath.Join(dataDir, "mcp-servers.json"), cloud: cloud}
}

func (s *Store) SetCloud(cloud Cloud) {
	s.mu.Lock()
	s.cloud = cloud
	s.mu.Unlock()
	// hydrate if still empty
	s.mu.Lock()
	_ = s.hydrateLocked(cloud)
	s.mu.Unlock()
}

func (s *Store) hydrateLocked(cloud Cloud) error {
	if cloud == nil {
		return nil
	}
	data, err := cloud.LoadMCPJSON()
	if err != nil || len(data) == 0 {
		return err
	}
	var cfg ConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if len(cfg.Servers) == 0 {
		return nil
	}
	// Merge cloud servers into local, preferring cloud on conflict by name.
	local, err := s.load()
	if err != nil {
		return err
	}
	byName := map[string]Server{}
	for _, srv := range local.Servers {
		byName[srv.Name] = srv
	}
	merged := append([]Server(nil), local.Servers...)
	for _, srv := range cfg.Servers {
		if _, exists := byName[srv.Name]; !exists {
			merged = append(merged, srv)
		}
	}
	local.Servers = merged
	return s.save(local)
}

func (s *Store) load() (ConfigFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConfigFile{}, nil
		}
		return ConfigFile{}, err
	}
	var cfg ConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ConfigFile{}, err
	}
	return cfg, nil
}

func (s *Store) save(cfg ConfigFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return err
	}
	if s.cloud != nil {
		// local is source of truth for this process; cloud is best-effort durable
		if err := s.cloud.SaveMCPJSON(data); err != nil {
			log.Warn().Err(err).Msg("mcp: cloud save failed")
		}
	}
	return nil
}

func (s *Store) List() ([]Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return nil, err
	}
	return cfg.Servers, nil
}

func (s *Store) Get(name string) (Server, bool, error) {
	list, err := s.List()
	if err != nil {
		return Server{}, false, err
	}
	name = strings.TrimSpace(name)
	for _, srv := range list {
		if strings.EqualFold(srv.Name, name) {
			return srv, true, nil
		}
	}
	return Server{}, false, nil
}

func (s *Store) Upsert(srv Server) error {
	if err := srv.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	found := false
	for i, existing := range cfg.Servers {
		if strings.EqualFold(existing.Name, srv.Name) {
			if srv.CreatedAt.IsZero() {
				srv.CreatedAt = existing.CreatedAt
			}
			srv.UpdatedAt = now
			cfg.Servers[i] = srv
			found = true
			break
		}
	}
	if !found {
		if srv.CreatedAt.IsZero() {
			srv.CreatedAt = now
		}
		srv.UpdatedAt = now
		cfg.Servers = append(cfg.Servers, srv)
	}
	return s.save(cfg)
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	out := cfg.Servers[:0]
	removed := false
	for _, srv := range cfg.Servers {
		if strings.EqualFold(srv.Name, name) {
			removed = true
			continue
		}
		out = append(out, srv)
	}
	if !removed {
		return fmt.Errorf("mcp server %q not found", name)
	}
	cfg.Servers = out
	return s.save(cfg)
}

func (s *Store) SetDisabled(name string, disabled bool) error {
	srv, ok, err := s.Get(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mcp server %q not found", name)
	}
	srv.Disabled = disabled
	return s.Upsert(srv)
}
