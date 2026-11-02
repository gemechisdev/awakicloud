package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/permission"
)

// uiModeMenu shows current permission mode + inline choices (also /mode with no args).
func (e *Engine) uiModeMenu(ctx context.Context, msg channel.Message) {
	chatID := ""
	if ch, err := e.chats.Resolve(); err == nil && ch != nil {
		chatID = ch.ID
	}
	cur := permission.ModeAuto
	if e.permStore != nil {
		cur = e.permStore.Get(chatID)
	}

	text := fmt.Sprintf(
		"%s **Permission mode:** `%s`\n\n%s\n\nChoose a mode for this chat:",
		cur.Emoji(), cur.Title(), cur.Description(),
	)
	km := channel.KeyboardMessage{
		Text: text,
		Rows: [][]channel.Button{
			{
				{Text: "⚡ Auto", CallbackData: "mode:set:auto"},
				{Text: "🛡️ Ask", CallbackData: "mode:set:ask"},
				{Text: "📋 Plan", CallbackData: "mode:set:plan"},
			},
			{{Text: "« Home", CallbackData: "ui:home"}},
		},
	}
	e.deliverKeyboard(ctx, msg, km)
}

func (e *Engine) setPermissionMode(ctx context.Context, msg channel.Message, raw string) {
	mode, err := permission.Parse(raw)
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error()+"\n\nUse: /mode auto | ask | plan")
		return
	}
	chatID := ""
	if ch, err := e.chats.Resolve(); err == nil && ch != nil {
		chatID = ch.ID
	}
	if e.permStore == nil {
		_ = msg.Reply.SendText(ctx, "Permission store not available")
		return
	}
	if err := e.permStore.Set(chatID, mode); err != nil {
		_ = msg.Reply.SendText(ctx, "Failed to save mode: "+err.Error())
		return
	}
	text := fmt.Sprintf("%s Permission mode set to **%s** for this chat.\n\n%s", mode.Emoji(), mode.Title(), mode.Description())
	km := channel.KeyboardMessage{
		Text: text,
		Rows: [][]channel.Button{
			{
				{Text: "⚡ Auto", CallbackData: "mode:set:auto"},
				{Text: "🛡️ Ask", CallbackData: "mode:set:ask"},
				{Text: "📋 Plan", CallbackData: "mode:set:plan"},
			},
			{{Text: "« Home", CallbackData: "ui:home"}},
		},
	}
	e.deliverKeyboard(ctx, msg, km)
}

func (e *Engine) handleModeCallback(ctx context.Context, msg channel.Message, data string) bool {
	if !strings.HasPrefix(data, "mode:") {
		return false
	}
	parts := strings.Split(data, ":")
	// mode:set:auto
	if len(parts) >= 3 && parts[1] == "set" {
		e.setPermissionMode(ctx, msg, parts[2])
		return true
	}
	e.uiModeMenu(ctx, msg)
	return true
}
