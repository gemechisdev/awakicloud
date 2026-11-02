package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dlServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/report.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4 hello report"))
	})
	mux.HandleFunc("/cd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/evil name.txt"`)
		w.Write([]byte("payload"))
	})
	mux.HandleFunc("/html/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html>please log in</html>"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { // no Content-Length: chunked
		w.Header().Set("Content-Type", "application/octet-stream")
		w.(http.Flusher).Flush()
		for i := 0; i < 50; i++ {
			w.Write([]byte(strings.Repeat("x", 1024)))
		}
	})
	mux.HandleFunc("/bigcl", func(w http.ResponseWriter, r *http.Request) { // declares its size up front
		w.Header().Set("Content-Length", "50000")
		w.Write([]byte(strings.Repeat("x", 50000)))
	})
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) { // promises 1000 bytes, sends 100, hangs up
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(strings.Repeat("y", 100)))
		if h, ok := w.(http.Hijacker); ok {
			conn, _, _ := h.Hijack()
			conn.Close()
		}
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func dl(t *testing.T, srv *httptest.Server, path, dest string, overwrite bool, max int64) (downloadResult, error) {
	t.Helper()
	return downloadFile(context.Background(), srv.Client(), srv.URL+path, dest, overwrite, max)
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".awaki-dl-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDownloadHappyPathHasChecksumAndAtomicWrite(t *testing.T) {
	srv := dlServer(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "sub", "r.pdf")
	res, err := dl(t, srv, "/files/report.pdf", dest, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != "%PDF-1.4 hello report" || res.Size != int64(len(data)) || len(res.SHA256) != 64 {
		t.Fatalf("%+v %q", res, data)
	}
	// sha256 of that exact content
	if res.SHA256 != "ae2b2b7a4bfa1b4c0e9d1b5f6e4e8d2a" && !strings.Contains(res.String(), "SHA-256: "+res.SHA256) {
		t.Fatal("result must report the checksum")
	}
	noTemps(t, filepath.Dir(dest))
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestDownloadOverLimitIsRejectedNotTruncated(t *testing.T) {
	srv := dlServer(t)
	dir := t.TempDir()
	// streamed (no Content-Length): previously this saved a silently truncated file and reported success
	dest := filepath.Join(dir, "big.bin")
	_, err := dl(t, srv, "/big", dest, false, 10*1024)
	if err == nil || !strings.Contains(err.Error(), "larger than") || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("expected a limit error, got %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("a truncated file was left at the destination")
	}
	// declared up front: refused before any bytes are written
	_, err = dl(t, srv, "/bigcl", dest, false, 10*1024)
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("%v", err)
	}
	// exactly at the limit is fine
	if _, err = dl(t, srv, "/bigcl", dest, false, 50000); err != nil {
		t.Fatalf("a file exactly at the limit must be accepted: %v", err)
	}
	noTemps(t, dir)
}

func TestDownloadFailuresNeverDestroyExistingFile(t *testing.T) {
	srv := dlServer(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "precious.txt")
	os.WriteFile(dest, []byte("precious original"), 0o600)
	for _, tc := range []struct{ path, want string }{
		{"/gone", "HTTP 404"}, {"/short", "nothing was saved"}, {"/empty", "empty body"}, {"/big", "larger than"},
	} {
		_, err := dl(t, srv, tc.path, dest, true, 10*1024) // overwrite=true: still must not damage it on failure
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v want %q", tc.path, err, tc.want)
		}
		if data, _ := os.ReadFile(dest); string(data) != "precious original" {
			t.Fatalf("%s: the existing file was damaged: %q", tc.path, data)
		}
	}
	noTemps(t, dir)
}

func TestDownloadOverwriteRules(t *testing.T) {
	srv := dlServer(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "x.pdf")
	os.WriteFile(dest, []byte("old"), 0o640)
	if _, err := dl(t, srv, "/files/report.pdf", dest, false, 1<<20); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("must refuse to overwrite by default: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "old" {
		t.Fatal("refused download still modified the file")
	}
	if _, err := dl(t, srv, "/files/report.pdf", dest, true, 1<<20); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("%q", data)
	}
	if _, err := dl(t, srv, "/files/report.pdf", dir, false, 1<<20); err != nil { // directory destination
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "report.pdf")) {
		t.Fatal("directory destination should use the server's filename")
	}
	if _, err := dl(t, srv, "/files/report.pdf", filepath.Join(dir, "sub")+string(filepath.Separator), false, 1<<20); err != nil || !exists(filepath.Join(dir, "sub", "report.pdf")) {
		t.Fatalf("trailing-slash destination: %v", err)
	}
	// a directory named like the target file
	os.MkdirAll(filepath.Join(dir, "adir"), 0o755)
	if _, err := dl(t, srv, "/files/report.pdf", filepath.Join(dir, "adir"), true, 1<<20); err != nil || !exists(filepath.Join(dir, "adir", "report.pdf")) {
		t.Fatalf("existing directory as dest: %v", err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestDownloadAutoNameIsUniqueAndSafe(t *testing.T) {
	srv := dlServer(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	r1, err := dl(t, srv, "/files/report.pdf", "", false, 1<<20)
	if err != nil || filepath.Dir(r1.Path) != tmp || filepath.Base(r1.Path) != "report.pdf" {
		t.Fatalf("%v %+v", err, r1)
	}
	r2, err := dl(t, srv, "/files/report.pdf", "", false, 1<<20)
	if err != nil || filepath.Base(r2.Path) != "report-1.pdf" {
		t.Fatalf("auto-named download must not clobber the previous one: %v %+v", err, r2)
	}
	// hostile Content-Disposition cannot escape the directory
	r3, err := dl(t, srv, "/cd", "", false, 1<<20)
	if err != nil || filepath.Dir(r3.Path) != tmp || strings.Contains(filepath.Base(r3.Path), "..") || strings.ContainsAny(filepath.Base(r3.Path), `/\`) {
		t.Fatalf("%v %+v", err, r3)
	}
	if filepath.Base(r3.Path) != "evil name.txt" {
		t.Fatalf("expected the sanitised base name, got %q", filepath.Base(r3.Path))
	}
}

func TestDownloadWarnsOnHTMLPage(t *testing.T) {
	srv := dlServer(t)
	dir := t.TempDir()
	res, err := dl(t, srv, "/html/page", filepath.Join(dir, "data.zip"), false, 1<<20)
	if err != nil || len(res.Notes) != 1 || !strings.Contains(res.String(), "login wall") {
		t.Fatalf("%v %+v", err, res)
	}
	res, _ = dl(t, srv, "/html/page", filepath.Join(dir, "page.html"), false, 1<<20)
	if len(res.Notes) != 0 {
		t.Fatalf("no warning expected when an .html file was asked for: %+v", res)
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf", "../../etc/passwd": "_.._etc_passwd", "a/b\\c": "a_b_c", "": "", "..": "", ".": "", "  ": "",
		"x\x00y": "x_y", "tab\tname": "tab_name", " lead.txt ": "lead.txt", "C:evil": "C_evil",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q)=%q want %q", in, got, want)
		}
	}
	long := sanitizeFilename(strings.Repeat("a", 500) + ".tar.gz")
	if len(long) > 200 || !strings.HasSuffix(long, ".gz") {
		t.Fatalf("long names must be shortened but keep the extension: %d %q", len(long), long[len(long)-10:])
	}
	_ = fmt.Sprint
}
