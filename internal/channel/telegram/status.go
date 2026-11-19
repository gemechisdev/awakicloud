package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel/telegram/format"
	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
	"github.com/gemechischala/awaki/internal/transport"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

var phaseEmoji = map[transport.Phase]string{
	transport.PhaseThinking: "\U0001f9e0",
	transport.PhasePlanning: "\U0001f4cb",
	transport.PhaseTool:     "\U0001f527",
	transport.PhaseWorking:  "\u26a1",
	transport.PhaseMessage:  "\U0001f4ac",
}

func taskStateEmoji(s transport.TaskStatus) string {
	switch s {
	case transport.TaskCompleted:
		return "✅"
	case transport.TaskInProgress:
		return "▶️"
	default:
		return "⬜"
	}
}

// formatStatus is the plain-text fallback (MarkdownV2) for short statuses.
func formatStatus(s transport.AgentStatus) string {
	var parts []string

	if len(s.Plan) > 0 {
		done := 0
		for _, t := range s.Plan {
			if t.Status == transport.TaskCompleted {
				done++
			}
		}
		parts = append(parts, fmt.Sprintf("📋 *Plan* %d/%d", done, len(s.Plan)))
	}

	if s.Percent != nil {
		parts = append(parts, formatProgressBar(*s.Percent))
	}

	emoji := phaseEmoji[s.Phase]
	if emoji == "" {
		emoji = "⏳"
	}

	label := s.Detail
	if label == "" {
		switch s.Phase {
		case transport.PhaseThinking:
			label = "Thinking"
		case transport.PhasePlanning:
			label = "Planning"
		default:
			label = s.Tool
		}
	}
	if label != "" {
		if s.Phase == transport.PhaseMessage {
			runes := []rune(label)
			if len(runes) > 300 {
				label = string(runes[:300]) + "…"
			}
			parts = append(parts, emoji+" "+label)
		} else {
			parts = append(parts, emoji+" "+label+"\\.\\.\\.")
		}
	}

	if len(parts) == 0 {
		return emoji + " Working\\.\\.\\."
	}
	return strings.Join(parts, "\n")
}

// formatStatusRich renders an AgentStatus as Telegram Rich HTML.
// Plan tasks are wrapped in a collapsible <details> drawer.
func formatStatusRich(s transport.AgentStatus) string {
	var b strings.Builder

	emoji := phaseEmoji[s.Phase]
	if emoji == "" {
		emoji = "⏳"
	}

	label := s.Detail
	if label == "" {
		switch s.Phase {
		case transport.PhaseThinking:
			label = "Thinking"
		case transport.PhasePlanning:
			label = "Planning"
		default:
			label = s.Tool
		}
	}
	if label != "" {
		b.WriteString(rich.Paragraph(emoji + " " + label))
		if s.Phase != transport.PhaseMessage {
			b.WriteString(rich.Paragraph("Working…"))
		}
	}

	// Progress bar as tap-to-copy code
	if s.Percent != nil {
		b.WriteString(rich.CodeBlock(formatProgressBar(*s.Percent)))
	}

	// Plan tasks in a collapsible drawer
	if len(s.Plan) > 0 {
		var planLines []string
		done := 0
		for _, t := range s.Plan {
			planLines = append(planLines, fmt.Sprintf("%s %s", taskStateEmoji(t.Status), t.Title))
			if t.Status == transport.TaskCompleted {
				done++
			}
		}
		planHTML := strings.Join(planLines, "\n")
		b.WriteString(rich.Details(fmt.Sprintf("📋 Plan %d/%d", done, len(s.Plan)), planHTML, false))
	}

	if b.Len() == 0 {
		return rich.Paragraph(emoji + " Working…")
	}
	return b.String()
}

func formatProgressBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	const width = 10
	filled := pct * width / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("%s %d%%", bar, pct)
}

// planKeyboard builds read-only inline buttons: one row per task.
// Callback data is ignored by the bot (plan:noop).
func planKeyboard(tasks []transport.PlanTask) [][]models.InlineKeyboardButton {
	if len(tasks) == 0 {
		return nil
	}
	var rows [][]models.InlineKeyboardButton
	for i, t := range tasks {
		label := taskStateEmoji(t.Status) + " " + t.Title
		// Telegram button text max 64 runes
		runes := []rune(label)
		if len(runes) > 64 {
			label = string(runes[:61]) + "…"
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("plan:noop:%d", i),
		}})
	}
	return rows
}

func (r *replier) sendStatus(ctx context.Context, status transport.AgentStatus) int {
	html := formatStatusRich(status)

	// Use rich message if content is substantial; otherwise fall back to plain.
	if rich.IsRichWorthy(html) {
		id, err := r.ch.client.SendRichMessage(ctx, r.chatID, "html", html, false, false)
		if err != nil {
			log.Warn().Err(err).Msg("telegram: send rich status failed, falling back to plain")
			text := formatStatus(status)
			return r.ch.sendAndGetID(ctx, r.chatID, text)
		}
		return id
	}

	text := formatStatus(status)
	kb := planKeyboard(status.Plan)
	if len(kb) == 0 {
		return r.ch.sendAndGetID(ctx, r.chatID, text)
	}
	id, err := r.ch.client.SendMessageWithKeyboard(ctx, r.chatID, text, models.ParseModeMarkdown, kb)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: send status with plan keyboard failed")
		return r.ch.sendAndGetID(ctx, r.chatID, text)
	}
	return id
}

func (r *replier) updateStatus(ctx context.Context, msgID int, status transport.AgentStatus) {
	if msgID == 0 {
		return
	}
	html := formatStatusRich(status)

	if rich.IsRichWorthy(html) {
		err := r.ch.client.EditRichMessage(ctx, r.chatID, msgID, "html", html, false)
		if err != nil {
			log.Warn().Err(err).Int("msg_id", msgID).Str("phase", string(status.Phase)).Msg("telegram: edit rich status failed, falling back to plain")
			text := formatStatus(status)
			kb := planKeyboard(status.Plan)
			var err2 error
			if len(kb) > 0 {
				err2 = r.ch.client.EditMessageWithKeyboard(ctx, r.chatID, msgID, text, models.ParseModeMarkdown, kb)
			} else {
				err2 = r.ch.client.EditMessage(ctx, r.chatID, msgID, text, models.ParseModeMarkdown)
			}
			if err2 != nil {
				log.Warn().Err(err2).Int("msg_id", msgID).Str("phase", string(status.Phase)).Msg("telegram: EditMessage status failed")
			}
		}
		return
	}

	text := formatStatus(status)
	kb := planKeyboard(status.Plan)
	var err error
	if len(kb) > 0 {
		err = r.ch.client.EditMessageWithKeyboard(ctx, r.chatID, msgID, text, models.ParseModeMarkdown, kb)
	} else {
		err = r.ch.client.EditMessage(ctx, r.chatID, msgID, text, models.ParseModeMarkdown)
	}
	if err != nil {
		log.Warn().Err(err).Int("msg_id", msgID).Str("phase", string(status.Phase)).Msg("telegram: EditMessage status failed")
	}
}

func (r *replier) deleteStatus(ctx context.Context, msgID int) {
	if msgID == 0 {
		return
	}
	if err := r.ch.client.DeleteMessage(ctx, r.chatID, msgID); err != nil {
		log.Warn().Err(err).Int("msg_id", msgID).Msg("telegram: DeleteMessage failed")
	}
}

// editToFinal replaces the status message with the final response text.
func (r *replier) editToFinal(ctx context.Context, msgID int, text string) {
	if msgID == 0 {
		r.ch.sendLong(ctx, r.chatID, text)
		return
	}

	html := rich.MarkdownToRichHTML(text)
	if rich.IsRichWorthy(html) {
		err := r.ch.client.EditRichMessage(ctx, r.chatID, msgID, "html", html, false)
		if err != nil {
			log.Warn().Err(err).Msg("telegram: edit to rich final failed, falling back to plain")
			r.editToFinalPlain(ctx, msgID, text)
		}
		return
	}
	r.editToFinalPlain(ctx, msgID, text)
}

func (r *replier) editToFinalPlain(ctx context.Context, msgID int, text string) {
	chunks := format.FormatHTMLChunks(text, maxMessageLen)
	if len(chunks) == 0 {
		r.deleteStatus(ctx, msgID)
		return
	}

	if len(chunks) == 1 {
		// Clear keyboard by editing to plain final text
		err := r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].HTML, models.ParseModeHTML)
		if err != nil {
			log.Warn().Err(err).Msg("telegram: edit to final HTML failed, trying plain text")
			err = r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].Text, "")
			if err != nil {
				log.Warn().Err(err).Msg("telegram: edit to final plain failed, falling back to delete+send")
				r.deleteStatus(ctx, msgID)
				r.ch.sendLong(ctx, r.chatID, text)
			}
		}
		return
	}

	err := r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].HTML, models.ParseModeHTML)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: edit first chunk failed, falling back to delete+send")
		r.deleteStatus(ctx, msgID)
		r.ch.sendLong(ctx, r.chatID, text)
		return
	}
	for _, chunk := range chunks[1:] {
		r.ch.sendHTMLChunk(ctx, r.chatID, chunk)
	}
}
