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
	// Upload the manifest and pin it (old db files are never deleted, so the
	// whole version history stays recoverable).
	mMsg, _, err := s.api.SendDocument(ctx, s.chatID, "awaki-manifest.json", manBytes,
		"awaki:dbmanifest;ver="+fmt.Sprint(ver))
	if err != nil {
		// Pin the database directly instead. It is the weaker recovery path —
		// no history and no recorded hash — but it keeps a healthy snapshot
		// recoverable, and on a first sync there is no previously pinned
		// manifest to fall back on, so without this a restart would find
		// nothing at all.
		log.Warn().Err(err).Msg("sqlite sync: manifest upload failed; pinning the database snapshot instead")
		if perr := s.api.PinMessage(ctx, s.chatID, msgID); perr != nil {
			log.Warn().Err(perr).Msg("sqlite sync: pinning the database snapshot also failed; this version is not recoverable")
		}
		return
	}
	// Pin the manifest. A chat has exactly one pinned message, so this has to be
	// the last pin of the cycle: pinning the db afterwards would displace the
	// manifest, and recovery reads the pinned document first. With the db pinned
	// the manifest is never fetched on restore, which silently disables the
	// rollback history and the hash check. Pinning only the manifest is also the
	// better recovery guarantee: the pinned manifest still points at the newest
	// db by file id, so nothing is lost.
	if err := s.api.PinMessage(ctx, s.chatID, mMsg); err != nil {
		// Not optional: without the pin, a restart finds no manifest and falls
		// back to the previous pinned snapshot, silently losing this version.
		log.Warn().Err(err).Msg("sqlite sync: pin manifest failed; recovery will fall back to the previously pinned snapshot")
	}

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
	// Checkpoint WAL into the main file so a plain copy is complete.
	_, _ = s.store.db.Exec(`PRAGMA wal_checkpoint(FULL)`)
	// VACUUM INTO yields a compact, consistent copy. Prefer it.
	if _, err := s.store.db.Exec(`VACUUM INTO ?`, tmp); err == nil {
		if data, rerr := os.ReadFile(tmp); rerr == nil {
			_ = os.Remove(tmp)
			sum := sha256.Sum256(data)
			return data, hex.EncodeToString(sum[:]), nil
		}
		_ = os.Remove(tmp)
	}
	// Fallback: read the database file directly. The hash must be computed from
	// the same bytes that are returned — reading the file before VACUUM ran and
	// then hashing that buffer would describe a payload the caller never
	// receives, so the recorded hash and the uploaded snapshot would disagree.
	data, err := os.ReadFile(s.store.path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// BootstrapFromTelegram restores the newest recoverable awaki.db from the
// storage chat.
//
// Order: the pinned document is expected to be the manifest; the manifest points
// at the newest database, with older versions in History. A pinned database is
// still accepted, because a flush that fails between the two uploads leaves one
// pinned and it is perfectly usable.
func BootstrapFromTelegram(ctx context.Context, api TGAPI, chatID int64, dbPath string) (bool, error) {
	caption, fileID, _, err := api.GetChatPinnedFile(ctx, chatID)
	if err != nil {
		return false, fmt.Errorf("get pinned: %w", err)
	}
	if fileID == "" {
		return false, fmt.Errorf("no pinned file in storage chat")
	}
	pinned, err := api.DownloadFile(ctx, fileID)
	if err != nil {
		return false, fmt.Errorf("download pinned: %w", err)
	}

	data, source, err := selectSnapshot(ctx, api, caption, pinned)
	if err != nil {
		return false, err
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

// selectSnapshot turns the pinned document into database bytes, or explains why
// it cannot. Every candidate is hash-checked against what was recorded at upload
// time: this snapshot is the only copy of everything the agent knows, so
// restoring a truncated download would replace a good database with a broken one
// and look like a success.
func selectSnapshot(ctx context.Context, api TGAPI, caption string, pinned []byte) ([]byte, string, error) {
	var man Manifest
	if json.Unmarshal(pinned, &man) == nil && man.LatestFileID != "" {
		newest, ver := man.LatestFileID, man.LatestVer
		data, err := api.DownloadFile(ctx, newest)
		if err == nil {
			if herr := verifyHash(data, man.LatestHash); herr != nil {
				err = herr
			}
		}
		if err == nil {
			log.Info().Int("ver", ver).Msg("sqlite bootstrap: using manifest latest")
			return data, fmt.Sprintf("manifest v%d", ver), nil
		}
		log.Warn().Err(err).Int("ver", ver).Msg("sqlite bootstrap: newest snapshot unusable, trying history")

		// The manifest still names older snapshots, and they are never deleted,
		// so walk back to the newest one that is present and intact. Better an
		// old database than a failed boot.
		for _, e := range man.History {
			if e.FileID == "" || e.FileID == newest {
				continue
			}
			older, herr := api.DownloadFile(ctx, e.FileID)
			if herr != nil {
				log.Debug().Err(herr).Int("ver", e.Ver).Msg("sqlite bootstrap: history entry unavailable")
				continue
			}
			if herr := verifyHash(older, e.Hash); herr != nil {
				log.Warn().Err(herr).Int("ver", e.Ver).Msg("sqlite bootstrap: history entry failed verification")
				continue
			}
			if !looksLikeSQLite(older) {
				continue
			}
			log.Warn().Int("want", ver).Int("using", e.Ver).Msg("sqlite bootstrap: recovered an earlier snapshot")
			return older, fmt.Sprintf("manifest v%d (v%d unusable)", e.Ver, ver), nil
		}
		return nil, "", fmt.Errorf("no usable snapshot in manifest: %w", err)
	}

	// Not a manifest. Accept it only if it is, or claims to be, a database.
	if stringsHasPrefix(caption, "awaki:db") || looksLikeSQLite(pinned) {
		log.Info().Msg("sqlite bootstrap: pinned document is a database snapshot")
		return pinned, "pinned-db", nil
	}
	return nil, "", fmt.Errorf("pinned document is not an awaki db or manifest (caption=%q)", trimCap(caption))
}

// verifyHash compares payload against an expected sha256 hex digest. An empty
// expectation is accepted: manifests written before hashes were recorded must
// still restore.
func verifyHash(payload []byte, want string) error {
	if want == "" {
		return nil
	}
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("payload hash mismatch: recorded %s, downloaded %s", short(want), short(got))
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
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
