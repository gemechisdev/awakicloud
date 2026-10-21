package telegram

import (
	"context"
	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/format"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

func (c *Channel) send(ctx context.Context, chatID int64, markdownText string) {
	_ = c.sendAndGetID(ctx, chatID, markdownText)
}

func (c *Channel) sendAndGetID(ctx context.Context, chatID int64, markdownText string) int {
	msgID, err := c.client.SendMessage(ctx, chatID, markdownText, models.ParseModeMarkdown)
	if err != nil {
		log.Error().Err(err).Msg("telegram send failed")
		return 0
	}
	return msgID
}

func (c *Channel) sendHTMLChunk(ctx context.Context, chatID int64, chunk format.FormattedChunk) {
	_, err := c.client.SendMessage(ctx, chatID, chunk.HTML, models.ParseModeHTML)
	if err != nil {
		log.Warn().Err(err).Msg("telegram HTML send failed, falling back to plain text")
		if _, err = c.client.SendMessage(ctx, chatID, chunk.Text, ""); err != nil {
			log.Error().Err(err).Msg("telegram plain text send failed")
		}
	}
}

func (c *Channel) sendLong(ctx context.Context, chatID int64, text string) {
	for _, chunk := range format.FormatHTMLChunks(text, maxMessageLen) {
		c.sendHTMLChunk(ctx, chatID, chunk)
	}
}


func (c *Channel) sendKeyboard(ctx context.Context, chatID int64, km channel.KeyboardMessage) {
	var rows [][]models.InlineKeyboardButton
	for _, row := range km.Rows {
		var r []models.InlineKeyboardButton
		for _, b := range row {
			r = append(r, models.InlineKeyboardButton{Text: b.Text, CallbackData: b.CallbackData})
		}
		rows = append(rows, r)
	}
	text := km.Text
	if text == "" {
		text = "Choose:"
	}
	_, err := c.client.SendMessageWithKeyboard(ctx, chatID, text, "", rows)
	if err != nil {
		c.send(ctx, chatID, text+"\n(keyboard failed)")
	}
}
