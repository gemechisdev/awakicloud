package project

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// archivedPaths walks root and returns the relative (slash-separated) paths that
// belong in an archive, sorted for deterministic output. Archive rules are
// applied here, so the zip builder, the content hash and CopyTreeIgnoring all
// see exactly the same file set.
func archivedPaths(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if shouldExcludeFromArchive(relSlash) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, relSlash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// hashPaths hashes a filtered, sorted path list: relative path, NUL, content,
// NUL — the same scheme the content hash has always used. pred selects the
// paths that take part. Returns ok=false when nothing matched, so callers can
// distinguish "no git state" from a real (empty) hash.
func hashPaths(root string, pred func(rel string) bool) (string, bool, error) {
	all, err := archivedPaths(root)
	if err != nil {
		return "", false, err
	}
	var files []string
	for _, p := range all {
		if pred(p) {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return "", false, nil
	}
	h := sha256.New()
	for _, rel := range files {
		h.Write([]byte(rel))
		h.Write([]byte("\x00"))
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", false, err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", false, err
		}
		f.Close()
		h.Write([]byte("\x00"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true, nil
}

// ZipTree archives root into a zip, applying archive rules. Returns zip data +
// content hash plus file stats (count, total size). The content hash is computed
// from file contents (not zip bytes), so identical file trees always produce the
// same hash.
//
// The hash covers every archived file, .git/objects included. That means git
// housekeeping (gc, repack) shifts the hash and costs one re-upload; see
// gitstate.go for why that is preferred over a git-aware heuristic.
func ZipTree(root string) (data []byte, contentHash string, fileCount int, totalSize int64, err error) {
	return zipTree(root, true)
}

// ZipTreeExceptGitObjects archives everything except the object database. Only
// used as the oversize fallback (see Manager.Sync): refs, HEAD and packed-refs
// still ship, so the branch tip is known, but the blobs of past commits are not
// and the project cannot be checked out at an old revision.
func ZipTreeExceptGitObjects(root string) (data []byte, contentHash string, fileCount int, totalSize int64, err error) {
	return zipTree(root, false)
}

func zipTree(root string, includeGitObjects bool) (data []byte, contentHash string, fileCount int, totalSize int64, err error) {
	zipFiles, err := archivedPaths(root)
	if err != nil {
		return nil, "", 0, 0, err
	}
	if !includeGitObjects {
		kept := zipFiles[:0]
		for _, p := range zipFiles {
			if !gitObjectRel(p) {
				kept = append(kept, p)
			}
		}
		zipFiles = kept
	}

	h := sha256.New()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, rel := range zipFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, statErr := os.Stat(full)
		if statErr != nil || !info.Mode().IsRegular() {
			continue // tree changed under us — skip, don't abort the snapshot
		}
		f, openErr := os.Open(full)
		if openErr != nil {
			continue
		}
		// Content hash: path + NUL + content + NUL.
		h.Write([]byte(rel))
		h.Write([]byte("\x00"))
		if _, copyErr := io.Copy(h, f); copyErr != nil {
			f.Close()
			_ = zw.Close()
			return nil, "", 0, 0, copyErr
		}
		h.Write([]byte("\x00"))
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			f.Close()
			_ = zw.Close()
			return nil, "", 0, 0, seekErr
		}
		fh := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: info.ModTime()}
		// Preserve the UNIX mode. Without this a restored shell script (or any
		// executable) comes back as 0644 and will not run.
		fh.SetMode(info.Mode())
		w, createErr := zw.CreateHeader(fh)
		if createErr != nil {
			f.Close()
			_ = zw.Close()
			return nil, "", 0, 0, createErr
		}
		if _, copyErr := io.Copy(w, f); copyErr != nil {
			f.Close()
			_ = zw.Close()
			return nil, "", 0, 0, copyErr
		}
		f.Close()
		fileCount++
		totalSize += info.Size()
	}
	if err := zw.Close(); err != nil {
		return nil, "", 0, 0, err
	}

	data = buf.Bytes()
	contentHash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return data, contentHash, fileCount, totalSize, nil
}

// LegacyZipHash computes the SHA-256 of the zip bytes (kept for backward
// compatibility with existing project meta that may store zip_hash).
func LegacyZipHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ContentHash computes the same content hash ZipTree produces (relative paths +
// file contents, honouring the archive rules) without building a zip. Auto-sync
// uses it to detect real changes in the tree, so it stays correct no matter
// which tool (edit_file, write_file, bash, a build script, …) changed a file.
func ContentHash(root string) (string, error) {
	hash, ok, err := hashPaths(root, func(string) bool { return true })
	if err != nil {
		return "", err
	}
	if !ok {
		// Empty tree — keep the historical sha256-of-nothing value.
		return "sha256:" + hex.EncodeToString(sha256.New().Sum(nil)), nil
	}
	return hash, nil
}

// ExtractZip writes zip data into dest (created if needed) and then repairs the
// git working state. Existing files may be overwritten.
func ExtractZip(data []byte, dest string) error {
	if err := extractZip(data, dest); err != nil {
		return err
	}
	// .git/index is not shipped (it is a cache); rebuild it from HEAD so a
	// restored repository reports a clean tree instead of "every file deleted".
	// Projects often contain a checkout one level down, so repair every repo.
	return EnsureGitIndexes(dest)
}

func extractZip(data []byte, dest string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") {
			return fmt.Errorf("invalid path in zip: %s", f.Name)
		}
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			return fmt.Errorf("zip path escapes dest: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
