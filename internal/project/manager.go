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
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	ChatID      string    `json:"chat_id,omitempty"`
	Hash        string    `json:"hash,omitempty"`         // legacy: zip-byte hash (backward compat)
	ContentHash string    `json:"content_hash,omitempty"` // content-based hash (file paths + contents)
	FileCount   int       `json:"file_count,omitempty"`   // number of non-ignored files
	TotalSize   int64     `json:"total_size,omitempty"`   // total bytes of non-ignored files
	LastEditor  string    `json:"last_editor,omitempty"`  // last chat_id that edited
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProjectState tracks in-memory dirty state for auto-sync. Not persisted.
type ProjectState struct {
	mu          sync.Mutex
	slug        string
	dirty       bool
	changeCount int
	lastSync    time.Time
}

func newProjectState(slug string) *ProjectState {
	return &ProjectState{slug: slug}
}

func (ps *ProjectState) markDirty() {
	ps.mu.Lock()
	ps.dirty = true
	ps.changeCount++
	ps.mu.Unlock()
}

func (ps *ProjectState) isDirtyAndClear() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	d := ps.dirty
	ps.dirty = false
	ps.changeCount = 0
	return d
}

func (ps *ProjectState) setLastSync(t time.Time) {
	ps.mu.Lock()
	ps.lastSync = t
	ps.mu.Unlock()
}

func (ps *ProjectState) lastSyncTime() time.Time {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastSync
}

var psMu sync.Mutex
var projectStates = map[string]*ProjectState{}

func stateFor(slug string) *ProjectState {
	psMu.Lock()
	defer psMu.Unlock()
	s, ok := projectStates[slug]
	if !ok {
		s = newProjectState(slug)
		projectStates[slug] = s
	}
	return s
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
// It refuses to reuse a slug that already exists locally or in the blob store
// (allocates slug-2, slug-3, … instead) so cloud archives are never clobbered.
func (m *Manager) Create(name, chatID string) (Meta, error) {
	return m.CreateCtx(context.Background(), name, chatID)
}

// CreateCtx is Create with a context so cloud existence can be checked.
func (m *Manager) CreateCtx(ctx context.Context, name, chatID string) (Meta, error) {
	slug := Slugify(name)
	base := slug
	for i := 2; ; i++ {
		exists, _, _ := m.ProjectExists(ctx, slug)
		if !exists {
			// Also require the directory itself to be free (race / empty dir).
			if _, err := os.Stat(m.ProjectDir(slug)); os.IsNotExist(err) {
				break
			}
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

// ImportProject ensures a project exists at the exact exported slug.
// Unlike Create, it never allocates a new slug or UUID.
//
//	merge (overwrite=false): if slug exists, leave local meta as-is and return it
//	                         (so chats can be remapped onto the local project id);
//	                         if missing, create with the exported id/slug.
//	overwrite: replace meta with the export (same slug), keeping the export id.
//
// Returns the effective meta that callers should use for chat linking.
func (m *Manager) ImportProject(meta Meta, overwrite bool) (effective Meta, changed bool, err error) {
	meta.Slug = strings.TrimSpace(meta.Slug)
	meta.Name = strings.TrimSpace(meta.Name)
	if meta.Slug == "" {
		if meta.Name != "" {
			meta.Slug = Slugify(meta.Name)
		} else {
			return Meta{}, false, fmt.Errorf("project slug is required")
		}
	}
	if meta.Name == "" {
		meta.Name = meta.Slug
	}
	if meta.ID == "" {
		meta.ID = uuid.NewString()
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = time.Now().UTC()
	}

	exists, local, _ := m.ProjectExists(context.Background(), meta.Slug)
	if exists && !overwrite {
		// Keep local meta (union semantics). Ensure directory exists for tree merge.
		_ = os.MkdirAll(m.ProjectDir(local.Slug), 0o755)
		if local.ID == "" {
			local.ID = meta.ID
			_ = m.writeMeta(local)
		}
		return local, false, nil
	}
	if err := os.MkdirAll(m.ProjectDir(meta.Slug), 0o755); err != nil {
		return Meta{}, false, err
	}
	if err := m.writeMeta(meta); err != nil {
		return Meta{}, false, err
	}
	m.touch()
	log.Info().Str("slug", meta.Slug).Str("id", meta.ID).Bool("overwrite", overwrite).Msg("project imported")
	return meta, true, nil
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

// ProjectExists reports whether a project is already known: local meta file,
// local project directory, or an archive in the blob store (cloud/local zip).
// Use this before creating so /project <slug> activates existing work instead
// of overwriting it with an empty tree.
func (m *Manager) ProjectExists(ctx context.Context, slug string) (bool, Meta, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return false, Meta{}, nil
	}
	if meta, err := m.ReadMeta(slug); err == nil {
		return true, meta, nil
	}
	// Directory present but meta missing (e.g. partial restore) — still exists.
	if info, err := os.Stat(m.ProjectDir(slug)); err == nil && info.IsDir() {
		return true, Meta{Name: slug, Slug: slug}, nil
	}
	if m.blobs != nil {
		if list, err := m.blobs.ListArchives(ctx); err == nil {
			for _, meta := range list {
				if meta.Slug == slug {
					return true, meta, nil
				}
			}
		}
		// List may be incomplete; try a direct load as a last resort.
		if meta, _, err := m.blobs.LoadArchive(ctx, slug); err == nil {
			return true, meta, nil
		}
	}
	return false, Meta{}, nil
}

// Sync zips the local tree and saves to the blob store if content changed.
// Change detection uses the content hash (file paths + contents), not zip bytes,
// so re-zipping the same files does not trigger a re-upload.
// lastEditor is the chat_id that triggered the change (may be empty).
func (m *Manager) Sync(ctx context.Context, slug string, lastEditor ...string) (Meta, error) {
	meta, err := m.ReadMeta(slug)
	if err != nil {
		// synthesize from slug
		meta = Meta{ID: uuid.NewString(), Name: slug, Slug: slug, UpdatedAt: time.Now().UTC()}
	}
	data, contentHash, fileCount, totalSize, err := ZipTree(m.ProjectDir(slug))
	if err != nil {
		return meta, err
	}
	if meta.ContentHash == contentHash && contentHash != "" {
		log.Debug().Str("slug", slug).Msg("project sync skipped (content unchanged)")
		return meta, nil
	}
	// Never push an empty local tree over a non-empty cloud archive.
	if fileCount == 0 && m.blobs != nil {
		if cloudMeta, _, loadErr := m.blobs.LoadArchive(ctx, slug); loadErr == nil {
			if cloudMeta.FileCount > 0 || (cloudMeta.ContentHash != "" && cloudMeta.ContentHash != contentHash) {
				log.Warn().
					Str("slug", slug).
					Int("local_files", fileCount).
					Int("cloud_files", cloudMeta.FileCount).
					Msg("project sync refused: local empty, cloud has data")
				return meta, fmt.Errorf("refusing to overwrite non-empty cloud archive for %q with empty local tree", slug)
			}
		}
	}
	// size guard ~45MB
	if len(data) > 45<<20 {
		return meta, fmt.Errorf("project archive too large (%d bytes); add ignores or split the project", len(data))
	}
	// Keep legacy zip-byte hash for backward compatibility.
	meta.Hash = LegacyZipHash(data)
	meta.ContentHash = contentHash
	meta.FileCount = fileCount
	meta.TotalSize = totalSize
	if len(lastEditor) > 0 && lastEditor[0] != "" {
		meta.LastEditor = lastEditor[0]
	}
	meta.UpdatedAt = time.Now().UTC()
	if err := m.blobs.SaveArchive(ctx, meta, data); err != nil {
		return meta, err
	}
	_ = m.writeMeta(meta)
	log.Info().Str("slug", slug).Str("content_hash", contentHash).Int("bytes", len(data)).Msg("project synced")
	return meta, nil
}

// MarkDirty marks the project tree as changed so a debounced auto-sync will
// pick it up on the next tick.
func (m *Manager) MarkDirty(slug string) {
	stateFor(slug).markDirty()
}

// IsDirtyAndClear returns the dirty flag and resets it.
func (m *Manager) IsDirtyAndClear(slug string) bool {
	return stateFor(slug).isDirtyAndClear()
}

// LastSync returns the last successful sync time for a slug.
func (m *Manager) LastSync(slug string) time.Time {
	return stateFor(slug).lastSyncTime()
}

// SyncIfDirty zips and uploads the project only if it has been marked dirty
// since the last sync AND the content hash actually changed. Returns
// (synced, meta, err). If not dirty, returns (false, meta, nil) without
// touching cloud storage. lastEditor is the chat_id that triggered the change.
func (m *Manager) SyncIfDirty(ctx context.Context, slug string, lastEditor ...string) (bool, Meta, error) {
	if !m.IsDirtyAndClear(slug) {
		meta, _ := m.ReadMeta(slug)
		return false, meta, nil
	}
	meta, err := m.Sync(ctx, slug, lastEditor...)
	if err != nil {
		return false, meta, err
	}
	stateFor(slug).setLastSync(meta.UpdatedAt)
	return true, meta, nil
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
//
// If a project with the same slug already exists (local or cloud), this returns
// an error — callers should ActivateProject instead of promote/create.
func (m *Manager) PromoteFromWorkspace(name, chatID, currentRoot string) (Meta, int, error) {
	return m.PromoteFromWorkspaceCtx(context.Background(), name, chatID, currentRoot)
}

// PromoteFromWorkspaceCtx is PromoteFromWorkspace with context for existence checks.
func (m *Manager) PromoteFromWorkspaceCtx(ctx context.Context, name, chatID, currentRoot string) (Meta, int, error) {
	slug := Slugify(name)
	if exists, existing, _ := m.ProjectExists(ctx, slug); exists {
		return existing, 0, fmt.Errorf("project %q already exists (use activate, not create)", existing.Slug)
	}
	meta, err := m.CreateCtx(ctx, name, chatID)
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

// Rename changes the display name of a project (the slug / directory never
// change, so chats bound to it stay valid). The new name is written to the
// local meta file and — best effort — to the cloud archive caption.
func (m *Manager) Rename(ctx context.Context, slug, name string) (Meta, error) {
	name = strings.TrimSpace(strings.Join(strings.Fields(name), " "))
	if name == "" {
		return Meta{}, fmt.Errorf("name is empty")
	}
	if len([]rune(name)) > 60 {
		return Meta{}, fmt.Errorf("name too long (max 60 characters)")
	}
	meta, err := m.ReadMeta(slug)
	if err != nil {
		if info, serr := os.Stat(m.ProjectDir(slug)); serr != nil || !info.IsDir() {
			return Meta{}, fmt.Errorf("project %q is not available locally", slug)
		}
		meta = Meta{ID: uuid.NewString(), Slug: slug, UpdatedAt: time.Now().UTC()}
	}
	meta.Name = name
	if err := m.writeMeta(meta); err != nil {
		return Meta{}, err
	}
	if m.blobs != nil {
		// Re-save the archive so the new caption reaches the cloud, but never
		// push an empty tree (Sync's safety rule).
		if data, _, fileCount, _, zerr := ZipTree(m.ProjectDir(slug)); zerr == nil && fileCount > 0 {
			if err := m.blobs.SaveArchive(ctx, meta, data); err != nil {
				log.Warn().Err(err).Str("slug", slug).Msg("project rename: cloud caption update failed")
			}
		}
	}
	m.touch()
	return meta, nil
}
