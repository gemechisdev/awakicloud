package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/providers"
)

// handleProvidersCommand implements /providers, /models and related actions.
func (e *Engine) handleProvidersCommand(ctx context.Context, cmd *channel.Command) {
	if e.providers == nil {
		cmd.Result.Error = fmt.Errorf("providers store not initialized")
		return
	}
	switch cmd.Name {
	case channel.CmdProviders:
		e.cmdListProviders(cmd)
	case channel.CmdModels:
		e.cmdListModels(cmd)
	case channel.CmdProviderAdd:
		e.cmdAddProvider(cmd)
	case channel.CmdModelAdd:
		e.cmdAddModel(cmd)
	}
}

func (e *Engine) cmdListProviders(cmd *channel.Command) {
	list := e.providers.ListProviders()
	activeID := ""
	if m, p, err := e.providers.ActivePair(); err == nil {
		activeID = p.ID
		_ = m
	}
	var b strings.Builder
	b.WriteString("LLM Providers (Awaki-style: protocol + base URL + key)\n\n")
	if len(list) == 0 {
		b.WriteString("None yet. Add one:\n")
		b.WriteString("/provider_add name|protocol|base_url|api_key\n\n")
		b.WriteString("protocol: openai_chat_completions | anthropic_messages | google_gemini | openai_responses\n")
		b.WriteString("Example:\n")
		b.WriteString("/provider_add OpenRouter|openai_chat_completions|https://openrouter.ai/api/v1|sk-or-...\n")
		cmd.Result.Data = map[string]string{"text": b.String()}
		return
	}
	var rows [][]channel.Button
	for _, p := range list {
		mark := ""
		if p.ID == activeID {
			mark = " ✅"
		}
		keyPreview := p.APIKey
		if len(keyPreview) > 8 {
			keyPreview = keyPreview[:4] + "…" + keyPreview[len(keyPreview)-4:]
		}
		b.WriteString(fmt.Sprintf("• %s%s\n  protocol: %s\n  base: %s\n  key: %s\n  id: %s\n\n",
			p.Name, mark, p.Protocol.DisplayName(), p.BaseURL, keyPreview, shortID(p.ID)))
		rows = append(rows, []channel.Button{
			{Text: "Models: " + p.Name, CallbackData: "prv:models:" + p.ID},
			{Text: "🗑 " + p.Name, CallbackData: "prv:del:" + p.ID},
		})
	}
	b.WriteString("Add: /provider_add name|protocol|base_url|api_key\n")
	b.WriteString("Then: /model_add provider_id_or_name|model_id|display_name")
	cmd.Result.Payload = channel.KeyboardMessage{Text: b.String(), Rows: rows}
}

func (e *Engine) cmdListModels(cmd *channel.Command) {
	list := e.providers.ListModels()
	active := e.providers.ActiveModelID()
	var b strings.Builder
	b.WriteString("Models\n\n")
	if len(list) == 0 {
		b.WriteString("None. Add a provider first, then:\n/model_add <provider>|model_id|Display Name\n")
		cmd.Result.Data = map[string]string{"text": b.String()}
		return
	}
	var rows [][]channel.Button
	for _, m := range list {
		p, _ := e.providers.GetProvider(m.ProviderID)
		mark := ""
		if m.ID == active {
			mark = " ✅"
		}
		b.WriteString(fmt.Sprintf("• %s%s (%s)\n  wire: %s\n  provider: %s\n\n",
			m.DisplayName, mark, shortID(m.ID), m.ModelID, p.Name))
		rows = append(rows, []channel.Button{
			{Text: "Select " + m.DisplayName, CallbackData: "mdl:sel:" + m.ID},
			{Text: "🗑", CallbackData: "mdl:del:" + m.ID},
		})
	}
	cmd.Result.Payload = channel.KeyboardMessage{Text: b.String(), Rows: rows}
}

func (e *Engine) cmdAddProvider(cmd *channel.Command) {
	raw := ""
	if cmd.Args != nil {
		raw = cmd.Args["raw"]
	}
	// format: name|protocol|base_url|api_key
	parts := splitPipe(raw)
	if len(parts) < 4 {
		cmd.Result.Data = map[string]string{"text": "Usage:\n/provider_add name|protocol|base_url|api_key\n\nProtocols:\n- openai_chat_completions\n- anthropic_messages\n- google_gemini\n- openai_responses\n\nBase URL and API key are required (nothing is hardcoded)."}
		return
	}
	proto, ok := providers.ParseProtocol(parts[1])
	if !ok {
		cmd.Result.Error = fmt.Errorf("unknown protocol %q", parts[1])
		return
	}
	p, err := e.providers.AddProvider(parts[0], parts[2], proto, parts[3])
	if err != nil {
		cmd.Result.Error = err
		return
	}
	cmd.Result.Data = map[string]string{"text": fmt.Sprintf("Provider added: %s\nid: %s\nprotocol: %s\nbase: %s\n\nNext: /model_add %s|your-model-id|Display Name",
		p.Name, p.ID, p.Protocol, p.BaseURL, p.ID)}
}

func (e *Engine) cmdAddModel(cmd *channel.Command) {
	raw := ""
	if cmd.Args != nil {
		raw = cmd.Args["raw"]
	}
	parts := splitPipe(raw)
	if len(parts) < 2 {
		cmd.Result.Data = map[string]string{"text": "Usage:\n/model_add provider_id_or_name|model_id|optional display name"}
		return
	}
	providerRef := parts[0]
	modelID := parts[1]
	display := modelID
	if len(parts) >= 3 && parts[2] != "" {
		display = parts[2]
	}
	pid := providerRef
	if _, ok := e.providers.GetProvider(providerRef); !ok {
		// match by name
		for _, p := range e.providers.ListProviders() {
			if strings.EqualFold(p.Name, providerRef) {
				pid = p.ID
				break
			}
		}
	}
	m, err := e.providers.AddModel(pid, modelID, display, 0)
	if err != nil {
		cmd.Result.Error = err
		return
	}
	_ = e.providers.SetActiveModel(m.ID)
	if err := e.reloadLLM(); err != nil {
		cmd.Result.Data = map[string]string{"text": fmt.Sprintf("Model added and selected, but LLM reload failed: %v", err)}
		return
	}
	cmd.Result.Data = map[string]string{"text": fmt.Sprintf("Model active: %s (%s)\nReload OK — next messages use this model.", m.DisplayName, m.ModelID)}
}

// handleProviderCallback processes inline button callbacks: prv:* and mdl:*.
func (e *Engine) handleProviderCallback(ctx context.Context, msg channel.Message) bool {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "prv:") && !strings.HasPrefix(text, "mdl:") {
		return false
	}
	if e.providers == nil {
		_ = msg.Reply.SendText(ctx, "providers store not ready")
		return true
	}
	parts := strings.Split(text, ":")
	if len(parts) < 3 {
		return true
	}
	switch parts[0] + ":" + parts[1] {
	case "prv:del":
		id := parts[2]
		if err := e.providers.DeleteProvider(id); err != nil {
			_ = msg.Reply.SendText(ctx, "Delete failed: "+err.Error())
			return true
		}
		_ = e.reloadLLM() // may clear active
		_ = msg.Reply.SendText(ctx, "Provider deleted.")
	case "prv:models":
		id := parts[2]
		models := e.providers.ListModelsByProvider(id)
		var b strings.Builder
		b.WriteString("Models for provider\n\n")
		var rows [][]channel.Button
		for _, m := range models {
			b.WriteString("• " + m.DisplayName + " (" + m.ModelID + ")\n")
			rows = append(rows, []channel.Button{
				{Text: "Use " + m.DisplayName, CallbackData: "mdl:sel:" + m.ID},
			})
		}
		if len(models) == 0 {
			b.WriteString("None. /model_add " + id + "|model_id|Name")
			_ = msg.Reply.SendText(ctx, b.String())
			return true
		}
		// send as plain text listing + select via next command if keyboard not on Reply
		_ = msg.Reply.SendText(ctx, b.String())
		for _, m := range models {
			_ = msg.Reply.SendText(ctx, "Select: send callback mdl:sel:"+m.ID+" or use /models buttons")
			_ = m
		}
		_ = rows
	case "mdl:sel":
		id := parts[2]
		if err := e.providers.SetActiveModel(id); err != nil {
			_ = msg.Reply.SendText(ctx, err.Error())
			return true
		}
		if err := e.reloadLLM(); err != nil {
			_ = msg.Reply.SendText(ctx, "Selected but reload failed: "+err.Error())
			return true
		}
		m, p, _ := e.providers.ActivePair()
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("Active model: %s via %s\n%s", m.DisplayName, p.Name, p.BaseURL))
	case "mdl:del":
		id := parts[2]
		if err := e.providers.DeleteModel(id); err != nil {
			_ = msg.Reply.SendText(ctx, err.Error())
			return true
		}
		_ = e.reloadLLM()
		_ = msg.Reply.SendText(ctx, "Model deleted.")
	}
	return true
}

func splitPipe(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
