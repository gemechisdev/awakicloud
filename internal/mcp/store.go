package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store persists MCP server configs as JSON under dataDir/mcp-servers.json.
type Store struct {
	mu   sync.Mutex
	path string
}

func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "mcp-servers.json")}
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
	return os.WriteFile(s.path, data, 0o600)
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
