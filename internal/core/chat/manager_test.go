package chat

import (
	"context"
	"testing"

	"github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
)

func newTestManager(t *testing.T) (*Manager, store.Store) {
	t.Helper()
	st, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(st, nil), st
}

// say appends a user turn and persists metadata (what the agent sink does).
func say(t *testing.T, ch *store.Chat, text string) {
	t.Helper()
	ch.AppendMessage(domain.Turn{Role: "user", Content: []domain.ContentBlock{domain.TextBlock(text)}})
	if err := ch.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestStartReusesEmptyChat(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()

	first, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	ch, reused, err := m.Start(ctx, Project{ID: "pid", Slug: "api-v2", Hash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || ch.ID != first.ID {
		t.Fatalf("expected the empty active chat to be reused (reused=%v)", reused)
	}
	if ch.Mode != "project" || ch.ProjectSlug != "api-v2" || ch.ProjectID != "pid" {
		t.Fatalf("project not applied: %+v", ch)
	}

	// re-binding an empty chat back to general
	ch, reused, err = m.Start(ctx, Project{})
	if err != nil || !reused || ch.ProjectSlug != "" || ch.Mode != "chat" {
		t.Fatalf("unbind failed: reused=%v err=%v ch=%+v", reused, err, ch)
	}
}

func TestStartCreatesNewChatAndClosesOld(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	old, _ := m.Resolve()
	say(t, old, "hello there")
	old.Summary = "x" // avoid summariser path
	_ = old.Save()

	ch, reused, err := m.Start(ctx, Project{Slug: "payments"})
	if err != nil || reused {
		t.Fatalf("expected a brand-new chat, reused=%v err=%v", reused, err)
	}
	if ch.ID == old.ID {
		t.Fatal("new chat must differ from old")
	}
	if m.ActiveID() != ch.ID {
		t.Fatal("new chat must be active")
	}
	reloaded, _ := st.LoadChat(old.ID)
	if reloaded.Status != "closed" {
		t.Fatalf("old chat should be closed, got %q", reloaded.Status)
	}
}

func TestSwitchReopensClosedChatAndClosesCurrent(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	a, _ := m.Resolve()
	say(t, a, "chat A")
	a.Summary = "s"
	_ = a.Save()

	b, _, err := m.Start(ctx, Project{Slug: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	say(t, b, "chat B")
	b.Summary = "s"
	_ = b.Save()

	got, err := m.Switch(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.Status != "active" || m.ActiveID() != a.ID {
		t.Fatalf("switch did not activate A: %+v active=%s", got, m.ActiveID())
	}
	bAfter, _ := st.LoadChat(b.ID)
	if bAfter.Status != "closed" {
		t.Fatalf("previous chat should be closed, got %q", bAfter.Status)
	}
	// round trip back keeps project binding
	got, err = m.Switch(ctx, b.ID)
	if err != nil || got.ProjectSlug != "proj" || got.Mode != "project" {
		t.Fatalf("round trip lost project binding: %+v err=%v", got, err)
	}
	// resolve must return the switched-to chat, not create a new one
	cur, _ := m.Resolve()
	if cur.ID != b.ID {
		t.Fatalf("Resolve returned %s want %s", cur.ID, b.ID)
	}
	if _, err := m.Switch(ctx, "does-not-exist"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestBindAndUnbind(t *testing.T) {
	m, _ := newTestManager(t)
	ch, _ := m.Resolve()
	got, err := m.Bind(ch.ID, Project{ID: "i", Slug: "s", Hash: "h"})
	if err != nil || got.Mode != "project" || got.ProjectSlug != "s" || got.ProjectHash != "h" {
		t.Fatalf("bind: %+v %v", got, err)
	}
	got, err = m.Bind(ch.ID, Project{})
	if err != nil || got.Mode != "chat" || got.ProjectSlug != "" || got.ProjectID != "" || got.ProjectHash != "" {
		t.Fatalf("unbind: %+v %v", got, err)
	}
}

func TestRenameLocksTitle(t *testing.T) {
	m, st := newTestManager(t)
	ch, _ := m.Resolve()
	if _, err := m.Rename(ch.ID, "   "); err == nil {
		t.Fatal("empty title must be rejected")
	}
	got, err := m.Rename(ch.ID, "My  custom\ntitle")
	if err != nil || got.Title != "My  custom title" || !got.TitleLocked {
		t.Fatalf("rename: %+v %v", got, err)
	}
	reloaded, _ := st.LoadChat(ch.ID)
	if !reloaded.TitleLocked || reloaded.Title != "My  custom title" {
		t.Fatalf("not persisted: %+v", reloaded)
	}
}
