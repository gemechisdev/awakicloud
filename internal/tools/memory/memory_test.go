package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	"github.com/gemechischala/awaki/internal/tools/types"
)

func tool(t *testing.T, st store.Store, name string) types.Handler {
	t.Helper()
	for _, tl := range MemoryTools(st) {
		if tl.Name == name {
			return tl.Handler
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func call(t *testing.T, st store.Store, name string, in map[string]any) types.Result {
	return tool(t, st, name)(context.Background(), in)
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestWriteReadNameConsistency(t *testing.T) {
	st := newStore(t)
	// the same logical file must be reachable with or without ".md"
	if r := call(t, st, "memory_write", map[string]any{"file": "notes", "content": "# Notes\nhello\n"}); r.IsErr || !strings.Contains(r.Output, "created") {
		t.Fatalf("%q", r.Output)
	}
	for _, name := range []string{"notes", "notes.md", "sub/dir/notes.md"} {
		if r := call(t, st, "memory_read", map[string]any{"file": name}); r.IsErr || !strings.HasPrefix(r.Output, "# Notes") {
			t.Fatalf("read %q: %q", name, r.Output)
		}
	}
	if r := call(t, st, "memory_write", map[string]any{"file": "notes.md", "content": "# Notes\nreplaced\n"}); !strings.Contains(r.Output, "updated (") {
		t.Fatalf("%q", r.Output)
	}
	if r := call(t, st, "memory_delete", map[string]any{"file": "notes"}); r.IsErr {
		t.Fatalf("delete without extension must work: %q", r.Output)
	}
	if r := call(t, st, "memory_read", map[string]any{"file": "notes"}); !r.IsErr {
		t.Fatal("deleted file should be gone")
	}
	for _, bad := range []string{"..", ".", ".hidden", "/"} {
		if r := call(t, st, "memory_write", map[string]any{"file": bad, "content": "x"}); !r.IsErr {
			t.Errorf("name %q must be rejected", bad)
		}
	}
}

func TestListShowsPreviewsSorted(t *testing.T) {
	st := newStore(t)
	call(t, st, "memory_write", map[string]any{"file": "zeta.md", "content": "# Server setup\nlines\n"})
	call(t, st, "memory_write", map[string]any{"file": "alpha.md", "content": "\n\n> **Owner** prefers short answers\n"})
	r := call(t, st, "memory_list", map[string]any{})
	lines := strings.Split(strings.TrimSpace(r.Output), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "alpha.md") || !strings.Contains(lines[0], "Owner** prefers short answers") || !strings.Contains(lines[1], "Server setup") {
		t.Fatalf("%q", r.Output)
	}
}

func TestReadMissingSuggestsNames(t *testing.T) {
	st := newStore(t)
	call(t, st, "memory_write", map[string]any{"file": "server-notes.md", "content": "x"})
	call(t, st, "memory_write", map[string]any{"file": "owner.md", "content": "x"})
	r := call(t, st, "memory_read", map[string]any{"file": "server"})
	if !r.IsErr || !strings.Contains(r.Output, "Did you mean: server-notes.md") {
		t.Fatalf("%q", r.Output)
	}
	r = call(t, st, "memory_read", map[string]any{"file": "zzz"})
	if !strings.Contains(r.Output, "Available files: owner.md, server-notes.md") {
		t.Fatalf("%q", r.Output)
	}
}

func TestAppend(t *testing.T) {
	st := newStore(t)
	if r := call(t, st, "memory_append", map[string]any{"file": "log", "content": "first"}); r.IsErr || !strings.Contains(r.Output, "created") {
		t.Fatalf("%q", r.Output)
	}
	call(t, st, "memory_append", map[string]any{"file": "log.md", "content": "second\n"})
	call(t, st, "memory_append", map[string]any{"file": "log", "content": "third"})
	got, _ := st.ReadMemory("log")
	if got != "first\nsecond\nthird\n" {
		t.Fatalf("%q", got)
	}
}

func TestConcurrentAppendsLoseNothing(t *testing.T) {
	st := newStore(t)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if r := call(t, st, "memory_append", map[string]any{"file": "shared", "content": fmt.Sprintf("entry-%d", i)}); r.IsErr {
				t.Errorf("%q", r.Output)
			}
		}(i)
	}
	wg.Wait()
	got, _ := st.ReadMemory("shared")
	for i := 0; i < n; i++ {
		if !strings.Contains(got, fmt.Sprintf("entry-%d\n", i)) {
			t.Fatalf("lost append %d", i)
		}
	}
}

func TestSearch(t *testing.T) {
	st := newStore(t)
	call(t, st, "memory_write", map[string]any{"file": "owner.md", "content": "# Owner\nName: Venopyx\nLikes Go and Python\n"})
	call(t, st, "memory_write", map[string]any{"file": "server.md", "content": "# Server\nRuns Go services\nPort 8080\n"})
	r := call(t, st, "memory_search", map[string]any{"query": "go"})
	if !strings.Contains(r.Output, "owner.md\n  3: Likes Go and Python") || !strings.Contains(r.Output, "server.md\n  2: Runs Go services") {
		t.Fatalf("%q", r.Output)
	}
	if r = call(t, st, "memory_search", map[string]any{"query": "go python"}); strings.Contains(r.Output, "server.md") || !strings.Contains(r.Output, "owner.md") {
		t.Fatalf("all-terms matching: %q", r.Output)
	}
	if r = call(t, st, "memory_search", map[string]any{"query": `port\s+\d+`, "regex": true}); !strings.Contains(r.Output, "3: Port 8080") {
		t.Fatalf("%q", r.Output)
	}
	if r = call(t, st, "memory_search", map[string]any{"query": "nonexistent-xyz"}); !strings.Contains(r.Output, "No matches") {
		t.Fatalf("%q", r.Output)
	}
	if r = call(t, st, "memory_search", map[string]any{"query": "(", "regex": true}); !r.IsErr {
		t.Fatal("bad regex should error")
	}
	if r = call(t, st, "memory_search", map[string]any{"query": "go", "max_results": float64(1)}); !strings.Contains(r.Output, "1 more matching line") {
		t.Fatalf("%q", r.Output)
	}
}

func TestSizeLimitsAndAtomicWriteLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	st, _ := local.New(dir)
	big := strings.Repeat("x", maxMemoryBytes+1)
	if r := call(t, st, "memory_write", map[string]any{"file": "big", "content": big}); !r.IsErr || !strings.Contains(r.Output, "limited to") {
		t.Fatalf("%q", r.Output)
	}
	call(t, st, "memory_write", map[string]any{"file": "ok", "content": "fine"})
	entries, _ := os.ReadDir(dir + "/memory")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mem-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	// near the cap, append refuses instead of silently growing past it
	call(t, st, "memory_write", map[string]any{"file": "near", "content": strings.Repeat("y", maxMemoryBytes-5)})
	if r := call(t, st, "memory_append", map[string]any{"file": "near", "content": "123456789"}); !r.IsErr {
		t.Fatalf("append past the limit must fail: %q", r.Output)
	}
}
