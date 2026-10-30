package project

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/gemechischala/awaki/internal/workspace"
)

const (
	ModeChat    = "chat"
	ModeProject = "project"
)

// Meta is persisted next to the working tree and in archive captions.
type Meta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	ChatID    string    `json:"chat_id,omitempty"`
	Hash      string    `json:"hash,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BlobStore is the durable backend (local files and/or Telegram).
type BlobStore interface {
	SaveArchive(ctx context.Context, meta Meta, zip []byte) error
	LoadArchive(ctx context.Context, slug string) (Meta, []byte, error)
	ListArchives(ctx context.Context) ([]Meta, error)
}

// Manager owns local project dirs under dataDir/projects.
type Manager struct {
	dataDir string
	blobs   BlobStore

	mu       sync.Mutex
	dirty    bool
	onChange func()
}

func NewManager(dataDir string, blobs BlobStore) *Manager {
	if blobs == nil {
		blobs = NewLocalBlobStore(filepath.Join(dataDir, "project-archives"))
	}
	return &Manager{dataDir: dataDir, blobs: blobs}
}

// SetOnChange registers a callback fired after project-affecting operations,
// so an auto-syncer can debounce zip uploads to cloud storage.
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *Manager) touch() {
	m.mu.Lock()
	m.dirty = true
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *Manager) projectsRoot() string {
	return filepath.Join(m.dataDir, "projects")
}

func (m *Manager) scratchRoot() string {
	return filepath.Join(m.dataDir, "scratch")
}

func (m *Manager) ProjectDir(slug string) string {
	return filepath.Join(m.projectsRoot(), slug)
}

func (m *Manager) metaPath(slug string) string {
	return filepath.Join(m.ProjectDir(slug), ".awaki-project.json")
}

// ActivateScratch sets workspace to a per-chat disposable dir (normal chats).
func (m *Manager) ActivateScratch(chatID string) error {
	dir := filepath.Join(m.scratchRoot(), chatID)
	return workspace.SetRoot(dir)
}

// ActivateProject sets workspace to projects/{slug}, extracting archive if missing.
func (m *Manager) ActivateProject(ctx context.Context, slug string) error {
	dir := m.ProjectDir(slug)
	marker := m.metaPath(slug)
	if _, err := os.Stat(marker); err != nil {
		// try restore from blob store
		meta, data, err := m.blobs.LoadArchive(ctx, slug)
		if err != nil {
			// create empty project dir
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		} else {
			if err := ExtractZip(data, dir); err != nil {
				return fmt.Errorf("extract project: %w", err)
			}
			_ = m.writeMeta(meta)
		}
	}
	return workspace.SetRoot(dir)
}

// Create creates a new project directory and meta; does not bind chat (caller does).
func (m *Manager) Create(name, chatID string) (Meta, error) {
	slug := Slugify(name)
	base := slug
	for i := 2; ; i++ {
		dir := m.ProjectDir(slug)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
		if i > 50 {
			return Meta{}, fmt.Errorf("could not allocate unique slug")
		}
	}
	meta := Meta{
		ID:        uuid.NewString(),
		Name:      strings.TrimSpace(name),
		Slug:      slug,
		ChatID:    chatID,
		UpdatedAt: time.Now().UTC(),
	}
	if meta.Name == "" {
		meta.Name = slug
	}
	if err := os.MkdirAll(m.ProjectDir(slug), 0o755); err != nil {
		return Meta{}, err
	}
	if err := m.writeMeta(meta); err != nil {
		return Meta{}, err
	}
	if err := workspace.SetRoot(m.ProjectDir(slug)); err != nil {
		return Meta{}, err
	}
	log.Info().Str("slug", slug).Str("name", meta.Name).Msg("project created")
	return meta, nil
}

func (m *Manager) writeMeta(meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.metaPath(meta.Slug), data, 0o644)
}

func (m *Manager) ReadMeta(slug string) (Meta, error) {
	data, err := os.ReadFile(m.metaPath(slug))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Sync zips the local tree and saves to the blob store if content changed.
func (m *Manager) Sync(ctx context.Context, slug string) (Meta, error) {
	meta, err := m.ReadMeta(slug)
	if err != nil {
		// synthesize from slug
		meta = Meta{ID: uuid.NewString(), Name: slug, Slug: slug, UpdatedAt: time.Now().UTC()}
	}
	data, hash, err := ZipTree(m.ProjectDir(slug))
	if err != nil {
		return meta, err
	}
	if meta.Hash == hash && hash != "" {
		log.Debug().Str("slug", slug).Msg("project sync skipped (unchanged)")
		return meta, nil
	}
	// size guard ~45MB
	if len(data) > 45<<20 {
		return meta, fmt.Errorf("project archive too large (%d bytes); add ignores or split the project", len(data))
	}
	meta.Hash = hash
	meta.UpdatedAt = time.Now().UTC()
	if err := m.blobs.SaveArchive(ctx, meta, data); err != nil {
		return meta, err
	}
	_ = m.writeMeta(meta)
	log.Info().Str("slug", slug).Str("hash", hash).Int("bytes", len(data)).Msg("project synced")
	return meta, nil
}

// MarkDirty marks the project tree as changed so a debounced auto-sync will
// pick it up on the next tick.
func (m *Manager) MarkDirty() {
	m.mu.Lock()
	m.dirty = true
	m.mu.Unlock()
}

// IsDirtyAndClear returns the dirty flag and resets it.
func (m *Manager) IsDirtyAndClear() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dirty
	m.dirty = false
	return d
}

// ListLocal returns projects present on disk.
func (m *Manager) ListLocal() ([]Meta, error) {
	entries, err := os.ReadDir(m.projectsRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := m.ReadMeta(e.Name())
		if err != nil {
			out = append(out, Meta{Name: e.Name(), Slug: e.Name()})
			continue
		}
		out = append(out, meta)
	}
	return out, nil
}

// EnsureSessionWorkspace sets scratch or project root for a chat.
func (m *Manager) EnsureSessionWorkspace(ctx context.Context, mode, slug, chatID string) error {
	if mode == ModeProject && slug != "" {
		return m.ActivateProject(ctx, slug)
	}
	if chatID == "" {
		chatID = "default"
	}
	return m.ActivateScratch(chatID)
}

// ListCloud returns archives from the blob backend (Telegram + local).
func (m *Manager) ListCloud(ctx context.Context) ([]Meta, error) {
	return m.blobs.ListArchives(ctx)
}

// CopyTreeIgnoring copies files from src into dest, applying DefaultIgnore rules.
// Existing files in dest are overwritten. Directories are created as needed.
// Returns the number of files copied.
func CopyTreeIgnoring(src, dest string) (int, error) {
	if src == "" || dest == "" {
		return 0, fmt.Errorf("src and dest required")
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return 0, err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return 0, err
	}
	if srcAbs == destAbs {
		return 0, nil
	}
	// Avoid copying a parent into a child
	if strings.HasPrefix(destAbs+string(os.PathSeparator), srcAbs+string(os.PathSeparator)) {
		return 0, fmt.Errorf("refusing to copy parent into child")
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return 0, err
	}
	copied := 0
	err = filepath.Walk(srcAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if shouldIgnore(relSlash) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destAbs, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		copied++
		return nil
	})
	return copied, err
}

// PromoteFromWorkspace creates a project and copies the current workspace
// (usually scratch) into it. Caller is responsible for updating chat mode/slug
// and calling Sync. Returns meta and how many files were copied.
func (m *Manager) PromoteFromWorkspace(name, chatID, currentRoot string) (Meta, int, error) {
	meta, err := m.Create(name, chatID)
	if err != nil {
		return Meta{}, 0, err
	}
	n := 0
	if currentRoot != "" {
		// Create already set workspace to the new project dir — copy from the
		// previous root before that happened would be ideal, so callers should
		// pass the old root. If currentRoot is already the project dir, skip.
		projDir := m.ProjectDir(meta.Slug)
		if filepath.Clean(currentRoot) != filepath.Clean(projDir) {
			n, err = CopyTreeIgnoring(currentRoot, projDir)
			if err != nil {
				return meta, n, fmt.Errorf("copy workspace into project: %w", err)
			}
		}
	}
	// Ensure workspace is bound to the new project
	_ = workspace.SetRoot(m.ProjectDir(meta.Slug))
	m.touch()
	return meta, n, nil
}
