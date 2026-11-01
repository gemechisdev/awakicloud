package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBuildRequestSpecBodyShapes(t *testing.T) {
	// JSON object body used to be silently dropped
	spec, err := buildRequestSpec(map[string]any{"url": "https://api.example.com/x", "method": "post", "body": map[string]any{"a": 1.0, "b": []any{"x", "y"}}})
	if err != nil || spec.Method != "POST" || spec.ContentType != "application/json" {
		t.Fatalf("%v %+v", err, spec)
	}
	var got map[string]any
	if json.Unmarshal(spec.Body, &got) != nil || got["a"] != 1.0 {
		t.Fatalf("body %q", spec.Body)
	}
	// the typed json parameter is the documented way to send an object
	spec, err = buildRequestSpec(map[string]any{"url": "https://e.com", "method": "POST", "json": map[string]any{"x": "y"}})
	if err != nil || string(spec.Body) != `{"x":"y"}` || spec.ContentType != "application/json" {
		t.Fatalf("%v %q", err, spec.Body)
	}
	if _, err = buildRequestSpec(map[string]any{"url": "https://e.com", "json": map[string]any{"x": 1.0}, "body": "raw"}); err == nil {
		t.Fatal("json+body must be rejected")
	}
	// string bodies pass through untouched
	spec, _ = buildRequestSpec(map[string]any{"url": "https://e.com", "body": `{"raw":true}`})
	if string(spec.Body) != `{"raw":true}` {
		t.Fatalf("%q", spec.Body)
	}
	// form
	spec, err = buildRequestSpec(map[string]any{"url": "https://e.com", "method": "POST", "form": map[string]any{"a": "1 2", "n": 3.0}})
	if err != nil || spec.ContentType != "application/x-www-form-urlencoded" || string(spec.Body) != "a=1+2&n=3" {
		t.Fatalf("%v %q %q", err, spec.ContentType, spec.Body)
	}
	if _, err = buildRequestSpec(map[string]any{"url": "https://e.com", "body": "x", "form": map[string]any{"a": "b"}}); err == nil {
		t.Fatal("body+form must be rejected")
	}
	// empty body means no body
	spec, _ = buildRequestSpec(map[string]any{"url": "https://e.com", "body": ""})
	if len(spec.Body) != 0 {
		t.Fatal("empty body should not be sent")
	}
}

func TestBuildRequestSpecQueryHeadersMethodTimeout(t *testing.T) {
	spec, err := buildRequestSpec(map[string]any{
		"url":     "https://e.com/path?keep=1",
		"query":   map[string]any{"q": "a b", "page": 2.0, "tags": []any{"x", "y"}},
		"headers": map[string]any{"authorization": "Bearer t", "X-Num": 5.0, "X-Bool": true},
		"timeout": "500",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"keep=1", "page=2", "q=a+b", "tags=x", "tags=y"} {
		if !strings.Contains(spec.URL, want) {
			t.Errorf("%q missing from %s", want, spec.URL)
		}
	}
	if spec.Headers["Authorization"] != "Bearer t" || spec.Headers["X-Num"] != "5" || spec.Headers["X-Bool"] != "true" {
		t.Fatalf("%v", spec.Headers)
	}
	if spec.Timeout != maxRequestTimeout {
		t.Fatalf("timeout %v should be capped", spec.Timeout)
	}
	if _, err := buildRequestSpec(map[string]any{"url": "https://e.com", "method": "TRACE"}); err == nil {
		t.Fatal("TRACE must be rejected")
	}
	if _, err := buildRequestSpec(map[string]any{"url": "https://e.com", "headers": map[string]any{"X-Bad": "a\r\nInjected: 1"}}); err == nil {
		t.Fatal("header injection must be rejected")
	}
	if _, err := buildRequestSpec(map[string]any{}); err == nil {
		t.Fatal("url required")
	}
}

func TestExecRequestAgainstServer(t *testing.T) {
	var gotBody, gotCT, gotAuth, gotMethod, gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotAuth, gotMethod, gotQuery = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Method, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "SECRET-VALUE"})
		http.SetCookie(w, &http.Cookie{Name: "pref", Value: "dark"})
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00binary"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("0123456789", 200_000))) // 2 MB
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/missing", 302) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	run := func(in map[string]any) (string, bool) {
		spec, err := buildRequestSpec(in)
		if err != nil {
			t.Fatal(err)
		}
		r := execRequest(context.Background(), srv.Client(), spec)
		return r.Output, r.IsErr
	}

	out, isErr := run(map[string]any{"url": srv.URL + "/echo", "method": "POST", "body": map[string]any{"k": "v"}, "headers": map[string]any{"Authorization": "Bearer z"}, "query": map[string]any{"a": "1"}})
	if isErr || !strings.HasPrefix(out, "HTTP 201 Created\n") || strings.Contains(out, "201 201") {
		t.Fatalf("status line: %q", out)
	}
	if gotBody != `{"k":"v"}` || gotCT != "application/json" || gotAuth != "Bearer z" || gotMethod != "POST" || gotQuery != "a=1" {
		t.Fatalf("server saw %q %q %q %q %q", gotBody, gotCT, gotAuth, gotMethod, gotQuery)
	}
	if !strings.Contains(out, "2 cookie(s) set (session, pref)") || strings.Contains(out, "SECRET-VALUE") {
		t.Fatalf("cookies must be summarised, never echoed: %q", out)
	}
	if !strings.HasSuffix(out, `{"ok":true}`) {
		t.Fatalf("body: %q", out)
	}

	if out, _ = run(map[string]any{"url": srv.URL + "/bin"}); !strings.Contains(out, "[binary body:") || strings.Contains(out, "PNG") {
		t.Fatalf("binary must not be dumped: %q", out)
	}
	out, _ = run(map[string]any{"url": srv.URL + "/big"})
	if !utf8.ValidString(out) || len(out) > 52*1024 {
		t.Fatalf("len=%d", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("truncation must be flagged: %q", out[len(out)-120:])
	}
	if out, isErr = run(map[string]any{"url": srv.URL + "/missing"}); !isErr || !strings.Contains(out, "404") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(map[string]any{"url": srv.URL + "/redir"}); !isErr || !strings.Contains(out, "Final-URL: "+srv.URL+"/missing") {
		t.Fatalf("redirect info missing: %q", out)
	}
	if out, _ = run(map[string]any{"url": srv.URL + "/empty"}); !strings.Contains(out, "(empty body)") {
		t.Fatalf("%q", out)
	}
	if out, _ = run(map[string]any{"url": srv.URL + "/echo", "method": "HEAD"}); !strings.Contains(out, "HEAD request: no body") {
		t.Fatalf("%q", out)
	}
}

func TestExecRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer srv.Close()
	spec, _ := buildRequestSpec(map[string]any{"url": srv.URL, "timeout": 1.0})
	start := time.Now()
	r := execRequest(context.Background(), srv.Client(), spec)
	if !r.IsErr || !strings.Contains(r.Output, "http error") || time.Since(start) > 1800*time.Millisecond {
		t.Fatalf("%q after %v", r.Output, time.Since(start))
	}
}

func TestPaginateTextIsRuneSafe(t *testing.T) {
	s := strings.Repeat("ሰላም ", 1000) // 4000 runes, 3 bytes each for most
	page, start, end, total := paginateText(s, 0, 1000)
	if total != 4000 || start != 0 || end != 1000 || !utf8.ValidString(page) || len([]rune(page)) != 1000 {
		t.Fatalf("%d %d %d %d", start, end, total, len([]rune(page)))
	}
	page, start, end, _ = paginateText(s, 3500, 1000)
	if end != 4000 || len([]rune(page)) != 500 || start != 3500 {
		t.Fatalf("tail page %d-%d", start, end)
	}
	if page, _, _, _ = paginateText(s, 9999, 10); page != "" {
		t.Fatal("past-the-end page must be empty")
	}
}

func TestFormatFetchedPaging(t *testing.T) {
	body := strings.Repeat("a", 250)
	first := formatFetched("https://x.test", "direct", "T", body, 100, 0, false)
	if !strings.Contains(first, "characters 0-100 of 250") || !strings.Contains(first, "offset=100") || !strings.Contains(first, "title: T") {
		t.Fatalf("%q", first)
	}
	mid := formatFetched("https://x.test", "direct", "", body, 100, 100, false)
	if !strings.Contains(mid, "characters 100-200 of 250") || !strings.Contains(mid, "offset=200") {
		t.Fatalf("%q", mid)
	}
	last := formatFetched("https://x.test", "direct", "", body, 100, 200, false)
	if !strings.Contains(last, "end of page: characters 200-250 of 250") || strings.Contains(last, "offset=") {
		t.Fatalf("%q", last)
	}
	if past := formatFetched("https://x.test", "direct", "", body, 100, 900, false); !strings.Contains(past, "past the end") {
		t.Fatalf("%q", past)
	}
	whole := formatFetched("https://x.test", "direct", "", "short", 100, 0, false)
	if strings.Contains(whole, "[") || !strings.HasSuffix(whole, "short") {
		t.Fatalf("a page that fits must have no footer: %q", whole)
	}
	cut := formatFetched("https://x.test", "jina.ai", "", body[:100], 100, 0, true)
	if !strings.Contains(cut, "cut at 100 characters") || !strings.Contains(cut, "offset=100") {
		t.Fatalf("%q", cut)
	}
}
