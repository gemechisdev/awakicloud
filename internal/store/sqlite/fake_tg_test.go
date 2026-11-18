package sqlite

import (
	"context"
	"fmt"
	"sync"
)

// fakeTG is an in-memory stand-in for the Telegram document API.
//
// The sqlite store's most dangerous surface is the cloud round-trip: a snapshot
// that uploads but cannot be restored means silent total data loss on the next
// container restart, because /data is not a persistent volume. Testing that path
// needs the real bytes to come back, so this fake stores documents verbatim and
// returns the same bytes from DownloadFile.
//
// It also records call order, because the recovery path depends on it: the
// pinned file must resolve to the manifest, and the manifest must point at the
// newest db. A fake that only stores end state could not catch a reordered
// write, so tests assert on the sequence too.
type fakeTG struct {
	mu sync.Mutex

	docs     map[string][]byte // file id -> bytes
	caps     map[string]string // file id -> caption
	byName   map[string]string // filename of the most recent upload -> file id
	byMsgID  map[int]string    // message id -> file id
	pinned   string
	pinnedAt int
	nextID   int
	calls    []string

	// failUpload makes SendDocument fail for this exact filename.
	failUpload string
	// failDownload makes DownloadFile fail for this exact file id.
	failDownload string
}

func newFakeTG() *fakeTG {
	return &fakeTG{
		docs:    map[string][]byte{},
		caps:    map[string]string{},
		byName:  map[string]string{},
		byMsgID: map[int]string{},
	}
}

func (f *fakeTG) record(format string, a ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeTG) SendDocument(_ context.Context, _ int64, filename string, data []byte, caption string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("send %s (%d bytes)", filename, len(data))
	if f.failUpload == filename {
		return 0, "", fmt.Errorf("fake: upload of %s rejected", filename)
	}
	f.nextID++
	id := f.nextID
	fileID := fmt.Sprintf("file-%d:%s", id, filename)
	// Copy the payload: the caller reuses its buffer, and an aliasing fake would
	// hide exactly the corruption a real network hop cannot produce.
	cp := append([]byte(nil), data...)
	f.docs[fileID] = cp
	f.caps[fileID] = caption
	f.byName[filename] = fileID
	f.byMsgID[id] = fileID
	return id, fileID, nil
}

func (f *fakeTG) PinMessage(_ context.Context, _ int64, messageID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("pin msg %d", messageID)
	if fid, ok := f.byMsgID[messageID]; ok {
		f.pinned = fid
		f.pinnedAt = messageID
	}
	return nil
}

func (f *fakeTG) GetChatPinnedFile(context.Context, int64) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("getpinned")
	if f.pinned == "" {
		return "", "", 0, fmt.Errorf("fake: nothing is pinned")
	}
	return f.caps[f.pinned], f.pinned, f.pinnedAt, nil
}

func (f *fakeTG) DownloadFile(_ context.Context, fileID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("download")
	if f.failDownload == fileID {
		return nil, fmt.Errorf("fake: download of %s rejected", fileID)
	}
	data, ok := f.docs[fileID]
	if !ok {
		return nil, fmt.Errorf("fake: unknown file id %q", fileID)
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeTG) SendOrEditText(_ context.Context, _ int64, text string, _ int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("text")
	f.nextID++
	f.caps[fmt.Sprintf("text-%d", f.nextID)] = text
	return f.nextID, nil
}

// docCount reports how many documents were uploaded.
func (f *fakeTG) docCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.docs)
}

// doc returns the bytes last uploaded under a filename.
func (f *fakeTG) doc(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	fid, ok := f.byName[name]
	if !ok {
		return nil
	}
	return f.docs[fid]
}

// callLog returns a copy of the recorded call sequence.
func (f *fakeTG) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
