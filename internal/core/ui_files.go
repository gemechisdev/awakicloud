package core

import (
	"context"
	"os"
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
		draft := map[string]string{"id": strconv.Itoa(res.Prompt.ID)}
		if res.Prompt.Data != "" {
			draft["find"] = res.Prompt.Data
		}
		e.ui.set(&uiWizard{Kind: res.Prompt.Kind, Step: "input", Draft: draft})
	}
	if res.SendPath != "" {
		if res.SendTemp {
			defer os.Remove(res.SendPath) // temporary zip: always clean up
		}
		if msg.Reply != nil {
			icon := "📄 "
			if res.SendTemp {
				icon = "📦 "
			}
			if err := msg.Reply.SendFile(ctx, res.SendPath, icon+res.SendCaption); err != nil {
				_ = msg.Reply.SendText(ctx, "Could not send the file: "+err.Error())
			}
		}
	}
	if res.Inject != "" {
		e.injectFilesFocus(ctx, msg, res.Inject)
	}
}

// wizardFiles completes one step of a file-manager prompt (new file/folder,
// rename, upload, find & replace, patch).
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
	var res filemgr.Result
	switch w.Kind {
	case filemgr.WizardUpload:
		ups := make([]filemgr.Upload, 0, len(msg.Attachments))
		for _, a := range msg.Attachments {
			ups = append(ups, filemgr.Upload{Name: a.Filename, Data: a.Data})
		}
		if len(ups) == 0 {
			// Nothing attached: keep waiting, remind what is expected.
			p := fm.Handle(filemgr.Action{Kind: filemgr.KindUpload, ID: id}, opts)
			p.Screen.Text = "⚠️ I need a file (send it as a document).\n\n" + p.Screen.Text
			e.applyFilesResult(ctx, msg, p)
			return true
		}
		res = fm.SubmitUpload(id, ups, opts)
	case filemgr.WizardDoc:
		if len(msg.Attachments) == 0 {
			p := fm.Handle(filemgr.Action{Kind: filemgr.KindGetDoc, ID: id}, opts)
			p.SendPath, p.SendCaption = "", "" // do not resend the file, just keep waiting
			p.Screen.Text = "⚠️ Send the edited file as a document.\n\n" + p.Screen.Text
			e.applyFilesResult(ctx, msg, p)
			return true
		}
		res = fm.SubmitDoc(id, msg.Attachments[0].Data, opts)
	case filemgr.WizardPatch:
		if len(msg.Attachments) > 0 {
			text = string(msg.Attachments[0].Data)
		}
		res = fm.Submit(w.Kind, id, text, opts)
	case filemgr.WizardReplaceWith:
		res = fm.SubmitReplace(id, w.Draft["find"], text, opts)
	default:
		res = fm.Submit(w.Kind, id, text, opts)
	}
	e.applyFilesResult(ctx, msg, res)
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
