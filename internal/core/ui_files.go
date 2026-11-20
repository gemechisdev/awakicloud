package core

import (
	"context"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/filemgr"
	"github.com/gemechischala/awaki/internal/permission"
	"github.com/gemechischala/awaki/internal/workspace"
	"github.com/rs/zerolog/log"
)

// File manager: a Telegram-native browser for the active workspace.
//
// All listing / reading / jailing / paging / delete rules live in
// internal/filemgr (pure, unit-tested). This file only adapts it to the
// engine: it points the manager at the current workspace root, applies the
// permission mode, delivers screens, and hands "Ask agent to edit" to the
// agent as an ordinary user message.

// fileManager returns the lazily-created file manager (Engine literals built
// in tests have no constructor, hence sync.Once rather than New()).
func (e *Engine) fileManager() *filemgr.Manager {
	e.filesOnce.Do(func() { e.filesMgr = filemgr.New() })
	return e.filesMgr
}

// prepareFiles points the manager at the current workspace root and works out
// what the owner may do in it.
func (e *Engine) prepareFiles(ctx context.Context) (*filemgr.Manager, filemgr.Options, error) {
	fm := e.fileManager()
	root := workspace.Root()
	if root == "" {
		// Fresh process: no chat has bound a workspace yet.
		if e.chats != nil {
			e.rebindActiveWorkspace(ctx)
		}
		root = workspace.Root()
	}
	// A changed root drops the id index, so buttons from another
	// project/chat expire instead of pointing at the wrong tree.
	if err := fm.SetRoot(root); err != nil {
		return fm, filemgr.Options{}, err
	}
	return fm, e.filesOptions(), nil
}

// filesOptions derives UI capabilities from the chat's permission mode:
// plan mode is read-only, so Delete is hidden and refused.
func (e *Engine) filesOptions() filemgr.Options {
	mode := permission.ModeAuto
	if e.permStore != nil {
		chatID := ""
		if e.chats != nil {
			chatID = e.chats.ActiveID()
		}
		mode = e.permStore.Get(chatID)
	}
	writable := mode != permission.ModePlan
	return filemgr.Options{AllowDelete: writable, AllowWrite: writable}
}

// uiFiles opens the browser (/files [path]).
func (e *Engine) uiFiles(ctx context.Context, msg channel.Message, pathHint string) {
	e.ui.clear()
	fm, opts, err := e.prepareFiles(ctx)
	if err != nil {
		e.deliverFileScreen(ctx, msg, filemgr.ErrorScreen(err))
		return
	}
	e.applyFilesResult(ctx, msg, fm.Open(strings.TrimSpace(pathHint), opts))
}

// handleFilesCallback routes `files:*` callback data. Returns true (consumed).
func (e *Engine) handleFilesCallback(ctx context.Context, msg channel.Message, data string) bool {
	a, err := filemgr.ParseCallback(data)
	if err != nil {
		log.Debug().Err(err).Str("data", data).Msg("files: ignoring malformed callback")
		return true
	}
	// Any files button (Cancel included) abandons an open name prompt.
	if w := e.ui.get(); w != nil && strings.HasPrefix(w.Kind, "files_") {
		e.ui.clear()
	}
	fm, opts, err := e.prepareFiles(ctx)
	if err != nil {
		e.deliverFileScreen(ctx, msg, filemgr.ErrorScreen(err))
		return true
	}
	e.applyFilesResult(ctx, msg, fm.Handle(a, opts))
	return true
}

// applyFilesResult delivers the screen, then performs the side effects.
func (e *Engine) applyFilesResult(ctx context.Context, msg channel.Message, res filemgr.Result) {
	if !res.NoScreen {
		e.deliverFileScreen(ctx, msg, res.Screen)
	}
	if res.Copy != "" && msg.Reply != nil {
		// A code span is tap-to-copy on mobile.
		_ = msg.Reply.SendText(ctx, "📋 `"+strings.ReplaceAll(res.Copy, "`", "'")+"`")
	}
	if res.Prompt != nil {
		e.ui.set(&uiWizard{Kind: res.Prompt.Kind, Step: "name", Draft: map[string]string{"id": strconv.Itoa(res.Prompt.ID)}})
	}
	if res.SendPath != "" && msg.Reply != nil {
		if err := msg.Reply.SendFile(ctx, res.SendPath, "📄 "+res.SendCaption); err != nil {
			_ = msg.Reply.SendText(ctx, "Could not send the file: "+err.Error())
		}
	}
	if res.Inject != "" {
		e.injectFilesFocus(ctx, msg, res.Inject)
	}
}

// wizardFiles completes a new-file / new-folder / rename prompt.
func (e *Engine) wizardFiles(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	id, err := strconv.Atoi(w.Draft["id"])
	e.ui.clear()
	if err != nil {
		return true
	}
	fm, opts, perr := e.prepareFiles(ctx)
	if perr != nil {
		e.deliverFileScreen(ctx, msg, filemgr.ErrorScreen(perr))
		return true
	}
	res := fm.Submit(w.Kind, id, text, opts)
	e.applyFilesResult(ctx, msg, res)
	if res.Retry && res.Prompt == nil { // defensive: keep the prompt open
		e.ui.set(w)
	}
	return true
}

// injectFilesFocus hands the focus message to the agent exactly as if the
// owner had typed it: it answers a pending ask_user, queues behind a running
// turn (steer card), or starts a new turn.
func (e *Engine) injectFilesFocus(ctx context.Context, msg channel.Message, text string) {
	e.ui.clear() // a half-finished wizard must not swallow the message as input
	e.handleMessage(ctx, channel.Message{Text: text, Reply: msg.Reply})
}

func (e *Engine) deliverFileScreen(ctx context.Context, msg channel.Message, s filemgr.Screen) {
	e.deliverKeyboard(ctx, msg, filesKeyboard(s))
}

func filesKeyboard(s filemgr.Screen) channel.KeyboardMessage {
	rows := make([][]channel.Button, 0, len(s.Rows))
	for _, r := range s.Rows {
		row := make([]channel.Button, 0, len(r))
		for _, b := range r {
			row = append(row, channel.Button{Text: b.Text, CallbackData: b.CallbackData, CopyText: b.CopyText})
		}
		rows = append(rows, row)
	}
	return channel.KeyboardMessage{Text: s.Text, Rows: rows}
}
