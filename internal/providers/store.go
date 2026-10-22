package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Store persists providers and models as JSON (runtime-configurable, Awaki-style).
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  ConfigFile
}

func NewStore(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "providers.json")
	s := &Store{path: path}
	if err := s.load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		s.cfg = ConfigFile{}
		_ = s.save()
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.cfg)
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s *Store) Snapshot() ConfigFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := ConfigFile{ActiveModelID: s.cfg.ActiveModelID}
	out.Providers = append([]Provider(nil), s.cfg.Providers...)
	out.Models = append([]Model(nil), s.cfg.Models...)
	return out
}

// --- Providers CRUD ---

func (s *Store) ListProviders() []Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Provider(nil), s.cfg.Providers...)
}

func (s *Store) GetProvider(id string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.cfg.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

func (s *Store) AddProvider(name, baseURL string, protocol Protocol, apiKey string) (Provider, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if name == "" {
		return Provider{}, fmt.Errorf("name is required")
	}
	if baseURL == "" {
		return Provider{}, fmt.Errorf("base_url is required (no hardcoded defaults — set the endpoint like the Awaki app)")
	}
	if apiKey == "" {
		return Provider{}, fmt.Errorf("api_key is required")
	}
	if protocol == "" {
		return Provider{}, fmt.Errorf("protocol is required")
	}
	p := Provider{
		ID:       uuid.NewString(),
		Name:     name,
		BaseURL:  baseURL,
		Protocol: protocol,
		APIKey:   apiKey,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Providers = append(s.cfg.Providers, p)
	if err := s.save(); err != nil {
		return Provider{}, err
	}
	return p, nil
}

func (s *Store) UpdateProvider(id string, name, baseURL, apiKey *string, protocol *Protocol) (Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.cfg.Providers {
		if p.ID != id {
			continue
		}
		if name != nil && strings.TrimSpace(*name) != "" {
			p.Name = strings.TrimSpace(*name)
		}
		if baseURL != nil && strings.TrimSpace(*baseURL) != "" {
			p.BaseURL = strings.TrimRight(strings.TrimSpace(*baseURL), "/")
		}
		if apiKey != nil && strings.TrimSpace(*apiKey) != "" {
			p.APIKey = strings.TrimSpace(*apiKey)
		}
		if protocol != nil && *protocol != "" {
			p.Protocol = *protocol
		}
		s.cfg.Providers[i] = p
		if err := s.save(); err != nil {
			return Provider{}, err
		}
		return p, nil
	}
	return Provider{}, fmt.Errorf("provider %q not found", id)
}

func (s *Store) DeleteProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	out := s.cfg.Providers[:0]
	for _, p := range s.cfg.Providers {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return fmt.Errorf("provider %q not found", id)
	}
	s.cfg.Providers = out
	// Drop models bound to this provider
	models := s.cfg.Models[:0]
	for _, m := range s.cfg.Models {
		if m.ProviderID == id {
			if s.cfg.ActiveModelID == m.ID {
				s.cfg.ActiveModelID = ""
			}
			continue
		}
		models = append(models, m)
	}
	s.cfg.Models = models
	return s.save()
}

// --- Models CRUD ---

func (s *Store) ListModels() []Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Model(nil), s.cfg.Models...)
}

func (s *Store) ListModelsByProvider(providerID string) []Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Model
	for _, m := range s.cfg.Models {
		if m.ProviderID == providerID {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) GetModel(id string) (Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.cfg.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (s *Store) AddModel(providerID, modelID, displayName string, contextWindow int) (Model, error) {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	displayName = strings.TrimSpace(displayName)
	if providerID == "" || modelID == "" {
		return Model{}, fmt.Errorf("provider_id and model_id are required")
	}
	if _, ok := s.GetProvider(providerID); !ok {
		// re-check under lock below
	}
	if displayName == "" {
		displayName = modelID
	}
	m := Model{
		ID:            uuid.NewString(),
		ProviderID:    providerID,
		ModelID:       modelID,
		DisplayName:   displayName,
		ContextWindow: contextWindow,
		Capabilities: Capabilities{
			Tools: true, Images: true, Streaming: true, MaxTokensParameter: true,
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, p := range s.cfg.Providers {
		if p.ID == providerID {
			found = true
			break
		}
	}
	if !found {
		return Model{}, fmt.Errorf("provider %q not found", providerID)
	}
	s.cfg.Models = append(s.cfg.Models, m)
	if s.cfg.ActiveModelID == "" {
		s.cfg.ActiveModelID = m.ID
	}
	if err := s.save(); err != nil {
		return Model{}, err
	}
	return m, nil
}

func (s *Store) DeleteModel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	out := s.cfg.Models[:0]
	for _, m := range s.cfg.Models {
		if m.ID == id {
			found = true
			if s.cfg.ActiveModelID == id {
				s.cfg.ActiveModelID = ""
			}
			continue
		}
		out = append(out, m)
	}
	if !found {
		return fmt.Errorf("model %q not found", id)
	}
	s.cfg.Models = out
	return s.save()
}

func (s *Store) UpdateModel(id string, displayName, modelID *string, contextWindow *int, caps *Capabilities) (Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.cfg.Models {
		if m.ID != id {
			continue
		}
		if displayName != nil && strings.TrimSpace(*displayName) != "" {
			m.DisplayName = strings.TrimSpace(*displayName)
		}
		if modelID != nil && strings.TrimSpace(*modelID) != "" {
			m.ModelID = strings.TrimSpace(*modelID)
		}
		if contextWindow != nil && *contextWindow > 0 {
			m.ContextWindow = *contextWindow
		}
		if caps != nil {
			m.Capabilities = *caps
		}
		s.cfg.Models[i] = m
		if err := s.save(); err != nil {
			return Model{}, err
		}
		return m, nil
	}
	return Model{}, fmt.Errorf("model %q not found", id)
}

func (s *Store) SetActiveModel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, m := range s.cfg.Models {
		if m.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("model %q not found", id)
	}
	s.cfg.ActiveModelID = id
	return s.save()
}

func (s *Store) ActiveModelID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.ActiveModelID
}

// ActivePair returns the active model and its provider.
func (s *Store) ActivePair() (Model, Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.ActiveModelID == "" {
		return Model{}, Provider{}, fmt.Errorf("no active model — use /providers to add one and /models to select")
	}
	var m Model
	found := false
	for _, x := range s.cfg.Models {
		if x.ID == s.cfg.ActiveModelID {
			m = x
			found = true
			break
		}
	}
	if !found {
		return Model{}, Provider{}, fmt.Errorf("active model %q missing", s.cfg.ActiveModelID)
	}
	for _, p := range s.cfg.Providers {
		if p.ID == m.ProviderID {
			return m, p, nil
		}
	}
	return Model{}, Provider{}, fmt.Errorf("provider %q for active model not found", m.ProviderID)
}
