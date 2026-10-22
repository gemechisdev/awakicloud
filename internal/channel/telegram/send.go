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
	c.sendOrEditKeyboard(ctx, chatID, 0, km)
}

func (c *Channel) sendOrEditKeyboard(ctx context.Context, chatID int64, editMsgID int, km channel.KeyboardMessage) {
	text := km.Text
	if text == "" {
		text = "Choose:"
	}

	// Reply keyboard only when explicitly provided (e.g. /start) — new message.
	if len(km.ReplyKeyboard) > 0 && editMsgID == 0 {
		if _, err := c.client.SendMessageWithReplyKeyboard(ctx, chatID, "Navigation", km.ReplyKeyboard); err != nil {
			log.Warn().Err(err).Msg("telegram reply keyboard failed")
		}
	}

	var rows [][]models.InlineKeyboardButton
	for _, row := range km.Rows {
		var r []models.InlineKeyboardButton
		for _, b := range row {
			r = append(r, models.InlineKeyboardButton{Text: b.Text, CallbackData: b.CallbackData})
		}
		rows = append(rows, r)
	}

	if editMsgID > 0 {
		err := c.client.EditMessageWithKeyboard(ctx, chatID, editMsgID, text, "", rows)
		if err != nil {
			log.Warn().Err(err).Int("msg_id", editMsgID).Msg("telegram edit keyboard failed; sending new")
			// fall through to send new
		} else {
			return
		}
	}

	if len(rows) == 0 {
		c.send(ctx, chatID, text)
		return
	}
	_, err := c.client.SendMessageWithKeyboard(ctx, chatID, text, "", rows)
	if err != nil {
		log.Warn().Err(err).Msg("telegram inline keyboard failed")
		c.send(ctx, chatID, text+"\n(keyboard failed)")
	}
}

// SendKeyboardToOwner implements core.keyboardSender for UI screens.
func (c *Channel) SendKeyboardToOwner(ctx context.Context, km channel.KeyboardMessage) {
	c.sendOrEditKeyboard(ctx, c.ownerID, 0, km)
}

// SendOrEditKeyboardToOwner edits the callback message when editMsgID > 0.
func (c *Channel) SendOrEditKeyboardToOwner(ctx context.Context, editMsgID int, km channel.KeyboardMessage) {
	c.sendOrEditKeyboard(ctx, c.ownerID, editMsgID, km)
}
