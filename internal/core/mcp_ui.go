package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/mcp"
)

func (e *Engine) cmdMCPList(ctx context.Context, msg channel.Message) {
	if e.mcp == nil {
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return
	}
	list, err := e.mcp.ListConfigured()
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	var b strings.Builder
	b.WriteString("🔌 MCP Servers\n\n")
	if len(list) == 0 {
		b.WriteString("None configured yet.\nAdd with /mcp_add or the button below.")
	} else {
		b.WriteString(e.mcp.StatusLine())
		b.WriteString("\n")
	}
	rows := [][]channel.Button{
		{{Text: "➕ Add MCP server", CallbackData: "ui:mcpadd"}},
	}
	for _, s := range list {
		rows = append(rows, []channel.Button{
			{Text: "▶ " + truncateUI(s.Name, 28), CallbackData: "ui:mcp:" + s.Name},
		})
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Settings", CallbackData: "ui:settings"}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) cmdMCPDetail(ctx context.Context, msg channel.Message, name string) {
	if e.mcp == nil {
		return
	}
	srv, ok, err := e.mcp.Store().Get(name)
	if err != nil || !ok {
		_ = msg.Reply.SendText(ctx, "Not found: "+name)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔌 %s\nTransport: %s\n", srv.Name, srv.Transport())
	if srv.Command != "" {
		fmt.Fprintf(&b, "Command: %s %s\n", srv.Command, strings.Join(srv.Args, " "))
	}
	if srv.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", srv.URL)
	}
	if len(srv.Headers) > 0 {
		b.WriteString("Headers: set\n")
	}
	if len(srv.Env) > 0 {
		b.WriteString("Env: set\n")
	}
	if srv.Disabled {
		b.WriteString("Status: disabled\n")
	}
	rows := [][]channel.Button{
		{{Text: "🔗 Connect", CallbackData: "ui:mcpconn:" + srv.Name},
			{Text: "⏸ Disconnect", CallbackData: "ui:mcpdisc:" + srv.Name}},
	}
	if srv.Transport() == mcp.TransportStdio {
		rows = append(rows, []channel.Button{
			{Text: "🔑 Update env / key", CallbackData: "ui:mcpkey:" + srv.Name},
		})
	} else {
		rows = append(rows, []channel.Button{
			{Text: "🔐 Authorize (OAuth)", CallbackData: "ui:mcpauth:" + srv.Name},
			{Text: "🔑 Update API key", CallbackData: "ui:mcpkey:" + srv.Name},
		})
	}
	rows = append(rows,
		[]channel.Button{{Text: "🗑 Remove", CallbackData: "ui:mcprem:" + srv.Name}},
		[]channel.Button{{Text: "⬅ MCP list", CallbackData: "ui:mcp"}},
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) uiStartAddMCP(ctx context.Context, msg channel.Message) {
	e.ui.set(&uiWizard{Kind: "mcp_add", Step: "name", Draft: map[string]string{}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "🔌 Add MCP server\n\nStep 1/4 — Name\n\nSend a short name (e.g. filesystem, github, linear).",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:mcp"}}},
	})
}

func (e *Engine) wizardAddMCP(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	text = strings.TrimSpace(text)
	switch w.Step {
	case "name":
		if text == "" {
			_ = msg.Reply.SendText(ctx, "Name required.")
			return true
		}
		e.ui.setDraft("name", text)
		e.ui.setStep("transport")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Step 2/4 — Transport\n\nChoose how to reach the server:",
			Rows: [][]channel.Button{
				{{Text: "stdio (local command)", CallbackData: "ui:mcptrans:stdio"}},
				{{Text: "http (Streamable HTTP)", CallbackData: "ui:mcptrans:http"}},
				{{Text: "sse (legacy)", CallbackData: "ui:mcptrans:sse"}},
				{{Text: "❌ Cancel", CallbackData: "ui:mcp"}},
			},
		})
		return true
	case "command":
		// stdio: "command arg1 arg2"
		if text == "" {
			_ = msg.Reply.SendText(ctx, "Send the command (and optional args), e.g.\nnpx -y @modelcontextprotocol/server-filesystem /tmp")
			return true
		}
		parts := strings.Fields(text)
		e.ui.setDraft("command", parts[0])
		if len(parts) > 1 {
			e.ui.setDraft("args", strings.Join(parts[1:], "\x00"))
		}
		e.ui.setStep("auth")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Step 4/4 — Auth (optional)\n\nSend env KEY=VALUE pairs, one per line (e.g. GITHUB_PERSONAL_ACCESS_TOKEN=ghp_…),\nor send /skip if none.",
			Rows: [][]channel.Button{
				{{Text: "⏭ Skip", CallbackData: "ui:mcpauthskip"}},
				{{Text: "❌ Cancel", CallbackData: "ui:mcp"}},
			},
		})
		return true
	case "url":
		if text == "" || !strings.HasPrefix(text, "http") {
			_ = msg.Reply.SendText(ctx, "Send a full URL, e.g. https://mcp.example.com/mcp")
			return true
		}
		e.ui.setDraft("url", text)
		e.ui.setStep("auth")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Step 4/4 — Auth (optional)\n\n• Send a Bearer token / API key, or header lines like:\n  Authorization: Bearer sk-…\n• Or tap 🔐 Authorize to use OAuth (browser login)\n• Or /skip / ⏭ if no auth is needed",
			Rows: [][]channel.Button{
				{{Text: "🔐 Authorize (OAuth)", CallbackData: "ui:mcpaddoauth"}},
				{{Text: "⏭ Skip", CallbackData: "ui:mcpauthskip"}},
				{{Text: "❌ Cancel", CallbackData: "ui:mcp"}},
			},
		})
		return true
	case "auth":
		e.finishMCPAdd(ctx, msg, text)
		return true
	}
	return false
}

func (e *Engine) finishMCPAdd(ctx context.Context, msg channel.Message, authText string) {
	w := e.ui.get()
	if w == nil || w.Kind != "mcp_add" {
		return
	}
	name := w.Draft["name"]
	typ := w.Draft["type"]
	if typ == "" {
		typ = mcp.TransportStdio
	}
	srv := mcp.Server{Name: name, Type: typ}
	if typ == mcp.TransportStdio {
		srv.Command = w.Draft["command"]
		if a := w.Draft["args"]; a != "" {
			srv.Args = strings.Split(a, "\x00")
		}
		if authText != "" && authText != "/skip" {
			srv.Env = parseEnvLines(authText)
		}
	} else {
		srv.URL = w.Draft["url"]
		if authText != "" && authText != "/skip" {
			srv.Headers = parseHeaderOrToken(authText)
		}
	}
	e.ui.clear()
	if e.mcp == nil {
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return
	}
	if err := e.mcp.Add(srv); err != nil {
		_ = msg.Reply.SendText(ctx, "Save failed: "+err.Error())
		return
	}
	connErr := e.mcp.Connect(ctx, srv.Name)
	msgText := fmt.Sprintf("✅ MCP «%s» saved (%s).", srv.Name, srv.Transport())
	if connErr != nil {
		msgText += "\nConnect deferred: " + connErr.Error()
	} else {
		msgText += "\nConnected. Agent can use mcp_list / call_mcp_tool."
	}
	_ = msg.Reply.SendText(ctx, msgText)
	e.cmdMCPList(ctx, msg)
}

func parseEnvLines(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

func parseHeaderOrToken(text string) map[string]string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// If it looks like a header block
	if strings.Contains(text, ":") && (strings.Contains(strings.ToLower(text), "authorization") || strings.Contains(text, "\n")) {
		out := map[string]string{}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if i := strings.Index(line, ":"); i > 0 {
				out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
			}
		}
		return out
	}
	// bare token → Bearer
	token := text
	if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = "Bearer " + token
	}
	return map[string]string{"Authorization": token}
}

func (e *Engine) startMCPOAuth(ctx context.Context, msg channel.Message, name string) {
	if e.mcp == nil {
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return
	}
	authURL, err := e.mcp.BeginOAuth(ctx, name)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Authorize failed: "+err.Error())
		return
	}
		e.ui.set(&uiWizard{Kind: "mcp_oauth", Step: "code", Draft: map[string]string{"name": name}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "🔐 Authorize «" + name + "»\n\n" +
			"1. Open this link (or tap the button):\n" + authURL + "\n\n" +
			"2. Sign in and approve access.\n" +
			"3. Copy the full redirect URL (or just the code) and paste it here.\n\n" +
			"Waiting for callback…",
		Rows: [][]channel.Button{
			{{Text: "🌐 Open authorization page", URL: authURL}},
			{{Text: "❌ Cancel", CallbackData: "ui:mcp:" + name}},
		},
	})
}

func (e *Engine) wizardMCPOAuth(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	name := w.Draft["name"]
	text = strings.TrimSpace(text)
	if text == "" {
		_ = msg.Reply.SendText(ctx, "Paste the redirect URL or authorization code.")
		return true
	}
	e.ui.clear()
	if err := e.mcp.CompleteOAuth(ctx, name, text); err != nil {
		_ = msg.Reply.SendText(ctx, "Token exchange failed: "+err.Error())
		e.cmdMCPDetail(ctx, msg, name)
		return true
	}
	_ = msg.Reply.SendText(ctx, "✅ Authorized and connected «"+name+"». Tools hot-reloaded.")
	e.cmdMCPDetail(ctx, msg, name)
	return true
}


// finishMCPAddThenOAuth saves the server from the add wizard without a key,
// then immediately starts the OAuth browser flow.
func (e *Engine) finishMCPAddThenOAuth(ctx context.Context, msg channel.Message) {
	w := e.ui.get()
	if w == nil || w.Kind != "mcp_add" {
		_ = msg.Reply.SendText(ctx, "No active add wizard.")
		return
	}
	name := w.Draft["name"]
	typ := w.Draft["type"]
	if typ == "" {
		typ = mcp.TransportStdio
	}
	if typ == mcp.TransportStdio {
		_ = msg.Reply.SendText(ctx, "OAuth is only for http/sse MCP servers.")
		return
	}
	srv := mcp.Server{Name: name, Type: typ, URL: w.Draft["url"]}
	e.ui.clear()
	if e.mcp == nil {
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return
	}
	if err := e.mcp.Add(srv); err != nil {
		_ = msg.Reply.SendText(ctx, "Save failed: "+err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ MCP «%s» saved. Starting OAuth…", name))
	e.startMCPOAuth(ctx, msg, name)
}

func (e *Engine) startMCPUpdateKey(ctx context.Context, msg channel.Message, name string) {
	if e.mcp == nil {
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return
	}
	srv, ok, err := e.mcp.Store().Get(name)
	if err != nil || !ok {
		_ = msg.Reply.SendText(ctx, "Server not found: "+name)
		return
	}
	e.ui.set(&uiWizard{Kind: "mcp_key", Step: "value", Draft: map[string]string{"name": name, "type": srv.Transport()}})
	hint := "Send a Bearer token / API key, or header lines (Authorization: Bearer …).\n\nSend /clear to remove stored keys/headers."
	if srv.Transport() == mcp.TransportStdio {
		hint = "Send env KEY=VALUE pairs, one per line.\n\nSend /clear to remove stored env vars."
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "🔑 Update auth for «" + name + "»\n\n" + hint,
		Rows: [][]channel.Button{
			{{Text: "❌ Cancel", CallbackData: "ui:mcp:" + name}},
		},
	})
}

func (e *Engine) wizardMCPUpdateKey(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	name := w.Draft["name"]
	text = strings.TrimSpace(text)
	if text == "" {
		_ = msg.Reply.SendText(ctx, "Send a key, headers, env lines, or /clear.")
		return true
	}
	if e.mcp == nil {
		e.ui.clear()
		_ = msg.Reply.SendText(ctx, "MCP not available.")
		return true
	}
	srv, ok, err := e.mcp.Store().Get(name)
	if err != nil || !ok {
		e.ui.clear()
		_ = msg.Reply.SendText(ctx, "Server not found: "+name)
		return true
	}
	if text == "/clear" {
		srv.Headers = nil
		srv.Env = nil
	} else if srv.Transport() == mcp.TransportStdio {
		srv.Env = parseEnvLines(text)
	} else {
		srv.Headers = parseHeaderOrToken(text)
	}
	e.ui.clear()
	if err := e.mcp.Add(srv); err != nil {
		_ = msg.Reply.SendText(ctx, "Update failed: "+err.Error())
		return true
	}
	e.mcp.Disconnect(name)
	if cerr := e.mcp.Connect(ctx, name); cerr != nil {
		_ = msg.Reply.SendText(ctx, "✅ Key updated. Connect deferred: "+cerr.Error())
	} else {
		_ = msg.Reply.SendText(ctx, "✅ Key updated and reconnected.")
	}
	e.cmdMCPDetail(ctx, msg, name)
	return true
}
