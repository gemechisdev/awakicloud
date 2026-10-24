package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/project"
)

// ProjectBlob adapts the Telegram document store into project.BlobStore.
type ProjectBlob struct {
	s *Store
}

func NewProjectBlob(s *Store) *ProjectBlob {
	return &ProjectBlob{s: s}
}

func (p *ProjectBlob) SaveArchive(ctx context.Context, meta project.Meta, zip []byte) error {
	caption := fmt.Sprintf("awaki:project;id=%s;slug=%s;chat=%s;hash=%s;name=%s;ver=%d",
		meta.ID, meta.Slug, meta.ChatID, meta.Hash, escapeCaption(meta.Name), meta.UpdatedAt.Unix())
	// delete previous message if indexed
	if e, ok := p.s.idx.get("project", meta.Slug); ok && e.MessageID != 0 {
		_ = p.s.api.deleteMessage(ctx, p.s.chatID, e.MessageID)
	}
	msgID, fileID, err := p.s.api.sendDocument(ctx, p.s.chatID, meta.Slug+".zip", zip, caption)
	if err != nil {
		return err
	}
	return p.s.idx.set("project", meta.Slug, indexEntry{
		MessageID: msgID,
		FileID:    fileID,
		Kind:      "project",
		Key:       meta.Slug,
	})
}

func (p *ProjectBlob) LoadArchive(ctx context.Context, slug string) (project.Meta, []byte, error) {
	e, ok := p.s.idx.get("project", slug)
	if !ok || e.FileID == "" {
		return project.Meta{}, nil, fmt.Errorf("project %q not in telegram index", slug)
	}
	data, err := p.s.api.downloadFile(ctx, e.FileID)
	if err != nil {
		return project.Meta{}, nil, err
	}
	meta := project.Meta{Slug: slug, Name: slug}
	// meta sidecar optional — try local index only
	return meta, data, nil
}

func (p *ProjectBlob) ListArchives(ctx context.Context) ([]project.Meta, error) {
	entries := p.s.idx.list("project")
	out := make([]project.Meta, 0, len(entries))
	for _, e := range entries {
		out = append(out, project.Meta{
			Slug:      e.Key,
			Name:      e.Key,
			UpdatedAt: time.Now(), // unknown without caption parse
		})
	}
	_ = ctx
	return out, nil
}

func escapeCaption(s string) string {
	s = strings.ReplaceAll(s, ";", "%3B")
	s = strings.ReplaceAll(s, " ", "%20")
	return s
}

// Marshal meta for debugging.
func metaJSON(m project.Meta) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func parseUnix(s string) time.Time {
	n, _ := strconv.ParseInt(s, 10, 64)
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
