package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// Store persists per-chat permission modes under dataDir/permission_modes.json.
// Thread-safe. Missing file → all chats use Default.
type Store struct {
	path string
	mu   sync.Mutex
	// chatID → mode
	modes   map[string]Mode
	defMode Mode
}

type fileShape struct {
	Default string            `json:"default"`
	Chats   map[string]string `json:"chats"`
}

func NewStore(dataDir string) *Store {
	s := &Store{
		path:    filepath.Join(dataDir, "permission_modes.json"),
		modes:   make(map[string]Mode),
		defMode: ModeAuto,
	}
	if v := strings.TrimSpace(os.Getenv("AWAKI_PERMISSION_MODE")); v != "" {
		if m, err := Parse(v); err == nil {
			s.defMode = m
		}
	}
	s.load()
	return s
}

func (s *Store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f fileShape
	if err := json.Unmarshal(b, &f); err != nil {
		log.Warn().Err(err).Msg("permission store: corrupt file, using defaults")
		return
	}
	if m, err := Parse(f.Default); err == nil {
		s.defMode = m
	}
	for id, raw := range f.Chats {
		if m, err := Parse(raw); err == nil {
			s.modes[id] = m
		}
	}
}

func (s *Store) saveLocked() error {
	f := fileShape{
		Default: string(s.defMode),
		Chats:   make(map[string]string, len(s.modes)),
	}
	for id, m := range s.modes {
		f.Chats[id] = string(m)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Get returns the mode for chatID (or default).
func (s *Store) Get(chatID string) Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	chatID = strings.TrimSpace(chatID)
	if chatID != "" {
		if m, ok := s.modes[chatID]; ok {
			return m
		}
	}
	return s.defMode
}

// Set stores mode for a chat and persists.
func (s *Store) Set(chatID string, m Mode) error {
	if m == "" {
		m = ModeAuto
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		s.defMode = m
	} else {
		s.modes[chatID] = m
	}
	return s.saveLocked()
}

// Default returns the global default mode for new chats.
func (s *Store) Default() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.defMode
}

// SetDefault updates the global default.
func (s *Store) SetDefault(m Mode) error {
	return s.Set("", m)
}
