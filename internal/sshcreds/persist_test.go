package sshcreds

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePersister is an in-memory Persister that can be told to fail.
type fakePersister struct {
	rows   map[string]DurableRow
	failOn bool
}

func newFakePersister() *fakePersister {
	return &fakePersister{rows: map[string]DurableRow{}}
}

func (f *fakePersister) ListSSHIdentities() ([]DurableRow, error) {
	if f.failOn {
		return nil, errors.New("db down")
	}
	out := make([]DurableRow, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakePersister) UpsertSSHIdentity(r DurableRow) error {
	if f.failOn {
		return errors.New("db down")
	}
	f.rows[r.ID] = r
	return nil
}

func (f *fakePersister) DeleteSSHIdentity(id string) error {
	if f.failOn {
		return errors.New("db down")
	}
	delete(f.rows, id)
	return nil
}

// TestIdentityPersistsToDurableStore proves Add() writes the private key to the
// DB-backed store — the whole point of "reload on restart like chats/skills".
func TestIdentityPersistsToDurableStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st := New(filepath.Join(dir, "data"))
	fp := newFakePersister()
	if err := st.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	if !st.Durable() {
		t.Fatal("store reports not durable after SetPersister")
	}
	if _, err := st.Add(AddInput{
		Host: "github.com-gemechisdev", HostName: "github.com", User: "git",
		PrivateKey: samplePriv, PublicKey: samplePub, GitHubUser: "gemechisdev",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fp.rows) != 1 {
		t.Fatalf("expected 1 durable row, got %d", len(fp.rows))
	}
	row := fp.rows["github.com-gemechisdev"]
	if len(row.PrivateKey) == 0 {
		t.Fatal("private key not stored in DB")
	}
	if row.GitHubUser != "gemechisdev" || row.HostName != "github.com" || row.User != "git" {
		t.Fatalf("row fields wrong: %+v", row)
	}
	// Removing must also clear the durable row.
	if err := st.Remove("github.com-gemechisdev"); err != nil {
		t.Fatal(err)
	}
	if len(fp.rows) != 0 {
		t.Fatalf("durable row not deleted, %d left", len(fp.rows))
	}
}

// TestAddSurfacesPersistFailure proves a failing durable write is reported
// instead of silently pretending the identity was saved.
func TestAddSurfacesPersistFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st := New(filepath.Join(dir, "data"))
	fp := newFakePersister()
	if err := st.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	// Now the DB goes down: the save must be reported, not silently dropped.
	fp.failOn = true
	_, err := st.Add(AddInput{Host: "h1", PrivateKey: samplePriv})
	if err == nil {
		t.Fatal("expected an error when the durable write fails")
	}
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("expected ErrNotDurable, got %v", err)
	}
}

// TestHydrateBackfillsDiskOnlyIdentities proves an identity that exists only on
// disk (created before the DB backend) is pushed into the DB on the next start,
// so it becomes durable instead of being lost.
func TestHydrateBackfillsDiskOnlyIdentities(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("HOME", t.TempDir())

	// Phase 1: no persister — identity lives only on disk.
	disk := New(dataDir)
	if _, err := disk.Add(AddInput{
		Host: "github.com-legacy", HostName: "github.com", User: "git",
		PrivateKey: samplePriv, PublicKey: samplePub, GitHubUser: "legacy",
	}); err != nil {
		t.Fatal(err)
	}

	// Phase 2: a fresh store for the same data dir, now with an empty DB.
	fp := newFakePersister()
	live := New(dataDir)
	if err := live.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	if len(fp.rows) != 1 {
		t.Fatalf("disk-only identity was not back-filled into the DB (rows=%d)", len(fp.rows))
	}
	if _, ok := fp.rows["github.com-legacy"]; !ok {
		t.Fatalf("wrong row back-filled: %+v", fp.rows)
	}
	if len(fp.rows["github.com-legacy"].PrivateKey) == 0 {
		t.Fatal("back-filled row is missing the private key")
	}
}

// TestHydrateRestoresKeysFromDB proves the restart path: a fresh, empty disk
// plus a populated DB yields working key files again.
func TestHydrateRestoresKeysFromDB(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	home := t.TempDir()
	t.Setenv("HOME", home)

	fp := newFakePersister()
	first := New(dataDir)
	if err := first.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Add(AddInput{
		Host: "github.com-restore", HostName: "github.com", User: "git",
		PrivateKey: samplePriv, PublicKey: samplePub, GitHubUser: "restore",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a container restart: wipe the on-disk ssh dir, keep the DB.
	if err := os.RemoveAll(filepath.Join(dataDir, "ssh")); err != nil {
		t.Fatal(err)
	}

	restarted := New(dataDir)
	if err := restarted.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	list, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("identity not restored from DB after simulated restart (got %d)", len(list))
	}
	priv, err := os.ReadFile(list[0].IdentityFile)
	if err != nil {
		t.Fatalf("key file not materialised: %v", err)
	}
	if string(priv) != samplePriv {
		t.Fatal("restored private key does not match")
	}
	fi, _ := os.Stat(list[0].IdentityFile)
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("restored key perms too open: %v", fi.Mode())
	}
	// Config regenerated so ssh/git can use the alias.
	cfg, err := os.ReadFile(restarted.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "Host github.com-restore") {
		t.Fatalf("config not regenerated:\n%s", cfg)
	}
}

// TestHydrateReportsDiskOnlyIdentities proves the deliberate "database wins"
// direction does not lose data SILENTLY: an identity that exists on disk but not
// in the DB is recorded so the UI can warn, rather than vanishing on restart.
func TestHydrateReportsDiskOnlyIdentities(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("HOME", t.TempDir())

	// Phase 1: create two identities with no DB at all.
	disk := New(dataDir)
	for _, host := range []string{"github.com-kept", "github.com-doomed"} {
		if _, err := disk.Add(AddInput{
			Host: host, HostName: "github.com", User: "git",
			PrivateKey: samplePriv, PublicKey: samplePub,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: a DB that knows about only ONE of them.
	fp := newFakePersister()
	fp.rows["github.com-kept"] = DurableRow{
		ID: "github.com-kept", Host: "github.com-kept", HostName: "github.com",
		User: "git", PrivateKey: []byte(samplePriv), PublicKey: samplePub,
	}

	live := New(dataDir)
	if err := live.SetPersister(fp); err != nil {
		t.Fatal(err)
	}

	// The DB wins, so only the known identity survives on disk…
	list, err := live.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "github.com-kept" {
		t.Fatalf("expected disk rebuilt from DB with 1 identity, got %+v", list)
	}
	// …but the dropped one must be reported, not silently forgotten.
	only := live.DiskOnlyIdentities()
	if len(only) != 1 {
		t.Fatalf("expected 1 disk-only identity reported, got %d (%+v)", len(only), only)
	}
	if only[0].ID != "github.com-doomed" {
		t.Fatalf("wrong identity reported as disk-only: %+v", only[0])
	}
	// The report must never carry key material.
	if strings.Contains(only[0].Host, "PRIVATE") {
		t.Fatal("disk-only report leaked key material")
	}
}

// TestDiskOnlyIsEmptyWhenConsistent guards the common case: a healthy store must
// not nag the user on every /ssh render.
func TestDiskOnlyIsEmptyWhenConsistent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("HOME", t.TempDir())

	fp := newFakePersister()
	st := New(dataDir)
	if err := st.SetPersister(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(AddInput{
		Host: "github.com-ok", HostName: "github.com", User: "git",
		PrivateKey: samplePriv, PublicKey: samplePub,
	}); err != nil {
		t.Fatal(err)
	}
	// Re-hydrate (a restart) with a consistent DB+disk pair.
	if err := st.HydrateFromDB(); err != nil {
		t.Fatal(err)
	}
	if got := st.DiskOnlyIdentities(); len(got) != 0 {
		t.Fatalf("consistent store reported disk-only identities: %+v", got)
	}
}
