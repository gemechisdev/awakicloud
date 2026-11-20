package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/workspace"
)

func filesHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package src\n"), 0o644)
	if err := workspace.SetRoot(root); err != nil {
		t.Fatal(err)
	}
	return h, root
}

func TestFilesCommandOpensRoot(t *testing.T) {
	h, _ := filesHarness(t)
	c := h.cmd(channel.CmdFiles, "")
	if !c.Result.Handled {
		t.Fatal("/files must mark the command handled")
	}
	km := h.ch.last()
	if !strings.Contains(km.Text, "📂") || !strings.Contains(km.Text, "main.go") {
		t.Fatalf("unexpected root screen:\n%s", km.Text)
	}
	if !hasCallback(km, "files:f:") || !hasCallback(km, "files:d:") || !hasCallback(km, "ui:home") {
		t.Fatalf("missing navigation buttons: %+v", buttons(km))
	}
	h.assertCallbackLimits()
}

func TestFilesCommandPathHintAndJail(t *testing.T) {
	h, _ := filesHarness(t)
	h.cmd(channel.CmdFiles, "src")
	if !strings.Contains(h.ch.last().Text, "a.go") {
		t.Fatalf("path hint ignored:\n%s", h.ch.last().Text)
	}
	h.cmd(channel.CmdFiles, "../../etc")
	if !strings.Contains(h.ch.last().Text, "outside the workspace") {
		t.Fatalf("escape must be refused:\n%s", h.ch.last().Text)
	}
}

func TestFilesCallbacksBrowseOpenDelete(t *testing.T) {
	h, root := filesHarness(t)
	h.tap("files:d:0")
	open := callbackFor(t, h.ch.last(), "files:f:") // main.go (only file at root)
	h.tap(open)
	if !strings.Contains(h.ch.last().Text, "package main") {
		t.Fatalf("file view:\n%s", h.ch.last().Text)
	}
	ask := callbackFor(t, h.ch.last(), "files:delask:")
	h.tap(ask)
	if _, err := os.Stat(filepath.Join(root, "main.go")); err != nil {
		t.Fatal("delete confirmation must not delete")
	}
	h.tap(callbackFor(t, h.ch.last(), "files:del:"))
	if _, err := os.Stat(filepath.Join(root, "main.go")); !os.IsNotExist(err) {
		t.Fatal("confirmed delete should remove the file")
	}
	if !strings.Contains(h.ch.last().Text, "Deleted") {
		t.Fatalf("post-delete screen:\n%s", h.ch.last().Text)
	}
}

func TestFilesCallbackNotSwallowedByWizard(t *testing.T) {
	h, _ := filesHarness(t)
	h.e.ui.set(&uiWizard{Kind: "add_provider", Step: "name"})
	h.tap("files:d:0")
	if h.e.ui.get() == nil {
		t.Fatal("a files tap must not cancel or consume the wizard")
	}
	if !strings.Contains(h.ch.last().Text, "📂") {
		t.Fatalf("files screen not shown:\n%s", h.ch.last().Text)
	}
}

func TestFilesStaleAndMalformedCallbacks(t *testing.T) {
	h, _ := filesHarness(t)
	h.tap("files:f:99999")
	if !strings.Contains(h.ch.last().Text, "expired") {
		t.Fatalf("stale id:\n%s", h.ch.last().Text)
	}
	n := len(h.ch.all)
	h.tap("files:bogus") // consumed silently
	if len(h.ch.all) != n {
		t.Fatal("malformed callback must not send anything")
	}
}
