package workspace

import (
	"os"
	"path/filepath"
	"sync"
)

// Session holds the active working directory for agent tools.
// Normal chats use scratch/; projects use projects/{slug}/.
type Session struct {
	mu   sync.RWMutex
	root string
}

var active Session

// SetRoot updates the process-wide workspace root used by tools.
func SetRoot(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	active.mu.Lock()
	active.root = abs
	active.mu.Unlock()
	return nil
}

// Root returns the current workspace root (may be empty).
func Root() string {
	active.mu.RLock()
	defer active.mu.RUnlock()
	return active.root
}

// Resolve turns a relative path into an absolute path under the workspace root.
// Absolute paths are returned cleaned as-is (still allowed for power users).
func Resolve(path string) string {
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		return path
	}
	root := Root()
	if root == "" {
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, path)
	}
	return filepath.Join(root, path)
}
