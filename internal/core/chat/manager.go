package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/llm"
	"github.com/gemechischala/awaki/internal/store"

	"github.com/rs/zerolog/log"
)

const (
	modeChat    = "chat"
	modeProject = "project"
)

// ErrNotFound is returned when a chat id does not exist.
var ErrNotFound = errors.New("chat not found")

type Manager struct {
	store store.Store
	llm   llm.LLM
	bgGuard
}

func NewManager(st store.Store, client llm.LLM) *Manager {
	return &Manager{
		store:   st,
		llm:     client,
		bgGuard: bgGuard{bgRunning: make(map[string]bool)},
	}
}

func (m *Manager) SetLLM(client llm.LLM) { m.llm = client }

// Resolve returns the active chat, creating one if needed, with messages loaded.
func (m *Manager) Resolve() (*store.Chat, error) {
	ch, err := m.loadOrCreateActiveChat()
	if err != nil {
		return nil, err
	}
	if err := ch.LoadMessages(); err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	return ch, nil
}

// ActiveID returns the id of the active chat ("" if none is mapped yet).
func (m *Manager) ActiveID() string {
	id, err := m.store.GetActiveChatID()
	if err != nil {
		return ""
	}
	return id
}

// Get loads any chat by full id (messages are NOT loaded).
func (m *Manager) Get(id string) (*store.Chat, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}
	ch, err := m.store.LoadChat(id)
	if err != nil || ch == nil {
		return nil, ErrNotFound
	}
	return ch, nil
}

// Project is the binding applied to a chat. The zero value means "no project".
type Project struct {
	ID   string
	Slug string
	Hash string
}

func (p Project) bound() bool { return strings.TrimSpace(p.Slug) != "" }

func applyProject(ch *store.Chat, p Project) {
	if p.bound() {
		ch.Mode = modeProject
		ch.ProjectSlug = p.Slug
		ch.ProjectID = p.ID
		ch.ProjectHash = p.Hash
		return
	}
	ch.Mode = modeChat
	ch.ProjectSlug = ""
	ch.ProjectID = ""
	ch.ProjectHash = ""
}

// Reset force-closes the current chat and creates a new general chat
// (kept for callers that do not care about projects).
func (m *Manager) Reset(ctx context.Context) error {
	_, _, err := m.Start(ctx, Project{})
	return err
}

// Start makes a fresh chat the active one, optionally bound to a project.
//
// If the active chat is still empty it is re-used (and re-bound) instead of
// leaving an orphaned empty chat behind — the same behaviour ChatGPT/Claude
// have when you click "New chat" on an untouched conversation.
// reused reports whether that happened.
func (m *Manager) Start(ctx context.Context, p Project) (ch *store.Chat, reused bool, err error) {
	if cur, cerr := m.loadOrCreateActiveChat(); cerr == nil && cur != nil {
		if !cur.HasMessages() {
			applyProject(cur, p)
			cur.Status = "active"
			cur.EndedAt = time.Time{}
			if err := cur.Save(); err != nil {
				return nil, false, err
			}
			return cur, true, nil
		}
		if err := m.Close(ctx, cur); err != nil {
			log.Error().Err(err).Str("chat", cur.ID).Msg("chat: failed to close chat on new")
		}
	}
	nc := m.store.NewChat()
	applyProject(nc, p)
	if err := nc.Save(); err != nil {
		return nil, false, err
	}
	if err := m.store.SetActiveChatID(nc.ID); err != nil {
		return nil, false, err
	}
	return nc, false, nil
}

// Switch makes an existing chat the active one (re-opening it if it was
// closed) and returns it with messages loaded. The previously active chat is
// closed (summarised) when it has content.
func (m *Manager) Switch(ctx context.Context, id string) (*store.Chat, error) {
	target, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	curID := m.ActiveID()
	if curID != "" && curID != target.ID {
		if cur, cerr := m.store.LoadChat(curID); cerr == nil && cur != nil && cur.HasMessages() && cur.Status == "active" {
			if err := m.Close(ctx, cur); err != nil {
				log.Error().Err(err).Str("chat", cur.ID).Msg("chat: failed to close previous chat on switch")
			}
		}
	}
	if target.Status != "active" {
		target.Status = "active"
		target.EndedAt = time.Time{}
		if err := target.Save(); err != nil {
			return nil, err
		}
	}
	if curID != target.ID {
		if err := m.store.SetActiveChatID(target.ID); err != nil {
			return nil, err
		}
	}
	if err := target.LoadMessages(); err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	return target, nil
}

// Bind moves a chat into a project (or out of it when p is the zero value).
func (m *Manager) Bind(id string, p Project) (*store.Chat, error) {
	ch, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	applyProject(ch, p)
	if err := ch.Save(); err != nil {
		return nil, err
	}
	return ch, nil
}

// Rename sets a user-chosen title; the background summariser will not
// overwrite it afterwards.
func (m *Manager) Rename(id, title string) (*store.Chat, error) {
	title = strings.TrimSpace(strings.ReplaceAll(title, "\n", " "))
	if title == "" {
		return nil, errors.New("title is empty")
	}
	ch, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	ch.Title = TruncTitle(title)
	ch.TitleLocked = true
	if err := ch.Save(); err != nil {
		return nil, err
	}
	return ch, nil
}

func (m *Manager) createAndMap() (*store.Chat, error) {
	ch := m.store.NewChat()
	if err := ch.Save(); err != nil {
		return nil, err
	}
	if err := m.store.SetActiveChatID(ch.ID); err != nil {
		return nil, err
	}
	return ch, nil
}

func (m *Manager) loadOrCreateActiveChat() (*store.Chat, error) {
	chatID, err := m.store.GetActiveChatID()
	if err == nil && chatID != "" {
		ch, err := m.store.LoadChat(chatID)
		if err == nil && ch.Status == "active" {
			return ch, nil
		}
	}
	return m.createAndMap()
}
