// Package filemgr is the model behind the Telegram file browser (/files).
//
// It is deliberately free of Telegram / engine dependencies: it owns the
// short-id → path index (callback_data must stay ≤ 64 bytes, so buttons carry
// an integer id, never a path), the workspace jail, directory listing, paged
// file reading, and the screens (markdown text + button rows) the UI renders.
// internal/core/ui_files.go is a thin adapter that delivers those screens and
// performs the side effects (agent hand-off, permission checks).
package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Limits (see FILE_MANAGER plan §8).
const (
	CallbackPrefix = "files:"

	PageSize     = 24   // entries per browse page (2-column grid)
	MaxList      = 500  // hard cap on entries listed from one directory
	PreviewRunes = 4500 // visible runes of file content per screen
	MaxLineRunes = 1000 // a single line is clipped past this
	MaxNodes     = 2000 // path-index size before it is reset
	maxCopyText  = 256  // Telegram copy_text limit

	GridCols       = 2         // buttons per row in the folder grid
	GridLabelRunes = 15        // label width that fits a 2-column row on phones
	MaxSendBytes   = 50 << 20  // Telegram bot upload limit
	MaxCreateBytes = 256 << 10 // initial content accepted when creating a file
	MaxNameDepth   = 8
)

var (
	// ErrExpired means the button's id is no longer in the index (the index
	// was reset, the workspace changed, or the bot restarted).
	ErrExpired = errors.New("this button has expired — reopen /files")
	// ErrOutside means a path resolves outside the workspace root.
	ErrOutside = errors.New("path is outside the workspace")
	// ErrNotFound means the path no longer exists.
	ErrNotFound = errors.New("no longer exists")
	// ErrNoRoot means no workspace root is configured.
	ErrNoRoot = errors.New("no workspace is active yet — start a chat first")
)

// Manager holds the id index for one workspace root. Safe for concurrent use.
//
// Ids are monotonic and never reused, even after the index is reset: a stale
// button can only ever hit "expired", never a different file. Id 0 is always
// the current root.
type Manager struct {
	mu    sync.Mutex
	root  string
	nodes map[int]string
	ids   map[string]int
	next  int

	// undo history for edits (upload / replace / patch); see undo.go.
	undo    []undoGroup
	undoSeq int
}

// New returns an empty manager. Call SetRoot before use.
func New() *Manager {
	return &Manager{nodes: map[int]string{}, ids: map[string]int{}, next: 1}
}

// SetRoot points the manager at a workspace root. Changing the root drops the
// whole index; re-setting the same root keeps it (so open screens stay live).
func (m *Manager) SetRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return ErrNoRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	m.mu.Lock()
	defer m.mu.Unlock()
	if abs == m.root && m.nodes[0] == abs {
		return nil
	}
	m.root = abs
	m.resetLocked()
	return nil
}

// Root returns the current root ("" if unset).
func (m *Manager) Root() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root
}

func (m *Manager) resetLocked() {
	m.nodes = map[int]string{}
	m.ids = map[string]int{}
	if m.root != "" {
		m.nodes[0] = m.root
		m.ids[m.root] = 0
	}
}

// Len reports how many paths are indexed (for tests / diagnostics).
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.nodes)
}

// registerLocked returns the stable id for abs, allocating one if needed.
func (m *Manager) registerLocked(abs string) int {
	abs = filepath.Clean(abs)
	if id, ok := m.ids[abs]; ok {
		return id
	}
	id := m.next
	m.next++
	m.nodes[id] = abs
	m.ids[abs] = id
	return id
}

// touch registers abs for a new screen. When the index has grown past
// MaxNodes it is reset first — and because ids are only ever handed out while
// building a screen, a reset here can never invalidate ids of that same screen.
func (m *Manager) touch(abs string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.nodes) > MaxNodes {
		m.resetLocked()
	}
	return m.registerLocked(abs)
}

// register is touch without the reset check, for the children of a screen
// that already called touch.
func (m *Manager) register(abs string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registerLocked(abs)
}

// Resolve maps an id to an absolute path and re-checks the jail (symlink
// aware) at call time, so a link swapped after listing cannot escape.
func (m *Manager) Resolve(id int) (string, error) {
	m.mu.Lock()
	abs, ok := m.nodes[id]
	root := m.root
	m.mu.Unlock()
	if root == "" {
		return "", ErrNoRoot
	}
	if !ok {
		return "", ErrExpired
	}
	if err := checkInside(root, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// Within reports whether p is root or lexically inside root.
func Within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkInside enforces the workspace jail: lexically, then after resolving
// symlinks on both sides.
func checkInside(root, abs string) error {
	if !Within(root, abs) {
		return ErrOutside
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("resolve path: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	if !Within(realRoot, real) {
		return ErrOutside
	}
	return nil
}

// Rel returns abs relative to the root with forward slashes ("." for the root).
func (m *Manager) Rel(abs string) string {
	root := m.Root()
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// Display is the header path: "<root-name>" or "<root-name>/<rel>".
func (m *Manager) Display(abs string) string {
	root := m.Root()
	name := filepath.Base(root)
	rel := m.Rel(abs)
	if rel == "." {
		return name
	}
	return name + "/" + rel
}
