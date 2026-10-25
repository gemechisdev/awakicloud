package sqlite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// MigrateFromDir imports legacy filesystem data into SQLite when the DB is empty.
func (s *Store) MigrateFromDir(dataDir string) {
	// Only migrate if essentially empty
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM kv`).Scan(&n)
	var nMem int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM memory`).Scan(&nMem)
	if n > 0 || nMem > 0 {
		return
	}

	imported := 0
	if b, err := os.ReadFile(filepath.Join(dataDir, "soul.md")); err == nil && len(b) > 0 {
		_ = s.WriteSoul(string(b))
		imported++
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "user.md")); err == nil && len(b) > 0 {
		_ = s.WriteUser(string(b))
		imported++
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "active_episode.txt")); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			_ = s.SetActiveChatID(id)
			imported++
		}
	}
	memDir := filepath.Join(dataDir, "memory")
	if entries, err := os.ReadDir(memDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(memDir, e.Name())); err == nil {
				_ = s.WriteMemory(e.Name(), string(b))
				imported++
			}
		}
	}
	skillsDir := filepath.Join(dataDir, "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			body, err := os.ReadFile(filepath.Join(skillsDir, e.Name(), "SKILL.md"))
			if err != nil {
				continue
			}
			_, _ = s.CreateSkill(e.Name(), e.Name(), string(body))
			imported++
		}
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "providers.json")); err == nil && len(b) > 0 {
		_ = s.SetConfigJSON("providers", b)
		imported++
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "schedules.json")); err == nil && len(b) > 0 {
		_ = s.SetConfigJSON("schedules", b)
		imported++
	}
	// chats
	epDir := filepath.Join(dataDir, "episodes")
	_ = filepath.Walk(epDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".messages.jsonl") {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var meta map[string]any
		if json.Unmarshal(data, &meta) != nil {
			return nil
		}
		id, _ := meta["id"].(string)
		if id == "" {
			return nil
		}
		_, _ = s.db.Exec(
			`INSERT OR IGNORE INTO chats(id,meta_json,updated_at) VALUES(?,?,?)`,
			id, string(data), now(),
		)
		// messages sidecar
		msgPath := strings.TrimSuffix(path, ".json") + ".messages.jsonl"
		if raw, err := os.ReadFile(msgPath); err == nil {
			lines := strings.Split(string(raw), "\n")
			seq := 0
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				seq++
				_, _ = s.db.Exec(`INSERT OR IGNORE INTO messages(chat_id,seq,turn_json) VALUES(?,?,?)`, id, seq, line)
			}
		}
		imported++
		return nil
	})

	if imported > 0 {
		log.Info().Int("items", imported).Msg("sqlite: migrated legacy filesystem data")
	}
}
