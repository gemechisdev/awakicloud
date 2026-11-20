package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Wizard kinds used by the engine for the name prompts.
const (
	WizardNewDir  = "files_newdir"
	WizardNewFile = "files_newfile"
	WizardRename  = "files_rename"
)

// Prompt asks the engine to collect one line of text from the owner.
type Prompt struct {
	Kind       string // Wizard* constant
	ID         int    // folder id (new*) or item id (rename)
	CancelData string // callback_data of the Cancel button
}

// ValidateName checks one path segment.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	if name == "." || name == ".." {
		return errors.New("that name is not allowed")
	}
	if len(name) > 255 {
		return errors.New("name is too long (max 255 bytes)")
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return errors.New("name cannot contain slashes or control characters")
		}
	}
	return nil
}

// splitName turns "a/b/c.go" into validated segments.
func splitName(name string) ([]string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Trim(name, "/")
	if name == "" {
		return nil, errors.New("name is empty")
	}
	segs := strings.Split(name, "/")
	if len(segs) > MaxNameDepth {
		return nil, fmt.Errorf("path is too deep (max %d levels)", MaxNameDepth)
	}
	for _, s := range segs {
		if err := ValidateName(strings.TrimSpace(s)); err != nil {
			return nil, err
		}
	}
	for i := range segs {
		segs[i] = strings.TrimSpace(segs[i])
	}
	return segs, nil
}

// Create makes a folder or file called name (which may be a relative path
// such as "pkg/util.go"; missing parents are created) inside folder parentID.
// It never overwrites and never follows a symlink out of the workspace.
func (m *Manager) Create(parentID int, name string, dir bool, content string) (id int, rel string, err error) {
	parent, err := m.Resolve(parentID)
	if err != nil {
		return 0, "", err
	}
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return 0, "", errors.New("the target is not a folder")
	}
	segs, err := splitName(name)
	if err != nil {
		return 0, "", err
	}
	if len(content) > MaxCreateBytes {
		return 0, "", fmt.Errorf("initial content is too large (max %d KB)", MaxCreateBytes>>10)
	}
	root := m.Root()
	cur := parent
	for i, seg := range segs {
		last := i == len(segs)-1
		next := filepath.Join(cur, seg)
		if _, lerr := os.Lstat(next); lerr == nil {
			if last {
				return 0, "", fmt.Errorf("%s already exists", code(m.Rel(next)))
			}
			if err := checkInside(root, next); err != nil {
				return 0, "", err
			}
			if st, err := os.Stat(next); err != nil || !st.IsDir() {
				return 0, "", fmt.Errorf("%s is a file, not a folder", code(m.Rel(next)))
			}
			cur = next
			continue
		} else if !os.IsNotExist(lerr) {
			return 0, "", lerr
		}
		if !last || dir {
			if err := os.Mkdir(next, 0o755); err != nil {
				return 0, "", fmt.Errorf("cannot create folder: %w", err)
			}
			cur = next
			continue
		}
		body := content
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		f, err := os.OpenFile(next, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return 0, "", fmt.Errorf("cannot create file: %w", err)
		}
		_, werr := f.WriteString(body)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return 0, "", fmt.Errorf("cannot write file: %w", werr)
		}
		cur = next
	}
	return m.touch(cur), m.Rel(cur), nil
}

// Rename renames the item behind id within its folder. The id stays valid.
func (m *Manager) Rename(id int, newName string) (rel string, err error) {
	abs, err := m.Resolve(id)
	if err != nil {
		return "", err
	}
	if abs == m.Root() {
		return "", errors.New("the workspace root cannot be renamed")
	}
	newName = strings.TrimSpace(newName)
	if err := ValidateName(newName); err != nil {
		return "", err
	}
	target := filepath.Join(filepath.Dir(abs), newName)
	if target == abs {
		return "", errors.New("that is already its name")
	}
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%s already exists", code(m.Rel(target)))
	}
	if err := os.Rename(abs, target); err != nil {
		return "", fmt.Errorf("rename failed: %w", err)
	}
	m.mu.Lock()
	delete(m.ids, abs)
	m.nodes[id] = target
	m.ids[target] = id
	m.mu.Unlock()
	return m.Rel(target), nil
}

// prompt builds the name prompt for a KindNewDir / KindNewFile / KindRename.
func (m *Manager) prompt(a Action) Result {
	abs, err := m.Resolve(a.ID)
	if err != nil {
		return errResult(err)
	}
	switch a.Kind {
	case KindRename:
		if abs == m.Root() {
			return errResult(errors.New("the workspace root cannot be renamed"))
		}
		self := m.touch(abs)
		cancel := cbFile(self, 0)
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			cancel = cbDir(self, 0)
		}
		return Result{
			Screen: promptScreen("✏️ **Rename** "+code(m.Rel(abs))+"\n\nSend the new name (just the name, no slashes).", cancel),
			Prompt: &Prompt{Kind: WizardRename, ID: self, CancelData: cancel},
		}
	default:
		if st, err := os.Stat(abs); err == nil && !st.IsDir() {
			abs = filepath.Dir(abs)
		}
		self := m.touch(abs)
		cancel := cbDir(self, 0)
		var text, kind string
		if a.Kind == KindNewDir {
			kind = WizardNewDir
			text = "📁 **New folder** in " + code(m.Display(abs)) + "\n\nSend the folder name (e.g. `docs` or `a/b/c`)."
		} else {
			kind = WizardNewFile
			text = "📄 **New file** in " + code(m.Display(abs)) + "\n\nSend the file name (e.g. `util.go` or `pkg/util.go`).\nOptional: put the initial content on the lines after the name."
		}
		return Result{Screen: promptScreen(text, cancel), Prompt: &Prompt{Kind: kind, ID: self, CancelData: cancel}}
	}
}

func promptScreen(text, cancel string) Screen {
	return Screen{Text: text, Rows: [][]Button{{{Text: "❌ Cancel", CallbackData: cancel}}}}
}

// Submit completes a prompt with the owner's text. On a fixable mistake
// (bad or taken name) Retry is set so the engine keeps the prompt open.
func (m *Manager) Submit(kind string, id int, text string, opts Options) Result {
	if !opts.AllowWrite {
		opts.Notice = "🔒 Creating and renaming are disabled in plan mode."
		return Result{Screen: m.File(id, 0, opts)}
	}
	retry := func(err error) Result {
		var a Action
		switch kind {
		case WizardNewDir:
			a = Action{Kind: KindNewDir, ID: id}
		case WizardNewFile:
			a = Action{Kind: KindNewFile, ID: id}
		default:
			a = Action{Kind: KindRename, ID: id}
		}
		p := m.prompt(a)
		p.Screen.Text = "⚠️ " + err.Error() + "\n\n" + p.Screen.Text
		p.Retry = true
		return p
	}
	switch kind {
	case WizardNewDir, WizardNewFile:
		name, content := text, ""
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			name, content = text[:i], text[i+1:]
		}
		isDir := kind == WizardNewDir
		if isDir {
			content = ""
		}
		newID, rel, err := m.Create(id, name, isDir, content)
		if err != nil {
			return retry(err)
		}
		opts.Notice = "✅ Created " + code(rel)
		return Result{Screen: m.File(newID, 0, opts)}
	case WizardRename:
		rel, err := m.Rename(id, text)
		if err != nil {
			return retry(err)
		}
		opts.Notice = "✅ Renamed to " + code(rel)
		return Result{Screen: m.File(id, 0, opts)}
	}
	return errResult(fmt.Errorf("unknown prompt %q", kind))
}
