package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LocalBlobStore keeps zip archives under a directory (VPS cache / offline).
// Telegram backend can wrap or replace this for durable cloud storage.
type LocalBlobStore struct {
	dir string
}

func NewLocalBlobStore(dir string) *LocalBlobStore {
	return &LocalBlobStore{dir: dir}
}

func (s *LocalBlobStore) SaveArchive(_ context.Context, meta Meta, zip []byte) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(s.dir, meta.Slug+".zip")
	metaPath := filepath.Join(s.dir, meta.Slug+".json")
	if err := os.WriteFile(zipPath, zip, 0o600); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0o600)
}

func (s *LocalBlobStore) LoadArchive(_ context.Context, slug string) (Meta, []byte, error) {
	metaPath := filepath.Join(s.dir, slug+".json")
	zipPath := filepath.Join(s.dir, slug+".zip")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return Meta{}, nil, err
	}
	var meta Meta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return Meta{}, nil, err
	}
	zip, err := os.ReadFile(zipPath)
	if err != nil {
		return meta, nil, err
	}
	return meta, zip, nil
}

func (s *LocalBlobStore) ListArchives(_ context.Context) ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var meta Meta
		if json.Unmarshal(data, &meta) == nil {
			out = append(out, meta)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// Ensure LocalBlobStore implements BlobStore.
var _ BlobStore = (*LocalBlobStore)(nil)

// MultiBlobStore writes to all backends; reads from the first that succeeds.
type MultiBlobStore struct {
	Primary   BlobStore
	Secondary BlobStore // optional (e.g. Telegram)
}

func (m *MultiBlobStore) SaveArchive(ctx context.Context, meta Meta, zip []byte) error {
	if m.Primary != nil {
		if err := m.Primary.SaveArchive(ctx, meta, zip); err != nil {
			return err
		}
	}
	if m.Secondary != nil {
		if err := m.Secondary.SaveArchive(ctx, meta, zip); err != nil {
			// durable secondary failure is important
			return fmt.Errorf("cloud archive: %w", err)
		}
	}
	return nil
}

func (m *MultiBlobStore) LoadArchive(ctx context.Context, slug string) (Meta, []byte, error) {
	if m.Primary != nil {
		if meta, data, err := m.Primary.LoadArchive(ctx, slug); err == nil {
			return meta, data, nil
		}
	}
	if m.Secondary != nil {
		return m.Secondary.LoadArchive(ctx, slug)
	}
	return Meta{}, nil, fmt.Errorf("archive %q not found", slug)
}

func (m *MultiBlobStore) ListArchives(ctx context.Context) ([]Meta, error) {
	seen := map[string]Meta{}
	if m.Secondary != nil {
		if list, err := m.Secondary.ListArchives(ctx); err == nil {
			for _, x := range list {
				seen[x.Slug] = x
			}
		}
	}
	if m.Primary != nil {
		if list, err := m.Primary.ListArchives(ctx); err == nil {
			for _, x := range list {
				seen[x.Slug] = x
			}
		}
	}
	out := make([]Meta, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}
