package comm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gemechischala/awaki/internal/transport"
	"github.com/gemechischala/awaki/internal/workspace"
)

type fakeReplier struct {
	transport.Replier
	sent    []string
	caption string
}

func (f *fakeReplier) SendFile(_ context.Context, path, caption string) error {
	f.sent = append(f.sent, path)
	f.caption = caption
	return nil
}

func setup(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if err := workspace.SetRoot(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func put(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func send(t *testing.T, path string) (string, bool, *fakeReplier) {
	rep := &fakeReplier{}
	ctx := transport.WithReplier(context.Background(), rep)
	r := sendFileHandler(ctx, map[string]any{"path": path, "caption": "cap"})
	return r.Output, r.IsErr, rep
}

func TestSendRelativeWorkspaceFile(t *testing.T) {
	dir := setup(t)
	put(t, filepath.Join(dir, "out/report.pdf"), "data")
	out, isErr, rep := send(t, "out/report.pdf")
	if isErr || len(rep.sent) != 1 || rep.sent[0] != filepath.Join(dir, "out/report.pdf") || rep.caption != "cap" || !strings.Contains(out, "report.pdf (4 bytes)") {
		t.Fatalf("%v %q %v", isErr, out, rep.sent)
	}
}

func TestSendRefusesOutsideAndSecrets(t *testing.T) {
	dir := setup(t)
	outside := t.TempDir()
	// the OS temp dir is an allowed root, so use a path that is neither workspace nor temp
	if runtime.GOOS != "windows" {
		for _, p := range []string{"/etc/passwd", "/etc/hostname"} {
			if _, err := os.Stat(p); err == nil {
				if out, isErr, rep := send(t, p); !isErr || len(rep.sent) != 0 || !strings.Contains(out, "outside") {
					t.Errorf("%s: %v %q", p, isErr, out)
				}
			}
		}
	}
	for _, name := range []string{".env", ".env.production", "id_rsa", "server.pem", "tg.session", "credentials.json", ".ssh/config", "secrets.yaml"} {
		put(t, filepath.Join(dir, name), "SECRET")
		if out, isErr, rep := send(t, name); !isErr || len(rep.sent) != 0 || !strings.Contains(out, "credential") {
			t.Errorf("%s was not refused: %v %q", name, isErr, out)
		}
	}
	_ = outside
}

func TestSendSymlinkEscapeBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := setup(t)
	if _, err := os.Stat("/etc/hostname"); err != nil {
		t.Skip("no /etc/hostname")
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "innocent.txt")); err != nil {
		t.Skip("symlink unsupported")
	}
	if out, isErr, rep := send(t, "innocent.txt"); !isErr || len(rep.sent) != 0 {
		t.Fatalf("symlink escaped the allowed roots: %v %q", isErr, out)
	}
}

func TestSendValidation(t *testing.T) {
	dir := setup(t)
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	put(t, filepath.Join(dir, "empty.txt"), "")
	for path, want := range map[string]string{"nope.txt": "not found", "d": "directory", "empty.txt": "empty"} {
		if out, isErr, _ := send(t, path); !isErr || !strings.Contains(out, want) {
			t.Errorf("%s: %v %q", path, isErr, out)
		}
	}
	t.Setenv("AWAKI_SEND_MAX_MB", "1")
	put(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 2<<20))
	if out, isErr, _ := send(t, "big.bin"); !isErr || !strings.Contains(out, "limit is 1 MB") {
		t.Fatalf("%v %q", isErr, out)
	}
}

func TestSendExtraRoots(t *testing.T) {
	setup(t)
	extra, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("AWAKI_SEND_FILE_ROOTS", extra)
	put(t, filepath.Join(extra, "export.txt"), "ok")
	if out, isErr, rep := send(t, filepath.Join(extra, "export.txt")); isErr || len(rep.sent) != 1 {
		t.Fatalf("%v %q", isErr, out)
	}
}
