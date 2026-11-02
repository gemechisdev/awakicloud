package telegram

import (
	"context"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/format"
	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

func (c *Channel) send(ctx context.Context, chatID int64, markdownText string) {
	msgID, err := c.client.SendMessage(ctx, chatID, markdownText, models.ParseModeMarkdown)
	if err != nil {
		log.Warn().Err(err).Msg("telegram markdown send failed, falling back to HTML/plain")
		c.sendLong(ctx, chatID, markdownText)
		_ = msgID
	}
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


// sendAndGetID sends a short status-style message (Markdown preferred) and returns the message ID.
func (c *Channel) sendAndGetID(ctx context.Context, chatID int64, text string) int {
	id, err := c.client.SendMessage(ctx, chatID, text, models.ParseModeMarkdown)
	if err != nil {
		log.Warn().Err(err).Msg("telegram sendAndGetID markdown failed; trying HTML")
		chunks := format.FormatHTMLChunks(text, maxMessageLen)
		if len(chunks) == 0 {
			id, err = c.client.SendMessage(ctx, chatID, text, "")
			if err != nil {
				log.Error().Err(err).Msg("telegram sendAndGetID plain failed")
				return 0
			}
			return id
		}
		id, err = c.client.SendMessage(ctx, chatID, chunks[0].HTML, models.ParseModeHTML)
		if err != nil {
			id, err = c.client.SendMessage(ctx, chatID, chunks[0].Text, "")
			if err != nil {
				log.Error().Err(err).Msg("telegram sendAndGetID failed")
				return 0
			}
		}
	}
	return id
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
			btn := models.InlineKeyboardButton{Text: b.Text}
			if b.URL != "" {
				btn.URL = b.URL
			} else {
				btn.CallbackData = b.CallbackData
			}
			r = append(r, btn)
		}
		rows = append(rows, r)
	}

	// Format body the same way as normal messages (Markdown → Telegram HTML).
	// Use a tighter limit when a keyboard is attached so the HTML text stays
	// under Telegram's 4096-character hard limit after tag/escape expansion.
	limit := maxMessageLen
	if len(rows) > 0 {
		limit = maxKeyboardMessageLen
	}
	chunks := format.FormatHTMLChunks(text, limit)
	if len(chunks) == 0 {
		chunks = []format.FormattedChunk{{HTML: text, Text: text}}
	}
	primary := chunks[0]

	if editMsgID > 0 {
		err := c.client.EditMessageWithKeyboard(ctx, chatID, editMsgID, primary.HTML, models.ParseModeHTML, rows)
		if err != nil {
			log.Warn().Err(err).Int("msg_id", editMsgID).Msg("telegram edit keyboard HTML failed; trying plain")
			err = c.client.EditMessageWithKeyboard(ctx, chatID, editMsgID, primary.Text, "", rows)
			if err != nil {
				log.Warn().Err(err).Int("msg_id", editMsgID).Msg("telegram edit keyboard failed; sending new")
			} else {
				// spill remaining chunks as follow-ups without keyboard
				for _, ch := range chunks[1:] {
					c.sendHTMLChunk(ctx, chatID, ch)
				}
				return
			}
		} else {
			for _, ch := range chunks[1:] {
				c.sendHTMLChunk(ctx, chatID, ch)
			}
			return
		}
	}

	if len(rows) == 0 {
		c.sendLong(ctx, chatID, text)
		return
	}

	_, err := c.client.SendMessageWithKeyboard(ctx, chatID, primary.HTML, models.ParseModeHTML, rows)
	if err != nil {
		log.Warn().Err(err).Msg("telegram inline keyboard HTML failed; trying plain")
		_, err = c.client.SendMessageWithKeyboard(ctx, chatID, primary.Text, "", rows)
		if err != nil {
			log.Warn().Err(err).Msg("telegram inline keyboard failed")
			c.sendLong(ctx, chatID, text+"\n(keyboard failed)")
			return
		}
	}
	for _, ch := range chunks[1:] {
		c.sendHTMLChunk(ctx, chatID, ch)
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