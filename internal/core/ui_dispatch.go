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
	if strings.HasPrefix(text, "mode:") {
		return e.handleModeCallback(ctx, msg, text)
	}

	// Persistent reply-keyboard labels
	switch text {
	case navNewChat:
		e.cmdNewChat(ctx, msg, "")
		return true
	case navChats, navHistory:
		e.uiChats(ctx, msg, scopeAll, 0)
		return true
	case navSettings:
		e.uiSettings(ctx, msg)
		return true
	case navProfile:
		e.uiProfile(ctx, msg)
		return true
	case navProjects:
		e.uiProjects(ctx, msg, 0)
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
	case "noop":
		// pagination counter / disabled buttons
	case "projects":
		e.uiProjects(ctx, msg, argPage(parts, 2))
	case "projnew":
		e.startNewProjectWizard(ctx, msg, "view", "")
	case "projopen", "pd":
		if len(parts) >= 3 {
			e.uiProjectDetail(ctx, msg, parts[2], "")
		}
	case "psync":
		if len(parts) >= 3 {
			e.uiProjectSync(ctx, msg, parts[2])
		}
	case "pren":
		if len(parts) >= 3 {
			e.startRenameProjectWizard(ctx, msg, parts[2])
		}
	case "pmv":
		if len(parts) >= 3 {
			e.cmdMove(ctx, msg, parts[2])
		}
	case "pnm": // new project, then move chat <ref> into it
		if len(parts) >= 3 {
			e.startNewProjectWizard(ctx, msg, "move", parts[2])
		}
	case "ncp": // new project, then start a chat in it
		e.startNewProjectWizard(ctx, msg, "chat", "")
	case "new":
		e.uiNewChatPicker(ctx, msg, 0)
	case "newp":
		e.uiNewChatPicker(ctx, msg, argPage(parts, 2))
	case "nc":
		if len(parts) >= 3 {
			slug := parts[2]
			if slug == "-" {
				slug = ""
			}
			e.startChat(ctx, msg, slug)
		}
	case "chats":
		scope := scopeAll
		if len(parts) >= 3 {
			scope = parseScope(parts[2])
		}
		e.uiChats(ctx, msg, scope, argPage(parts, 3))
	case "hist": // legacy
		e.uiChats(ctx, msg, scopeAll, argPage(parts, 2))
	case "cfp":
		e.uiChatFilterProjects(ctx, msg, argPage(parts, 2))
	case "co", "open", "chatopen":
		if len(parts) >= 3 {
			e.openChat(ctx, msg, parts[2])
		}
	case "cm":
		if len(parts) >= 3 {
			if info, err := e.resolveChatRef(parts[2]); err == nil {
				e.uiChatCard(ctx, msg, info.ID, "")
			} else {
				e.uiChats(ctx, msg, scopeAll, 0)
			}
		}
	case "cur":
		e.uiCurrentChat(ctx, msg)
	case "cmv":
		if len(parts) >= 3 {
			e.uiMovePicker(ctx, msg, parts[2], argPage(parts, 3))
		}
	case "cmt":
		if len(parts) >= 4 {
			e.moveChat(ctx, msg, parts[2], parts[3])
		}
	case "cren":
		if len(parts) >= 3 {
			e.startRenameChatWizard(ctx, msg, parts[2])
		}
	case "settings":
		e.uiSettings(ctx, msg)
	case "mode":
		e.uiModeMenu(ctx, msg)
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
	case "skills":
		page := 0
		if len(parts) >= 3 {
			page = parsePage(parts[2])
		}
		e.cmdSkillsList(ctx, msg, page)
	case "skilladd":
		e.uiStartAddSkill(ctx, msg)
	case "skillmarket":
		e.cmdSkillMarket(ctx, msg)
	case "marketadd":
		if len(parts) >= 3 {
			src := strings.Join(parts[2:], "/")
			// ui:marketadd:anthropics/skills → parts anthropics, skills
			if len(parts) >= 4 {
				src = parts[2] + "/" + parts[3]
			}
			e.handlePluginArgs(ctx, msg, "marketplace add "+src)
		}
	case "skill":
		if len(parts) >= 3 {
			e.cmdSkillDetail(ctx, msg, strings.Join(parts[2:], ":"))
		}
	case "skillview":
		if len(parts) >= 3 {
			e.cmdSkillView(ctx, msg, strings.Join(parts[2:], ":"))
		}
	case "skillrem":
		if len(parts) >= 3 && e.store != nil {
			name := strings.Join(parts[2:], ":")
			_ = e.store.DeleteSkill(name)
			_ = msg.Reply.SendText(ctx, "Removed skill: "+name)
			e.cmdSkillsList(ctx, msg, 0)
		}
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
	case "mcpauth":
		// ui:mcpauth:<server-name>
		if len(parts) >= 3 {
			name := strings.Join(parts[2:], ":")
			e.startMCPOAuth(ctx, msg, name)
		}
	case "mcpaddoauth":
		// During add wizard: save without key, then start OAuth
		e.finishMCPAddThenOAuth(ctx, msg)
	case "mcpkey":
		// ui:mcpkey:<server-name> — update API key / headers
		if len(parts) >= 3 {
			name := strings.Join(parts[2:], ":")
			e.startMCPUpdateKey(ctx, msg, name)
		}
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
	case "mem":
		if len(parts) >= 3 {
			name := strings.Join(parts[2:], ":")
			content, err := e.store.ReadMemory(name)
			text := "🧠 Memory — " + name + "\n\n"
			if err != nil {
				text += "⚠️ Could not read: " + err.Error()
			} else {
				text += truncateUI(content, 4000)
			}
			e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
				Text: text,
				Rows: [][]channel.Button{
					{{Text: "◀ Back", CallbackData: "ui:memory"}},
					{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
				},
			})
		}
	case "memory":
		files, err := e.store.ListMemory()
		text := "🧠 Memory\n\n"
		if err != nil || len(files) == 0 {
			text += "Memory is empty."
		} else {
			text += "Select a file to read:\n\n"
			var rows [][]channel.Button
			for _, f := range files {
				text += "• " + f.Name + "\n"
				rows = append(rows, []channel.Button{
					{Text: "📄 " + truncateUI(f.Name, 40), CallbackData: "ui:mem:" + f.Name},
				})
			}
			rows = append(rows, []channel.Button{{Text: "⬅ Settings", CallbackData: "ui:settings"}})
			e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: text, Rows: rows})
			return true
		}
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: text,
			Rows: [][]channel.Button{{{Text: "⬅ Settings", CallbackData: "ui:settings"}}},
		})
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
		e.createProject(ctx, msg, text, w.Draft["then"], w.Draft["ref"])
		return true
	case "rename_chat":
		e.ui.clear()
		e.renameChat(ctx, msg, w.Draft["ref"], text)
		return true
	case "rename_project":
		e.ui.clear()
		e.renameProject(ctx, msg, w.Draft["slug"], text)
		return true
	case "upgrade_project":
		e.ui.clear()
		e.cmdUpgradeProject(ctx, msg, text)
		return true
	case "mcp_add":
		return e.wizardAddMCP(ctx, msg, w, text)
	case "mcp_oauth":
		return e.wizardMCPOAuth(ctx, msg, w, text)
	case "skill_add":
		return e.wizardAddSkill(ctx, msg, w, text)
	case "mcp_key":
		return e.wizardMCPUpdateKey(ctx, msg, w, text)
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

// argPage parses an optional numeric page argument at parts[i].
func argPage(parts []string, i int) int {
	if len(parts) > i {
		return parsePage(parts[i])
	}
	return 0
}
