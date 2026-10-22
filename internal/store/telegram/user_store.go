//go:build !nogotd

package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	"github.com/gemechischala/awaki/internal/store/telegram/user"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// UserStore is Telegrive-style storage: ordinary Telegram user account + private
// channel as source of truth. Local disk is only a cache / index.
type UserStore struct {
	local    store.Store
	idx      *index
	cacheDir string
	timeout  time.Duration

	mu     sync.RWMutex
	client *user.Client
}

var _ store.Store = (*UserStore)(nil)

// StartUserStore authenticates as a Telegram user (phone + code / session file),
// ensures the private "Awaki" channel exists, and returns a Store.
// ctx should be process-lifetime; cancel on shutdown.
func StartUserStore(ctx context.Context, cfg UserConfig) (store.Store, error) {
	if cfg.CacheDir == "" {
		home, _ := os.UserHomeDir()
		cfg.CacheDir = filepath.Join(home, ".awaki", "tg-cache")
	}
	if cfg.SessionPath == "" {
		cfg.SessionPath = filepath.Join(cfg.CacheDir, "telegram.session")
	}
	if cfg.CodeFile == "" {
		cfg.CodeFile = filepath.Join(cfg.CacheDir, "login_code.txt")
	}
	if cfg.ChannelTitle == "" {
		cfg.ChannelTitle = "Awaki"
	}

	loc, err := local.New(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	s := &UserStore{
		local:    loc,
		idx:      loadIndex(filepath.Join(cfg.CacheDir, "tg_index.json")),
		cacheDir: cfg.CacheDir,
		timeout:  60 * time.Second,
	}

	ready := make(chan *user.Client, 1)
	errCh := make(chan error, 1)

	go func() {
		err := user.Run(ctx, user.Config{
			APIID:        cfg.APIID,
			APIHash:      cfg.APIHash,
			Phone:        cfg.Phone,
			Password:     cfg.Password,
			SessionPath:  cfg.SessionPath,
			CodeFile:     cfg.CodeFile,
			ChannelTitle: cfg.ChannelTitle,
		}, func(runCtx context.Context, c *user.Client) error {
			ready <- c
			<-runCtx.Done()
			return runCtx.Err()
		})
		errCh <- err
	}()

	select {
	case c := <-ready:
		s.mu.Lock()
		s.client = c
		s.mu.Unlock()
		log.Info().Msg("Telegrive-style user storage online")
		// Best-effort rebuild of index captions from recent history if index empty
		go s.rebuildIndexFromHistory(context.Background())
		return s, nil
	case err := <-errCh:
		if err == nil {
			err = fmt.Errorf("telegram user client exited before ready")
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("telegram user auth timed out — set AWAKI_TG_CODE or write %s", cfg.CodeFile)
	}
}

func (s *UserStore) cli() (*user.Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return nil, fmt.Errorf("telegram user client not ready")
	}
	return s.client, nil
}

func (s *UserStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.timeout)
}

func (s *UserStore) rebuildIndexFromHistory(ctx context.Context) {
	c, err := s.cli()
	if err != nil {
		return
	}
	_ = c.IterHistory(ctx, 200, func(msgID int, caption string, hasDoc bool) error {
		rec, ok := DecodeCaption(caption)
		if !ok {
			// also try full message body for text records
			rec, ok = DecodeCaption(extractDetailsLine(caption))
			if !ok {
				return nil
			}
		}
		key := rec.Fields["name"]
		if key == "" {
			key = rec.Fields["id"]
		}
		if key == "" {
			key = rec.Kind
		}
		_ = s.idx.set(rec.Kind, key, indexEntry{MessageID: msgID, Kind: rec.Kind, Key: key})
		return nil
	})
}

func extractDetailsLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "awaki:") {
			return line
		}
	}
	return s
}

// --- Identity ---

func (s *UserStore) ReadSoul() string {
	if body, ok := s.fetch("soul", "soul"); ok {
		_ = s.local.WriteSoul(body)
		return body
	}
	return s.local.ReadSoul()
}

func (s *UserStore) WriteSoul(content string) error {
	_ = s.local.WriteSoul(content)
	return s.upsert("soul", "soul", "soul.md", content, map[string]string{"name": "soul"})
}

func (s *UserStore) ReadUser() string {
	if body, ok := s.fetch("user", "user"); ok {
		_ = s.local.WriteUser(body)
		return body
	}
	return s.local.ReadUser()
}

func (s *UserStore) WriteUser(content string) error {
	_ = s.local.WriteUser(content)
	return s.upsert("user", "user", "user.md", content, map[string]string{"name": "user"})
}

func (s *UserStore) LoadTimezone() *time.Location { return s.local.LoadTimezone() }

// --- Memory ---

func (s *UserStore) ListMemory() ([]store.MemoryFile, error) {
	list, err := s.local.ListMemory()
	if err != nil {
		list = nil
	}
	seen := map[string]bool{}
	for _, m := range list {
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
		if body, ok := s.fetch("memory", e.Key); ok {
			_ = s.local.WriteMemory(name, body)
			list = append(list, store.MemoryFile{Name: name, Size: int64(len(body)), Content: body})
		}
	}
	return list, nil
}

func (s *UserStore) ReadMemory(filename string) (string, error) {
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	if body, ok := s.fetch("memory", key); ok {
		_ = s.local.WriteMemory(key+".md", body)
		return body, nil
	}
	return s.local.ReadMemory(filename)
}

func (s *UserStore) WriteMemory(filename, content string) error {
	if err := s.local.WriteMemory(filename, content); err != nil {
		return err
	}
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	return s.upsert("memory", key, key+".md", content, map[string]string{"name": key})
}

func (s *UserStore) DeleteMemory(filename string) error {
	key := strings.TrimSuffix(filepath.Base(filename), ".md")
	_ = s.local.DeleteMemory(filename)
	return s.deleteRec("memory", key)
}

// --- Skills ---

func (s *UserStore) SkillsDir() string { return s.local.SkillsDir() }

func (s *UserStore) LoadSkills() ([]store.Skill, error) {
	skills, err := s.local.LoadSkills()
	if err != nil {
		return nil, err
	}
	if len(skills) > 0 {
		return skills, nil
	}
	for _, e := range s.idx.list("skill") {
		if body, ok := s.fetch("skill", e.Key); ok {
			dir := filepath.Join(s.SkillsDir(), e.Key)
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644)
		}
	}
	return s.local.LoadSkills()
}

func (s *UserStore) CreateSkill(name, description, body string) (string, error) {
	path, err := s.local.CreateSkill(name, description, body)
	if err != nil {
		return "", err
	}
	content, _ := os.ReadFile(path)
	key := sanitizeName(name)
	_ = s.upsert("skill", key, key+".md", string(content), map[string]string{"name": key, "description": description})
	return path, nil
}

func (s *UserStore) UpdateSkill(name, content string) error {
	if err := s.local.UpdateSkill(name, content); err != nil {
		return err
	}
	key := sanitizeName(name)
	return s.upsert("skill", key, key+".md", content, map[string]string{"name": key})
}

func (s *UserStore) DeleteSkill(name string) error {
	_ = s.local.DeleteSkill(name)
	return s.deleteRec("skill", sanitizeName(name))
}

func (s *UserStore) ReadSkill(name string) (string, error) {
	key := sanitizeName(name)
	if body, ok := s.fetch("skill", key); ok {
		return body, nil
	}
	return s.local.ReadSkill(name)
}

// --- Chats ---

func (s *UserStore) NewChat() *store.Chat {
	ch := s.local.NewChat()
	ch.SetPersister(s)
	return ch
}

func (s *UserStore) LoadChat(id string) (*store.Chat, error) {
	ch, err := s.local.LoadChat(id)
	if err == nil {
		ch.SetPersister(s)
		return ch, nil
	}
	if body, ok := s.fetch("chat", id); ok {
		var meta store.Chat
		if json.Unmarshal([]byte(body), &meta) == nil {
			meta.SetPersister(s)
			_ = s.local.SaveChat(&meta)
			if msgBody, ok2 := s.fetch("chat_msgs", id); ok2 {
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

func (s *UserStore) ListChats() ([]store.ChatInfo, error) { return s.local.ListChats() }

func (s *UserStore) GetActiveChatID() (string, error) { return s.local.GetActiveChatID() }

func (s *UserStore) SetActiveChatID(id string) error {
	if err := s.local.SetActiveChatID(id); err != nil {
		return err
	}
	return s.upsert("config", "active_chat", "active_chat.txt", id, map[string]string{"key": "active_chat"})
}

func (s *UserStore) SaveChat(ch *store.Chat) error {
	if err := s.local.SaveChat(ch); err != nil {
		return err
	}
	meta, _ := json.Marshal(ch)
	_ = s.upsert("chat", ch.ID, ch.ID+".json", string(meta), map[string]string{
		"id": ch.ID, "title": ch.Title, "status": ch.Status,
	})
	if msgs := ch.Messages(); len(msgs) > 0 {
		data, _ := json.Marshal(msgs)
		_ = s.upsert("chat_msgs", ch.ID, ch.ID+".messages.json", string(data), map[string]string{
			"id": ch.ID, "count": fmt.Sprintf("%d", len(msgs)),
		})
	}
	return nil
}

func (s *UserStore) LoadMessages(ch *store.Chat) error { return s.local.LoadMessages(ch) }

func (s *UserStore) AppendMessage(ch *store.Chat, msg store.Turn) {
	s.local.AppendMessage(ch, msg)
}

func (s *UserStore) ReplaceMessages(ch *store.Chat, msgs []store.Turn) error {
	if err := s.local.ReplaceMessages(ch, msgs); err != nil {
		return err
	}
	data, _ := json.Marshal(msgs)
	return s.upsert("chat_msgs", ch.ID, ch.ID+".messages.json", string(data), map[string]string{
		"id": ch.ID, "count": fmt.Sprintf("%d", len(msgs)),
	})
}

func (s *UserStore) TextMessages(ch *store.Chat) ([]store.Message, error) {
	return s.local.TextMessages(ch)
}

func (s *UserStore) HasMessages(ch *store.Chat) bool { return s.local.HasMessages(ch) }

// --- Telegram IO ---

func (s *UserStore) upsert(kind, key, filename, content string, fields map[string]string) error {
	c, err := s.cli()
	if err != nil {
		log.Warn().Err(err).Msg("user store upsert skipped")
		return nil // cache already written; durable sync when online
	}
	ctx, cancel := s.ctx()
	defer cancel()

	caption := EncodeCaption(fmt.Sprintf("%s/%s", kind, key), kind, fields)

	if e, ok := s.idx.get(kind, key); ok && e.MessageID > 0 {
		_ = c.DeleteMessage(ctx, e.MessageID)
	}

	msgID, err := c.SendDocument(ctx, filename, []byte(content), caption)
	if err != nil {
		if len(content) < 3500 {
			text := caption + "\n\n" + content
			msgID, err = c.SendText(ctx, text)
		}
		if err != nil {
			log.Warn().Err(err).Str("kind", kind).Str("key", key).Msg("user store upsert failed")
			return err
		}
	}
	return s.idx.set(kind, key, indexEntry{MessageID: msgID, Kind: kind, Key: key})
}

func (s *UserStore) fetch(kind, key string) (string, bool) {
	e, ok := s.idx.get(kind, key)
	if !ok || e.MessageID == 0 {
		return "", false
	}
	c, err := s.cli()
	if err != nil {
		return "", false
	}
	ctx, cancel := s.ctx()
	defer cancel()
	data, err := c.DownloadMessageDocument(ctx, e.MessageID)
	if err != nil {
		log.Warn().Err(err).Str("kind", kind).Str("key", key).Msg("user store download failed")
		return "", false
	}
	// If text message, body may include caption + content
	body := string(data)
	if strings.Contains(body, "awaki:") {
		if i := strings.Index(body, "\n\n"); i >= 0 {
			body = body[i+2:]
		}
	}
	return body, true
}

func (s *UserStore) deleteRec(kind, key string) error {
	e, ok := s.idx.get(kind, key)
	if !ok {
		return nil
	}
	c, err := s.cli()
	if err != nil {
		return s.idx.del(kind, key)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_ = c.DeleteMessage(ctx, e.MessageID)
	return s.idx.del(kind, key)
}

var safeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	name = safeRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = uuid.NewString()[:8]
	}
	return name
}
