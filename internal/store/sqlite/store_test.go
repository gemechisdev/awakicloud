package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	domain "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/store"
)

// This package is the source of truth for chats, messages, skills, memory, SSH
// identities, provider/schedule config, blob pointers and MCP servers — and it
// had no tests at all. That is a bad combination with /data not being a
// persistent volume: the only thing standing between a restart and total data
// loss is that Open + snapshot + BootstrapFromTelegram all work, and nothing
// was checking that they did.

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------------------------------------------------------------------------
// Identity, memory, timezone
// ---------------------------------------------------------------------------

func TestIdentityAndMemoryRoundTrip(t *testing.T) {
	s := openTestStore(t)

	if err := s.WriteSoul("I am Awaki."); err != nil {
		t.Fatalf("WriteSoul: %v", err)
	}
	if err := s.WriteUser("name: Gemechis\ntimezone: Africa/Addis_Ababa\n"); err != nil {
		t.Fatalf("WriteUser: %v", err)
	}
	if got := s.ReadSoul(); got != "I am Awaki." {
		t.Errorf("ReadSoul = %q", got)
	}
	if got := s.ReadUser(); !strings.Contains(got, "Gemechis") {
		t.Errorf("ReadUser = %q", got)
	}

	// Timezone is parsed out of user.md, not a separate column — so it must
	// survive the round trip through the kv blob.
	loc := s.LoadTimezone()
	if loc == nil || loc.String() != "Africa/Addis_Ababa" {
		t.Errorf("LoadTimezone = %v, want Africa/Addis_Ababa", loc)
	}

	if err := s.WriteMemory("owner.md", "# Owner"); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	files, err := s.ListMemory()
	if err != nil {
		t.Fatalf("ListMemory: %v", err)
	}
	if len(files) != 1 || files[0].Name != "owner.md" || files[0].Content != "# Owner" {
		t.Fatalf("ListMemory = %+v", files)
	}
	if got, err := s.ReadMemory("owner.md"); err != nil || got != "# Owner" {
		t.Fatalf("ReadMemory = %q, %v", got, err)
	}
	if _, err := s.ReadMemory("nope.md"); err == nil {
		t.Error("ReadMemory on a missing file should error")
	}
	if err := s.DeleteMemory("owner.md"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	if files, _ := s.ListMemory(); len(files) != 0 {
		t.Errorf("after delete, ListMemory = %+v", files)
	}
}

// safeName strips path traversal; a memory name of "../../etc/passwd" must not
// become a key that collides with, or escapes, another entry.
func TestMemoryNameIsSanitised(t *testing.T) {
	s := openTestStore(t)
	if err := s.WriteMemory("../../owner.md", "x"); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if _, err := s.ReadMemory("owner.md"); err != nil {
		t.Errorf("traversal name should normalise to owner.md: %v", err)
	}
	if err := s.WriteMemory("..", "y"); err != nil {
		t.Fatalf("WriteMemory(..): %v", err)
	}
	if _, err := s.ReadMemory(".."); err != nil {
		t.Errorf("empty normalised name should still be readable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Chats and messages
// ---------------------------------------------------------------------------

// The project binding lives on the chat row. H-8 was six call sites in
// core/projects.go discarding Save()'s error, so this pins the behaviour that a
// saved binding actually comes back after a restart.
func TestChatBindingSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ch := s.NewChat()
	ch.Title = "Fix the parser"
	ch.Mode = "project"
	ch.ProjectSlug = "awaki-server"
	ch.ProjectID = "pid-1"
	ch.Tags = []string{"go", "parser"}
	if err := s.SaveChat(ch); err != nil {
		t.Fatalf("SaveChat: %v", err)
	}
	ch.AppendMessage(store.Turn{
		Role:      "user",
		Content:   []domain.ContentBlock{{Type: "text", Text: "hello"}},
		Timestamp: time.Now(),
	})
	ch.AppendMessage(store.Turn{
		Role:      "assistant",
		Content:   []domain.ContentBlock{{Type: "text", Text: "hi"}},
		Timestamp: time.Now(),
	})
	if err := s.SetActiveChatID(ch.ID); err != nil {
		t.Fatalf("SetActiveChatID: %v", err)
	}
	_ = s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.LoadChat(ch.ID)
	if err != nil {
		t.Fatalf("LoadChat: %v", err)
	}
	if got.ProjectSlug != "awaki-server" || got.Mode != "project" || got.ProjectID != "pid-1" {
		t.Errorf("binding lost: %+v", got)
	}
	if got.Title != "Fix the parser" {
		t.Errorf("title = %q", got.Title)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v", got.Tags)
	}
	if !got.HasMessages() {
		t.Error("messages did not survive reopen")
	}
	if msgs, err := got.TextMessages(); err != nil || len(msgs) != 2 {
		t.Fatalf("TextMessages = %+v, %v", msgs, err)
	} else if msgs[0].Content != "hello" || msgs[1].Content != "hi" {
		t.Errorf("message order wrong: %+v", msgs)
	}

	byProject, err := s2.ListChatsByProject("awaki-server")
	if err != nil || len(byProject) != 1 {
		t.Fatalf("ListChatsByProject = %+v, %v", byProject, err)
	}
	if id, _ := s2.GetActiveChatID(); id != ch.ID {
		t.Errorf("active chat = %q, want %q", id, ch.ID)
	}
}

func TestReplaceMessagesRewritesHistory(t *testing.T) {
	s := openTestStore(t)
	ch := s.NewChat()
	ch.AppendMessage(store.Turn{Role: "user", Content: []domain.ContentBlock{{Type: "text", Text: "one"}}})
	ch.AppendMessage(store.Turn{Role: "assistant", Content: []domain.ContentBlock{{Type: "text", Text: "two"}}})

	if err := ch.ReplaceMessages([]store.Turn{
		{Role: "user", Content: []domain.ContentBlock{{Type: "text", Text: "rewritten"}}},
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}
	reloaded, err := s.LoadChat(ch.ID)
	if err != nil {
		t.Fatalf("LoadChat: %v", err)
	}
	msgs, err := reloaded.TextMessages()
	if err != nil {
		t.Fatalf("TextMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "rewritten" {
		t.Errorf("replace did not take: %+v", msgs)
	}
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func TestSkillRoundTripExtractsPackage(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateSkill("demo", "A demo skill", "Do the thing."); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	// A reference file, so the stored package is more than just SKILL.md.
	refDir := filepath.Join(s.SkillsDir(), "demo", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "guide.md"), []byte("details"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	if _, _, err := s.SyncSkillPackage("demo"); err != nil {
		t.Fatalf("SyncSkillPackage: %v", err)
	}

	// Wipe disk: this is what a container restart does to /data.
	if err := os.RemoveAll(s.SkillsDir()); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	skills, err := s.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("LoadSkills = %+v", skills)
	}
	if !skills[0].HasReferences {
		t.Error("references/ was not restored from the DB package")
	}
	if _, err := os.Stat(filepath.Join(skills[0].Dir, "references", "guide.md")); err != nil {
		t.Errorf("reference file missing after extract: %v", err)
	}
	if body, err := s.ReadSkill("demo"); err != nil || !strings.Contains(body, "Do the thing.") {
		t.Errorf("ReadSkill = %q, %v", body, err)
	}
}

func TestSyncSkillPackageReportsChange(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateSkill("demo", "d", "v1"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	// CreateSkill already hashed the package; syncing again must be a no-op.
	if _, changed, err := s.SyncSkillPackage("demo"); err != nil || changed {
		t.Fatalf("no-op sync reported changed=%v err=%v", changed, err)
	}
	if err := s.UpdateSkill("demo", "v2"); err != nil {
		t.Fatalf("UpdateSkill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "demo", "extra.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	hash, changed, err := s.SyncSkillPackage("demo")
	if err != nil || !changed || hash == "" {
		t.Fatalf("changed sync = %q, %v, %v", hash, changed, err)
	}
	if _, err := s.ReadSkill("missing"); err == nil {
		t.Error("ReadSkill on a missing skill should error")
	}
}

// repairSkillsTable exists because a malformed row previously took down /skills
// and skill loading entirely. Drop the bad rows, keep the good ones.
func TestRepairSkillsTableDropsMalformedRows(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateSkill("good", "fine", "body"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	// SQLite permits NULL in a TEXT PRIMARY KEY (a legacy quirk) and '' passes
	// the NOT NULL constraint, so both of these are insertable corruption.
	if _, err := s.db.Exec(`INSERT INTO skills(name,description,body,package_hash,updated_at) VALUES(NULL,'d','b','','t')`); err != nil {
		t.Fatalf("insert null-name row: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO skills(name,description,body,package_hash,updated_at) VALUES('nostamp','d','b','','')`); err != nil {
		t.Fatalf("insert empty-updated_at row: %v", err)
	}

	dropped, err := s.repairSkillsTable()
	if err != nil {
		t.Fatalf("repairSkillsTable: %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped = %v, want 2 entries", dropped)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM skills`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("skills rows after repair = %d, want 1", n)
	}
	if _, err := s.ReadSkill("good"); err != nil {
		t.Errorf("the healthy skill was lost: %v", err)
	}

	// A clean table must be left completely alone.
	dropped, err = s.repairSkillsTable()
	if err != nil || len(dropped) != 0 {
		t.Errorf("clean repair = %v, %v", dropped, err)
	}
}

// ---------------------------------------------------------------------------
// Config, blobs, SSH identities
// ---------------------------------------------------------------------------

func TestConfigJSONRoundTrip(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetConfigJSON("providers"); err == nil {
		t.Error("expected an error for a missing config")
	}
	if err := s.SetConfigJSON("providers", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("SetConfigJSON: %v", err)
	}
	got, err := s.GetConfigJSON("providers")
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("GetConfigJSON = %q, %v", got, err)
	}
}

func TestBlobUpsertAndListByKind(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.GetBlob("project", "awaki-server"); ok {
		t.Error("expected no blob yet")
	}
	ref := BlobRef{Kind: "project", Key: "awaki-server", FileID: "f1", MessageID: 7,
		Hash: "abc", Size: 123, MetaJSON: `{"v":1}`}
	if err := s.UpsertBlob(ref); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	got, ok := s.GetBlob("project", "awaki-server")
	if !ok || got.FileID != "f1" || got.Hash != "abc" || got.Size != 123 {
		t.Fatalf("GetBlob = %+v, %v", got, ok)
	}
	// Upsert must replace, not duplicate.
	ref.FileID = "f2"
	ref.Size = 456
	if err := s.UpsertBlob(ref); err != nil {
		t.Fatalf("UpsertBlob 2: %v", err)
	}
	if got, _ := s.GetBlob("project", "awaki-server"); got.FileID != "f2" || got.Size != 456 {
		t.Errorf("upsert did not replace: %+v", got)
	}
	if err := s.UpsertBlob(BlobRef{Kind: "db", Key: "meta", FileID: "f3"}); err != nil {
		t.Fatalf("UpsertBlob 3: %v", err)
	}
	if list := s.ListBlobs("project"); len(list) != 1 {
		t.Errorf("ListBlobs(project) = %+v", list)
	}
	if list := s.ListBlobs("db"); len(list) != 1 {
		t.Errorf("ListBlobs(db) = %+v", list)
	}
}

func TestSSHIdentityDeleteMatchesIDOrHost(t *testing.T) {
	s := openTestStore(t)
	row := SSHIdentityRow{
		ID: "github.com-gemechisdev", Host: "github.com-gemechisdev", HostName: "github.com",
		User: "git", PrivateKey: []byte("PRIVATE KEY MATERIAL"),
		PublicKey: "ssh-ed25519 AAAA", GitName: "Gemechis", GitEmail: "g@example.com",
		GitHubUser: "gemechisdev",
	}
	if err := s.UpsertSSHIdentity(row); err != nil {
		t.Fatalf("UpsertSSHIdentity: %v", err)
	}
	list, err := s.ListSSHIdentities()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSSHIdentities = %+v, %v", list, err)
	}
	first := list[0].CreatedAt
	if first == "" {
		t.Fatal("CreatedAt should default to now when empty")
	}
	// created_at is deliberately absent from the ON CONFLICT update list, so an
	// upsert refreshes the key material without rewriting when the identity was
	// first added. Pin that: a silent reset would make "added when" unreliable.
	row.CreatedAt = "2020-01-01T00:00:00Z"
	row.PrivateKey = []byte("ROTATED KEY MATERIAL")
	if err := s.UpsertSSHIdentity(row); err != nil {
		t.Fatalf("UpsertSSHIdentity 2: %v", err)
	}
	list, _ = s.ListSSHIdentities()
	if list[0].CreatedAt != first {
		t.Errorf("CreatedAt was rewritten by an upsert: %q -> %q", first, list[0].CreatedAt)
	}
	if string(list[0].PrivateKey) != "ROTATED KEY MATERIAL" {
		t.Errorf("key rotation did not persist: %q", list[0].PrivateKey)
	}
	// A fresh identity does honour an explicit CreatedAt.
	if err := s.UpsertSSHIdentity(SSHIdentityRow{
		ID: "other", Host: "other", PrivateKey: []byte("k"), CreatedAt: "2020-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range mustList(t, s) {
		if r.ID == "other" && r.CreatedAt != "2020-01-01T00:00:00Z" {
			t.Errorf("explicit CreatedAt ignored on insert: %q", r.CreatedAt)
		}
	}
	// Delete accepts either the id or the host, and must not take anything else
	// with it.
	if err := s.DeleteSSHIdentity("github.com-gemechisdev"); err != nil {
		t.Fatalf("DeleteSSHIdentity: %v", err)
	}
	remaining := mustList(t, s)
	for _, r := range remaining {
		if r.ID == "github.com-gemechisdev" {
			t.Errorf("identity not deleted: %+v", remaining)
		}
	}
	if len(remaining) != 1 || remaining[0].ID != "other" {
		t.Errorf("delete removed more than the target: %+v", remaining)
	}
	// The host form must work too.
	if err := s.UpsertSSHIdentity(SSHIdentityRow{ID: "by-host", Host: "github.com-byhost", PrivateKey: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSSHIdentity("github.com-byhost"); err != nil {
		t.Fatalf("DeleteSSHIdentity by host: %v", err)
	}
	for _, r := range mustList(t, s) {
		if r.Host == "github.com-byhost" {
			t.Error("delete by host did not remove the identity")
		}
	}
}

// ---------------------------------------------------------------------------
// Snapshot -> cloud -> restore: the whole point of the package
// ---------------------------------------------------------------------------

// seedStore fills a store with one of everything that must survive a restart.
func seedStore(t *testing.T, s *Store) string {
	t.Helper()
	if err := s.WriteSoul("soul-v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteUser("name: Gemechis\ntimezone: Africa/Addis_Ababa\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMemory("owner.md", "memory-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSkill("demo", "desc", "body-v1"); err != nil {
		t.Fatal(err)
	}
	ch := s.NewChat()
	ch.Title = "Persisted chat"
	ch.Mode = "project"
	ch.ProjectSlug = "awaki-server"
	if err := s.SaveChat(ch); err != nil {
		t.Fatal(err)
	}
	ch.AppendMessage(store.Turn{Role: "user", Content: []domain.ContentBlock{{Type: "text", Text: "remember me"}}})
	if err := s.SetActiveChatID(ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigJSON("providers", []byte(`{"openrouter":{"key":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSSHIdentity(SSHIdentityRow{
		ID: "github.com", Host: "github.com", HostName: "github.com", User: "git",
		PrivateKey: []byte("KEY"), PublicKey: "ssh-rsa AAAA", CreatedAt: "2024-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBlob(BlobRef{Kind: "project", Key: "awaki-server", FileID: "blob-1", Hash: "h", Size: 9}); err != nil {
		t.Fatal(err)
	}
	return ch.ID
}

func TestSnapshotUploadsDbThenPinsManifest(t *testing.T) {
	s := openTestStore(t)
	chatID := seedStore(t, s)

	api := newFakeTG()
	syncer := NewSyncer(s, api, 12345, time.Hour)
	syncer.flush(true)

	if api.docCount() != 2 {
		t.Fatalf("expected db + manifest uploads, got %v", api.callLog())
	}
	log := strings.Join(api.callLog(), " | ")
	if !strings.Contains(log, "send awaki-v1.db") || !strings.Contains(log, "send awaki-manifest.json") {
		t.Fatalf("unexpected call order: %s", log)
	}

	// The manifest is what recovery reads, so it must be the pinned document
	// and must point at the db that was just written.
	manBytes := api.doc("awaki-manifest.json")
	if manBytes == nil {
		t.Fatal("manifest document not found")
	}
	var man Manifest
	if err := json.Unmarshal(manBytes, &man); err != nil {
		t.Fatalf("manifest is not valid json: %v", err)
	}
	if man.LatestVer != 1 || man.LatestFileID == "" || len(man.History) != 1 {
		t.Fatalf("manifest = %+v", man)
	}
	if man.LatestHash != man.History[0].Hash {
		t.Errorf("hash mismatch between manifest head and history: %+v", man)
	}
	caption, fileID, _, err := api.GetChatPinnedFile(context.Background(), 12345)
	if err != nil {
		t.Fatalf("GetChatPinnedFile: %v", err)
	}
	if fileID != api.byName["awaki-manifest.json"] {
		t.Errorf("pinned file is %q (caption %q), want the manifest", fileID, caption)
	}
	if !strings.HasPrefix(caption, "awaki:dbmanifest") {
		t.Errorf("manifest caption = %q", caption)
	}
	// The db snapshot must actually be a SQLite file, not a truncated buffer.
	if !looksLikeSQLite(api.doc("awaki-v1.db")) {
		t.Error("uploaded db snapshot is not a SQLite file")
	}
	// And it must contain the seed, not just a valid header.
	if man.History[0].Size < 5000 {
		t.Errorf("snapshot is suspiciously small: %d bytes", man.History[0].Size)
	}
	if chatID == "" {
		t.Error("seed produced no chat id")
	}
}

// The critical end-to-end property: everything seeded must come back after a
// restore into a brand new file, exactly as it would after a restart.
func TestSnapshotRestoresEverythingIntoAFreshDatabase(t *testing.T) {
	s := openTestStore(t)
	chatID := seedStore(t, s)

	api := newFakeTG()
	NewSyncer(s, api, 12345, time.Hour).flush(true)

	// Fresh location, nothing on disk — a cold container.
	restoreDir := t.TempDir()
	dbPath := filepath.Join(restoreDir, "awaki.db")
	ok, err := BootstrapFromTelegram(context.Background(), api, 12345, dbPath)
	if err != nil || !ok {
		t.Fatalf("BootstrapFromTelegram = %v, %v", ok, err)
	}

	s2, err := Open(restoreDir)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	defer s2.Close()

	if got := s2.ReadSoul(); got != "soul-v1" {
		t.Errorf("soul = %q", got)
	}
	if got := s2.LoadTimezone(); got == nil || got.String() != "Africa/Addis_Ababa" {
		t.Errorf("timezone = %v", got)
	}
	if got, err := s2.ReadMemory("owner.md"); err != nil || got != "memory-v1" {
		t.Errorf("memory = %q, %v", got, err)
	}
	if skills, err := s2.LoadSkills(); err != nil || len(skills) != 1 || skills[0].Name != "demo" {
		t.Errorf("skills = %+v, %v", skills, err)
	}
	ch, err := s2.LoadChat(chatID)
	if err != nil {
		t.Fatalf("chat did not survive: %v", err)
	}
	if ch.ProjectSlug != "awaki-server" || ch.Title != "Persisted chat" {
		t.Errorf("chat binding lost: %+v", ch)
	}
	if msgs, _ := ch.TextMessages(); len(msgs) != 1 || msgs[0].Content != "remember me" {
		t.Errorf("messages = %+v", msgs)
	}
	if cfg, err := s2.GetConfigJSON("providers"); err != nil || !strings.Contains(string(cfg), "openrouter") {
		t.Errorf("providers = %q, %v", cfg, err)
	}
	if idents, err := s2.ListSSHIdentities(); err != nil || len(idents) != 1 || string(idents[0].PrivateKey) != "KEY" {
		t.Errorf("ssh identities = %+v, %v", idents, err)
	}
	if blob, ok := s2.GetBlob("project", "awaki-server"); !ok || blob.FileID != "blob-1" {
		t.Errorf("blob = %+v, %v", blob, ok)
	}
	if id, _ := s2.GetActiveChatID(); id != chatID {
		t.Errorf("active chat = %q, want %q", id, chatID)
	}
}

// A second flush with no changes must not burn a Telegram upload. MarkDirty is
// what gates this, so the test drives the store, not the syncer internals.
func TestFlushSkipsUnchangedSnapshot(t *testing.T) {
	s := openTestStore(t)
	seedStore(t, s)
	api := newFakeTG()
	syncer := NewSyncer(s, api, 1, time.Hour)

	syncer.flush(true)
	first := api.docCount()
	if first != 2 {
		t.Fatalf("first flush uploaded %d documents", first)
	}
	// Not dirty and not forced: nothing at all.
	syncer.flush(false)
	if api.docCount() != first {
		t.Errorf("unforced flush with no changes uploaded anyway")
	}
	// Forced, but the content hash is identical: the hash check must catch it.
	syncer.flush(true)
	if api.docCount() != first {
		t.Errorf("forced flush with identical content re-uploaded (%s)", strings.Join(api.callLog(), " | "))
	}

	// A real change must produce a new version.
	if err := s.WriteSoul("soul-v2"); err != nil {
		t.Fatal(err)
	}
	syncer.flush(false) // MarkDirty was fired by WriteSoul
	if api.docCount() != first+2 {
		t.Fatalf("changed snapshot did not upload: %s", strings.Join(api.callLog(), " | "))
	}
	man := api.doc("awaki-manifest.json")
	var m Manifest
	if err := json.Unmarshal(man, &m); err != nil {
		t.Fatal(err)
	}
	if m.LatestVer != 2 {
		t.Errorf("version = %d, want 2", m.LatestVer)
	}
	if len(m.History) != 2 {
		t.Errorf("history = %+v", m.History)
	}
}

// A failed upload must leave the store dirty so the next tick retries, rather
// than quietly believing it is in sync.
func TestFailedUploadKeepsStoreDirty(t *testing.T) {
	s := openTestStore(t)
	seedStore(t, s)
	api := newFakeTG()
	api.failUpload = "awaki-v1.db"
	syncer := NewSyncer(s, api, 1, time.Hour)

	syncer.flush(true)
	if api.docCount() != 0 {
		t.Fatalf("upload should have failed, log: %v", api.callLog())
	}
	if !syncer.dirty {
		t.Fatal("failed upload left the store marked clean; the change would be lost")
	}

	// Recover and retry.
	api.failUpload = ""
	syncer.flush(false)
	if api.docCount() != 2 {
		t.Errorf("retry did not upload: %v", api.callLog())
	}
}

// A failed manifest upload still leaves a usable db pinned, so recovery must
// work from the pinned db alone.
func TestBootstrapFallsBackToPinnedDatabaseWhenManifestFails(t *testing.T) {
	s := openTestStore(t)
	chatID := seedStore(t, s)
	api := newFakeTG()
	api.failUpload = "awaki-manifest.json"
	NewSyncer(s, api, 1, time.Hour).flush(true)

	if api.doc("awaki-manifest.json") != nil {
		t.Fatal("manifest should not have been uploaded")
	}
	caption, _, _, err := api.GetChatPinnedFile(context.Background(), 1)
	if err != nil {
		t.Fatalf("nothing pinned: %v", err)
	}
	if !strings.HasPrefix(caption, "awaki:db") {
		t.Fatalf("pinned caption = %q, want the db snapshot", caption)
	}

	dir := t.TempDir()
	if ok, err := BootstrapFromTelegram(context.Background(), api, 1, filepath.Join(dir, "awaki.db")); err != nil || !ok {
		t.Fatalf("bootstrap from pinned db = %v, %v", ok, err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()
	if got := s2.ReadSoul(); got != "soul-v1" {
		t.Errorf("soul = %q after fallback restore", got)
	}
	if _, err := s2.LoadChat(chatID); err != nil {
		t.Errorf("chat did not survive fallback restore: %v", err)
	}
}

// Bootstrap must refuse anything that is not a database rather than overwriting
// a good local file with junk.
func TestBootstrapRejectsNonSQLitePayload(t *testing.T) {
	s := openTestStore(t)
	seedStore(t, s)
	api := newFakeTG()
	NewSyncer(s, api, 1, time.Hour).flush(true)

	// Point the manifest at a document that is not a database.
	api.mu.Lock()
	api.docs[api.byName["awaki-manifest.json"]] = []byte(`{"latest_file_id":"garbage"}`)
	api.docs["garbage"] = []byte("this is not a sqlite file at all")
	api.mu.Unlock()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "awaki.db")
	if err := os.WriteFile(dbPath, []byte("PRECIOUS LOCAL DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := BootstrapFromTelegram(context.Background(), api, 1, dbPath)
	if err == nil || ok {
		t.Fatalf("bootstrap accepted a non-database: ok=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(dbPath); string(b) != "PRECIOUS LOCAL DATA" {
		t.Errorf("a failed bootstrap clobbered the existing database: %q", b)
	}
}

func TestBootstrapReportsWhenNothingIsPinned(t *testing.T) {
	api := newFakeTG()
	ok, err := BootstrapFromTelegram(context.Background(), api, 1, filepath.Join(t.TempDir(), "awaki.db"))
	if err == nil || ok {
		t.Fatalf("expected a clear error with nothing pinned, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("error should mention pinning: %v", err)
	}
}

// A truncated or partial download must not be mistaken for a database.
func TestBootstrapRejectsTruncatedDownload(t *testing.T) {
	api := newFakeTG()
	ctx := context.Background()
	if _, _, err := api.SendDocument(ctx, 1, "awaki-v1.db", []byte("SQLite format 3\x00rest"), "awaki:db;ver=1"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	fid := api.byName["awaki-v1.db"]
	api.pinned, api.pinnedAt = fid, 1
	api.caps[fid] = "awaki:db;ver=1"
	api.docs[fid] = []byte("SQLite") // truncated mid-header
	api.mu.Unlock()

	if ok, err := BootstrapFromTelegram(ctx, api, 1, filepath.Join(t.TempDir(), "awaki.db")); err == nil || ok {
		t.Fatalf("truncated payload accepted: ok=%v err=%v", ok, err)
	}
}

func TestLocalDBUsable(t *testing.T) {
	dir := t.TempDir()

	if LocalDBUsable(filepath.Join(dir, "missing.db")) {
		t.Error("a missing file is not usable")
	}
	tiny := filepath.Join(dir, "tiny.db")
	if err := os.WriteFile(tiny, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if LocalDBUsable(tiny) {
		t.Error("a file under 100 bytes is not usable")
	}
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if LocalDBUsable(junk) {
		t.Error("a non-sqlite file is not usable")
	}

	// A real database must be accepted, and a valid db whose WAL holds the data
	// is exactly the "restarted mid-flight" case worth pinning.
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSoul("x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !LocalDBUsable(filepath.Join(dir, "awaki.db")) {
		t.Error("a real database was rejected")
	}
}

// ---------------------------------------------------------------------------
// Legacy filesystem migration
// ---------------------------------------------------------------------------

// MigrateFromDir is the one-shot import of pre-SQLite data. It is only ever
// exercised on a real upgrade, which is the worst place to find a bug.
func TestMigrateFromDirImportsLegacyData(t *testing.T) {
	dataDir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("soul.md", "legacy soul")
	write("user.md", "name: Gemechis\ntimezone: Africa/Addis_Ababa\n")
	write("active_episode.txt", "chat-legacy\n")
	write("memory/owner.md", "legacy memory")
	write("memory/notes/todo.md", "ignored: not a top-level .md") // subdir must be skipped
	write("skills/demo/SKILL.md", "---\nname: demo\n---\nlegacy skill body")
	write("providers.json", `{"legacy":true}`)
	write("schedules.json", `[{"id":"legacy"}]`)
	write("episodes/chat-legacy.json", `{"id":"chat-legacy","title":"Legacy chat","mode":"project","project_slug":"old-proj"}`)
	write("episodes/chat-legacy.messages.jsonl",
		`{"role":"user","content":[{"type":"text","text":"first"}]}`+"\n"+
			`{"role":"assistant","content":[{"type":"text","text":"second"}]}`+"\n")
	write("ssh/identities.json", `[{"id":"github.com","host":"github.com","host_name":"github.com","user":"git","public_key":"ssh-ed25519 AAA","git_name":"G","git_email":"g@e.com","github_user":"gemechisdev","created_at":"2023-05-05T00:00:00Z"}]`)
	write("ssh/keys/github.com", "LEGACY PRIVATE KEY")

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.MigrateFromDir(dataDir)

	if got := s.ReadSoul(); got != "legacy soul" {
		t.Errorf("soul = %q", got)
	}
	if got := s.ReadUser(); !strings.Contains(got, "Gemechis") {
		t.Errorf("user = %q", got)
	}
	if got, err := s.ReadMemory("owner.md"); err != nil || got != "legacy memory" {
		t.Errorf("memory = %q, %v", got, err)
	}
	if files, _ := s.ListMemory(); len(files) != 1 {
		t.Errorf("nested memory should be skipped, got %+v", files)
	}
	if body, err := s.ReadSkill("demo"); err != nil || !strings.Contains(body, "legacy skill body") {
		t.Errorf("skill = %q, %v", body, err)
	}
	if cfg, err := s.GetConfigJSON("providers"); err != nil || string(cfg) != `{"legacy":true}` {
		t.Errorf("providers = %q, %v", cfg, err)
	}
	if cfg, err := s.GetConfigJSON("schedules"); err != nil || string(cfg) != `[{"id":"legacy"}]` {
		t.Errorf("schedules = %q, %v", cfg, err)
	}
	if id, err := s.GetActiveChatID(); err != nil || id != "chat-legacy" {
		t.Errorf("active chat = %q, %v", id, err)
	}
	ch, err := s.LoadChat("chat-legacy")
	if err != nil {
		t.Fatalf("legacy chat not imported: %v", err)
	}
	if ch.ProjectSlug != "old-proj" {
		t.Errorf("legacy chat binding = %+v", ch)
	}
	if msgs, _ := ch.TextMessages(); len(msgs) != 2 || msgs[0].Content != "first" {
		t.Errorf("legacy messages = %+v", msgs)
	}
	idents, err := s.ListSSHIdentities()
	if err != nil || len(idents) != 1 {
		t.Fatalf("legacy ssh identities = %+v, %v", idents, err)
	}
	if string(idents[0].PrivateKey) != "LEGACY PRIVATE KEY" {
		t.Errorf("private key not imported: %q", idents[0].PrivateKey)
	}
	if idents[0].GitHubUser != "gemechisdev" || idents[0].CreatedAt != "2023-05-05T00:00:00Z" {
		t.Errorf("identity metadata = %+v", idents[0])
	}

	// An identity whose key file is gone must be skipped rather than imported
	// as an unusable half-row.
	if err := os.Remove(filepath.Join(dataDir, "ssh", "keys", "github.com")); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.MigrateFromDir(dataDir)
	if idents, _ := s2.ListSSHIdentities(); len(idents) != 0 {
		t.Errorf("identity without key material was imported: %+v", idents)
	}
}

// Migration is guarded so it can never run over a populated database and
// duplicate or overwrite live data.
func TestMigrateFromDirIsSkippedWhenDatabaseIsNotEmpty(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "soul.md"), []byte("legacy soul"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := openTestStore(t)
	if err := s.WriteSoul("live soul"); err != nil {
		t.Fatal(err)
	}
	s.MigrateFromDir(dataDir)
	if got := s.ReadSoul(); got != "live soul" {
		t.Errorf("migration overwrote live data: %q", got)
	}
}

// A memory-only database is also "not empty"; the kv table alone must not be
// the only guard.
func TestMigrateFromDirIsSkippedWhenOnlyMemoryExists(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "soul.md"), []byte("legacy soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t)
	if err := s.WriteMemory("owner.md", "live memory"); err != nil {
		t.Fatal(err)
	}
	s.MigrateFromDir(dataDir)
	if got := s.ReadSoul(); got != "" {
		t.Errorf("migration ran with memory present: soul = %q", got)
	}
	if got, _ := s.ReadMemory("owner.md"); got != "live memory" {
		t.Errorf("memory was overwritten: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Misc invariants
// ---------------------------------------------------------------------------

// Every mutation must fire OnChange, otherwise the cloud syncer never learns
// the database moved and the change is lost on restart.
func TestMutationsFireOnChange(t *testing.T) {
	s := openTestStore(t)
	var n int
	s.SetOnChange(func() { n++ })

	ops := []struct {
		name string
		do   func() error
	}{
		{"WriteSoul", func() error { return s.WriteSoul("x") }},
		{"WriteUser", func() error { return s.WriteUser("y") }},
		{"WriteMemory", func() error { return s.WriteMemory("a.md", "z") }},
		{"DeleteMemory", func() error { return s.DeleteMemory("a.md") }},
		{"SetConfigJSON", func() error { return s.SetConfigJSON("providers", []byte(`{}`)) }},
		{"SetActiveChatID", func() error { return s.SetActiveChatID("c1") }},
		{"UpsertBlob", func() error { return s.UpsertBlob(BlobRef{Kind: "db", Key: "k"}) }},
		{"CreateSkill", func() error { _, err := s.CreateSkill("s", "d", "b"); return err }},
		{"DeleteSkill", func() error { return s.DeleteSkill("s") }},
		{"UpsertSSHIdentity", func() error { return s.UpsertSSHIdentity(SSHIdentityRow{ID: "h", Host: "h", PrivateKey: []byte("k")}) }},
		{"DeleteSSHIdentity", func() error { return s.DeleteSSHIdentity("h") }},
	}
	for _, op := range ops {
		before := n
		if err := op.do(); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		if n == before {
			t.Errorf("%s did not fire OnChange; its change would never reach the cloud", op.name)
		}
	}

	// SaveChat and AppendMessage too.
	ch := s.NewChat()
	before := n
	if err := s.SaveChat(ch); err != nil {
		t.Fatal(err)
	}
	if n == before {
		t.Error("SaveChat did not fire OnChange")
	}
	before = n
	ch.AppendMessage(store.Turn{Role: "user", Content: []domain.ContentBlock{{Type: "text", Text: "x"}}})
	if n == before {
		t.Error("AppendMessage did not fire OnChange")
	}
}

func TestOpenIsIdempotentAndSchemaUpgrades(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Path() != filepath.Join(dir, "awaki.db") {
		t.Errorf("Path = %q", s.Path())
	}
	if s.DB() == nil {
		t.Fatal("DB() is nil")
	}
	if st, err := os.Stat(s.SkillsDir()); err != nil || !st.IsDir() {
		t.Errorf("SkillsDir was not created: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopening an existing database must not fail on the ALTER TABLE upgrade
	// statements, which error once the columns exist.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if err := s2.WriteSoul("still works"); err != nil {
		t.Fatalf("WriteSoul after reopen: %v", err)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"owner.md":            "owner.md",
		"../../etc/passwd":    "passwd",
		"/abs/path/x.md":      "x.md",
		"":                    "unnamed",
		".":                   "unnamed",
		"..":                  "unnamed",
		"a/../b.md":           "b.md",
		"weird..name.md":      "weirdname.md",
		"  spaced.md  ":       "spaced.md",
		"nested/dir/file.md":  "file.md",
		"already/../../x.txt": "x.txt",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikeSQLiteAndTrimCap(t *testing.T) {
	if !looksLikeSQLite([]byte("SQLite format 3\x00")) {
		t.Error("valid header rejected")
	}
	for _, bad := range [][]byte{nil, []byte("SQLite"), []byte("SQLite format 3"), []byte("not a db at all")} {
		if looksLikeSQLite(bad) {
			t.Errorf("accepted non-sqlite header %q", bad)
		}
	}
	if got := trimCap("  short  "); got != "short" {
		t.Errorf("trimCap = %q", got)
	}
	// Truncation is by rune, so the result is 80 characters plus the ellipsis —
	// 83 bytes in UTF-8. Assert on the rune count, not the byte count.
	long := strings.Repeat("x", 200)
	got := trimCap(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("trimCap did not add an ellipsis: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 81 {
		t.Errorf("trimCap produced %d runes, want 81 (80 + ellipsis)", n)
	}
	// Exactly at the boundary it must be left alone.
	if got := trimCap(strings.Repeat("y", 80)); strings.HasSuffix(got, "…") {
		t.Errorf("80 characters should not be truncated: %q", got)
	}
}

func mustList(t *testing.T, s *Store) []SSHIdentityRow {
	t.Helper()
	rows, err := s.ListSSHIdentities()
	if err != nil {
		t.Fatalf("ListSSHIdentities: %v", err)
	}
	return rows
}

func TestListChatsOrdersNewestFirst(t *testing.T) {
	s := openTestStore(t)
	older := s.NewChat()
	older.Title = "older"
	if err := s.SaveChat(older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // updated_at has sub-second resolution, but keep it honest
	newer := s.NewChat()
	newer.Title = "newer"
	if err := s.SaveChat(newer); err != nil {
		t.Fatal(err)
	}
	chats, err := s.ListChats()
	if err != nil || len(chats) != 2 {
		t.Fatalf("ListChats = %+v, %v", chats, err)
	}
	if chats[0].Title != "newer" {
		t.Errorf("ListChats order = %q, %q", chats[0].Title, chats[1].Title)
	}
	if _, err := s.LoadChat("does-not-exist"); err == nil {
		t.Error("LoadChat on a missing chat should error")
	}
}
