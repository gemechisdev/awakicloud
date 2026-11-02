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

// ZipTree archives root into a zip, applying ignore rules. Returns zip data + content hash.
// The content hash is computed from file contents (not zip bytes), so identical
// file trees always produce the same hash regardless of zip metadata (timestamps,
// entry order, compression). This prevents false "changed" detections that cause
// unnecessary re-uploads to Telegram.
func ZipTree(root string) (data []byte, contentHash string, err error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	type fileEntry struct {
		rel  string
		path string
		info os.FileInfo
	}
	var entries []fileEntry

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
		if shouldIgnore(relSlash) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, fileEntry{rel: relSlash, path: path, info: info})
		return nil
	})
	if err != nil {
		_ = zw.Close()
		return nil, "", err
	}

	// Sort entries for deterministic zip output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].rel < entries[j].rel
	})

	// Compute content hash from sorted (path, content) pairs.
	h := sha256.New()
	for _, e := range entries {
		if e.info.IsDir() {
			_, err := zw.Create(e.rel + "/")
			if err != nil {
				_ = zw.Close()
				return nil, "", err
			}
			continue
		}
		if !e.info.Mode().IsRegular() {
			continue
		}
		// Hash the relative path + file content.
		h.Write([]byte(e.rel))
		h.Write([]byte("\x00"))
		f, err := os.Open(e.path)
		if err != nil {
			_ = zw.Close()
			return nil, "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			_ = zw.Close()
			return nil, "", err
		}
		f.Close()
		h.Write([]byte("\x00"))

		// Write to zip.
		w, err := zw.Create(e.rel)
		if err != nil {
			_ = zw.Close()
			return nil, "", err
		}
		f, err = os.Open(e.path)
		if err != nil {
			_ = zw.Close()
			return nil, "", err
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			_ = zw.Close()
			return nil, "", err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	data = buf.Bytes()
	contentHash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return data, contentHash, nil
}

// LegacyZipHash computes the SHA-256 of the zip bytes (kept for backward
// compatibility with existing project meta that may store zip_hash).
func LegacyZipHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExtractZip writes zip data into dest (created if needed). Existing files may be overwritten.
func ExtractZip(data []byte, dest string) error {
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
