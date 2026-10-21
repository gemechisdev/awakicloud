package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// index maps logical keys → Telegram message metadata.
// Persisted locally so we can edit/delete without scanning the channel.
// Size stays tiny (KB). Content itself lives on Telegram.

type indexEntry struct {
	MessageID int    `json:"message_id"`
	FileID    string `json:"file_id,omitempty"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`
}

type index struct {
	mu      sync.RWMutex
	path    string
	Entries map[string]indexEntry `json:"entries"` // key = kind+"/"+logicalKey
}

func loadIndex(path string) *index {
	idx := &index{path: path, Entries: map[string]indexEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return idx
	}
	_ = json.Unmarshal(data, idx)
	if idx.Entries == nil {
		idx.Entries = map[string]indexEntry{}
	}
	return idx
}

func (idx *index) get(kind, key string) (indexEntry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	e, ok := idx.Entries[kind+"/"+key]
	return e, ok
}

func (idx *index) set(kind, key string, e indexEntry) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e.Kind = kind
	e.Key = key
	idx.Entries[kind+"/"+key] = e
	return idx.saveLocked()
}

func (idx *index) del(kind, key string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.Entries, kind+"/"+key)
	return idx.saveLocked()
}

func (idx *index) list(kind string) []indexEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var out []indexEntry
	prefix := kind + "/"
	for k, e := range idx.Entries {
		if stringsHasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func (idx *index) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(idx.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(idx.path, data, 0o644)
}

func stringsHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
