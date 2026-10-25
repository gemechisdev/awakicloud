package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot/models"
)

// DBSyncAPI adapts the bot storage API for sqlite.Syncer.
type DBSyncAPI struct {
	a *api
}

func NewDBSyncAPI(token string) (*DBSyncAPI, error) {
	a, err := newAPI(token)
	if err != nil {
		return nil, err
	}
	return &DBSyncAPI{a: a}, nil
}

func (d *DBSyncAPI) SendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) (int, string, error) {
	return d.a.sendDocument(ctx, chatID, filename, data, caption)
}

func (d *DBSyncAPI) PinMessage(ctx context.Context, chatID int64, messageID int) error {
	return d.a.pinMessage(ctx, chatID, messageID)
}

func (d *DBSyncAPI) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	return d.a.downloadFile(ctx, fileID)
}

func (d *DBSyncAPI) SendOrEditText(ctx context.Context, chatID int64, text string, editMsgID int) (int, error) {
	if editMsgID > 0 {
		if err := d.a.editText(ctx, chatID, editMsgID, text); err == nil {
			return editMsgID, nil
		}
	}
	return d.a.sendText(ctx, chatID, text)
}

func (d *DBSyncAPI) GetChatPinnedFile(ctx context.Context, chatID int64) (caption, fileID string, msgID int, err error) {
	chat, err := d.a.getChat(ctx, chatID)
	if err != nil {
		return "", "", 0, err
	}
	if chat == nil || chat.PinnedMessage == nil {
		return "", "", 0, fmt.Errorf("no pinned message")
	}
	pm := chat.PinnedMessage
	cap := pm.Caption
	if cap == "" {
		cap = pm.Text
	}
	if pm.Document != nil {
		fileID = pm.Document.FileID
	}
	return cap, fileID, pm.ID, nil
}

// Ensure models import used if needed
var _ = models.Message{}
var _ = strings.Builder{}
