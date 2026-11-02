package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/workspace"
)

// FileOpTools are the structural file operations (move / copy / delete / mkdir)
// that previously required bash. They are mutating tools: they are listed in
// permission.PlanDenylist and require approval in ask mode.
func FileOpTools() []types.Tool {
	fromTo := func(input map[string]any) string {
		return filepath.Base(types.GetStringOpt(input, "from")) + " → " + filepath.Base(types.GetStringOpt(input, "to"))
	}
	return []types.Tool{
		{
			Name:  "move_file",
			Label: "Moving",
			Description: "Move or rename a file or directory (like mv: if `to` is an existing directory the source is moved into it). " +
				"Creates parent directories. Refuses to overwrite unless overwrite=true.",
			DetailFunc: fromTo,
			Parameters: map[string]any{
				"from":      map[string]any{"type": "string", "description": "Source path"},
				"to":        map[string]any{"type": "string", "description": "Destination path"},
				"overwrite": map[string]any{"type": "boolean", "description": "Replace an existing destination file (default false)"},
			},
			Required: []string{"from", "to"},
			Handler:  moveFile,
		},
		{
			Name:  "copy_file",
			Label: "Copying",
			Description: "Copy a file, or a directory with recursive=true (symlinks are copied as symlinks, permissions kept). " +
				"Creates parent directories. Refuses to overwrite unless overwrite=true.",
			DetailFunc: fromTo,
			Parameters: map[string]any{
				"from":      map[string]any{"type": "string", "description": "Source path"},
				"to":        map[string]any{"type": "string", "description": "Destination path"},
				"recursive": map[string]any{"type": "boolean", "description": "Required to copy a directory"},
				"overwrite": map[string]any{"type": "boolean", "description": "Replace an existing destination file (default false)"},
			},
			Required: []string{"from", "to"},
			Handler:  copyFileTool,
		},
		{
			Name:  "delete_file",
			Label: "Deleting",
			Description: "PERMANENTLY delete a file or an empty directory; a non-empty directory needs recursive=true. " +
				"There is no undo. Refuses filesystem roots, the home directory and anything containing the workspace root.",
			DetailFunc: pathDetail,
			Parameters: map[string]any{
				"path":      map[string]any{"type": "string", "description": "File or directory to delete"},
				"recursive": map[string]any{"type": "boolean", "description": "Allow deleting a non-empty directory tree (default false)"},
			},
			Required: []string{"path"},
			Handler:  deleteFile,
		},
		{
			Name:        "make_dir",
			Label:       "Creating directory",
			Description: "Create a directory and any missing parents (like mkdir -p). Succeeds if it already exists.",
			DetailFunc:  pathDetail,
			Parameters: map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory to create"},
			},
			Required: []string{"path"},
			Handler:  makeDir,
		},
	}
}

// protectedReason returns why abs must never be deleted/moved, or "".
func protectedReason(abs string) string {
	abs = filepath.Clean(abs)
	parent := filepath.Dir(abs)
	if parent == abs {
		return "it is a filesystem root"
	}
	if filepath.Dir(parent) == parent {
		return "it is a top-level system directory"
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == abs {
		return "it is the home directory"
	}
	guards := []string{workspace.Root(), os.Getenv("AWAKI_DATA_DIR")}
	for _, g := range guards {
		if g == "" {
			continue
		}
		g = filepath.Clean(g)
		if abs == g || strings.HasPrefix(g, abs+string(filepath.Separator)) {
			return "it is, or contains, the agent's workspace/data directory"
		}
	}
	return ""
}

// lockPaths takes the per-path edit locks for all paths in a stable order.
func lockPaths(paths ...string) func() {
	uniq := map[string]struct{}{}
	for _, p := range paths {
		uniq[p] = struct{}{}
	}
	keys := make([]string, 0, len(uniq))
	for p := range uniq {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	unlocks := make([]func(), 0, len(keys))
	for _, k := range keys {
		unlocks = append(unlocks, lockPath(k))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

func isWithin(child, parent string) bool {
	child, parent = filepath.Clean(child), filepath.Clean(parent)
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func moveFile(_ context.Context, input map[string]any) types.Result {
	from, err := types.GetString(input, "from")
	if err != nil {
		return types.ErrResult(err)
	}
	to, err := types.GetString(input, "to")
	if err != nil {
		return types.ErrResult(err)
	}
	src, dst := workspace.Resolve(from), workspace.Resolve(to)
	overwrite := types.GetBoolDefault(input, "overwrite", false)

	srcInfo, err := os.Lstat(src)
	if err != nil {
		return types.Errf("source not found: %s", src)
	}
	if why := protectedReason(src); why != "" {
		return types.Errf("refusing to move %s: %s", src, why)
	}
	if di, err := os.Stat(dst); err == nil && di.IsDir() && !(srcInfo.IsDir() && sameFile(srcInfo, di)) {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	if srcInfo.IsDir() && isWithin(dst, src) {
		return types.Errf("cannot move %s into itself (%s)", src, dst)
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return types.Errf("source and destination are the same path")
	}

	unlock := lockPaths(src, dst)
	defer unlock()

	if di, err := os.Lstat(dst); err == nil {
		if !overwrite {
			return types.Errf("destination exists: %s (pass overwrite=true to replace it)", dst)
		}
		if di.IsDir() || srcInfo.IsDir() {
			return types.Errf("overwrite only replaces files; %s is a directory — delete it first", dst)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return types.Errf("mkdir error: %v", err)
	}
	if err := os.Rename(src, dst); err != nil {
		// Cross-device (or otherwise refused) rename: copy then remove.
		if cerr := copyTree(src, dst); cerr != nil {
			_ = os.RemoveAll(dst)
			return types.Errf("move failed: %v (copy fallback: %v)", err, cerr)
		}
		if rerr := os.RemoveAll(src); rerr != nil {
			return types.Errf("copied to %s but could not remove source: %v", dst, rerr)
		}
	}
	markProjectDirty(src)
	markProjectDirty(dst)
	return types.Result{Output: fmt.Sprintf("Moved %s → %s", src, dst)}
}

func sameFile(a, b os.FileInfo) bool { return os.SameFile(a, b) }

func copyFileTool(_ context.Context, input map[string]any) types.Result {
	from, err := types.GetString(input, "from")
	if err != nil {
		return types.ErrResult(err)
	}
	to, err := types.GetString(input, "to")
	if err != nil {
		return types.ErrResult(err)
	}
	src, dst := workspace.Resolve(from), workspace.Resolve(to)
	recursive := types.GetBoolDefault(input, "recursive", false)
	overwrite := types.GetBoolDefault(input, "overwrite", false)

	srcInfo, err := os.Stat(src)
	if err != nil {
		return types.Errf("source not found: %s", src)
	}
	if srcInfo.IsDir() && !recursive {
		return types.Errf("%s is a directory — pass recursive=true to copy it", src)
	}
	if di, err := os.Stat(dst); err == nil && di.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	if srcInfo.IsDir() && isWithin(dst, src) {
		return types.Errf("cannot copy %s into itself (%s)", src, dst)
	}

	unlock := lockPaths(src, dst)
	defer unlock()

	if di, err := os.Lstat(dst); err == nil {
		if !overwrite {
			return types.Errf("destination exists: %s (pass overwrite=true to replace it)", dst)
		}
		if di.IsDir() || srcInfo.IsDir() {
			return types.Errf("overwrite only replaces files; %s is a directory", dst)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return types.Errf("mkdir error: %v", err)
	}
	files, bytes, err := countAndCopy(src, dst)
	if err != nil {
		return types.Errf("copy failed: %v", err)
	}
	markProjectDirty(dst)
	if srcInfo.IsDir() {
		return types.Result{Output: fmt.Sprintf("Copied directory %s → %s (%d files, %s)", src, dst, files, humanBytes(bytes))}
	}
	return types.Result{Output: fmt.Sprintf("Copied %s → %s (%s)", src, dst, humanBytes(bytes))}
}

func countAndCopy(src, dst string) (files int, bytes int64, err error) {
	if err = copyTree(src, dst); err != nil {
		return 0, 0, err
	}
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && d.Type().IsRegular() {
			files++
			if info, e := d.Info(); e == nil {
				bytes += info.Size()
			}
		}
		return nil
	})
	return files, bytes, nil
}

// copyTree copies a file, symlink or directory tree. Files are written via a
// temp file + rename so a failure never leaves a half-written destination.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode().Perm())
	case info.Mode().IsRegular():
		return copyRegular(src, dst, info.Mode().Perm())
	default:
		return fmt.Errorf("unsupported file type: %s", src)
	}
}

func copyRegular(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".awaki-copy-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func deleteFile(_ context.Context, input map[string]any) types.Result {
	rawPath, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	path := workspace.Resolve(rawPath)
	recursive := types.GetBoolDefault(input, "recursive", false)

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return types.Errf("not found: %s", path)
		}
		return types.Errf("stat error: %v", err)
	}
	if why := protectedReason(path); why != "" {
		return types.Errf("refusing to delete %s: %s", path, why)
	}

	unlock := lockPaths(path)
	defer unlock()

	if !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return types.Errf("delete error: %v", err)
		}
		markProjectDirty(path)
		return types.Result{Output: fmt.Sprintf("Deleted file %s (%s)", path, humanBytes(info.Size()))}
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return types.Errf("read dir error: %v", err)
	}
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil {
			return types.Errf("delete error: %v", err)
		}
		return types.Result{Output: fmt.Sprintf("Deleted empty directory %s", path)}
	}
	files, dirs, size := 0, 0, int64(0)
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || p == path {
			return nil
		}
		if d.IsDir() {
			dirs++
		} else {
			files++
			if i, e := d.Info(); e == nil {
				size += i.Size()
			}
		}
		return nil
	})
	if !recursive {
		return types.Errf("%s is a non-empty directory (%d files, %d subdirectories, %s). Pass recursive=true to delete it permanently.", path, files, dirs, humanBytes(size))
	}
	if err := os.RemoveAll(path); err != nil {
		return types.Errf("delete error: %v", err)
	}
	markProjectDirty(path)
	return types.Result{Output: fmt.Sprintf("Deleted directory %s (%d files, %d subdirectories, %s)", path, files, dirs, humanBytes(size))}
}

func makeDir(_ context.Context, input map[string]any) types.Result {
	rawPath, err := types.GetString(input, "path")
	if err != nil {
		return types.ErrResult(err)
	}
	path := workspace.Resolve(rawPath)
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return types.Result{Output: fmt.Sprintf("Directory already exists: %s", path)}
		}
		return types.Errf("%s exists and is a file", path)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return types.Errf("mkdir error: %v", err)
	}
	markProjectDirty(path)
	return types.Result{Output: fmt.Sprintf("Created directory %s", path)}
}
