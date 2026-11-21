package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Upload is one received file.
type Upload struct {
	Name string
	Data []byte
}

// SaveUploads stores received files into folder dirID, replacing same-named
// files. The whole batch is one undo step.
func (m *Manager) SaveUploads(dirID int, ups []Upload) (saved []string, seq int, err error) {
	dir, err := m.Resolve(dirID)
	if err != nil {
		return nil, 0, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, 0, errors.New("the target is not a folder")
	}
	if len(ups) == 0 {
		return nil, 0, errors.New("no files received")
	}
	var ops []undoOp
	fail := func(err error) ([]string, int, error) {
		for i := len(ops) - 1; i >= 0; i-- {
			_ = restore(ops[i])
		}
		return nil, 0, err
	}
	for _, u := range ups {
		name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(u.Name, "\\", "/")))
		if err := ValidateName(name); err != nil {
			return fail(fmt.Errorf("%q: %w", u.Name, err))
		}
		if len(u.Data) > MaxUploadBytes {
			return fail(fmt.Errorf("%s is %s — over the %s limit", code(name), HumanSize(int64(len(u.Data))), HumanSize(MaxUploadBytes)))
		}
		abs := filepath.Join(dir, name)
		if err := m.checkWritePath(abs); err != nil {
			return fail(err)
		}
		op, err := writeTracked(abs, u.Data)
		if err != nil {
			return fail(fmt.Errorf("cannot save %s: %w", code(name), err))
		}
		ops = append(ops, op)
		saved = append(saved, m.Rel(abs))
	}
	return saved, m.pushUndo(ops), nil
}

// EmptyReplacement lets the owner ask for "delete the matches" in a chat that
// cannot send an empty message.
const EmptyReplacement = "<empty>"

// ReplaceInFile replaces every match of find with with, in the text file
// behind id. find starting with "re:" is a Go (RE2) regular expression and
// with may use $1 / ${name}; otherwise matching is literal and case-sensitive.
func (m *Manager) ReplaceInFile(id int, find, with string) (count, seq int, err error) {
	abs, err := m.Resolve(id)
	if err != nil {
		return 0, 0, err
	}
	if find == "" {
		return 0, 0, errors.New("nothing to find")
	}
	if with == EmptyReplacement {
		with = ""
	}
	data, err := m.readEditable(abs)
	if err != nil {
		return 0, 0, err
	}
	text := string(data)
	var out string
	if strings.HasPrefix(find, "re:") {
		re, err := regexp.Compile("(?m)" + strings.TrimPrefix(find, "re:"))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid regular expression: %v", err)
		}
		count = len(re.FindAllStringIndex(text, -1))
		out = re.ReplaceAllString(text, with)
	} else {
		count = strings.Count(text, find)
		out = strings.ReplaceAll(text, find, with)
	}
	if count == 0 {
		return 0, 0, errors.New("no matches found — nothing changed")
	}
	if out == text {
		return count, 0, errors.New("the replacement is identical to the match — nothing changed")
	}
	if err := m.checkWritePath(abs); err != nil {
		return 0, 0, err
	}
	op, err := writeTracked(abs, []byte(out))
	if err != nil {
		return 0, 0, fmt.Errorf("write failed: %w", err)
	}
	return count, m.pushUndo([]undoOp{op}), nil
}

// readEditable reads a text file that is small enough to edit safely.
func (m *Manager) readEditable(abs string) ([]byte, error) {
	st, err := os.Stat(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	if st.IsDir() {
		return nil, errors.New("that is a folder")
	}
	if st.Size() > MaxEditBytes {
		return nil, fmt.Errorf("file is %s — too large to edit from chat (limit %s)", HumanSize(st.Size()), HumanSize(MaxEditBytes))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	head := data
	if len(head) > binarySniff {
		head = head[:binarySniff]
	}
	if looksBinary(head) {
		return nil, errors.New("binary file — cannot edit as text")
	}
	return data, nil
}
