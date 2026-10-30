package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// TGAPI is the minimal Telegram surface needed for DB snapshots.
type TGAPI interface {
	SendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) (msgID int, fileID string, err error)
	PinMessage(ctx context.Context, chatID int64, messageID int) error
	GetChatPinnedFile(ctx context.Context, chatID int64) (caption string, fileID string, msgID int, err error)
	DownloadFile(ctx context.Context, fileID string) ([]byte, error)
	SendOrEditText(ctx context.Context, chatID int64, text string, editMsgID int) (msgID int, err error)
}

// Manifest is pinned in the storage chat and points at versioned DB snapshots.
type Manifest struct {
	LatestVer    int             `json:"latest_ver"`
	LatestFileID string          `json:"latest_file_id"`
	LatestHash   string          `json:"latest_hash"`
	LatestMsgID  int             `json:"latest_msg_id"`
	History      []ManifestEntry `json:"history"`
	UpdatedAt    string          `json:"updated_at"`
}

type ManifestEntry struct {
	Ver    int    `json:"ver"`
	FileID string `json:"file_id"`
	MsgID  int    `json:"msg_id"`
	Hash   string `json:"hash"`
	TS     string `json:"ts"`
	Size   int    `json:"size"`
}

// Syncer debounces uploads of awaki.db to Telegram (append-only versions).
type Syncer struct {
	mu       sync.Mutex
	store    *Store
	api      TGAPI
	chatID   int64
	interval time.Duration
	dirty    bool
	lastHash string
	stopCh   chan struct{}
	doneCh   chan struct{}
	manifest Manifest
	manMsgID int    // text/doc message id for manifest pin target
	onSync   func() // fired after a successful DB upload to cloud
}

func NewSyncer(store *Store, api TGAPI, chatID int64, interval time.Duration) *Syncer {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s := &Syncer{
		store:    store,
		api:      api,
		chatID:   chatID,
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	store.SetOnChange(s.MarkDirty)
	return s
}

// SetOnSync registers a callback fired after a successful DB upload to cloud storage.
// Intended to also trigger a project zip sync so DB + project stay in step.
func (s *Syncer) SetOnSync(fn func()) {
	s.mu.Lock()
	s.onSync = fn
	s.mu.Unlock()
}

func (s *Syncer) MarkDirty() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
}

func (s *Syncer) Start() {
	go s.loop()
}

func (s *Syncer) Stop() {
	close(s.stopCh)
	<-s.doneCh
	// final flush
	s.flush(true)
}

func (s *Syncer) loop() {
	defer close(s.doneCh)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.flush(false)
		}
	}
}

func (s *Syncer) flush(force bool) {
	s.mu.Lock()
	if !s.dirty && !force {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.mu.Unlock()

	data, hash, err := s.snapshotBytes()
	if err != nil {
		log.Warn().Err(err).Msg("sqlite sync: snapshot failed")
		s.MarkDirty()
		return
	}
	s.mu.Lock()
	same := hash == s.lastHash && s.lastHash != ""
	s.mu.Unlock()
	if same {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s.mu.Lock()
	ver := s.manifest.LatestVer + 1
	s.mu.Unlock()

	filename := fmt.Sprintf("awaki-v%d.db", ver)
	caption := fmt.Sprintf("awaki:db;ver=%d;hash=%s", ver, hash)
	msgID, fileID, err := s.api.SendDocument(ctx, s.chatID, filename, data, caption)
	if err != nil {
		log.Warn().Err(err).Msg("sqlite sync: upload db failed")
		s.MarkDirty()
		return
	}

	entry := ManifestEntry{
		Ver: ver, FileID: fileID, MsgID: msgID, Hash: hash,
		TS: time.Now().UTC().Format(time.RFC3339), Size: len(data),
	}
	s.mu.Lock()
	s.manifest.LatestVer = ver
	s.manifest.LatestFileID = fileID
	s.manifest.LatestHash = hash
	s.manifest.LatestMsgID = msgID
	s.manifest.UpdatedAt = entry.TS
	s.manifest.History = append([]ManifestEntry{entry}, s.manifest.History...)
	if len(s.manifest.History) > 40 {
		s.manifest.History = s.manifest.History[:40]
	}
	s.lastHash = hash
	man := s.manifest
	s.mu.Unlock()

	manBytes, _ := json.MarshalIndent(man, "", "  ")
	// Upload manifest as document + pin it (never delete old db files).
	mMsg, mFile, err := s.api.SendDocument(ctx, s.chatID, "awaki-manifest.json", manBytes,
		"awaki:dbmanifest;ver="+fmt.Sprint(ver))
	if err != nil {
		log.Warn().Err(err).Msg("sqlite sync: upload manifest failed")
		return
	}
	_ = mFile
	if err := s.api.PinMessage(ctx, s.chatID, mMsg); err != nil {
		log.Debug().Err(err).Msg("sqlite sync: pin manifest (optional)")
	}
	// Also pin latest db as secondary recovery path
	_ = s.api.PinMessage(ctx, s.chatID, msgID)

	log.Info().Int("ver", ver).Str("hash", hash[:12]).Int("bytes", len(data)).Msg("sqlite sync: uploaded db snapshot")
	s.mu.Lock()
	onSync := s.onSync
	s.mu.Unlock()
	if onSync != nil {
		onSync()
	}
}

func (s *Syncer) snapshotBytes() ([]byte, string, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tmp := s.store.path + ".snap"
	// Checkpoint WAL into main file then copy
	_, _ = s.store.db.Exec(`PRAGMA wal_checkpoint(FULL)`)
	in, err := os.ReadFile(s.store.path)
	if err != nil {
		return nil, "", err
	}
	// Use VACUUM INTO when possible for compact consistent copy
	_, err = s.store.db.Exec(`VACUUM INTO ?`, tmp)
	if err == nil {
		if data, rerr := os.ReadFile(tmp); rerr == nil {
			_ = os.Remove(tmp)
			sum := sha256.Sum256(data)
			return data, hex.EncodeToString(sum[:]), nil
		}
	}
	_ = os.Remove(tmp)
	sum := sha256.Sum256(in)
	return in, hex.EncodeToString(sum[:]), nil
}

// BootstrapFromTelegram restores the latest versioned awaki.db from the storage chat.
// Order inside Telegram: pinned manifest → latest_file_id DB, or pinned DB document.
func BootstrapFromTelegram(ctx context.Context, api TGAPI, chatID int64, dbPath string) (bool, error) {
	cap, fileID, _, err := api.GetChatPinnedFile(ctx, chatID)
	if err != nil {
		return false, fmt.Errorf("get pinned: %w", err)
	}
	if fileID == "" {
		return false, fmt.Errorf("no pinned file in storage chat")
	}
	data, err := api.DownloadFile(ctx, fileID)
	if err != nil {
		return false, fmt.Errorf("download pinned: %w", err)
	}

	source := "pinned"
	var man Manifest
	if json.Unmarshal(data, &man) == nil && man.LatestFileID != "" {
		data, err = api.DownloadFile(ctx, man.LatestFileID)
		if err != nil {
			return false, fmt.Errorf("download manifest latest db: %w", err)
		}
		source = fmt.Sprintf("manifest v%d", man.LatestVer)
		log.Info().Int("ver", man.LatestVer).Str("hash", man.LatestHash).Msg("sqlite bootstrap: using manifest latest")
	} else if stringsHasPrefix(cap, "awaki:db") || looksLikeSQLite(data) {
		source = "pinned-db"
		log.Info().Msg("sqlite bootstrap: pinned message is a db snapshot")
	} else {
		return false, fmt.Errorf("pinned message is not an awaki db/manifest (caption=%q)", trimCap(cap))
	}
	if !looksLikeSQLite(data) {
		return false, fmt.Errorf("downloaded payload is not a sqlite database")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return false, err
	}
	tmp := dbPath + ".bootstrap"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	log.Info().Str("source", source).Int("bytes", len(data)).Str("path", dbPath).Msg("sqlite bootstrap: restored previous database")
	return true, nil
}

func trimCap(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func looksLikeSQLite(data []byte) bool {
	return len(data) >= 16 && string(data[:15]) == "SQLite format 3"
}

func stringsHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// LocalDBUsable reports whether path is an existing non-empty SQLite database.
func LocalDBUsable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < 100 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hdr := make([]byte, 16)
	n, _ := f.Read(hdr)
	return looksLikeSQLite(hdr[:n])
}
