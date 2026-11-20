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

func TestReplyKeyboardHasFilesAndOpensBrowser(t *testing.T) {
	h, _ := filesHarness(t)
	h.cmd(channel.CmdStart, "")
	flat := ""
	for _, r := range h.ch.last().ReplyKeyboard {
		flat += strings.Join(r, "|") + "\n"
	}
	if !strings.Contains(flat, navFiles) {
		t.Fatalf("reply keyboard lacks Files: %q", flat)
	}
	h.tap(navFiles)
	if !strings.Contains(h.ch.last().Text, "📂") || !hasCallback(h.ch.last(), "files:f:") {
		t.Fatalf("Files label should open the browser:\n%s", h.ch.last().Text)
	}
}

func TestCurrentChatAndProjectScreensOfferFiles(t *testing.T) {
	h := newHarness(t)
	slug := h.mkProject("Shop")
	h.tap("ui:nc:" + slug)
	h.say("hello")
	h.cmd(channel.CmdChat, "")
	if !hasCallback(h.ch.last(), "files:d:0") {
		t.Fatalf("current chat card lacks Files: %+v", buttons(h.ch.last()))
	}
	h.tap("ui:pd:" + slug)
	if !hasCallback(h.ch.last(), "files:d:0") {
		t.Fatalf("open project's detail lacks Files: %+v", buttons(h.ch.last()))
	}
	other := h.mkProject("Other")
	h.tap("ui:pd:" + other) // not the active workspace → no Files button
	if hasCallback(h.ch.last(), "files:d:0") {
		t.Fatal("Files must only be offered for the open project")
	}
}

func TestFilesCreateRenameViaPrompts(t *testing.T) {
	h, root := filesHarness(t)
	h.tap("files:d:0")
	h.tap("files:newfile:0")
	if w := h.e.ui.get(); w == nil || w.Kind != "files_newfile" {
		t.Fatalf("prompt wizard not open: %+v", w)
	}
	h.tap("notes/todo.md\n- [ ] ship it") // owner's reply is consumed by the wizard
	if b, err := os.ReadFile(filepath.Join(root, "notes", "todo.md")); err != nil || string(b) != "- [ ] ship it\n" {
		t.Fatalf("file not created: %q %v", b, err)
	}
	if h.e.ui.get() != nil {
		t.Fatal("wizard must close after success")
	}
	// a taken name keeps the prompt open
	h.tap("files:newfile:0")
	h.tap("main.go")
	if w := h.e.ui.get(); w == nil || !strings.Contains(h.ch.last().Text, "already exists") {
		t.Fatalf("expected retry prompt:\n%s", h.ch.last().Text)
	}
	// Cancel button (a files callback) closes the prompt
	h.tap(callbackFor(t, h.ch.last(), "files:d:"))
	if h.e.ui.get() != nil {
		t.Fatal("cancel must clear the prompt")
	}
	// rename via prompt
	h.tap("files:d:0")
	open := callbackFor(t, h.ch.last(), "files:f:")
	h.tap(open)
	h.tap(callbackFor(t, h.ch.last(), "files:ren:"))
	h.tap("renamed.go")
	entries, _ := os.ReadDir(root)
	found := false
	for _, e := range entries {
		if e.Name() == "renamed.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rename did not happen: %v", entries)
	}
}

func TestFilesSendUploadsFile(t *testing.T) {
	h, root := filesHarness(t)
	h.tap("files:d:0")
	h.tap(callbackFor(t, h.ch.last(), "files:f:"))
	h.tap(callbackFor(t, h.ch.last(), "files:send:"))
	if len(h.rep.files) != 1 || h.rep.files[0] != filepath.Join(root, "main.go") {
		t.Fatalf("uploaded = %v", h.rep.files)
	}
}
