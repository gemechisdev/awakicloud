package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/utils"
	"github.com/gemechischala/awaki/internal/workspace"
)

const downloadTimeout = 120 * time.Second

// maxDownloadSize is the largest file web_download will save (variable so tests can lower it).
var maxDownloadSize int64 = 100 * 1024 * 1024 // 100 MB

func WebDownloadTool() types.Tool {
	return types.Tool{
		Name:  "web_download",
		Label: "Downloading",
		Description: "Download a file from a URL and save it to disk. Use for PDFs, images, archives, binaries, or any file. Max size 100 MB: a larger file is rejected — never saved truncated. " +
			"path may be a file (relative paths are resolved against the workspace) or an existing directory. An existing file is never overwritten unless overwrite=true. " +
			"If path is omitted the file goes to the temp directory under the server's filename. The result includes size and SHA-256.",
		Parameters: map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The full URL of the file to download",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Where to save: a file path, or a directory (the server's filename is used). Relative paths are relative to the workspace root. Default: the temp directory.",
			},
			"overwrite": map[string]any{
				"type":        "boolean",
				"description": "Replace the destination if it already exists (default false)",
			},
		},
		Required: []string{"url"},
		Handler:  webDownload,
	}
}

func webDownload(ctx context.Context, input map[string]any) types.Result {
	rawURL, err := types.GetString(input, "url")
	if err != nil {
		return types.ErrResult(err)
	}
	if err := ValidatePublicURL(rawURL); err != nil {
		return types.Errf("url blocked: %v", err)
	}
	dest := types.GetStringOpt(input, "path")
	if dest != "" {
		trailing := strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, string(filepath.Separator))
		dest = workspace.Resolve(dest)
		if trailing {
			dest += string(filepath.Separator)
		}
	}
	res, err := downloadFile(ctx, safeHTTPClient(60*time.Second), rawURL, dest, types.GetBoolDefault(input, "overwrite", false), maxDownloadSize)
	if err != nil {
		return types.ErrResult(err)
	}
	return types.Result{Output: res.String()}
}

type downloadResult struct {
	Path        string
	Size        int64
	SHA256      string
	ContentType string
	Notes       []string
}

func (r downloadResult) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Downloaded to %s\n", r.Path)
	fmt.Fprintf(&sb, "Size: %s (%d bytes)\n", utils.HumanSize(r.Size), r.Size)
	fmt.Fprintf(&sb, "SHA-256: %s\n", r.SHA256)
	if r.ContentType != "" {
		fmt.Fprintf(&sb, "Content-Type: %s\n", r.ContentType)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&sb, "Note: %s\n", n)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// sanitizeFilename turns a server- or URL-supplied name into a safe single
// path component. It returns "" when nothing usable is left.
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.Trim(name, " .")
	if name == "" {
		return ""
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

func serverFilename(resp *http.Response, rawURL string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if n := sanitizeFilename(filepath.Base(strings.ReplaceAll(params["filename"], `\`, "/"))); n != "" {
				return n
			}
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		if n := sanitizeFilename(filepath.Base(u.Path)); n != "" && u.Path != "" && u.Path != "/" {
			return n
		}
	}
	return "download"
}

// uniquePath returns p, or "name-1.ext", "name-2.ext", ... when p exists.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Lstat(cand); err != nil {
			return cand
		}
	}
	return p
}

// downloadFile fetches rawURL and saves it, all-or-nothing: the destination is
// only touched (atomically) after the whole body arrived within the size limit.
// dest: "" = temp dir + server filename; a path ending in a separator or naming
// an existing directory = that directory + server filename; otherwise the file.
func downloadFile(ctx context.Context, client *http.Client, rawURL, dest string, overwrite bool, maxBytes int64) (downloadResult, error) {
	var res downloadResult
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return res, fmt.Errorf("bad url: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AwakiCloudBot/1.0)")
	resp, err := client.Do(req)
	if err != nil {
		return res, fmt.Errorf("http error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
	}
	if resp.ContentLength > maxBytes {
		return res, fmt.Errorf("file is %s, over the %s limit — not downloaded. Use bash (curl -o / wget) for very large files", utils.HumanSize(resp.ContentLength), utils.HumanSize(maxBytes))
	}
	res.ContentType = resp.Header.Get("Content-Type")

	// ---- destination
	autoName := false
	switch {
	case dest == "":
		dest = filepath.Join(os.TempDir(), serverFilename(resp, rawURL))
		autoName = true
	case strings.HasSuffix(dest, string(filepath.Separator)) || strings.HasSuffix(dest, "/"):
		dest = filepath.Join(dest, serverFilename(resp, rawURL))
		autoName = true
	default:
		if info, err := os.Stat(dest); err == nil && info.IsDir() {
			dest = filepath.Join(dest, serverFilename(resp, rawURL))
			autoName = true
		}
	}
	dest = filepath.Clean(dest)
	if autoName {
		dest = uniquePath(dest)
	} else if info, err := os.Lstat(dest); err == nil {
		if info.IsDir() {
			return res, fmt.Errorf("%s is a directory", dest)
		}
		if !overwrite {
			return res, fmt.Errorf("%s already exists (pass overwrite=true to replace it, or choose another path)", dest)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return res, fmt.Errorf("mkdir error: %v", err)
	}

	// ---- body -> temp file in the same directory
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".awaki-dl-*")
	if err != nil {
		return res, fmt.Errorf("create file error: %v", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		cleanup()
		return res, fmt.Errorf("download error after %s: %v (nothing was saved)", utils.HumanSize(written), err)
	}
	if written > maxBytes {
		cleanup()
		return res, fmt.Errorf("file is larger than the %s limit — download aborted, nothing was saved. Use bash (curl -o / wget) for very large files", utils.HumanSize(maxBytes))
	}
	if resp.ContentLength > 0 && !resp.Uncompressed && written != resp.ContentLength {
		cleanup()
		return res, fmt.Errorf("incomplete download: got %d of %d bytes (nothing was saved)", written, resp.ContentLength)
	}
	if written == 0 {
		cleanup()
		return res, errors.New("the server returned an empty body — nothing was saved")
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return res, fmt.Errorf("write error: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return res, fmt.Errorf("write error: %v", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return res, fmt.Errorf("chmod error: %v", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return res, fmt.Errorf("could not move the file into place: %v", err)
	}

	res.Path, res.Size, res.SHA256 = dest, written, hex.EncodeToString(h.Sum(nil))
	if ct := strings.ToLower(res.ContentType); strings.HasPrefix(ct, "text/html") {
		if ext := strings.ToLower(filepath.Ext(dest)); ext != ".html" && ext != ".htm" {
			res.Notes = append(res.Notes, "the server sent an HTML page, not a file — this is often a login wall, captcha or error page. Check it before using it.")
		}
	}
	return res, nil
}
