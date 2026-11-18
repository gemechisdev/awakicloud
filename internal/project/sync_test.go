package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type memBlobs struct {
	meta map[string]Meta
	data map[string][]byte
}

func newMemBlobs() *memBlobs { return &memBlobs{meta: map[string]Meta{}, data: map[string][]byte{}} }
func (b *memBlobs) SaveArchive(_ context.Context, m Meta, zip []byte) error {
	b.meta[m.Slug] = m
	b.data[m.Slug] = append([]byte(nil), zip...)
	return nil
}
func (b *memBlobs) LoadArchive(_ context.Context, slug string) (Meta, []byte, error) {
	m, ok := b.meta[slug]
	if !ok {
		return Meta{}, nil, os.ErrNotExist
	}
	return m, b.data[slug], nil
}
func (b *memBlobs) ListArchives(context.Context) ([]Meta, error) { return nil, nil }

// A file changed WITHOUT MarkDirty (bash / external write) must be synced.
func TestAutoSyncDetectsChangesNotMarkedDirty(t *testing.T) {
	dir := t.TempDir()
	blobs := newMemBlobs()
	m := NewManager(dir, blobs)
	slug := "demo"
	if _, err := m.Create(slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sync(context.Background(), slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	if len(blobs.data[slug]) == 0 {
		t.Fatal("initial sync produced no archive")
	}
	// unmarked change
	if err := os.WriteFile(filepath.Join(m.ProjectDir(slug), "hello.txt"), []byte("written by bash, no dirty flag\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	synced, meta, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatalf("SyncIfTreeChanged: %v", err)
	}
	if !synced {
		t.Fatal("auto-sync did NOT detect an unmarked tree change (the bug)")
	}
	if meta.ContentHash == "" {
		t.Error("content hash not recorded")
	}
	// idempotent: no change -> no upload
	synced2, _, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if synced2 {
		t.Error("auto-sync re-uploaded even though nothing changed")
	}
}

func TestSyncIfTreeChangedIgnoresIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, newMemBlobs())
	slug := "demo"
	if _, err := m.Create(slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sync(context.Background(), slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(m.ProjectDir(slug), "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "junk.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	synced, _, err := m.SyncIfTreeChanged(context.Background(), slug, "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if synced {
		t.Error("ignored files (node_modules) must not trigger a sync")
	}
}

// The meta file must not poison the content hash (it lives inside the tree).
func TestMetaFileDoesNotPoisonHash(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, newMemBlobs())
	slug := "demo"
	if _, err := m.Create(slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sync(context.Background(), slug, "chat1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if synced, _, _ := m.SyncIfTreeChanged(context.Background(), slug, "chat1"); synced {
			t.Fatalf("sync %d: re-uploaded with no file change (meta file poisoning the hash)", i)
		}
	}
}

func TestContentHashMatchesZipTreeHash(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("two"), 0o644)
	_, zipHash, _, _, err := ZipTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ch != zipHash {
		t.Fatalf("ContentHash %s != ZipTree hash %s", ch, zipHash)
	}
}
