package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// Config names under kind "config" in the storage chat.
const (
	ConfigProviders = "providers"
	ConfigSchedules = "schedules"
	ConfigMCP       = "mcp"
	ConfigIndex     = "index"
)

// SaveConfigJSON uploads a named JSON config blob and refreshes the durable index.
func (s *Store) SaveConfigJSON(name string, data []byte) error {
	if err := s.upsertText("config", name, name+".json", string(data), map[string]string{
		"name": name,
	}); err != nil {
		return err
	}
	// Pin bootstrap anchors so a blank disk can recover via GetChat.PinnedMessage.
	if name == ConfigProviders || name == ConfigIndex {
		if e, ok := s.idx.get("config", name); ok && e.MessageID > 0 {
			ctx, cancel := s.ctx()
			defer cancel()
			if err := s.api.pinMessage(ctx, s.chatID, e.MessageID); err != nil {
				log.Debug().Err(err).Str("name", name).Msg("telegram: pin config (optional)")
			}
		}
	}
	if name != ConfigIndex {
		_ = s.persistIndex()
	}
	return nil
}

// LoadConfigJSON fetches a named config from Telegram via the index.
func (s *Store) LoadConfigJSON(name string) ([]byte, error) {
	if body, ok := s.fetchText("config", name); ok {
		return []byte(body), nil
	}
	return nil, fmt.Errorf("config %q not found in telegram storage", name)
}

func (s *Store) persistIndex() error {
	s.idx.mu.RLock()
	data, err := json.MarshalIndent(s.idx, "", "  ")
	s.idx.mu.RUnlock()
	if err != nil {
		return err
	}
	return s.upsertText("config", ConfigIndex, "index.json", string(data), map[string]string{"name": ConfigIndex})
}

// BootstrapFromPinned recovers the index (and provider entry) from the storage
// chat's pinned message after a blank local disk.
func (s *Store) BootstrapFromPinned(ctx context.Context) {
	chat, err := s.api.getChat(ctx, s.chatID)
	if err != nil || chat == nil {
		log.Debug().Err(err).Msg("telegram: getChat for bootstrap failed")
		return
	}
	pm := chat.PinnedMessage
	if pm == nil {
		return
	}
	cap := pm.Caption
	if cap == "" {
		cap = pm.Text
	}
	rec, _ := DecodeCaption(cap)
	fileID := ""
	if pm.Document != nil {
		fileID = pm.Document.FileID
	}
	if fileID == "" {
		return
	}
	data, err := s.api.downloadFile(ctx, fileID)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: bootstrap download failed")
		return
	}

	// Prefer restoring the full index.
	if rec.Kind == "config" && rec.Fields["name"] == ConfigIndex || looksLikeIndexJSON(data) {
		var idx index
		if err := json.Unmarshal(data, &idx); err != nil {
			log.Warn().Err(err).Msg("telegram: bootstrap index unmarshal failed")
		} else {
			if idx.Entries == nil {
				idx.Entries = map[string]indexEntry{}
			}
			idx.path = s.idx.path
			s.idx = &idx
			_ = s.idx.saveLocked()
			log.Info().Int("entries", len(s.idx.Entries)).Msg("telegram: index restored from pinned message")
			s.hydrateAllFromIndex()
			return
		}
	}

	if rec.Kind == "config" && rec.Fields["name"] == ConfigProviders {
		_ = s.idx.set("config", ConfigProviders, indexEntry{
			MessageID: pm.ID,
			FileID:    fileID,
			Kind:      "config",
			Key:       ConfigProviders,
		})
		log.Info().Msg("telegram: providers bootstrap entry from pinned message")
		return
	}

	log.Debug().Str("kind", rec.Kind).Msg("telegram: pinned message not a recognized bootstrap payload")
}

func looksLikeIndexJSON(data []byte) bool {
	var probe struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Entries != nil
}

// hydrateAllFromIndex pulls soul, user, memory, skills, and config blobs into
// the local cache so the rest of the app can read them offline-fast.
func (s *Store) hydrateAllFromIndex() {
	// Identity
	if body, ok := s.fetchText("soul", "soul"); ok {
		_ = s.local.WriteSoul(body)
	}
	if body, ok := s.fetchText("user", "user"); ok {
		_ = s.local.WriteUser(body)
	}
	// Memory files
	for _, e := range s.idx.list("memory") {
		if body, ok := s.fetchText("memory", e.Key); ok {
			name := e.Key
			if !strings.HasSuffix(name, ".md") {
				name += ".md"
			}
			_ = s.local.WriteMemory(name, body)
		}
	}
	// Skills
	for _, e := range s.idx.list("skill") {
		if body, ok := s.fetchText("skill", e.Key); ok {
			dir := filepath.Join(s.SkillsDir(), e.Key)
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644)
		}
	}
	// Providers / schedules land via SetCloud hydrate on their own stores.
	log.Info().Msg("telegram: local cache hydrated from storage chat index")
}

// EnsureIdentityCloud forces a re-fetch of soul/user if local is empty.
func (s *Store) EnsureIdentityCloud() {
	if s.local.ReadSoul() == "" {
		if body, ok := s.fetchText("soul", "soul"); ok {
			_ = s.local.WriteSoul(body)
		}
	}
	if s.local.ReadUser() == "" {
		if body, ok := s.fetchText("user", "user"); ok {
			_ = s.local.WriteUser(body)
		}
	}
}

// MCPCloud adapts Store into mcp.Cloud.
type MCPCloud struct {
	S *Store
}

func (p MCPCloud) LoadMCPJSON() ([]byte, error) {
	return p.S.LoadConfigJSON(ConfigMCP)
}

func (p MCPCloud) SaveMCPJSON(data []byte) error {
	return p.S.SaveConfigJSON(ConfigMCP, data)
}
