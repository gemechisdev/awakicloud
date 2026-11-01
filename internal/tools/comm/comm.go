package comm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"
	"github.com/gemechischala/awaki/internal/workspace"
)

const defaultMaxSendMB = 50 // Telegram bot upload limit

func ChannelTools() []types.Tool {
	return []types.Tool{
		{
			Name:  "send_file",
			Label: "Sending file",
			Description: "Send a file (document, image, audio, video) back to the user through the current communication channel. " +
				"The file must be inside the workspace, the temp directory or an allowed export directory (copy it into the workspace first otherwise); " +
				"credential-like files (.env, keys, sessions) are refused. Max 50 MB.",
			DetailFunc: func(input map[string]any) string {
				if p, ok := input["path"].(string); ok {
					return filepath.Base(p)
				}
				return ""
			},
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file (absolute, or relative to the workspace root)",
				},
				"caption": map[string]any{
					"type":        "string",
					"description": "Optional caption for the file",
				},
			},
			Required: []string{"path"},
			Handler:  sendFileHandler,
		},
	}
}

// sendRoots are the directories files may be sent from.
func sendRoots() []string {
	roots := []string{os.TempDir()}
	if r := workspace.Root(); r != "" {
		roots = append(roots, r)
	}
	if extra := os.Getenv("AWAKI_SEND_FILE_ROOTS"); extra != "" {
		for _, p := range filepath.SplitList(extra) {
			if p = strings.TrimSpace(p); p != "" {
				roots = append(roots, p)
			}
		}
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		out = append(out, filepath.Clean(r))
	}
	return out
}

var deniedDirs = map[string]bool{".ssh": true, ".gnupg": true, ".aws": true, ".kube": true, ".docker": true}

// credentialLike reports names that almost always hold secrets.
func credentialLike(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if deniedDirs[part] {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	switch {
	case base == ".env", strings.HasPrefix(base, ".env."), strings.HasSuffix(base, ".env"):
		return true
	case strings.HasPrefix(base, "id_rsa"), strings.HasPrefix(base, "id_ed25519"), strings.HasPrefix(base, "id_ecdsa"):
		return true
	case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".key"), strings.HasSuffix(base, ".p12"), strings.HasSuffix(base, ".pfx"):
		return true
	case strings.HasSuffix(base, ".session"), strings.HasSuffix(base, ".session-journal"), base == ".netrc", base == ".npmrc", base == ".pgpass":
		return true
	case strings.HasPrefix(base, "credentials"), strings.HasPrefix(base, "secrets."), base == "secrets":
		return true
	}
	return false
}

func maxSendBytes() int64 {
	mb := int64(defaultMaxSendMB)
	if v, err := strconv.ParseInt(os.Getenv("AWAKI_SEND_MAX_MB"), 10, 64); err == nil && v > 0 {
		mb = v
	}
	return mb << 20
}

// checkSendPath resolves and validates a file the agent wants to send.
func checkSendPath(raw string) (string, error) {
	path := workspace.Resolve(raw)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s (resolved from %q)", path, raw)
		}
		return "", fmt.Errorf("cannot access %s: %v", path, err)
	}
	if credentialLike(path) || credentialLike(real) {
		return "", fmt.Errorf("refusing to send %s: it looks like a credential/secret file", filepath.Base(real))
	}
	allowed := false
	for _, root := range sendRoots() {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("%s is outside the directories files may be sent from (workspace, temp). Copy it into the workspace first (copy_file), or ask the owner to set AWAKI_SEND_FILE_ROOTS", real)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("cannot access %s: %v", real, err)
	}
	if !info.Mode().IsRegular() {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory — zip it first (bash: zip -r out.zip dir) and send the archive", real)
		}
		return "", fmt.Errorf("%s is not a regular file", real)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%s is empty", real)
	}
	if limit := maxSendBytes(); info.Size() > limit {
		return "", fmt.Errorf("%s is %.1f MB; the limit is %d MB — split or compress it first", filepath.Base(real), float64(info.Size())/(1<<20), limit>>20)
	}
	return real, nil
}

func sendFileHandler(ctx context.Context, input map[string]any) types.Result {
	reply, ok := transport.ReplierFromContext(ctx)
	if !ok {
		return types.Result{Output: "error: no replier in context", IsErr: true}
	}
	raw, _ := input["path"].(string)
	if raw == "" {
		return types.Result{Output: "error: path is required", IsErr: true}
	}
	path, err := checkSendPath(raw)
	if err != nil {
		return types.ErrResult(err)
	}
	caption, _ := input["caption"].(string)
	if err := reply.SendFile(ctx, path, caption); err != nil {
		return types.Result{Output: "failed to send file: " + err.Error(), IsErr: true}
	}
	info, _ := os.Stat(path)
	return types.Result{Output: fmt.Sprintf("File sent: %s (%d bytes)", filepath.Base(path), info.Size())}
}
