package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/providers"
)

func (e *Engine) uiProvidersList(ctx context.Context, msg channel.Message, page int) {
	list := e.providers.ListProviders()
	total := len(list)
	if total == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "🔌 AI Providers\n\nNo providers yet. Add one to connect an LLM.",
			Rows: [][]channel.Button{
				{{Text: "➕ Add Provider", CallbackData: "ui:prvadd"}},
				{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
			},
		})
		return
	}
	if page < 0 {
		page = 0
	}
	start := page * pageSizeProviders
	if start >= total {
		page = 0
		start = 0
	}
	end := start + pageSizeProviders
	if end > total {
		end = total
	}
	activePID := ""
	if _, p, err := e.providers.ActivePair(); err == nil {
		activePID = p.ID
	}
	pages := (total + pageSizeProviders - 1) / pageSizeProviders
	text := fmt.Sprintf("🔌 AI Providers  ·  page %d/%d\n\nTap a provider to manage it.", page+1, pages)
	var rows [][]channel.Button
	for _, p := range list[start:end] {
		mark := ""
		if p.ID == activePID {
			mark = " ✅"
		}
		rows = append(rows, []channel.Button{
			{Text: "🔹 " + truncateUI(p.Name, 28) + mark, CallbackData: "ui:prv:" + p.ID},
		})
	}
	var nav []channel.Button
	if page > 0 {
		nav = append(nav, channel.Button{Text: "◀ Previous", CallbackData: fmt.Sprintf("ui:providers:%d", page-1)})
	}
	if end < total {
		nav = append(nav, channel.Button{Text: "Next ▶", CallbackData: fmt.Sprintf("ui:providers:%d", page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows,
		[]channel.Button{{Text: "➕ Add Provider", CallbackData: "ui:prvadd"}},
		[]channel.Button{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: text, Rows: rows})
}

func (e *Engine) uiProviderDetail(ctx context.Context, msg channel.Message, id string, modelPage int) {
	p, ok := e.providers.GetProvider(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Provider not found.")
		e.uiProvidersList(ctx, msg, 0)
		return
	}
	models := e.providers.ListModelsByProvider(id)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🔹 Provider: %s\n\n", p.Name))
	b.WriteString(fmt.Sprintf("Protocol: %s\n", p.Protocol.DisplayName()))
	b.WriteString(fmt.Sprintf("Base URL: %s\n", p.BaseURL))
	b.WriteString(fmt.Sprintf("API key: %s\n\n", providers.MaskKey(p.APIKey)))
	b.WriteString(fmt.Sprintf("Models (%d):", len(models)))

	rows := [][]channel.Button{{
		{Text: "✏️ Edit", CallbackData: "ui:prvedit:" + id},
		{Text: "🗑 Delete", CallbackData: "ui:prvdelc:" + id},
	}}

	total := len(models)
	if total == 0 {
		b.WriteString("\n(none yet)")
	} else {
		if modelPage < 0 {
			modelPage = 0
		}
		start := modelPage * pageSizeModels
		if start >= total {
			modelPage = 0
			start = 0
		}
		end := start + pageSizeModels
		if end > total {
			end = total
		}
		active := e.providers.ActiveModelID()
		b.WriteString(fmt.Sprintf("\nPage %d", modelPage+1))
		for _, m := range models[start:end] {
			mark := ""
			if m.ID == active {
				mark = " ✅"
			}
			rows = append(rows, []channel.Button{
				{Text: "🤖 " + truncateUI(m.DisplayName, 28) + mark, CallbackData: "ui:mdl:" + m.ID},
			})
		}
		var nav []channel.Button
		if modelPage > 0 {
			nav = append(nav, channel.Button{Text: "◀", CallbackData: fmt.Sprintf("ui:prvpage:%s:%d", id, modelPage-1)})
		}
		if end < total {
			nav = append(nav, channel.Button{Text: "▶", CallbackData: fmt.Sprintf("ui:prvpage:%s:%d", id, modelPage+1)})
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
	}
	rows = append(rows,
		[]channel.Button{{Text: "➕ Add Model", CallbackData: "ui:mdladd:" + id}},
		[]channel.Button{{Text: "⬅ Providers", CallbackData: "ui:providers:0"}},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) uiConfirmDeleteProvider(ctx context.Context, msg channel.Message, id string) {
	p, ok := e.providers.GetProvider(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Provider not found.")
		return
	}
	n := len(e.providers.ListModelsByProvider(id))
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: fmt.Sprintf("⚠️ Delete provider «%s»?\n\nRemoves the provider and its %d model(s). Cannot be undone.", p.Name, n),
		Rows: [][]channel.Button{{
			{Text: "🔴 Yes, Delete", CallbackData: "ui:prvdel:" + id},
			{Text: "🔵 No, Cancel", CallbackData: "ui:prv:" + id},
		}},
	})
}

func (e *Engine) uiStartAddProvider(ctx context.Context, msg channel.Message) {
	e.ui.set(&uiWizard{Kind: "add_provider", Step: "name", Draft: map[string]string{}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "➕ Add Provider — step 1/4\n\nSend the provider display name.\nExample: OpenRouter",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:providers:0"}}},
	})
}

func (e *Engine) uiStartEditProvider(ctx context.Context, msg channel.Message, id string) {
	p, ok := e.providers.GetProvider(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Provider not found.")
		return
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: fmt.Sprintf("✏️ Edit «%s»\n\nChoose a field. API keys are never shown in full.", p.Name),
		Rows: [][]channel.Button{
			{{Text: "Name", CallbackData: "ui:prvf:" + id + ":name"}},
			{{Text: "Base URL", CallbackData: "ui:prvf:" + id + ":base_url"}},
			{{Text: "Protocol", CallbackData: "ui:prvf:" + id + ":protocol"}},
			{{Text: "API Key", CallbackData: "ui:prvf:" + id + ":api_key"}},
			{{Text: "⬅ Back", CallbackData: "ui:prv:" + id}},
		},
	})
}

func (e *Engine) uiEditProviderField(ctx context.Context, msg channel.Message, id, field string) {
	if field == "protocol" {
		var rows [][]channel.Button
		for _, p := range providers.AllProtocols() {
			rows = append(rows, []channel.Button{{
				Text: p.DisplayName(), CallbackData: "ui:prvproto:" + id + ":" + protocolShort(p),
			}})
		}
		rows = append(rows, []channel.Button{{Text: "⬅ Back", CallbackData: "ui:prvedit:" + id}})
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: "Select API protocol:", Rows: rows})
		return
	}
	e.ui.set(&uiWizard{Kind: "edit_provider", Step: field, ProviderID: id, Field: field, Draft: map[string]string{}})
	prompt := map[string]string{
		"name": "Send the new provider name.", "base_url": "Send the new base URL.",
		"api_key": "Send the new API key (stored securely, never logged in full).",
	}[field]
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "✏️ " + prompt + "\n\nSend /cancel to abort.",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:prv:" + id}}},
	})
}

func (e *Engine) uiModelDetail(ctx context.Context, msg channel.Message, id string) {
	m, ok := e.providers.GetModel(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Model not found.")
		return
	}
	p, _ := e.providers.GetProvider(m.ProviderID)
	mark := ""
	if e.providers.ActiveModelID() == id {
		mark = " ✅ active"
	}
	cw := m.ContextWindow
	if cw == 0 {
		cw = 128000
	}
	text := fmt.Sprintf("🤖 Model: %s%s\n\nWire ID: %s\nProvider: %s\nContext: %d\nTools: %v · Images: %v · Stream: %v",
		m.DisplayName, mark, m.ModelID, p.Name, cw, m.Capabilities.Tools, m.Capabilities.Images, m.Capabilities.Streaming)
	rows := [][]channel.Button{
		{{Text: "🟢 Use this model", CallbackData: "ui:mdlsel:" + id}},
		{{Text: "✏️ Display name", CallbackData: "ui:mdlf:" + id + ":display"}},
		{{Text: "✏️ Model ID", CallbackData: "ui:mdlf:" + id + ":model_id"}},
		{{Text: "✏️ Context length", CallbackData: "ui:mdlf:" + id + ":context"}},
		{
			{Text: toggleLabel("Tools", m.Capabilities.Tools), CallbackData: "ui:mdltog:" + id + ":tools"},
			{Text: toggleLabel("Images", m.Capabilities.Images), CallbackData: "ui:mdltog:" + id + ":images"},
		},
		{{Text: toggleLabel("Streaming", m.Capabilities.Streaming), CallbackData: "ui:mdltog:" + id + ":stream"}},
		{{Text: "🗑 Delete Model", CallbackData: "ui:mdldelc:" + id}},
		{{Text: "⬅ Provider", CallbackData: "ui:prv:" + m.ProviderID}},
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: text, Rows: rows})
}

func toggleLabel(name string, on bool) string {
	if on {
		return "🟢 " + name
	}
	return "⚪ " + name
}

func (e *Engine) uiConfirmDeleteModel(ctx context.Context, msg channel.Message, id string) {
	m, ok := e.providers.GetModel(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Model not found.")
		return
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: fmt.Sprintf("⚠️ Delete model «%s»?\n\nThis cannot be undone.", m.DisplayName),
		Rows: [][]channel.Button{{
			{Text: "🔴 Yes, Delete", CallbackData: "ui:mdldel:" + id},
			{Text: "🔵 No, Cancel", CallbackData: "ui:mdl:" + id},
		}},
	})
}

func (e *Engine) uiStartAddModel(ctx context.Context, msg channel.Message, providerID string) {
	if _, ok := e.providers.GetProvider(providerID); !ok {
		_ = msg.Reply.SendText(ctx, "Provider not found.")
		return
	}
	e.ui.set(&uiWizard{Kind: "add_model", Step: "model_id", ProviderID: providerID, Draft: map[string]string{}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "➕ Add Model — step 1/3\n\nSend the wire model ID the API expects.\nExample: anthropic/claude-sonnet-4.6",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:prv:" + providerID}}},
	})
}

func (e *Engine) uiAllModels(ctx context.Context, msg channel.Message, page int) {
	list := e.providers.ListModels()
	total := len(list)
	if total == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "🧠 No models yet.\nAdd a provider first, then add models under it.",
			Rows: [][]channel.Button{
				{{Text: "🔌 Providers", CallbackData: "ui:providers:0"}},
				{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
			},
		})
		return
	}
	if page < 0 {
		page = 0
	}
	start := page * pageSizeModels
	if start >= total {
		page = 0
		start = 0
	}
	end := start + pageSizeModels
	if end > total {
		end = total
	}
	active := e.providers.ActiveModelID()
	text := fmt.Sprintf("🧠 Models  ·  page %d\n", page+1)
	var rows [][]channel.Button
	for _, m := range list[start:end] {
		p, _ := e.providers.GetProvider(m.ProviderID)
		mark := ""
		if m.ID == active {
			mark = " ✅"
		}
		label := truncateUI(m.DisplayName+" · "+p.Name, 36) + mark
		rows = append(rows, []channel.Button{{Text: "🤖 " + label, CallbackData: "ui:mdl:" + m.ID}})
	}
	var nav []channel.Button
	if page > 0 {
		nav = append(nav, channel.Button{Text: "◀ Previous", CallbackData: fmt.Sprintf("ui:models:%d", page-1)})
	}
	if end < total {
		nav = append(nav, channel.Button{Text: "Next ▶", CallbackData: fmt.Sprintf("ui:models:%d", page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Settings", CallbackData: "ui:settings"}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: text, Rows: rows})
}

func (e *Engine) uiEditModelField(ctx context.Context, msg channel.Message, id, field string) {
	e.ui.set(&uiWizard{Kind: "edit_model", Step: field, ModelID: id, Field: field, Draft: map[string]string{}})
	prompts := map[string]string{
		"display": "Send the new display name.", "model_id": "Send the new wire model ID.",
		"context": "Send context window size as a number (e.g. 128000).",
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "✏️ " + prompts[field] + "\n\nSend /cancel to abort.",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:mdl:" + id}}},
	})
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
