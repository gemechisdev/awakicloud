package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gemechischala/awaki/internal/project"
)

// ProjectBlobStore keeps zip bytes on Telegram and pointers in SQLite.
type ProjectBlobStore struct {
	Store  *Store
	API    TGAPI
	ChatID int64
}

func (p *ProjectBlobStore) SaveArchive(ctx context.Context, meta project.Meta, zip []byte) error {
	caption := fmt.Sprintf("awaki:project;slug=%s;hash=%s;ver=%d", meta.Slug, meta.Hash, meta.UpdatedAt.Unix())
	msgID, fileID, err := p.API.SendDocument(ctx, p.ChatID, meta.Slug+".zip", zip, caption)
	if err != nil {
		return err
	}
	mj, _ := json.Marshal(meta)
	return p.Store.UpsertBlob(BlobRef{
		Kind:      "project",
		Key:       meta.Slug,
		FileID:    fileID,
		MessageID: msgID,
		Hash:      meta.Hash,
		Size:      int64(len(zip)),
		MetaJSON:  string(mj),
	})
}

func (p *ProjectBlobStore) LoadArchive(ctx context.Context, slug string) (project.Meta, []byte, error) {
	ref, ok := p.Store.GetBlob("project", slug)
	if !ok || ref.FileID == "" {
		return project.Meta{}, nil, fmt.Errorf("project %q not in db blob index", slug)
	}
	data, err := p.API.DownloadFile(ctx, ref.FileID)
	if err != nil {
		return project.Meta{}, nil, err
	}
	var meta project.Meta
	_ = json.Unmarshal([]byte(ref.MetaJSON), &meta)
	if meta.Slug == "" {
		meta.Slug = slug
		meta.Name = slug
		meta.Hash = ref.Hash
	}
	return meta, data, nil
}

func (p *ProjectBlobStore) ListArchives(ctx context.Context) ([]project.Meta, error) {
	_ = ctx
	refs := p.Store.ListBlobs("project")
	out := make([]project.Meta, 0, len(refs))
	for _, r := range refs {
		var meta project.Meta
		_ = json.Unmarshal([]byte(r.MetaJSON), &meta)
		if meta.Slug == "" {
			meta.Slug = r.Key
			meta.Name = r.Key
			meta.Hash = r.Hash
			meta.UpdatedAt = time.Now()
		}
		out = append(out, meta)
	}
	return out, nil
}
