package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemechischala/awaki/internal/store"
)

// memName maps whatever the caller passes ("notes", "notes.md", "a/b/notes.md")
// to the single flat file name used on disk. Read, write and delete all go
// through it so the same name always refers to the same file.
func memName(filename string) (string, error) {
	n := strings.TrimSpace(filepath.Base(filename))
	if n == "" || n == "." || n == ".." || n == string(filepath.Separator) || strings.HasPrefix(n, ".") {
		return "", fmt.Errorf("invalid memory file name %q", filename)
	}
	if !strings.HasSuffix(strings.ToLower(n), ".md") {
		n += ".md"
	}
	return n, nil
}

func (s *LocalStore) ListMemory() ([]store.MemoryFile, error) {
	entries, err := os.ReadDir(s.memoryDir())
	if err != nil {
		return nil, err
	}
	var out []store.MemoryFile
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(s.memoryDir(), e.Name()))
		out = append(out, store.MemoryFile{Name: e.Name(), Size: info.Size(), Content: string(data)})
	}
	return out, nil
}

func (s *LocalStore) ReadMemory(filename string) (string, error) {
	safe, err := memName(filename)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(s.memoryDir(), safe))
	if os.IsNotExist(err) {
		return "", fmt.Errorf("memory file %q not found", safe)
	}
	return string(data), err
}

// WriteMemory replaces the file atomically (temp file + rename), so a crash
// mid-write can never leave a truncated memory file behind.
func (s *LocalStore) WriteMemory(filename, content string) error {
	safe, err := memName(filename)
	if err != nil {
		return err
	}
	dir := s.memoryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mem-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, safe)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s *LocalStore) DeleteMemory(filename string) error {
	safe, err := memName(filename)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.memoryDir(), safe))
}
