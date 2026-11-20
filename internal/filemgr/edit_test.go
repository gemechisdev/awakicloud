package filemgr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var rw = Options{AllowDelete: true, AllowWrite: true}

func TestGridTwoColumnsPaddedAndFullPage(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 31; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file%02d.txt", i)), []byte("x"), 0o644)
	}
	m := New()
	m.SetRoot(root)
	s := m.Dir(0, 0, rw)
	files := 0
	for _, row := range s.Rows {
		if strings.HasPrefix(row[0].Text, "📄") {
			files++
			if len(row) != GridCols {
				t.Fatalf("grid row has %d buttons, want %d: %+v", len(row), GridCols, row)
			}
		}
	}
	if files != PageSize/GridCols {
		t.Fatalf("%d grid rows, want %d", files, PageSize/GridCols)
	}
	// second page has 7 files: 3 full rows + 1 padded row
	p2 := m.Dir(0, 1, rw)
	var last []Button
	for _, row := range p2.Rows {
		if strings.HasPrefix(row[0].Text, "📄") {
			last = row
		}
	}
	if len(last) != 2 || last[1].CallbackData != "ui:noop" || strings.TrimSpace(last[1].Text) == "" && last[1].Text == "" {
		t.Fatalf("odd tail must be padded with a blank no-op: %+v", last)
	}
}

func TestGridSingleEntryNotPadded(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "only.txt"), []byte("x"), 0o644)
	m := New()
	m.SetRoot(root)
	for _, row := range m.Dir(0, 0, rw).Rows {
		if strings.HasPrefix(row[0].Text, "📄") && len(row) != 1 {
			t.Fatalf("single entry should stay one wide button: %+v", row)
		}
	}
}

func TestLongNamesTruncatedInGrid(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a_really_really_long_file_name.go"), []byte("x"), 0o644)
	m := New()
	m.SetRoot(root)
	b := findBtn(t, m.Dir(0, 0, rw), "📄 a_really")
	if n := len([]rune(b.Text)); n > GridLabelRunes+3 {
		t.Fatalf("label too long (%d): %q", n, b.Text)
	}
	if !strings.HasSuffix(b.Text, "…") {
		t.Fatalf("truncation marker missing: %q", b.Text)
	}
}

// ------------------------------------------------------------ create

func TestCreateFileFolderNestedAndContent(t *testing.T) {
	m, root := newTree(t)
	id, rel, err := m.Create(0, "docs/api/readme.md", false, "# API")
	if err != nil || rel != "docs/api/readme.md" {
		t.Fatalf("Create = %d, %q, %v", id, rel, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "docs", "api", "readme.md"))
	if string(b) != "# API\n" {
		t.Fatalf("content = %q", b)
	}
	if _, _, err := m.Create(0, "empty.txt", false, ""); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "empty.txt")); fi == nil || fi.Size() != 0 {
		t.Fatal("empty file expected")
	}
	if _, _, err := m.Create(0, "a/b/c", true, ""); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, "a", "b", "c")); err != nil || !st.IsDir() {
		t.Fatal("nested folder missing")
	}
	// creating inside an existing subfolder id
	src := act(t, findBtn(t, m.Dir(0, 0, rw), "📁 src").CallbackData)
	if _, rel, err := m.Create(src.ID, "new.go", false, "package src"); err != nil || rel != "src/new.go" {
		t.Fatalf("in subfolder: %q %v", rel, err)
	}
}

func TestCreateNeverOverwritesOrEscapes(t *testing.T) {
	m, root := newTree(t)
	if _, _, err := m.Create(0, "main.go", false, "x"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite must be refused: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "main.go")); !strings.HasPrefix(string(b), "package main") {
		t.Fatal("existing file was modified")
	}
	for _, bad := range []string{"", "  ", "..", "../x", "a/../../x", "a/./b", ".", "x\x00y", strings.Repeat("a", 300), "a/b/c/d/e/f/g/h/i/j"} {
		if _, _, err := m.Create(0, bad, false, ""); err == nil {
			t.Errorf("Create(%q) should fail", bad)
		}
	}
	// file in the way of a folder path
	if _, _, err := m.Create(0, "main.go/inner.txt", false, ""); err == nil {
		t.Fatal("file used as a folder must fail")
	}
	// symlinked folder pointing outside
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "out")); err == nil {
		if _, _, err := m.Create(0, "out/pwn.txt", false, "x"); err == nil {
			t.Fatal("must not create through an escaping symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); err == nil {
			t.Fatal("file was created outside the workspace")
		}
	}
	// oversized initial content
	if _, _, err := m.Create(0, "big.txt", false, strings.Repeat("x", MaxCreateBytes+1)); err == nil {
		t.Fatal("oversized content must be refused")
	}
}

// ------------------------------------------------------------ rename

func TestRenameKeepsIDAndGuards(t *testing.T) {
	m, root := newTree(t)
	id := act(t, findBtn(t, m.Dir(0, 0, rw), "📄 go.mod").CallbackData).ID
	rel, err := m.Rename(id, "go.work")
	if err != nil || rel != "go.work" {
		t.Fatalf("Rename = %q, %v", rel, err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); !os.IsNotExist(err) {
		t.Fatal("old name should be gone")
	}
	if p, err := m.Resolve(id); err != nil || filepath.Base(p) != "go.work" {
		t.Fatalf("id must follow the rename: %q %v", p, err)
	}
	if _, err := m.Rename(id, "main.go"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision must be refused: %v", err)
	}
	for _, bad := range []string{"", "a/b", "..", "x\\y"} {
		if _, err := m.Rename(id, bad); err == nil {
			t.Errorf("Rename(%q) should fail", bad)
		}
	}
	if _, err := m.Rename(0, "x"); err == nil {
		t.Fatal("root rename must fail")
	}
	if _, err := m.Rename(id, "go.work"); err == nil {
		t.Fatal("same-name rename must fail")
	}
}

func TestRenameFolder(t *testing.T) {
	m, root := newTree(t)
	src := act(t, findBtn(t, m.Dir(0, 0, rw), "📁 src").CallbackData)
	if _, err := m.Rename(src.ID, "lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "lib", "a.go")); err != nil {
		t.Fatal("contents must move with the folder")
	}
	if s := m.Dir(src.ID, 0, rw); !strings.Contains(s.Text, "`a.go`") {
		t.Fatalf("renamed folder id still browsable:\n%s", s.Text)
	}
}

// ------------------------------------------------------- prompt flow

func TestPromptFlowCreateFile(t *testing.T) {
	m, root := newTree(t)
	res := m.Handle(Action{Kind: KindNewFile, ID: 0}, rw)
	if res.Prompt == nil || res.Prompt.Kind != WizardNewFile {
		t.Fatalf("want new-file prompt, got %+v", res)
	}
	if !strings.Contains(res.Screen.Text, "New file") {
		t.Fatalf("prompt text:\n%s", res.Screen.Text)
	}
	cancel := act(t, findBtn(t, res.Screen, "Cancel").CallbackData)
	if cancel.Kind != KindDir || cancel.ID != 0 {
		t.Fatalf("cancel returns to the folder: %+v", cancel)
	}
	out := m.Submit(res.Prompt.Kind, res.Prompt.ID, "hello.py\nprint('hi')\nprint('bye')", rw)
	if out.Retry || out.Prompt != nil {
		t.Fatalf("success must not retry: %+v", out)
	}
	if !strings.Contains(out.Screen.Text, "Created `hello.py`") || !strings.Contains(out.Screen.Text, "```python") {
		t.Fatalf("success screen:\n%s", out.Screen.Text)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "hello.py")); string(b) != "print('hi')\nprint('bye')\n" {
		t.Fatalf("content = %q", b)
	}
	// Taken name → retry keeps the prompt open with the reason.
	bad := m.Submit(WizardNewFile, 0, "hello.py", rw)
	if !bad.Retry || bad.Prompt == nil || !strings.Contains(bad.Screen.Text, "already exists") {
		t.Fatalf("expected retry: %+v\n%s", bad.Retry, bad.Screen.Text)
	}
}

func TestPromptFlowNewFolderAndRename(t *testing.T) {
	m, root := newTree(t)
	out := m.Submit(WizardNewDir, 0, "assets", rw)
	if !strings.Contains(out.Screen.Text, "Created `assets`") {
		t.Fatalf("got:\n%s", out.Screen.Text)
	}
	if st, err := os.Stat(filepath.Join(root, "assets")); err != nil || !st.IsDir() {
		t.Fatal("folder not created")
	}
	// folder prompt ignores content lines
	m.Submit(WizardNewDir, 0, "more\nignored", rw)
	if _, err := os.Stat(filepath.Join(root, "more")); err != nil {
		t.Fatal("folder with trailing lines should still be created")
	}
	id := act(t, findBtn(t, m.Dir(0, 0, rw), "📄 main.go").CallbackData).ID
	res := m.Handle(Action{Kind: KindRename, ID: id}, rw)
	if res.Prompt == nil || res.Prompt.Kind != WizardRename || act(t, findBtn(t, res.Screen, "Cancel").CallbackData).Kind != KindFile {
		t.Fatalf("rename prompt: %+v", res)
	}
	done := m.Submit(WizardRename, id, "app.go", rw)
	if !strings.Contains(done.Screen.Text, "Renamed to `app.go`") {
		t.Fatalf("got:\n%s", done.Screen.Text)
	}
}

func TestWriteActionsDisabledInPlanMode(t *testing.T) {
	m, root := newTree(t)
	plan := Options{}
	d := m.Dir(0, 0, plan)
	f := m.Open("main.go", plan).Screen
	for _, s := range []Screen{d, f} {
		for _, b := range allButtons(s) {
			for _, bad := range []string{"➕", "Rename", "Delete"} {
				if strings.Contains(b.Text, bad) {
					t.Fatalf("plan mode must hide %q", b.Text)
				}
			}
		}
	}
	id := act(t, findBtn(t, f, "Reload").CallbackData).ID
	for _, k := range []string{KindNewDir, KindNewFile, KindRename} {
		r := m.Handle(Action{Kind: k, ID: id}, plan)
		if r.Prompt != nil || !strings.Contains(r.Screen.Text, "disabled in plan mode") {
			t.Fatalf("%s must be refused: %+v", k, r)
		}
	}
	if r := m.Submit(WizardNewFile, 0, "x.txt", plan); !strings.Contains(r.Screen.Text, "disabled") {
		t.Fatal("Submit must refuse in plan mode")
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("file created despite plan mode")
	}
}

func TestWriteButtonsShownWhenAllowed(t *testing.T) {
	m, _ := newTree(t)
	d := m.Dir(0, 0, rw)
	findBtn(t, d, "➕ File")
	findBtn(t, d, "➕ Folder")
	for _, b := range allButtons(d) {
		if strings.Contains(b.Text, "Rename") {
			t.Fatal("root folder must not offer Rename")
		}
	}
	src := act(t, findBtn(t, d, "📁 src").CallbackData)
	findBtn(t, m.Dir(src.ID, 0, rw), "✏️ Rename")
	findBtn(t, m.Open("main.go", rw).Screen, "✏️ Rename")
}

// -------------------------------------------------------------- send

func TestSendFile(t *testing.T) {
	m, root := newTree(t)
	s := m.Open("main.go", rw).Screen
	send := act(t, findBtn(t, s, "📤 Send file").CallbackData)
	r := m.Handle(send, rw)
	if !r.NoScreen || r.SendPath != filepath.Join(root, "main.go") || r.SendCaption != "main.go" {
		t.Fatalf("send result = %+v", r)
	}
	os.WriteFile(filepath.Join(root, "empty.bin"), nil, 0o644)
	id := m.touch(filepath.Join(root, "empty.bin"))
	if r := m.Handle(Action{Kind: KindSend, ID: id}, rw); r.SendPath != "" || !strings.Contains(r.Screen.Text, "empty") {
		t.Fatalf("empty file: %+v", r)
	}
	src := act(t, findBtn(t, m.Dir(0, 0, rw), "📁 src").CallbackData)
	if r := m.Handle(Action{Kind: KindSend, ID: src.ID}, rw); r.SendPath != "" {
		t.Fatal("folders cannot be sent")
	}
	if r := m.Handle(Action{Kind: KindSend, ID: 777}, rw); !strings.Contains(r.Screen.Text, "expired") {
		t.Fatal("stale send id")
	}
}

func TestLargeFileHintAndBinarySendable(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "big.log"), []byte(strings.Repeat("line of log text\n", 20000)), 0o644)
	os.WriteFile(filepath.Join(root, "x.bin"), []byte{0, 1, 2}, 0o644)
	m := New()
	m.SetRoot(root)
	s := m.Open("big.log", rw).Screen
	if !strings.Contains(s.Text, "Large file") {
		t.Fatalf("large-file hint missing:\n%s", firstLines(s.Text, 4))
	}
	if n := len([]rune(s.Text)); n > 8000 {
		t.Fatalf("screen is %d runes, must stay within the safe rich limit", n)
	}
	findBtn(t, m.Open("x.bin", rw).Screen, "📤 Send file")
}

func TestLangExtensions(t *testing.T) {
	cases := map[string]string{
		"a.kt": "kotlin", "b.swift": "swift", "c.cs": "csharp", "d.vue": "html", "e.diff": "diff", "f.patch": "diff",
		".env": "ini", "g.tf": "hcl", "h.gql": "graphql", "Dockerfile": "dockerfile", "i.scss": "scss", "j.PS1": "powershell",
	}
	for in, want := range cases {
		if got := Lang(in); got != want {
			t.Errorf("Lang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewCallbacksParse(t *testing.T) {
	for _, d := range []string{"files:send:3", "files:newdir:0", "files:newfile:12", "files:ren:9"} {
		if _, err := ParseCallback(d); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
	if _, err := ParseCallback("files:ren:9:1"); err == nil {
		t.Error("extra segment must fail")
	}
}
