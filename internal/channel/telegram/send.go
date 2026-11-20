package telegram

import (
	"context"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/format"
	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)


// richHTMLFromMarkdown converts markdown to Telegram Rich HTML with structural
// tags (<p>, <br/>, lists). Prefer this over native markdown for multi-line text.
func richHTMLFromMarkdown(md string) string {
	md = rich.ConvertMathMarkdown(md)
	html := format.FormatRichHTML(md)
	if html == "" {
		html = rich.MarkdownToRichHTML(md)
	}
	return rich.EnsureSafeLength(html)
}

// sendRichPreferHTML sends multi-line/structured text as rich HTML first so
// CommonMark soft-breaks are not collapsed to spaces (native markdown mode).
func (c *Channel) sendRichPreferHTML(ctx context.Context, chatID int64, text string) bool {
	isRTL := rich.NeedsRTL(text)
	multiline := rich.IsRichWorthy(text) || strings.Contains(text, "\n")
	if multiline {
		html := richHTMLFromMarkdown(text)
		if _, err := c.client.SendRichMessage(ctx, chatID, "html", html, isRTL, false); err == nil {
			return true
		} else {
			log.Warn().Err(err).Msg("telegram rich HTML send failed, trying rich markdown")
		}
		if _, err := c.client.SendRichMessage(ctx, chatID, "markdown", text, isRTL, false); err == nil {
			return true
		} else {
			log.Warn().Err(err).Msg("telegram rich markdown send failed")
		}
		return false
	}
	if _, err := c.client.SendRichMessage(ctx, chatID, "markdown", text, isRTL, false); err == nil {
		return true
	} else {
		log.Warn().Err(err).Msg("telegram rich markdown send failed, trying rich HTML")
	}
	html := richHTMLFromMarkdown(text)
	if _, err := c.client.SendRichMessage(ctx, chatID, "html", html, isRTL, false); err == nil {
		return true
	} else {
		log.Warn().Err(err).Msg("telegram rich HTML send failed")
	}
	return false
}


func (c *Channel) send(ctx context.Context, chatID int64, markdownText string) {
	if c.sendRichPreferHTML(ctx, chatID, markdownText) {
		return
	}
	c.sendLongClassic(ctx, chatID, markdownText)
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

// sendLongThreaded is like sendLong but the first chunk is sent with
// ReplyParameters pointing at replyToID, so the response visually threads
// under the user's message. Continuations are unthreaded.
func (c *Channel) sendLongThreaded(ctx context.Context, chatID, replyToID int64, text string) {
	// Rich path has no reply_to on all clients; still prefer rich for content quality.
	if c.sendRichPreferHTML(ctx, chatID, text) {
		return
	}
	for i, chunk := range format.FormatHTMLChunks(text, maxMessageLen) {
		if i == 0 {
			c.sendThreadedHTMLChunk(ctx, chatID, replyToID, chunk)
		} else {
			c.sendHTMLChunk(ctx, chatID, chunk)
		}
	}
}

func (c *Channel) sendThreadedHTMLChunk(ctx context.Context, chatID, replyToID int64, chunk format.FormattedChunk) {
	if err := c.client.SendMessageThreaded(ctx, chatID, chunk.HTML, models.ParseModeHTML, replyToID); err != nil {
		log.Warn().Err(err).Msg("telegram threaded HTML send failed, falling back to plain")
		if err := c.client.SendMessageThreaded(ctx, chatID, chunk.Text, "", replyToID); err != nil {
			log.Error().Err(err).Msg("telegram threaded plain send failed")
		}
	}
}

// sendLong is the general multi-chunk sender — prefers a single rich message.
func (c *Channel) sendLong(ctx context.Context, chatID int64, text string) {
	if c.sendRichPreferHTML(ctx, chatID, text) {
		return
	}
	c.sendLongClassic(ctx, chatID, text)
}

// sendLongClassic splits into classic 4k HTML chunks (last-resort fallback).
func (c *Channel) sendLongClassic(ctx context.Context, chatID int64, text string) {
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

// SendRichScreen sends a Telegram Rich Message screen with styled buttons.
// km.Text is treated as Markdown and converted to Rich HTML before send so
// **bold**, `code`, lists, etc. render correctly (never as raw asterisks).
func (c *Channel) SendRichScreen(ctx context.Context, km channel.KeyboardMessage, editMsgID int) (int, error) {
	return c.sendRichScreen(ctx, c.ownerID, km.Text, km.Rows, editMsgID)
}

// SendOrEditKeyboardToOwner edits the callback message when editMsgID > 0.
func (c *Channel) SendOrEditKeyboardToOwner(ctx context.Context, editMsgID int, km channel.KeyboardMessage) {
	c.sendOrEditKeyboard(ctx, c.ownerID, editMsgID, km)
}

// markdownToTelegramHTML converts Markdown UI copy into Telegram HTML.
// Uses FormatRichHTML so paragraphs, soft breaks, and lists keep structure.
func markdownToTelegramHTML(md string) (html string, plain string) {
	md = rich.ConvertMathMarkdown(md)
	html = format.FormatRichHTML(md)
	if html == "" {
		html = rich.MarkdownToRichHTML(md)
	}
	if html == "" {
		html = rich.EscapeHTML(md)
	}
	// Plain text companion for classic fallback (strip tags roughly via chunks).
	chunks := format.FormatHTMLChunks(md, 0)
	if len(chunks) == 0 {
		return html, md
	}
	var pb strings.Builder
	for i, ch := range chunks {
		if i > 0 {
			pb.WriteString("\n")
		}
		pb.WriteString(ch.Text)
	}
	return html, pb.String()
}

// sendRichScreen sends a Rich Message with HTML body and rich <tg-button-row>
// buttons instead of legacy inline keyboards. Used for UI screens that benefit
// from rich styling (tables, drawers, styled buttons).
func (c *Channel) sendRichScreen(ctx context.Context, chatID int64, markdownText string, rows [][]channel.Button, editMsgID int) (int, error) {
	// ALWAYS convert Markdown → HTML first. Sending markdown as contentType
	// "html" leaves **asterisks** visible (see /start welcome screen bug).
	bodyHTML, bodyPlain := markdownToTelegramHTML(markdownText)
	if bodyHTML == "" {
		bodyHTML = rich.EscapeHTML(markdownText)
		bodyPlain = markdownText
	}

	// Convert channel.Button rows to <tg-button-row> HTML
	var buttonHTML strings.Builder
	for _, row := range rows {
		var btns []map[string]string
		for _, btn := range row {
			spec := map[string]string{
				"type":  "callback_data",
				"style": "primary",
				"data":  btn.CallbackData,
				"label": btn.Text,
			}
			if btn.URL != "" {
				spec["type"] = "url"
				spec["style"] = "primary"
				spec["url"] = btn.URL
				delete(spec, "data")
			}
			// Tap-to-copy (rich messages only). Classic keyboards ignore CopyText
			// and use CallbackData, so the button still works as a fallback.
			if btn.CopyText != "" && btn.URL == "" {
				spec["type"] = "copy_text"
				spec["text"] = btn.CopyText
				delete(spec, "data")
			}
			btns = append(btns, spec)
		}
		if len(btns) > 0 {
			buttonHTML.WriteString(rich.ButtonRow(btns, "center"))
			buttonHTML.WriteString("\n")
		}
	}

	body := bodyHTML
	if buttonHTML.Len() > 0 {
		body += "\n" + buttonHTML.String()
	}
	body = rich.EnsureSafeLength(body)
	isRTL := rich.NeedsRTL(markdownText)

	if rich.IsRichWorthy(markdownText) || len(rows) > 0 {
		if editMsgID > 0 {
			err := c.client.EditRichMessage(ctx, chatID, editMsgID, "html", body, isRTL)
			if err != nil {
				log.Warn().Err(err).Int("msg_id", editMsgID).Msg("telegram: edit rich screen failed, falling back")
				return c.sendScreenPlain(ctx, chatID, bodyHTML, rows, editMsgID)
			}
			return editMsgID, nil
		}
		id, err := c.client.SendRichMessage(ctx, chatID, "html", body, isRTL, false)
		if err != nil {
			log.Warn().Err(err).Msg("telegram: send rich screen failed, falling back")
			return c.sendScreenPlain(ctx, chatID, bodyHTML, rows, editMsgID)
		}
		return id, nil
	}

	// Short content — classic inline keyboard with already-converted HTML
	_ = bodyPlain
	if editMsgID > 0 {
		_, err := c.sendScreenPlain(ctx, chatID, bodyHTML, rows, editMsgID)
		return editMsgID, err
	}
	return c.sendScreenPlain(ctx, chatID, bodyHTML, rows, editMsgID)
}

// sendScreenPlain falls back to legacy inline keyboard for short messages.
func (c *Channel) sendScreenPlain(ctx context.Context, chatID int64, html string, rows [][]channel.Button, editMsgID int) (int, error) {
	var kbRows [][]models.InlineKeyboardButton
	for _, row := range rows {
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
		kbRows = append(kbRows, r)
	}

	var err error
	if editMsgID > 0 {
		if len(kbRows) > 0 {
			err = c.client.EditMessageWithKeyboard(ctx, chatID, editMsgID, html, models.ParseModeHTML, kbRows)
		} else {
			err = c.client.EditMessage(ctx, chatID, editMsgID, html, models.ParseModeHTML)
		}
		if err != nil {
			log.Warn().Err(err).Int("msg_id", editMsgID).Msg("telegram: edit plain screen failed")
		}
		return editMsgID, err
	}

	if len(kbRows) == 0 {
		// No buttons — just send text
		msgID, err := c.client.SendMessage(ctx, chatID, html, models.ParseModeHTML)
		if err != nil {
			return 0, err
		}
		return msgID, nil
	}

	sentID, err := c.client.SendMessageWithKeyboard(ctx, chatID, html, models.ParseModeHTML, kbRows)
	if err != nil {
		return 0, err
	}
	return sentID, nil
}
