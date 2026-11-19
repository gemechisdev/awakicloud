package telegram

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
	"github.com/gemechischala/awaki/internal/transport"
	"github.com/gemechischala/awaki/internal/utils"
	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

type replier struct {
	ch         *Channel
	chatID     int64
	replyToID  int64 // 0 = no thread; first message threads, continuations don't
	firstReply bool  // whether the next SendText/SendFile should set the thread

	draftMu   sync.Mutex
	nextDraft int
}

func (r *replier) SendText(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	isRTL := rich.NeedsRTL(text)

	// Prefer native Markdown for agent output: Telegram's rich Markdown
	// already understands headings, tables, lists, code fences, etc.
	// Fall back to classic HTML/plain paths on any failure so replies are never lost.
	id, err := r.ch.client.SendRichMessage(ctx, r.chatID, "markdown", text, isRTL, false)
	if err == nil {
		_ = id
		if r.replyToID != 0 && r.firstReply {
			r.firstReply = false
		}
		return nil
	}
	log.Warn().Err(err).Msg("telegram: rich markdown send failed, trying HTML converter")

	html := rich.MarkdownToRichHTML(text)
	html = rich.EnsureSafeLength(html)
	id, err = r.ch.client.SendRichMessage(ctx, r.chatID, "html", html, isRTL, false)
	if err == nil {
		_ = id
		if r.replyToID != 0 && r.firstReply {
			r.firstReply = false
		}
		return nil
	}
	log.Warn().Err(err).Msg("telegram: rich HTML send failed, falling back to classic send")

	// Classic path restores threading behaviour.
	if r.replyToID != 0 && r.firstReply {
		r.ch.sendLongThreaded(ctx, r.chatID, r.replyToID, text)
		r.firstReply = false
		return nil
	}
	r.ch.sendLong(ctx, r.chatID, text)
	return nil
}

func (r *replier) SendFile(ctx context.Context, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	lower := strings.ToLower(path)
	upload := &models.InputFileUpload{Filename: filepath.Base(path), Data: bytes.NewReader(data)}

	switch {
	case utils.HasAnySuffix(lower, ".jpg", ".jpeg", ".png", ".webp"):
		return r.ch.client.SendPhoto(ctx, r.chatID, upload, caption)
	case utils.HasAnySuffix(lower, ".mp4", ".mov", ".avi", ".mkv"):
		return r.ch.client.SendVideo(ctx, r.chatID, upload, caption)
	case utils.HasAnySuffix(lower, ".mp3", ".wav", ".flac", ".aac", ".m4a"):
		return r.ch.client.SendAudio(ctx, r.chatID, upload, caption)
	case utils.HasAnySuffix(lower, ".ogg", ".opus"):
		return r.ch.client.SendVoice(ctx, r.chatID, upload, caption)
	case utils.HasAnySuffix(lower, ".gif"):
		return r.ch.client.SendAnimation(ctx, r.chatID, upload, caption)
	default:
		return r.ch.client.SendDocument(ctx, r.chatID, upload, caption)
	}
}

// SendPhotoBytes sends an in-memory image to the chat as a photo.
func (r *replier) SendPhotoBytes(ctx context.Context, filename string, data []byte, caption string) error {
	if filename == "" {
		filename = "image.png"
	}
	upload := &models.InputFileUpload{Filename: filename, Data: bytes.NewReader(data)}
	return r.ch.client.SendPhoto(ctx, r.chatID, upload, caption)
}

func (r *replier) SendAsk(ctx context.Context, prompt string, kind transport.AskKind, options []transport.AskOption) error {
	text := "❓ " + prompt

	switch kind {
	case transport.AskConfirm:
		keyboard := [][]models.InlineKeyboardButton{{
			{Text: "✅ Yes", CallbackData: "yes"},
			{Text: "❌ No", CallbackData: "no"},
		}}
		_, err := r.ch.client.SendMessageWithKeyboard(ctx, r.chatID, text, "", keyboard)
		return err

	case transport.AskChoice:
		var rows [][]models.InlineKeyboardButton
		for _, opt := range options {
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: opt.Label, CallbackData: opt.Value},
			})
		}
		_, err := r.ch.client.SendMessageWithKeyboard(ctx, r.chatID, text, "", rows)
		return err

	default:
		r.ch.sendLong(ctx, r.chatID, text)
		return nil
	}
}

func (r *replier) SendTyping(ctx context.Context) error {
	return r.ch.client.SendTyping(ctx, r.chatID)
}

// SendRichMessage sends a persistent Telegram Rich Message.
func (r *replier) SendRichMessage(ctx context.Context, contentType, content string, isRTL bool, disableNotification bool) error {
	id, err := r.ch.client.SendRichMessage(ctx, r.chatID, contentType, content, isRTL, disableNotification)
	if err != nil {
		return err
	}
	_ = id
	return nil
}

// SendRichMessageDraft streams an ephemeral rich message draft (30s lifetime).
func (r *replier) SendRichMessageDraft(ctx context.Context, draftID int, contentType, content string, canStop bool, keepOnStop bool) error {
	return r.ch.client.SendRichMessageDraft(ctx, r.chatID, draftID, contentType, content, canStop, keepOnStop)
}

// EditRichMessage updates an existing message to a Rich Message.
func (r *replier) EditRichMessage(ctx context.Context, messageID int, contentType, content string, isRTL bool) error {
	return r.ch.client.EditRichMessage(ctx, r.chatID, messageID, contentType, content, isRTL)
}

// nextDraftID returns a unique draft ID for this replier's chat.
func (r *replier) nextDraftID() int {
	r.draftMu.Lock()
	defer r.draftMu.Unlock()
	r.nextDraft++
	return r.nextDraft
}
