package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// api is a thin wrapper around go-telegram/bot for storage operations.
type api struct {
	b *bot.Bot
}

func newAPI(token string) (*api, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("telegram storage bot: %w", err)
	}
	return &api{b: b}, nil
}

func (a *api) sendText(ctx context.Context, chatID int64, text string) (int, error) {
	sent, err := a.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		return 0, err
	}
	return sent.ID, nil
}

func (a *api) editText(ctx context.Context, chatID int64, messageID int, text string) error {
	_, err := a.b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	})
	return err
}

func (a *api) sendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) (msgID int, fileID string, err error) {
	sent, err := a.b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID: chatID,
		Document: &models.InputFileUpload{
			Filename: filename,
			Data:     bytes.NewReader(data),
		},
		Caption: caption,
	})
	if err != nil {
		return 0, "", err
	}
	if sent.Document != nil {
		fileID = sent.Document.FileID
	}
	return sent.ID, fileID, nil
}

func (a *api) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := a.b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    chatID,
		MessageID: messageID,
	})
	return err
}

func (a *api) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	file, err := a.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	url := a.b.FileDownloadLink(file)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (a *api) pinMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := a.b.PinChatMessage(ctx, &bot.PinChatMessageParams{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: true,
	})
	return err
}

func (a *api) getChat(ctx context.Context, chatID int64) (*models.ChatFullInfo, error) {
	return a.b.GetChat(ctx, &bot.GetChatParams{ChatID: chatID})
}
