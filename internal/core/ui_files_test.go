package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/permission"
	"github.com/gemechischala/awaki/internal/transport"
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

func TestFilesUploadReplacePatchUndoFlows(t *testing.T) {
	h, root := filesHarness(t)
	h.tap("files:d:0")

	// upload: a document message is consumed by the wizard
	h.tap("files:upl:0")
	if w := h.e.ui.get(); w == nil || w.Kind != "files_upload" {
		t.Fatalf("upload wizard not open: %+v", w)
	}
	h.say2(channel.Message{Text: "[File: notes.txt]\n```\nhello\n```", Attachments: []transport.Attachment{{Filename: "notes.txt", Data: []byte("hello\n")}}})
	if b, err := os.ReadFile(filepath.Join(root, "notes.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("upload not saved: %q %v", b, err)
	}
	undo := callbackFor(t, h.ch.last(), "files:undo:")
	h.tap(undo)
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("undo should remove the uploaded file")
	}

	// a text message while waiting for a file keeps the prompt open
	h.tap("files:upl:0")
	h.tap("just words")
	if h.e.ui.get() == nil || !strings.Contains(h.ch.last().Text, "need a file") {
		t.Fatalf("expected reminder:\n%s", h.ch.last().Text)
	}
	h.tap("files:d:0") // cancel via navigation

	// find & replace (two steps)
	h.tap("files:f:" + idOf(t, h, "main.go"))
	h.tap(callbackFor(t, h.ch.last(), "files:repl:"))
	h.tap("package main")
	if w := h.e.ui.get(); w == nil || w.Kind != "files_replace_with" || w.Draft["find"] != "package main" {
		t.Fatalf("second step not armed: %+v", w)
	}
	h.tap("package app")
	if b, _ := os.ReadFile(filepath.Join(root, "main.go")); string(b) != "package app\n" {
		t.Fatalf("replace failed: %q", b)
	}

	// patch via a .patch attachment
	h.tap("files:d:0")
	h.tap("files:patch:0")
	patch := "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-package app\n+package core\n"
	h.say2(channel.Message{Text: "[File: x.patch]", Attachments: []transport.Attachment{{Filename: "x.patch", Data: []byte(patch)}}})
	if b, _ := os.ReadFile(filepath.Join(root, "main.go")); string(b) != "package core\n" {
		t.Fatalf("patch failed: %q", b)
	}
	if !strings.Contains(h.ch.last().Text, "Patched 1 file") {
		t.Fatalf("summary:\n%s", h.ch.last().Text)
	}
}

func idOf(t *testing.T, h *harness, name string) string {
	t.Helper()
	for _, b := range buttons(h.ch.last()) {
		if strings.Contains(b.Text, name) && strings.HasPrefix(b.CallbackData, "files:f:") {
			return strings.TrimPrefix(b.CallbackData, "files:f:")
		}
	}
	t.Fatalf("no button for %s: %+v", name, buttons(h.ch.last()))
	return ""
}

// say2 delivers a full message (text + attachments) through the UI router.
func (h *harness) say2(in channel.Message) {
	h.t.Helper()
	m := h.msg()
	m.Text, m.Attachments = in.Text, in.Attachments
	if !h.e.handleUICallback(h.ctx, m) {
		h.t.Fatalf("message %q was not handled", in.Text)
	}
}

func TestFilesLineEditAndManualEditFlows(t *testing.T) {
	h, root := filesHarness(t)
	os.WriteFile(filepath.Join(root, "t.txt"), []byte("a\nb\nc\n"), 0o644)
	h.tap("files:d:0")
	h.tap("files:f:" + idOf(t, h, "t.txt"))

	// numbered view toggle
	h.tap(callbackFor(t, h.ch.last(), "files:fn:"))
	if !strings.Contains(h.ch.last().Text, "1│ a") {
		t.Fatalf("numbers:\n%s", h.ch.last().Text)
	}
	// line edit via prompt
	h.tap(callbackFor(t, h.ch.last(), "files:lines:"))
	if w := h.e.ui.get(); w == nil || w.Kind != "files_lines" {
		t.Fatalf("wizard: %+v", w)
	}
	h.tap("@2\n-b\n+BEE")
	if b, _ := os.ReadFile(filepath.Join(root, "t.txt")); string(b) != "a\nBEE\nc\n" {
		t.Fatalf("line edit failed: %q", b)
	}
	// manual edit: file is sent, then the edited document replaces it
	h.tap(callbackFor(t, h.ch.last(), "files:getdoc:"))
	if len(h.rep.files) != 1 || h.rep.files[0] != filepath.Join(root, "t.txt") {
		t.Fatalf("file not sent for editing: %v", h.rep.files)
	}
	h.tap("some words") // text instead of a document: keep waiting
	if h.e.ui.get() == nil || !strings.Contains(h.ch.last().Text, "Send the edited file") {
		t.Fatalf("reminder:\n%s", h.ch.last().Text)
	}
	if len(h.rep.files) != 1 {
		t.Fatal("reminder must not resend the file")
	}
	h.say2(channel.Message{Text: "[File: whatever.txt]", Attachments: []transport.Attachment{{Filename: "whatever.txt", Data: []byte("edited\n")}}})
	if b, _ := os.ReadFile(filepath.Join(root, "t.txt")); string(b) != "edited\n" {
		t.Fatalf("manual edit failed: %q", b)
	}
	h.tap(callbackFor(t, h.ch.last(), "files:undo:"))
	if b, _ := os.ReadFile(filepath.Join(root, "t.txt")); string(b) != "a\nBEE\nc\n" {
		t.Fatalf("undo failed: %q", b)
	}
	// pasted code block with a path header
	h.tap("files:patch:0")
	h.tap("File: gen/x.go\n```go\npackage gen\n```")
	if b, _ := os.ReadFile(filepath.Join(root, "gen", "x.go")); string(b) != "package gen\n" {
		t.Fatalf("path-header paste failed: %q", b)
	}
}

func TestFilesSearchZipRecentFlows(t *testing.T) {
	h, root := filesHarness(t)
	os.WriteFile(filepath.Join(root, "src", "needle.txt"), []byte("find the NEEDLE here\n"), 0o644)
	h.tap("files:d:0")

	// search prompt → results → open the hit
	h.tap("files:find:0")
	if w := h.e.ui.get(); w == nil || w.Kind != "files_search" {
		t.Fatalf("search wizard: %+v", w)
	}
	h.tap("needle")
	if h.e.ui.get() != nil {
		t.Fatal("wizard must close after the search")
	}
	if !strings.Contains(h.ch.last().Text, "src/needle.txt") {
		t.Fatalf("results:\n%s", h.ch.last().Text)
	}
	h.tap(callbackFor(t, h.ch.last(), "files:fn:")) // content hit opens numbered view
	if !strings.Contains(h.ch.last().Text, "NEEDLE") {
		t.Fatalf("hit view:\n%s", h.ch.last().Text)
	}

	// recent
	h.tap("files:rec:0")
	if !strings.Contains(h.ch.last().Text, "needle.txt") {
		t.Fatalf("recent:\n%s", h.ch.last().Text)
	}

	// zip: sent, and the temp file is gone afterwards
	h.tap("files:d:0")
	h.tap(callbackFor(t, h.ch.last(), "files:zip:"))
	var zipPath string
	for _, f := range h.rep.files {
		if strings.HasSuffix(f, ".zip") {
			zipPath = f
		}
	}
	if zipPath == "" {
		t.Fatalf("zip was not sent: %v", h.rep.files)
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("temp zip must be deleted after sending: %v", err)
	}
}

func TestFilesPlanModeKeepsReadOnlyFeaturesAndBlocksWrites(t *testing.T) {
	h, root := filesHarness(t)
	store := permission.NewStore(t.TempDir())
	h.e.permStore = store
	if err := store.SetDefault(permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	h.tap("files:d:0")
	for _, b := range buttons(h.ch.last()) {
		for _, bad := range []string{"➕", "Upload", "patch", "Rename"} {
			if strings.Contains(b.Text, bad) {
				t.Fatalf("plan mode must hide %q", b.Text)
			}
		}
	}
	// read-only: search works
	h.tap("files:find:0")
	h.tap("main")
	if !strings.Contains(h.ch.last().Text, "main.go") {
		t.Fatalf("search in plan mode:\n%s", h.ch.last().Text)
	}
	// writes are refused even if someone forges the callback
	h.tap("files:newfile:0")
	if h.e.ui.get() != nil {
		t.Fatal("no prompt may open in plan mode")
	}
	if !strings.Contains(h.ch.last().Text, "disabled in plan mode") {
		t.Fatalf("refusal:\n%s", h.ch.last().Text)
	}
	h.tap("files:d:0")
	h.tap("files:f:" + idOf(t, h, "main.go"))
	h.tap("files:del:" + strings.TrimPrefix(callbackFor(t, h.ch.last(), "files:copy:"), "files:copy:"))
	if _, err := os.Stat(filepath.Join(root, "main.go")); err != nil {
		t.Fatal("forged delete must not work in plan mode")
	}
}
