package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed store.Store. Optional OnChange fires after mutations
// so a cloud syncer can debounce snapshot uploads.
type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	path     string
	skills   string // export dir for tools that expect SkillsDir on disk
	onChange func()
}

var _ store.Store = (*Store)(nil)

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "awaki.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // safe with WAL + single writer
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	s := &Store{
		db:     db,
		path:   path,
		skills: filepath.Join(dataDir, "skills"),
	}
	_ = os.MkdirAll(s.skills, 0o755)
	log.Info().Str("path", path).Msg("sqlite store open")
	return s, nil
}

func (s *Store) Path() string { return s.path }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) SetOnChange(fn func()) { s.onChange = fn }

func (s *Store) touch() {
	if s.onChange != nil {
		s.onChange()
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) setKV(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO kv(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now(),
	)
	return err
}

func (s *Store) getKV(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

func (s *Store) ReadSoul() string {
	v, _ := s.getKV("soul")
	return v
}

func (s *Store) WriteSoul(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setKV("soul", content); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) ReadUser() string {
	v, _ := s.getKV("user")
	return v
}

func (s *Store) WriteUser(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setKV("user", content); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) LoadTimezone() *time.Location {
	if user := s.ReadUser(); user != "" {
		for _, line := range strings.Split(user, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToLower(line), "timezone:") {
				tz := strings.TrimSpace(line[len("timezone:"):])
				if loc, err := time.LoadLocation(tz); err == nil {
					return loc
				}
			}
		}
	}
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

func (s *Store) ListMemory() ([]store.MemoryFile, error) {
	rows, err := s.db.Query(`SELECT name, content FROM memory ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MemoryFile
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			return nil, err
		}
		out = append(out, store.MemoryFile{Name: name, Size: int64(len(content)), Content: content})
	}
	return out, rows.Err()
}

func (s *Store) ReadMemory(filename string) (string, error) {
	name := safeName(filename)
	var content string
	err := s.db.QueryRow(`SELECT content FROM memory WHERE name=?`, name).Scan(&content)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("memory file %q not found", name)
	}
	return content, err
}

func (s *Store) WriteMemory(filename, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := safeName(filename)
	_, err := s.db.Exec(
		`INSERT INTO memory(name,content,updated_at) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET content=excluded.content, updated_at=excluded.updated_at`,
		name, content, now(),
	)
	if err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) DeleteMemory(filename string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM memory WHERE name=?`, safeName(filename))
	if err != nil {
		return err
	}
	s.touch()
	return nil
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func (s *Store) SkillsDir() string { return s.skills }

func (s *Store) LoadSkills() ([]store.Skill, error) {
	rows, err := s.db.Query(`SELECT name, description, body FROM skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Skill
	for rows.Next() {
		var name, desc, body string
		if err := rows.Scan(&name, &desc, &body); err != nil {
			return nil, err
		}
		// Export to disk so tools that open SkillsDir work.
		dir := filepath.Join(s.skills, name)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644)
		if desc == "" {
			desc = name
		}
		out = append(out, store.Skill{Name: name, Description: desc, FilePath: filepath.Join(dir, "SKILL.md"), Dir: dir})
	}
	return out, rows.Err()
}

func (s *Store) CreateSkill(name, description, body string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = safeName(name)
	content := body
	if description != "" && !strings.Contains(body, "---") {
		content = fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s", name, description, body)
	}
	_, err := s.db.Exec(
		`INSERT INTO skills(name,description,body,updated_at) VALUES(?,?,?,?)`,
		name, description, content, now(),
	)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.skills, name)
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "SKILL.md")
	_ = os.WriteFile(path, []byte(content), 0o644)
	s.touch()
	return path, nil
}

func (s *Store) UpdateSkill(name, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = safeName(name)
	res, err := s.db.Exec(
		`UPDATE skills SET body=?, updated_at=? WHERE name=?`,
		content, now(), name,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("skill %q not found", name)
	}
	dir := filepath.Join(s.skills, name)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
	s.touch()
	return nil
}

func (s *Store) DeleteSkill(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = safeName(name)
	_, err := s.db.Exec(`DELETE FROM skills WHERE name=?`, name)
	if err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(s.skills, name))
	s.touch()
	return nil
}

func (s *Store) ReadSkill(name string) (string, error) {
	name = safeName(name)
	var body string
	err := s.db.QueryRow(`SELECT body FROM skills WHERE name=?`, name).Scan(&body)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("skill %q not found", name)
	}
	return body, err
}

// ---------------------------------------------------------------------------
// Active chat
// ---------------------------------------------------------------------------

func (s *Store) GetActiveChatID() (string, error) {
	v, ok := s.getKV("active_chat")
	if !ok || v == "" {
		return "", fmt.Errorf("no active chat")
	}
	return v, nil
}

func (s *Store) SetActiveChatID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setKV("active_chat", id); err != nil {
		return err
	}
	s.touch()
	return nil
}

// ---------------------------------------------------------------------------
// Chats
// ---------------------------------------------------------------------------

func (s *Store) NewChat() *store.Chat {
	nowt := time.Now()
	ch := &store.Chat{
		ID:        uuid.NewString(),
		Status:    "active",
		StartedAt: nowt,
		UpdatedAt: nowt,
	}
	ch.SetPersister(s)
	return ch
}

func (s *Store) SaveChat(ch *store.Chat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch.UpdatedAt = time.Now()
	data, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO chats(id,meta_json,updated_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET meta_json=excluded.meta_json, updated_at=excluded.updated_at`,
		ch.ID, string(data), now(),
	)
	if err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) LoadChat(id string) (*store.Chat, error) {
	var meta string
	err := s.db.QueryRow(`SELECT meta_json FROM chats WHERE id=?`, id).Scan(&meta)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("chat %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	var ch store.Chat
	if err := json.Unmarshal([]byte(meta), &ch); err != nil {
		return nil, err
	}
	ch.SetPersister(s)
	_ = s.LoadMessages(&ch)
	return &ch, nil
}

func (s *Store) ListChats() ([]store.ChatInfo, error) {
	rows, err := s.db.Query(`SELECT meta_json FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ChatInfo
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			return nil, err
		}
		var ch store.Chat
		if json.Unmarshal([]byte(meta), &ch) != nil {
			continue
		}
		out = append(out, store.ChatInfo{
			ID:          ch.ID,
			Title:       ch.Title,
			Summary:     ch.Summary,
			Mode:        ch.Mode,
			ProjectSlug: ch.ProjectSlug,
			Tags:        ch.Tags,
			Status:      ch.Status,
			StartedAt:   ch.StartedAt,
			EndedAt:     ch.EndedAt,
		})
	}
	return out, rows.Err()
}

func (s *Store) LoadMessages(ch *store.Chat) error {
	rows, err := s.db.Query(`SELECT turn_json FROM messages WHERE chat_id=? ORDER BY seq`, ch.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []store.Turn
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var t store.Turn
		if json.Unmarshal([]byte(raw), &t) != nil {
			continue
		}
		msgs = append(msgs, t)
	}
	ch.SetMessages(msgs)
	return rows.Err()
}

func (s *Store) AppendMessage(ch *store.Chat, msg store.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seq int
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM messages WHERE chat_id=?`, ch.ID).Scan(&seq)
	seq++
	raw, _ := json.Marshal(msg)
	_, _ = s.db.Exec(`INSERT INTO messages(chat_id,seq,turn_json) VALUES(?,?,?)`, ch.ID, seq, string(raw))
	// keep meta in sync
	ch.UpdatedAt = time.Now()
	data, _ := json.Marshal(ch)
	_, _ = s.db.Exec(
		`INSERT INTO chats(id,meta_json,updated_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET meta_json=excluded.meta_json, updated_at=excluded.updated_at`,
		ch.ID, string(data), now(),
	)
	s.touch()
}

func (s *Store) ReplaceMessages(ch *store.Chat, msgs []store.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id=?`, ch.ID); err != nil {
		return err
	}
	for i, msg := range msgs {
		raw, _ := json.Marshal(msg)
		if _, err := tx.Exec(`INSERT INTO messages(chat_id,seq,turn_json) VALUES(?,?,?)`, ch.ID, i+1, string(raw)); err != nil {
			return err
		}
	}
	ch.UpdatedAt = time.Now()
	data, _ := json.Marshal(ch)
	if _, err := tx.Exec(
		`INSERT INTO chats(id,meta_json,updated_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET meta_json=excluded.meta_json, updated_at=excluded.updated_at`,
		ch.ID, string(data), now(),
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) TextMessages(ch *store.Chat) ([]store.Message, error) {
	if err := s.LoadMessages(ch); err != nil {
		return nil, err
	}
	var out []store.Message
	for _, t := range ch.Messages() {
		var text strings.Builder
		for _, b := range t.Content {
			if b.Type == "text" || b.Type == "" {
				text.WriteString(b.Text)
			}
		}
		out = append(out, store.Message{Role: t.Role, Content: text.String()})
	}
	return out, nil
}

func (s *Store) HasMessages(ch *store.Chat) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE chat_id=?`, ch.ID).Scan(&n)
	return n > 0
}

// ---------------------------------------------------------------------------
// Config blobs (providers / schedules JSON)
// ---------------------------------------------------------------------------

func (s *Store) GetConfigJSON(name string) ([]byte, error) {
	v, ok := s.getKV("config:"+name)
	if !ok {
		return nil, fmt.Errorf("config %q not found", name)
	}
	return []byte(v), nil
}

func (s *Store) SetConfigJSON(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setKV("config:"+name, string(data)); err != nil {
		return err
	}
	s.touch()
	return nil
}

// ---------------------------------------------------------------------------
// Blob pointers (project zips etc.)
// ---------------------------------------------------------------------------

type BlobRef struct {
	Kind      string
	Key       string
	FileID    string
	MessageID int
	Hash      string
	Size      int64
	MetaJSON  string
}

func (s *Store) UpsertBlob(ref BlobRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO blobs(kind,key,file_id,message_id,hash,size,meta_json,updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(kind,key) DO UPDATE SET
		   file_id=excluded.file_id, message_id=excluded.message_id,
		   hash=excluded.hash, size=excluded.size, meta_json=excluded.meta_json,
		   updated_at=excluded.updated_at`,
		ref.Kind, ref.Key, ref.FileID, ref.MessageID, ref.Hash, ref.Size, ref.MetaJSON, now(),
	)
	if err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Store) GetBlob(kind, key string) (BlobRef, bool) {
	var r BlobRef
	err := s.db.QueryRow(
		`SELECT kind,key,file_id,message_id,hash,size,meta_json FROM blobs WHERE kind=? AND key=?`,
		kind, key,
	).Scan(&r.Kind, &r.Key, &r.FileID, &r.MessageID, &r.Hash, &r.Size, &r.MetaJSON)
	if err != nil {
		return BlobRef{}, false
	}
	return r, true
}

func (s *Store) ListBlobs(kind string) []BlobRef {
	rows, err := s.db.Query(`SELECT kind,key,file_id,message_id,hash,size,meta_json FROM blobs WHERE kind=?`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BlobRef
	for rows.Next() {
		var r BlobRef
		if rows.Scan(&r.Kind, &r.Key, &r.FileID, &r.MessageID, &r.Hash, &r.Size, &r.MetaJSON) == nil {
			out = append(out, r)
		}
	}
	return out
}

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "..", "")
	if name == "" || name == "." {
		return "unnamed"
	}
	return name
}
