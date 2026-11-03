package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/exim"
	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Export / Import UI (Settings → Export data / Import data)
// ---------------------------------------------------------------------------

func (e *Engine) uiExportStart(ctx context.Context, msg channel.Message) {
	sel := map[string]bool{}
	// Default: everything except chats & projects (can be large)
	for _, c := range exim.AllCategories {
		if c == exim.CatChats || c == exim.CatProjects {
			sel[c] = false
		} else {
			sel[c] = true
		}
	}
	e.ui.set(&uiWizard{
		Kind:         "export",
		Step:         "cats",
		SelectedCats: sel,
	})
	e.uiExportCats(ctx, msg)
}

func (e *Engine) uiExportCats(ctx context.Context, msg channel.Message) {
	w := e.ui.get()
	if w == nil || w.Kind != "export" {
		return
	}
	var b strings.Builder
	b.WriteString("📤 **Export data**\n\n")
	b.WriteString("Toggle categories, then *Export selected*.\n")
	b.WriteString("Chats & projects can be large — enable only if needed.\n")
	b.WriteString("Export **includes API keys / tokens** — keep the file private.\n\n")

	var rows [][]channel.Button
	for _, c := range exim.AllCategories {
		on := w.SelectedCats[c]
		mark := "☐"
		if on {
			mark = "✅"
		}
		rows = append(rows, []channel.Button{{
			Text:         fmt.Sprintf("%s %s", mark, exim.CategoryLabel(c)),
			CallbackData: "ui:export:tog:" + c,
		}})
	}
	rows = append(rows,
		[]channel.Button{
			{Text: "✅ All", CallbackData: "ui:export:all"},
			{Text: "⬜ None", CallbackData: "ui:export:none"},
		},
		[]channel.Button{
			{Text: "📦 Export selected", CallbackData: "ui:export:run"},
		},
		[]channel.Button{
			{Text: "⬅ Settings", CallbackData: "ui:settings"},
		},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) uiExportToggle(ctx context.Context, msg channel.Message, cat string) {
	e.ui.mu.Lock()
	if e.ui.wizard != nil && e.ui.wizard.Kind == "export" {
		if e.ui.wizard.SelectedCats == nil {
			e.ui.wizard.SelectedCats = map[string]bool{}
		}
		e.ui.wizard.SelectedCats[cat] = !e.ui.wizard.SelectedCats[cat]
	}
	e.ui.mu.Unlock()
	e.uiExportCats(ctx, msg)
}

func (e *Engine) uiExportSetAll(ctx context.Context, msg channel.Message, on bool) {
	e.ui.mu.Lock()
	if e.ui.wizard != nil && e.ui.wizard.Kind == "export" {
		if e.ui.wizard.SelectedCats == nil {
			e.ui.wizard.SelectedCats = map[string]bool{}
		}
		for _, c := range exim.AllCategories {
			e.ui.wizard.SelectedCats[c] = on
		}
	}
	e.ui.mu.Unlock()
	e.uiExportCats(ctx, msg)
}

func (e *Engine) uiExportRun(ctx context.Context, msg channel.Message) {
	w := e.ui.get()
	if w == nil || w.Kind != "export" {
		return
	}
	var cats []string
	for _, c := range exim.AllCategories {
		if w.SelectedCats[c] {
			cats = append(cats, c)
		}
	}
	if len(cats) == 0 {
		_ = msg.Reply.SendText(ctx, "Select at least one category.")
		return
	}
	_ = msg.Reply.SendText(ctx, "⏳ Building export…")

	src := e.eximSource()
	data, result, err := exim.Export(src, exim.Selection{Categories: cats})
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Export failed: "+err.Error())
		return
	}

	tmpDir := filepath.Join(os.TempDir(), "awaki-export")
	_ = os.MkdirAll(tmpDir, 0o755)
	fname := fmt.Sprintf("awaki-export-%s.zip", time.Now().UTC().Format("20060102-150405"))
	path := filepath.Join(tmpDir, fname)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_ = msg.Reply.SendText(ctx, "Write temp file: "+err.Error())
		return
	}
	defer os.Remove(path)

	var summary strings.Builder
	summary.WriteString("📤 Export ready\n\n")
	for _, c := range cats {
		summary.WriteString(fmt.Sprintf("• %s: %d\n", exim.CategoryLabel(c), result.Counts[c]))
	}
	if result.Manifest.HasSecrets {
		summary.WriteString("\n⚠️ Contains secrets (API keys / tokens). Keep private.")
	}
	for _, w := range result.Warnings {
		summary.WriteString("\n⚠ " + w)
	}

	caption := summary.String()
	if len(caption) > 900 {
		caption = caption[:900] + "…"
	}
	if err := msg.Reply.SendFile(ctx, path, caption); err != nil {
		log.Error().Err(err).Msg("export send file")
		_ = msg.Reply.SendText(ctx, "Failed to send file: "+err.Error())
		return
	}
	e.ui.clear()
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

func (e *Engine) uiImportStart(ctx context.Context, msg channel.Message) {
	e.ui.set(&uiWizard{
		Kind: "import",
		Step: "wait_file",
	})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "📥 **Import data**\n\n" +
			"Send an **awaki-export-*.zip** file (as a document).\n\n" +
			"Or cancel to go back.",
		Rows: [][]channel.Button{
			{{Text: "❌ Cancel", CallbackData: "ui:settings"}},
		},
	})
}

// tryImportAttachment is called from message handling when an attachment arrives
// while the import wizard is waiting for a file.
func (e *Engine) tryImportAttachment(ctx context.Context, msg channel.Message) bool {
	w := e.ui.get()
	if w == nil || w.Kind != "import" || w.Step != "wait_file" {
		return false
	}
	var zipData []byte
	for _, att := range msg.Attachments {
		name := strings.ToLower(att.Filename)
		mime := strings.ToLower(att.MimeType)
		if strings.HasSuffix(name, ".zip") || mime == "application/zip" || mime == "application/x-zip-compressed" {
			zipData = att.Data
			break
		}
	}
	if len(zipData) == 0 {
		return false
	}
	man, err := exim.Inspect(zipData)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Not a valid Awaki export: "+err.Error())
		return true
	}

	sel := map[string]bool{}
	for _, c := range man.Categories {
		sel[c] = true
	}
	e.ui.set(&uiWizard{
		Kind:         "import",
		Step:         "cats",
		SelectedCats: sel,
		PendingData:  zipData,
		Overwrite:    false,
	})

	var b strings.Builder
	b.WriteString("📥 **Import package**\n\n")
	b.WriteString(fmt.Sprintf("Created: %s\n", man.CreatedAt.Format(time.RFC3339)))
	if man.HasSecrets {
		b.WriteString("⚠️ Package contains secrets.\n")
	}
	b.WriteString("\nToggle categories, choose merge/overwrite, then confirm.\nMerge unions data (e.g. chats under the same project). Overwrite replaces matching IDs only.\n")
	e.uiImportCats(ctx, msg, b.String())
	return true
}

func (e *Engine) uiImportCats(ctx context.Context, msg channel.Message, header string) {
	w := e.ui.get()
	if w == nil || w.Kind != "import" {
		return
	}
	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteByte('\n')
	} else {
		b.WriteString("📥 **Import data**\n\nToggle categories, then confirm.\n\n")
	}
	mode := "🔀 Merge — add new, keep local, never delete"
	if w.Overwrite {
		mode = "♻️ Overwrite — replace matching keys"
	}
	b.WriteString("Mode: " + mode + "\n")

	var rows [][]channel.Button
	for _, c := range exim.AllCategories {
		if !w.SelectedCats[c] && (w.PendingData == nil || !categoryInPending(w, c)) {
			// still show if in package; SelectedCats was seeded from manifest
		}
		on := w.SelectedCats[c]
		mark := "☐"
		if on {
			mark = "✅"
		}
		rows = append(rows, []channel.Button{{
			Text:         fmt.Sprintf("%s %s", mark, exim.CategoryLabel(c)),
			CallbackData: "ui:import:tog:" + c,
		}})
	}
	rows = append(rows,
		[]channel.Button{
			{Text: mode, CallbackData: "ui:import:mode"},
		},
		[]channel.Button{
			{Text: "✅ Confirm import", CallbackData: "ui:import:run"},
		},
		[]channel.Button{
			{Text: "❌ Cancel", CallbackData: "ui:settings"},
		},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func categoryInPending(w *uiWizard, cat string) bool {
	// heuristic: SelectedCats already limited to manifest categories
	return w.SelectedCats[cat]
}

func (e *Engine) uiImportToggle(ctx context.Context, msg channel.Message, cat string) {
	e.ui.mu.Lock()
	if e.ui.wizard != nil && e.ui.wizard.Kind == "import" {
		if e.ui.wizard.SelectedCats == nil {
			e.ui.wizard.SelectedCats = map[string]bool{}
		}
		e.ui.wizard.SelectedCats[cat] = !e.ui.wizard.SelectedCats[cat]
	}
	e.ui.mu.Unlock()
	e.uiImportCats(ctx, msg, "")
}

func (e *Engine) uiImportToggleMode(ctx context.Context, msg channel.Message) {
	e.ui.mu.Lock()
	if e.ui.wizard != nil && e.ui.wizard.Kind == "import" {
		e.ui.wizard.Overwrite = !e.ui.wizard.Overwrite
	}
	e.ui.mu.Unlock()
	e.uiImportCats(ctx, msg, "")
}

func (e *Engine) uiImportRun(ctx context.Context, msg channel.Message) {
	w := e.ui.get()
	if w == nil || w.Kind != "import" || len(w.PendingData) == 0 {
		_ = msg.Reply.SendText(ctx, "No package loaded. Start Import again and send the ZIP.")
		return
	}
	var cats []string
	for _, c := range exim.AllCategories {
		if w.SelectedCats[c] {
			cats = append(cats, c)
		}
	}
	if len(cats) == 0 {
		_ = msg.Reply.SendText(ctx, "Select at least one category.")
		return
	}
	_ = msg.Reply.SendText(ctx, "⏳ Importing…")

	tgt := e.eximTarget()
	result, err := exim.Import(w.PendingData, tgt, exim.Selection{
		Categories: cats,
		Overwrite:  w.Overwrite,
	})
	e.ui.clear()
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Import failed: "+err.Error())
		return
	}
	var b strings.Builder
	b.WriteString("✅ **Import complete**\n\n")
	for _, c := range cats {
		b.WriteString(fmt.Sprintf("• %s: %d\n", exim.CategoryLabel(c), result.Counts[c]))
	}
	for _, w := range result.Warnings {
		b.WriteString("\n⚠ " + w)
	}
	_ = msg.Reply.SendText(ctx, b.String())

	// reconnect MCP if imported
	if result.Counts[exim.CatMCP] > 0 && e.mcp != nil {
		go e.mcp.ConnectAll(context.Background())
	}
	if result.Counts[exim.CatProviders] > 0 && e.rebuildAgent != nil {
		_ = e.rebuildAgent()
	}
}

func (e *Engine) eximSource() exim.Source {
	src := exim.Source{
		Store:      e.store,
		Providers:  e.providers,
		Projects:   e.projects,
		Scheduler:  e.scheduler,
		PermStore:  e.permStore,
		AppVersion: "", // optional
	}
	if e.mcp != nil {
		src.MCP = e.mcp.Store()
	}
	return src
}

func (e *Engine) eximTarget() exim.Target {
	tgt := exim.Target{
		Store:     e.store,
		Providers: e.providers,
		Projects:  e.projects,
		Scheduler: e.scheduler,
		PermStore: e.permStore,
	}
	if e.mcp != nil {
		tgt.MCP = e.mcp.Store()
	}
	return tgt
}

func (e *Engine) handleExportUI(ctx context.Context, msg channel.Message, parts []string) {
	if len(parts) < 2 {
		return
	}
	if len(parts) == 2 {
		e.uiExportStart(ctx, msg)
		return
	}
	switch parts[2] {
	case "tog":
		if len(parts) >= 4 {
			e.uiExportToggle(ctx, msg, parts[3])
		}
	case "all":
		e.uiExportSetAll(ctx, msg, true)
	case "none":
		e.uiExportSetAll(ctx, msg, false)
	case "run":
		e.uiExportRun(ctx, msg)
	default:
		e.uiExportStart(ctx, msg)
	}
}

func (e *Engine) handleImportUI(ctx context.Context, msg channel.Message, parts []string) {
	if len(parts) < 2 {
		return
	}
	if len(parts) == 2 {
		e.uiImportStart(ctx, msg)
		return
	}
	switch parts[2] {
	case "tog":
		if len(parts) >= 4 {
			e.uiImportToggle(ctx, msg, parts[3])
		}
	case "mode":
		e.uiImportToggleMode(ctx, msg)
	case "run":
		e.uiImportRun(ctx, msg)
	default:
		e.uiImportStart(ctx, msg)
	}
}
