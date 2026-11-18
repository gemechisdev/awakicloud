package telegram

import (
	"context"
	"fmt"
	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/transport"
	"strings"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

type mediaExtractor struct {
	check   func(*models.Message) bool
	process func(c *Channel, ctx context.Context, msg *models.Message) ([]transport.Attachment, error)
}

var mediaExtractors = []mediaExtractor{
	{
		check: func(m *models.Message) bool { return m.Animation != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processAnimation(ctx, m.Animation)
		},
	},
	{
		check: func(m *models.Message) bool { return m.Document != nil && m.Animation == nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processDocument(ctx, m.Document)
		},
	},
	{
		check: func(m *models.Message) bool { return len(m.Photo) > 0 },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processPhoto(ctx, m.Photo)
		},
	},
	{
		check: func(m *models.Message) bool { return m.Audio != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processAudio(ctx, m.Audio)
		},
	},
	{
		check: func(m *models.Message) bool { return m.Voice != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processVoice(ctx, m.Voice)
		},
	},
	{
		check: func(m *models.Message) bool { return m.Video != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processVideo(ctx, m.Video)
		},
	},
	{
		check: func(m *models.Message) bool { return m.VideoNote != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processVideoNote(ctx, m.VideoNote)
		},
	},
	{
		check: func(m *models.Message) bool { return m.Sticker != nil },
		process: func(c *Channel, ctx context.Context, m *models.Message) ([]transport.Attachment, error) {
			return c.processSticker(ctx, m.Sticker)
		},
	},
}

func (c *Channel) convertAndDispatch(ctx context.Context, msgs []*models.Message) {
	if len(msgs) == 0 {
		return
	}

	chatID := msgs[0].Chat.ID
	var textParts []string
	var attachments []transport.Attachment

	for _, msg := range msgs {
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text != "" {
			textParts = append(textParts, text)
		}

		for _, ex := range mediaExtractors {
			if ex.check(msg) {
				atts, err := ex.process(c, ctx, msg)
				if err != nil {
					log.Error().Err(err).Msg("failed to process media")
				} else {
					attachments = append(attachments, atts...)
				}
			}
		}

		if msg.Venue != nil {
			textParts = append(textParts, fmt.Sprintf("[Venue: %s, %s — lat=%.6f, lon=%.6f]",
				msg.Venue.Title, msg.Venue.Address,
				msg.Venue.Location.Latitude, msg.Venue.Location.Longitude))
		} else if msg.Location != nil {
			textParts = append(textParts, fmt.Sprintf("[Location: lat=%.6f, lon=%.6f]",
				msg.Location.Latitude, msg.Location.Longitude))
		}

		if msg.Contact != nil {
			textParts = append(textParts, fmt.Sprintf("[Contact: %s %s, phone: %s]",
				msg.Contact.FirstName, msg.Contact.LastName, msg.Contact.PhoneNumber))
		}

		if msg.Poll != nil {
			opts := make([]string, len(msg.Poll.Options))
			for i, o := range msg.Poll.Options {
				opts[i] = o.Text
			}
			textParts = append(textParts, fmt.Sprintf("[Poll: %s — options: %s]",
				msg.Poll.Question, strings.Join(opts, ", ")))
		}

		if msg.Dice != nil {
			textParts = append(textParts, fmt.Sprintf("[Dice: %s = %d]",
				msg.Dice.Emoji, msg.Dice.Value))
		}
	}

	text := strings.Join(textParts, "\n\n")

	var fileTexts []string
	for _, att := range attachments {
		if !strings.HasPrefix(att.MimeType, "image/") && isTextMIME(att.MimeType) {
			fileTexts = append(fileTexts, fmt.Sprintf("[File: %s]\n```\n%s\n```", att.Filename, string(att.Data)))
		}
	}

	if len(fileTexts) > 0 {
		filesStr := strings.Join(fileTexts, "\n\n")
		if text != "" {
			text = filesStr + "\n\n" + text
		} else {
			text = filesStr
		}
	}

	if text == "" && len(attachments) == 0 {
		return
	}

	if text == "" && len(attachments) > 0 {
		text = "What's in these files?"
	}

	// Default replier; commands always use this one (no thread on /help etc.).
	var reply transport.Replier = c.newReplier(chatID)

	// Prepend a "Replying to:" block when the user quoted or replied to
	// something. Only the first message of a media group carries the quote.
	if quoted := extractQuote(msgs[0]); quoted != "" {
		text = formatQuotedBlock(quoted) + "\n\n" + text
		// Thread the bot's own first response back under the user's message.
		// The Replier only applies the thread to its first SendText; any
		// continuation chunks are sent unthreaded.
		reply = c.NewReplierForThread(chatID, int64(msgs[0].ID))
	}

	c.handler(ctx, channel.Message{
		Text:        text,
		Attachments: attachments,
		Reply:       reply,
	})
}

// formatQuotedBlock renders the quoted text as a markdown blockquote
// with a "Replying to:" prefix, truncated to keep the agent's context small.
// Truncation preserves the most recent part of the quoted text (where the
// user's pointer usually is) — if a 20-line log was quoted, the agent
// should see the tail, not the head.
func formatQuotedBlock(quoted string) string {
	const maxRunes = 4000 // generous but bounded; full replies to long code
	runes := []rune(quoted)
	if len(runes) > maxRunes {
		quoted = string(runes[len(runes)-maxRunes:])
	}
	var b strings.Builder
	b.WriteString("> Replying to:\n")
	for _, line := range strings.Split(quoted, "\n") {
		if line == "" {
			b.WriteString("> \n")
		} else {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// extractQuote builds a human-readable quoted block from a message's
// reply/quote context. Empty string when the message has no usable reply.
//
// Telegram exposes three shapes of "replying to something":
//
//  1. ReplyToMessage *Message — same-chat reply to a visible message. The
//     replied-to message's full Text/Caption is available.
//  2. ExternalReply   *ExternalReplyInfo — reply to a message in *another*
//     chat (forwarded or via a channel post). The message's own text is not
//     present on the reply info; only the origin is. Fall back to a
//     "[reply to {origin}] " placeholder.
//  3. Quote           *TextQuote — user long-pressed and selected a slice of
//     the replied-to message's text. This is the exact substring they meant;
//     it should take precedence over the full message text.
//
// We prefer the quote over the full message text when both are present,
// because it's what the user deliberately highlighted.
func extractQuote(msg *models.Message) string {
	if msg == nil {
		return ""
	}
	var quoted string

	// 1) Same-chat reply.
	if r := msg.ReplyToMessage; r != nil {
		if r.Text != "" {
			quoted = strings.TrimSpace(r.Text)
		} else if r.Caption != "" {
			quoted = strings.TrimSpace(r.Caption)
		}
	}

	// 2) External reply: no inline text, just the origin.
	if msg.ExternalReply != nil && quoted == "" {
		quoted = externalReplyLabel(msg.ExternalReply)
	}

	// 3) TextQuote overrides: it's the exact substring the user highlighted.
	if msg.Quote != nil && msg.Quote.Text != "" {
		quoted = strings.TrimSpace(msg.Quote.Text)
	}

	if quoted == "" {
		return ""
	}
	return quoted
}

// externalReplyLabel renders a minimal label for an external reply,
// e.g. "chat 'Awaki' message #1234". The bot only sees the origin — it has
// no body text for messages it didn't author — so this is informational.
func externalReplyLabel(e *models.ExternalReplyInfo) string {
	var chatName string
	if e.Chat != nil {
		switch {
		case e.Chat.Title != "":
			chatName = e.Chat.Title
		case e.Chat.FirstName != "":
			chatName = e.Chat.FirstName
		}
	}
	if chatName == "" {
		chatName = "external chat"
	}
	if e.MessageID > 0 {
		return fmt.Sprintf("(external reply to %s, message #%d)", chatName, e.MessageID)
	}
	return fmt.Sprintf("(external reply to %s)", chatName)
}
