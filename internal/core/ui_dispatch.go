package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/rs/zerolog/log"
)

// handleUICallback routes Telegram callback data / reply-keyboard labels.
// Returns true if the message was consumed.
func (e *Engine) handleUICallback(ctx context.Context, msg channel.Message) bool {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "steer:") {
		return e.handleSteerCallback(ctx, msg, text)
	}

	// Persistent reply-keyboard labels
	switch text {
	case navNewChat:
		cmd := &channel.Command{Name: channel.CmdNewChat, Result: &channel.CommandResult{}}
		e.handleCommand(ctx, channel.Message{Reply: msg.Reply, Command: cmd})
		if cmd.Result.Error != nil {
			_ = msg.Reply.SendText(ctx, cmd.Result.Error.Error())
		} else {
			_ = msg.Reply.SendText(ctx, "✨ New conversation started.")
		}
		return true
	case navHistory:
		e.uiHistory(ctx, msg, 0)
		return true
	case navSettings:
		e.uiSettings(ctx, msg)
		return true
	case navProfile:
		e.uiProfile(ctx, msg)
		return true
	case "📁 Projects":
		e.cmdListProjects(ctx, msg)
		return true
	case "/cancel", "cancel":
		if e.ui.get() != nil {
			e.ui.clear()
			_ = msg.Reply.SendText(ctx, "Cancelled.")
			e.uiHome(ctx, msg)
			return true
		}
	}

	// Active wizard consumes free text (not commands starting with / except cancel)
	if w := e.ui.get(); w != nil && !strings.HasPrefix(text, "ui:") && !strings.HasPrefix(text, "prv:") && !strings.HasPrefix(text, "mdl:") {
		if strings.HasPrefix(text, "/") && text != "/cancel" {
			// allow other slash commands to break out
			e.ui.clear()
			return false
		}
		return e.handleWizardInput(ctx, msg, w, text)
	}

	if strings.HasPrefix(text, "ui:") {
		return e.routeUI(ctx, msg, text)
	}
	// legacy provider callbacks
	if strings.HasPrefix(text, "prv:") || strings.HasPrefix(text, "mdl:") {
		return e.handleProviderCallback(ctx, msg)
	}
	return false
}

func (e *Engine) routeUI(ctx context.Context, msg channel.Message, data string) bool {
	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return true
	}
	switch parts[1] {
	case "home":
		e.uiHome(ctx, msg)
	case "projects":
		e.cmdListProjects(ctx, msg)
	case "projnew":
		e.cmdNewProject(ctx, msg, "")
	case "projopen":
		if len(parts) >= 3 {
			e.openProjectBySlug(ctx, msg, parts[2])
		}
	case "settings":
		e.uiSettings(ctx, msg)
	case "tasks":
		e.uiTasksBoard(ctx, msg)
	case "taskstop":
		if len(parts) >= 3 {
			e.uiTaskStop(ctx, msg, parts[2])
		}
	case "taskstart":
		if len(parts) >= 3 {
			e.uiTaskStart(ctx, msg, parts[2])
		}
	case "taskget":
		if len(parts) >= 3 {
			e.uiTaskGet(ctx, msg, parts[2])
		}
	case "profile":
		e.uiProfile(ctx, msg)
	case "new":
		cmd := &channel.Command{Name: channel.CmdNewChat, Result: &channel.CommandResult{}}
		e.handleCommand(ctx, channel.Message{Reply: msg.Reply, Command: cmd})
		_ = msg.Reply.SendText(ctx, "✨ New conversation started.")
	case "hist":
		page := 0
		if len(parts) >= 3 {
			page = parsePage(parts[2])
		}
		e.uiHistory(ctx, msg, page)
	case "providers":
		page := 0
		if len(parts) >= 3 {
			page = parsePage(parts[2])
		}
		e.uiProvidersList(ctx, msg, page)
	case "mcp":
		if len(parts) >= 3 {
			e.cmdMCPDetail(ctx, msg, parts[2])
		} else {
			e.cmdMCPList(ctx, msg)
		}
	case "mcpadd":
		e.uiStartAddMCP(ctx, msg)
	case "mcpconn":
		if len(parts) >= 3 && e.mcp != nil {
			name := parts[2]
			if err := e.mcp.Connect(ctx, name); err != nil {
				_ = msg.Reply.SendText(ctx, "Connect failed: "+err.Error())
			} else {
				_ = msg.Reply.SendText(ctx, "Connected: "+name)
			}
			e.cmdMCPDetail(ctx, msg, name)
		}
	case "mcpdisc":
		if len(parts) >= 3 && e.mcp != nil {
			e.mcp.Disconnect(parts[2])
			_ = msg.Reply.SendText(ctx, "Disconnected: "+parts[2])
			e.cmdMCPDetail(ctx, msg, parts[2])
		}
	case "mcprem":
		if len(parts) >= 3 && e.mcp != nil {
			_ = e.mcp.Remove(parts[2])
			_ = msg.Reply.SendText(ctx, "Removed: "+parts[2])
			e.cmdMCPList(ctx, msg)
		}
	case "mcptrans":
		if len(parts) >= 3 {
			typ := parts[2]
			e.ui.setDraft("type", typ)
			if typ == "stdio" {
				e.ui.setStep("command")
				e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
					Text: "Step 3/4 — Command\n\nSend the executable and args, e.g.\nnpx -y @modelcontextprotocol/server-filesystem /data",
					Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:mcp"}}},
				})
			} else {
				e.ui.setStep("url")
				e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
					Text: "Step 3/4 — URL\n\nSend the MCP endpoint URL, e.g.\nhttps://mcp.linear.app/mcp",
					Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:mcp"}}},
				})
			}
		}
	case "mcpauthskip":
		e.finishMCPAdd(ctx, msg, "/skip")
	case "models":
		page := 0
		if len(parts) >= 3 {
			page = parsePage(parts[2])
		}
		e.uiAllModels(ctx, msg, page)
	case "prvadd":
		e.uiStartAddProvider(ctx, msg)
	case "prv":
		if len(parts) >= 3 {
			e.uiProviderDetail(ctx, msg, parts[2], 0)
		}
	case "prvpage":
		if len(parts) >= 4 {
			e.uiProviderDetail(ctx, msg, parts[2], parsePage(parts[3]))
		}
	case "prvedit":
		if len(parts) >= 3 {
			e.uiStartEditProvider(ctx, msg, parts[2])
		}
	case "prvf":
		if len(parts) >= 4 {
			e.uiEditProviderField(ctx, msg, parts[2], parts[3])
		}
	case "prvproto":
		if len(parts) >= 4 {
			e.applyProviderProtocol(ctx, msg, parts[2], parts[3])
		}
	case "prvdelc":
		if len(parts) >= 3 {
			e.uiConfirmDeleteProvider(ctx, msg, parts[2])
		}
	case "prvdel":
		if len(parts) >= 3 {
			id := parts[2]
			if err := e.providers.DeleteProvider(id); err != nil {
				_ = msg.Reply.SendText(ctx, "Delete failed: "+err.Error())
			} else {
				_ = e.reloadLLM()
				_ = msg.Reply.SendText(ctx, "Provider deleted.")
			}
			e.uiProvidersList(ctx, msg, 0)
		}
	case "mdl":
		if len(parts) >= 3 {
			e.uiModelDetail(ctx, msg, parts[2])
		}
	case "mdladd":
		if len(parts) >= 3 {
			e.uiStartAddModel(ctx, msg, parts[2])
		}
	case "mdlsel":
		if len(parts) >= 3 {
			id := parts[2]
			if err := e.providers.SetActiveModel(id); err != nil {
				_ = msg.Reply.SendText(ctx, err.Error())
			} else if err := e.reloadLLM(); err != nil {
				_ = msg.Reply.SendText(ctx, "Selected but reload failed: "+err.Error())
			} else if m, p, err := e.providers.ActivePair(); err == nil {
				_ = msg.Reply.SendText(ctx, fmt.Sprintf("🟢 Active: %s via %s", m.DisplayName, p.Name))
			}
			e.uiModelDetail(ctx, msg, id)
		}
	case "mdlf":
		if len(parts) >= 4 {
			e.uiEditModelField(ctx, msg, parts[2], parts[3])
		}
	case "mdltog":
		if len(parts) >= 4 {
			e.toggleModelCap(ctx, msg, parts[2], parts[3])
		}
	case "mdldelc":
		if len(parts) >= 3 {
			e.uiConfirmDeleteModel(ctx, msg, parts[2])
		}
	case "mdldel":
		if len(parts) >= 3 {
			id := parts[2]
			m, _ := e.providers.GetModel(id)
			pid := m.ProviderID
			if err := e.providers.DeleteModel(id); err != nil {
				_ = msg.Reply.SendText(ctx, err.Error())
			} else {
				_ = e.reloadLLM()
				_ = msg.Reply.SendText(ctx, "Model deleted.")
			}
			if pid != "" {
				e.uiProviderDetail(ctx, msg, pid, 0)
			} else {
				e.uiAllModels(ctx, msg, 0)
			}
		}
	case "soul":
		soul := e.store.ReadSoul()
		if soul == "" {
			soul = "Soul not configured yet."
		}
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "🧬 Soul\n\n" + truncateUI(soul, 3000),
			Rows: [][]channel.Button{{{Text: "⬅ Settings", CallbackData: "ui:settings"}}},
		})
	case "memory":
		files, err := e.store.ListMemory()
		text := "🧠 Memory\n\n"
		if err != nil || len(files) == 0 {
			text += "Memory is empty."
		} else {
			for _, f := range files {
				text += "• " + f.Name + "\n"
			}
		}
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: text,
			Rows: [][]channel.Button{{{Text: "⬅ Settings", CallbackData: "ui:settings"}}},
		})
	case "open":
		if len(parts) >= 3 {
			_ = msg.Reply.SendText(ctx, "Open chat: use /history list in chat UI — chat id "+parts[2]+" (resume via normal messaging after /new if needed).")
		}
	case "proto":
		// add_provider protocol pick: ui:proto:oc
		if len(parts) >= 3 {
			return e.wizardPickProtocol(ctx, msg, parts[2])
		}
	default:
		log.Debug().Str("data", data).Msg("ui: unknown callback")
	}
	return true
}

func (e *Engine) applyProviderProtocol(ctx context.Context, msg channel.Message, id, short string) {
	proto, ok := protocolFromShort(short)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Unknown protocol.")
		return
	}
	if _, err := e.providers.UpdateProvider(id, nil, nil, nil, &proto); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, "Protocol updated: "+proto.DisplayName())
	e.uiProviderDetail(ctx, msg, id, 0)
}

func (e *Engine) toggleModelCap(ctx context.Context, msg channel.Message, id, which string) {
	m, ok := e.providers.GetModel(id)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Model not found.")
		return
	}
	caps := m.Capabilities
	switch which {
	case "tools":
		caps.Tools = !caps.Tools
	case "images":
		caps.Images = !caps.Images
	case "stream":
		caps.Streaming = !caps.Streaming
	}
	if _, err := e.providers.UpdateModel(id, nil, nil, nil, &caps); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	e.uiModelDetail(ctx, msg, id)
}

func (e *Engine) handleWizardInput(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	switch w.Kind {
	case "add_provider":
		return e.wizardAddProvider(ctx, msg, w, text)
	case "edit_provider":
		return e.wizardEditProvider(ctx, msg, w, text)
	case "add_model":
		return e.wizardAddModel(ctx, msg, w, text)
	case "edit_model":
		return e.wizardEditModel(ctx, msg, w, text)
	case "new_project":
		e.ui.clear()
		e.cmdNewProject(ctx, msg, text)
		return true
	case "upgrade_project":
		e.ui.clear()
		e.cmdUpgradeProject(ctx, msg, text)
		return true
	case "mcp_add":
		return e.wizardAddMCP(ctx, msg, w, text)
	case "mcp_oauth":
		return e.wizardMCPOAuth(ctx, msg, w, text)
	}
	e.ui.clear()
	return false
}

func (e *Engine) wizardAddProvider(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	switch w.Step {
	case "name":
		e.ui.setDraft("name", text)
		e.ui.setStep("base_url")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "➕ Step 2/4 — Base URL\n\nSend the API base URL.\nExample: https://openrouter.ai/api/v1",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:providers:0"}}},
		})
	case "base_url":
		if !strings.HasPrefix(text, "http://") && !strings.HasPrefix(text, "https://") {
			_ = msg.Reply.SendText(ctx, "URL must start with http:// or https:// — try again.")
			return true
		}
		e.ui.setDraft("base_url", text)
		e.ui.setStep("protocol")
		var rows [][]channel.Button
		for _, p := range providers.AllProtocols() {
			rows = append(rows, []channel.Button{{
				Text: p.DisplayName(), CallbackData: "ui:proto:" + protocolShort(p),
			}})
		}
		rows = append(rows, []channel.Button{{Text: "❌ Cancel", CallbackData: "ui:providers:0"}})
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "➕ Step 3/4 — Protocol\n\nChoose the API wire format:",
			Rows: rows,
		})
	case "api_key":
		e.ui.setDraft("api_key", text)
		w2 := e.ui.get()
		if w2 == nil {
			return true
		}
		proto, ok := providers.ParseProtocol(w2.Draft["protocol"])
		if !ok {
			proto = providers.ProtocolOpenAIChat
		}
		p, err := e.providers.AddProvider(w2.Draft["name"], w2.Draft["base_url"], proto, w2.Draft["api_key"])
		e.ui.clear()
		if err != nil {
			_ = msg.Reply.SendText(ctx, "Failed: "+err.Error())
			e.uiProvidersList(ctx, msg, 0)
			return true
		}
		_ = msg.Reply.SendText(ctx, "✅ Provider «"+p.Name+"» added.\nKey: "+providers.MaskKey(p.APIKey))
		e.uiProviderDetail(ctx, msg, p.ID, 0)
	}
	return true
}

func (e *Engine) wizardPickProtocol(ctx context.Context, msg channel.Message, short string) bool {
	w := e.ui.get()
	if w == nil || w.Kind != "add_provider" {
		return true
	}
	proto, ok := protocolFromShort(short)
	if !ok {
		_ = msg.Reply.SendText(ctx, "Unknown protocol.")
		return true
	}
	e.ui.setDraft("protocol", string(proto))
	e.ui.setStep("api_key")
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "➕ Step 4/4 — API key\n\nSend the API key. It is stored locally and only shown masked.",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:providers:0"}}},
	})
	return true
}

func (e *Engine) wizardEditProvider(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	id := w.ProviderID
	var name, base, key *string
	switch w.Field {
	case "name":
		name = &text
	case "base_url":
		base = &text
	case "api_key":
		key = &text
	}
	if _, err := e.providers.UpdateProvider(id, name, base, key, nil); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
	} else {
		_ = msg.Reply.SendText(ctx, "✅ Updated.")
	}
	e.ui.clear()
	e.uiProviderDetail(ctx, msg, id, 0)
	return true
}

func (e *Engine) wizardAddModel(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	switch w.Step {
	case "model_id":
		e.ui.setDraft("model_id", text)
		e.ui.setStep("display_name")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "➕ Step 2/3 — Display name\n\nSend a friendly name (or the same as the model id).",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:prv:" + w.ProviderID}}},
		})
	case "display_name":
		e.ui.setDraft("display_name", text)
		e.ui.setStep("context_window")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "➕ Step 3/3 — Context length\n\nSend a number (e.g. 128000), or 0 for default.",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:prv:" + w.ProviderID}}},
		})
	case "context_window":
		w2 := e.ui.get()
		if w2 == nil {
			return true
		}
		cw := atoiDefault(text, 0)
		m, err := e.providers.AddModel(w2.ProviderID, w2.Draft["model_id"], w2.Draft["display_name"], cw)
		e.ui.clear()
		if err != nil {
			_ = msg.Reply.SendText(ctx, "Failed: "+err.Error())
			e.uiProviderDetail(ctx, msg, w.ProviderID, 0)
			return true
		}
		_ = e.providers.SetActiveModel(m.ID)
		_ = e.reloadLLM()
		_ = msg.Reply.SendText(ctx, "✅ Model «"+m.DisplayName+"» added and activated.")
		e.uiModelDetail(ctx, msg, m.ID)
	}
	return true
}

func (e *Engine) wizardEditModel(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	id := w.ModelID
	var display, mid *string
	var cw *int
	switch w.Field {
	case "display":
		display = &text
	case "model_id":
		mid = &text
	case "context":
		n := atoiDefault(text, 0)
		cw = &n
	}
	if _, err := e.providers.UpdateModel(id, display, mid, cw, nil); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
	} else {
		_ = msg.Reply.SendText(ctx, "✅ Updated.")
		_ = e.reloadLLM()
	}
	e.ui.clear()
	e.uiModelDetail(ctx, msg, id)
	return true
}
