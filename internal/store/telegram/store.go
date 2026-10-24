package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Store is a hybrid persistence layer:
//   - Local filesystem cache for fast reads and workspace-adjacent data
//   - Telegram private channel/chat as durable source of truth for identity,
//     memory, skills, and chat transcripts
//
// Design inspired by Telegrive: every durable object is a Telegram message
// (document) with a machine-readable caption. A small local index maps
// logical keys → message_id / file_id so we never need to scan the channel.
//
// Free-tier friendly: wipe the local data dir and restart — identity/memory/
// skills/chats are recovered from Telegram via the index (index itself is
// small and can be rebuilt if we also pin a snapshot later).
type Store struct {
	local     store.Store // local.LocalStore for cache + chat JSONL speed
	api       *api
	chatID    int64
	cacheDir  string
	idx       *index
	timeout   time.Duration
}

var _ store.Store = (*Store)(nil)

// Config for New.
type Config struct {
	Token    string // bot token (same as channel)
	ChatID   int64  // private channel or group used as storage
	CacheDir string // local cache root (can be ephemeral)
}

func New(cfg Config) (store.Store, error) {
	if cfg.Token == "" || cfg.ChatID == 0 {
		return nil, fmt.Errorf("telegram store requires Token and ChatID")
	}
	if cfg.CacheDir == "" {
		home, _ := os.UserHomeDir()
		cfg.CacheDir = filepath.Join(home, ".awaki", "tg-cache")
	}
	loc, err := local.New(cfg.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("local cache: %w", err)
	}
	a, err := newAPI(cfg.Token)
	if err != nil {
		return nil, err
	}
	s := &Store{
		local:    loc,
		api:      a,
		chatID:   cfg.ChatID,
		cacheDir: cfg.CacheDir,
		idx:      loadIndex(filepath.Join(cfg.CacheDir, "tg_index.json")),
		timeout:  45 * time.Second,
	}
	// Cold-start recovery: empty local index → try pinned message on storage chat.
	if len(s.idx.Entries) == 0 {
		ctx, cancel := s.ctx()
		s.BootstrapFromPinned(ctx)
		cancel()
	} else {
		// Index present but local files may be missing after a volume wipe with
		// a restored index file — rehydrate content from Telegram.
		s.hydrateAllFromIndex()
	}
	s.EnsureIdentityCloud()
	log.Info().Int64("storage_chat", cfg.ChatID).Str("cache", cfg.CacheDir).Int("index_entries", len(s.idx.Entries)).Msg("telegram store ready")
	return s, nil
}

func (s *Store) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.timeout)
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

func (s *Store) ReadSoul() string {
	if body, ok := s.fetchText("soul", "soul"); ok {
		_ = s.local.WriteSoul(body)
		return body
	}
	return s.local.ReadSoul()
}

func (s *Store) WriteSoul(content string) error {
	if err := s.local.WriteSoul(content); err != nil {
		return err
	}
	if err := s.upsertText("soul", "soul", "soul.md", content, map[string]string{"name": "soul"}); err != nil {
		return err
	}
	_ = s.persistIndex()
	return nil
}

func (s *Store) ReadUser() string {
	if body, ok := s.fetchText("user", "user"); ok {
		_ = s.local.WriteUser(body)
		return body
	}
	return s.local.ReadUser()
}

func (s *Store) WriteUser(content string) error {
	if err := s.local.WriteUser(content); err != nil {
		return err
	}
	if err := s.upsertText("user", "user", "user.md", content, map[string]string{"name": "user"}); err != nil {
		return err
	}
	_ = s.persistIndex()
	return nil
}

func (s *Store) LoadTimezone() *time.Location {
	return s.local.LoadTimezone()
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

func (s *Store) ListMemory() ([]store.MemoryFile, error) {
	// Prefer local list; ensure TG-backed entries are present
	localList, err := s.local.ListMemory()
	if err != nil {
		localList = nil
	}
	seen := map[string]bool{}
	for _, m := range localList {
		seen[m.Name] = true
	}
	for _, e := range s.idx.list("memory") {
		name := e.Key
		if !strings.HasSuffix(name, ".md") {
			name += ".md"
		}
		if seen[name] {
			continue
		}
		if body, ok := s.fetchText("memory", e.Key); ok {
			_ = s.local.WriteMemory(name, body)
			localList = append(localList, store.MemoryFile{Name: name, Size: int64(len(body)), Content: body})
		}
	}
	return localList, nil
}

func (s *Store) ReadMemory(filename string) (string, error) {
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	if body, ok := s.fetchText("memory", key); ok {
		_ = s.local.WriteMemory(key+".md", body)
		return body, nil
	}
	return s.local.ReadMemory(filename)
}

func (s *Store) WriteMemory(filename, content string) error {
	if err := s.local.WriteMemory(filename, content); err != nil {
		return err
	}
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	if err := s.upsertText("memory", key, key+".md", content, map[string]string{"name": key}); err != nil {
		return err
	}
	_ = s.persistIndex()
	return nil
}

func (s *Store) DeleteMemory(filename string) error {
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	_ = s.local.DeleteMemory(filename)
	return s.deleteRecord("memory", key)
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func (s *Store) SkillsDir() string { return s.local.SkillsDir() }

func (s *Store) LoadSkills() ([]store.Skill, error) {
	// Hydrate from TG index if local empty
	skills, err := s.local.LoadSkills()
	if err != nil {
		return nil, err
	}
	if len(skills) > 0 {
		return skills, nil
	}
	for _, e := range s.idx.list("skill") {
		if body, ok := s.fetchText("skill", e.Key); ok {
			// Write into local skill dir structure
			name := e.Key
			dir := filepath.Join(s.SkillsDir(), name)
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644)
		}
	}
	return s.local.LoadSkills()
}

func (s *Store) CreateSkill(name, description, body string) (string, error) {
	path, err := s.local.CreateSkill(name, description, body)
	if err != nil {
		return "", err
	}
	content, _ := os.ReadFile(path)
	key := sanitize(name)
	_ = s.upsertText("skill", key, key+".md", string(content), map[string]string{
		"name": key, "description": description,
	})
	return path, nil
}

func (s *Store) UpdateSkill(name, content string) error {
	if err := s.local.UpdateSkill(name, content); err != nil {
		return err
	}
	key := sanitize(name)
	return s.upsertText("skill", key, key+".md", content, map[string]string{"name": key})
}

func (s *Store) DeleteSkill(name string) error {
	_ = s.local.DeleteSkill(name)
	return s.deleteRecord("skill", sanitize(name))
}

func (s *Store) ReadSkill(name string) (string, error) {
	key := sanitize(name)
	if body, ok := s.fetchText("skill", key); ok {
		return body, nil
	}
	return s.local.ReadSkill(name)
}

// ---------------------------------------------------------------------------
// Chats — local for speed; snapshot metadata + messages to Telegram on Save
// ---------------------------------------------------------------------------

func (s *Store) NewChat() *store.Chat {
	ch := s.local.NewChat()
	// Ensure appends hit our persister
	ch.SetPersister(s)
	return ch
}

func (s *Store) LoadChat(id string) (*store.Chat, error) {
	ch, err := s.local.LoadChat(id)
	if err == nil {
		ch.SetPersister(s)
		return ch, nil
	}
	// Try hydrate from Telegram
	if body, ok := s.fetchText("chat", id); ok {
		var meta store.Chat
		if json.Unmarshal([]byte(body), &meta) == nil {
			meta.SetPersister(s)
			_ = s.local.SaveChat(&meta)
			// messages
			if msgBody, ok2 := s.fetchText("chat_msgs", id); ok2 {
				var turns []store.Turn
				if json.Unmarshal([]byte(msgBody), &turns) == nil {
					_ = s.local.ReplaceMessages(&meta, turns)
					meta.SetMessages(turns)
				}
			}
			return &meta, nil
		}
	}
	return nil, err
}

func (s *Store) ListChats() ([]store.ChatInfo, error) {
	return s.local.ListChats()
}

func (s *Store) GetActiveChatID() (string, error) { return s.local.GetActiveChatID() }
func (s *Store) SetActiveChatID(id string) error {
	if err := s.local.SetActiveChatID(id); err != nil {
		return err
	}
	return s.upsertText("config", "active_chat", "active_chat.txt", id, map[string]string{"key": "active_chat"})
}

func (s *Store) SaveChat(ch *store.Chat) error {
	if err := s.local.SaveChat(ch); err != nil {
		return err
	}
	meta, _ := json.Marshal(ch)
	_ = s.upsertText("chat", ch.ID, ch.ID+".json", string(meta), map[string]string{
		"id": ch.ID, "title": ch.Title, "status": ch.Status,
	})
	// Snapshot in-memory messages so free-tier restarts can recover history.
	if msgs := ch.Messages(); len(msgs) > 0 {
		_ = s.SnapshotChatMessages(ch)
	}
	return nil
}

func (s *Store) LoadMessages(ch *store.Chat) error {
	return s.local.LoadMessages(ch)
}

func (s *Store) AppendMessage(ch *store.Chat, msg store.Turn) {
	s.local.AppendMessage(ch, msg)
	// Periodic snapshot every few messages would be better; snapshot on SaveChat is enough for durability of meta.
}

func (s *Store) ReplaceMessages(ch *store.Chat, msgs []store.Turn) error {
	if err := s.local.ReplaceMessages(ch, msgs); err != nil {
		return err
	}
	data, _ := json.Marshal(msgs)
	return s.upsertText("chat_msgs", ch.ID, ch.ID+".messages.json", string(data), map[string]string{
		"id": ch.ID, "count": fmt.Sprintf("%d", len(msgs)),
	})
}

func (s *Store) TextMessages(ch *store.Chat) ([]store.Message, error) {
	return s.local.TextMessages(ch)
}

func (s *Store) HasMessages(ch *store.Chat) bool {
	return s.local.HasMessages(ch)
}

// SnapshotChatMessages uploads full message history for a chat (call after important turns).
func (s *Store) SnapshotChatMessages(ch *store.Chat) error {
	msgs := ch.Messages()
	data, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	return s.upsertText("chat_msgs", ch.ID, ch.ID+".messages.json", string(data), map[string]string{
		"id": ch.ID, "count": fmt.Sprintf("%d", len(msgs)),
	})
}

// ---------------------------------------------------------------------------
// Telegram upsert / fetch helpers
// ---------------------------------------------------------------------------

func (s *Store) upsertText(kind, key, filename, content string, fields map[string]string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	caption := EncodeCaption(fmt.Sprintf("%s/%s", kind, key), kind, fields)
	data := []byte(content)

	if e, ok := s.idx.get(kind, key); ok && e.MessageID > 0 {
		// Bot API cannot replace document body in-place reliably; delete + re-send.
		_ = s.api.deleteMessage(ctx, s.chatID, e.MessageID)
	}

	msgID, fileID, err := s.api.sendDocument(ctx, s.chatID, filename, data, caption)
	if err != nil {
		// Fallback: send as text if small
		if len(content) < 3500 {
			text := caption + "\n\n" + content
			msgID, err = s.api.sendText(ctx, s.chatID, text)
			if err != nil {
				log.Warn().Err(err).Str("kind", kind).Str("key", key).Msg("telegram store upsert failed")
				return err
			}
			fileID = ""
		} else {
			log.Warn().Err(err).Str("kind", kind).Str("key", key).Msg("telegram store upsert failed")
			return err
		}
	}
	return s.idx.set(kind, key, indexEntry{MessageID: msgID, FileID: fileID, Kind: kind, Key: key})
}

func (s *Store) fetchText(kind, key string) (string, bool) {
	e, ok := s.idx.get(kind, key)
	if !ok {
		return "", false
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if e.FileID != "" {
		data, err := s.api.downloadFile(ctx, e.FileID)
		if err != nil {
			log.Warn().Err(err).Str("kind", kind).Str("key", key).Msg("telegram store download failed")
			return "", false
		}
		return string(data), true
	}
	// Text-only messages: we don't re-fetch by ID easily without getChat; rely on local cache.
	return "", false
}

func (s *Store) deleteRecord(kind, key string) error {
	e, ok := s.idx.get(kind, key)
	if !ok {
		return nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_ = s.api.deleteMessage(ctx, s.chatID, e.MessageID)
	return s.idx.del(kind, key)
}

var safeNameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitize(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	name = safeNameRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = uuid.NewString()[:8]
	}
	return name
}
