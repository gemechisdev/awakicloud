package filemgr

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// MaxEditBytes bounds files the editing features (replace, patch) will
	// read into memory and keep an undo copy of.
	MaxEditBytes = 5 << 20
	// MaxUploadBytes bounds one uploaded file.
	MaxUploadBytes = 20 << 20

	maxUndoGroups = 20
)

// undoOp is the before/after state of one path touched by an edit.
type undoOp struct {
	abs     string
	existed bool
	old     []byte
	mode    os.FileMode
	newSum  [32]byte // sha256 of what we wrote
	deleted bool     // the edit removed the file
	noUndo  bool     // old content too large to keep
}

type undoGroup struct {
	seq int
	ops []undoOp
}

// pushUndo records one edit (possibly several files) and returns its seq, or 0
// when the edit cannot be undone.
func (m *Manager) pushUndo(ops []undoOp) int {
	if len(ops) == 0 {
		return 0
	}
	for _, o := range ops {
		if o.noUndo {
			return 0
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undoSeq++
	m.undo = append(m.undo, undoGroup{seq: m.undoSeq, ops: ops})
	if len(m.undo) > maxUndoGroups {
		m.undo = m.undo[len(m.undo)-maxUndoGroups:]
	}
	return m.undoSeq
}

// writeTracked atomically writes data to abs (creating it if needed) and
// returns the undo record. abs must already have been jail-checked.
func writeTracked(abs string, data []byte) (undoOp, error) {
	op := undoOp{abs: abs, mode: 0o644, newSum: sha256.Sum256(data)}
	if st, err := os.Lstat(abs); err == nil {
		if !st.Mode().IsRegular() {
			return op, errors.New("not a regular file")
		}
		op.existed = true
		op.mode = st.Mode().Perm()
		if st.Size() > MaxEditBytes {
			op.noUndo = true
		} else if old, err := os.ReadFile(abs); err == nil {
			op.old = old
		} else {
			return op, err
		}
	}
	if err := atomicWrite(abs, data, op.mode); err != nil {
		return op, err
	}
	return op, nil
}

func atomicWrite(abs string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".awaki-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, abs)
	}
	if werr != nil {
		os.Remove(name)
		return werr
	}
	return nil
}

// deleteTracked removes abs and returns the undo record.
func deleteTracked(abs string) (undoOp, error) {
	st, err := os.Lstat(abs)
	if err != nil {
		return undoOp{}, err
	}
	op := undoOp{abs: abs, existed: true, deleted: true, mode: st.Mode().Perm()}
	if st.Size() > MaxEditBytes {
		op.noUndo = true
	} else if old, err := os.ReadFile(abs); err == nil {
		op.old = old
	} else {
		return op, err
	}
	return op, os.Remove(abs)
}

// restore reverts one op.
func restore(op undoOp) error {
	if op.existed {
		return atomicWrite(op.abs, op.old, op.mode)
	}
	return os.Remove(op.abs)
}

// Undo reverts the edit with the given seq. It refuses when any touched file
// was changed again since, so it can never clobber newer work.
func (m *Manager) Undo(seq int) (restored []string, err error) {
	m.mu.Lock()
	idx := -1
	for i, g := range m.undo {
		if g.seq == seq {
			idx = i
		}
	}
	var g undoGroup
	if idx >= 0 {
		g = m.undo[idx]
	}
	m.mu.Unlock()
	if idx < 0 {
		return nil, errors.New("nothing to undo — it was already undone or is too old")
	}
	for _, op := range g.ops {
		cur, rerr := os.ReadFile(op.abs)
		switch {
		case op.deleted:
			if rerr == nil {
				return nil, fmt.Errorf("%s was recreated since — not undoing", code(m.Rel(op.abs)))
			}
		case rerr != nil:
			return nil, fmt.Errorf("%s changed since — not undoing", code(m.Rel(op.abs)))
		default:
			if sum := sha256.Sum256(cur); !bytes.Equal(sum[:], op.newSum[:]) {
				return nil, fmt.Errorf("%s was modified since — not undoing", code(m.Rel(op.abs)))
			}
		}
	}
	for i := len(g.ops) - 1; i >= 0; i-- {
		if err := restore(g.ops[i]); err != nil {
			return restored, fmt.Errorf("undo failed on %s: %w", code(m.Rel(g.ops[i].abs)), err)
		}
		restored = append(restored, m.Rel(g.ops[i].abs))
	}
	m.mu.Lock()
	m.undo = append(m.undo[:idx:idx], m.undo[idx+1:]...)
	m.mu.Unlock()
	return restored, nil
}

// checkWritePath jails a path about to be written: its folder must be inside
// the workspace and, if it exists, it must be a regular file inside too.
func (m *Manager) checkWritePath(abs string) error {
	root := m.Root()
	if !Within(root, abs) {
		return ErrOutside
	}
	// Jail the nearest ancestor that already exists (new folders are created
	// by us, so they cannot be symlinks).
	anc := filepath.Dir(abs)
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		next := filepath.Dir(anc)
		if next == anc || !Within(root, next) {
			break
		}
		anc = next
	}
	if err := checkInside(root, anc); err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err == nil {
		if err := checkInside(root, abs); err != nil {
			return err
		}
	}
	return nil
}
