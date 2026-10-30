package transport

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	types "github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/utils"
	"github.com/gemechischala/awaki/internal/workspace"

	"github.com/rs/zerolog/log"
)

// ProcessAttachments saves every uploaded file as-is and tells the agent the
// paths. No special-casing for zip/tar.gz/docx/pdf — the agent decides what to
// do with each file (extract, open, merge, etc.).
func ProcessAttachments(chatID, task string, attachments []Attachment) ([]types.ContentBlock, func()) {
	if len(attachments) == 0 {
		return []types.ContentBlock{types.TextBlock(task)}, func() {}
	}

	root := workspace.Root()
	// Prefer writing into the active workspace when available so project mode
	// already has the files in the durable tree; otherwise use a temp dir.
	baseDir := root
	tmpOnly := false
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "awaki-uploads",
			fmt.Sprintf("%s-%d", chatID, time.Now().UnixNano()))
		tmpOnly = true
	}
	uploadDir := filepath.Join(baseDir, ".awaki-uploads")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Error().Err(err).Msg("failed to create upload dir")
		return []types.ContentBlock{types.TextBlock(task)}, func() {}
	}

	var imageBlocks []types.ContentBlock
	var paths []string
	nameCount := map[string]int{}

	for _, att := range attachments {
		name := utils.UniqueName(att.Filename, nameCount)
		fpath := filepath.Join(uploadDir, name)
		if err := os.WriteFile(fpath, att.Data, 0644); err != nil {
			log.Error().Err(err).Str("path", fpath).Msg("failed to save attachment")
			continue
		}
		paths = append(paths, fpath)

		if strings.HasPrefix(att.MimeType, "image/") {
			b64 := base64.StdEncoding.EncodeToString(att.Data)
			imageBlocks = append(imageBlocks, types.ImageBlock(att.MimeType, b64))
		}
	}

	pathList := strings.Join(paths, "\n- ")
	info := fmt.Sprintf(
		"[Attached files saved as-is (agent decides what to do with each):\n- %s]\n\n"+
			"Persistence note: only files under an active project (/data/projects/{slug}/) "+
			"are durable and get synced. If this work needs more than a couple of files, "+
			"promote/upgrade to a project first.",
		pathList,
	)
	text := task
	if text != "" {
		text += "\n\n" + info
	} else {
		text = info
	}

	blocks := make([]types.ContentBlock, 0, 1+len(imageBlocks))
	blocks = append(blocks, types.TextBlock(text))
	blocks = append(blocks, imageBlocks...)

	cleanup := func() {
		if tmpOnly {
			os.RemoveAll(baseDir)
		}
		// When uploads live inside a project workspace, leave them for the agent.
	}
	return blocks, cleanup
}
