package core

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	skillpkg "github.com/gemechischala/awaki/internal/skills"
)

func (e *Engine) skillMgr() *skillpkg.Manager {
	if e.store == nil {
		return nil
	}
	return skillpkg.NewManager(e.store)
}

// skillsRawArgs returns the argument string of the current slash command.
func skillsRawArgs(msg channel.Message) string {
	if msg.Command != nil && msg.Command.Args != nil {
		if raw := strings.TrimSpace(msg.Command.Args["raw"]); raw != "" {
			return raw
		}
	}
	fields := strings.Fields(strings.TrimSpace(msg.Text))
	if len(fields) > 1 {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), fields[0]))
	}
	return ""
}

const skillsUsage = "Usage:\n" +
	"/skills — list installed skills\n" +
	"/skills add <url-or-repo> [--skill name] — install from a URL\n" +
	"/skills remove <name>\n\n" +
	"Sources:\n" +
	"• owner/repo, owner/repo@skill\n" +
	"• https://github.com/owner/repo/tree/main/skills/foo\n" +
	"• https://skills.sh/owner/repo/skill\n" +
	"• https://smithery.ai/skills/owner/skill\n" +
	"• https://example.com (/.well-known/skills)\n" +
	"• direct .zip / SKILL.md URL\n\n" +
	"A pasted `npx skills add <url>` command also works."

// cmdSkills handles /skills [list|add|remove|market] ...
func (e *Engine) cmdSkills(ctx context.Context, msg channel.Message) {
	args := skillsRawArgs(msg)
	fields := strings.Fields(args)
	if len(fields) == 0 {
		e.cmdSkillsList(ctx, msg)
		return
	}
	switch strings.ToLower(fields[0]) {
	case "list", "ls":
		e.cmdSkillsList(ctx, msg)
	case "add", "install", "i":
		e.cmdSkillsAdd(ctx, msg, fields[1:])
	case "remove", "rm", "delete", "uninstall":
		if len(fields) < 2 {
			_ = msg.Reply.SendText(ctx, "Usage: /skills remove <name>")
			return
		}
		mgr := e.skillMgr()
		if mgr == nil {
			_ = msg.Reply.SendText(ctx, "Skills not available.")
			return
		}
		if err := mgr.Remove(fields[1]); err != nil {
			_ = msg.Reply.SendText(ctx, "Remove failed: "+err.Error())
			return
		}
		_ = msg.Reply.SendText(ctx, "Removed skill: "+fields[1])
		e.cmdSkillsList(ctx, msg)
	case "market", "marketplace":
		e.cmdSkillMarket(ctx, msg)
	case "help", "?":
		_ = msg.Reply.SendText(ctx, skillsUsage)
	default:
		// `/skills https://…` or `/skills npx skills add …` without "add".
		if skillpkg.LooksLikeSource(fields[0]) || strings.EqualFold(fields[0], "npx") {
			e.cmdSkillsAdd(ctx, msg, fields)
			return
		}
		_ = msg.Reply.SendText(ctx, skillsUsage)
	}
}

// cmdSkillsAdd implements `/skills add <url-to-skill> [--skill name] [--all]`.
func (e *Engine) cmdSkillsAdd(ctx context.Context, msg channel.Message, fields []string) {
	src, opts, err := skillpkg.ParseAddArgs(fields)
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error()+"\n\n"+skillsUsage)
		return
	}
	e.installSkillsFromSource(ctx, msg, src, opts)
}

func (e *Engine) installSkillsFromSource(ctx context.Context, msg channel.Message, src string, opts skillpkg.InstallOptions) {
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}
	_ = msg.Reply.SendText(ctx, "Installing from "+src+"…")
	list, err := mgr.InstallFromSource(ctx, src, opts)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Install failed: "+err.Error())
		return
	}
	e.ui.clear()
	if len(list) == 1 {
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Installed «%s» (%s) from %s. Package synced to DB.", list[0].Name, list[0].LevelLabel(), src))
		e.cmdSkillDetail(ctx, msg, list[0].Name)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Installed %d skills from %s:\n", len(list), src)
	for _, sk := range list {
		fmt.Fprintf(&b, "• %s (%s)\n", sk.Name, sk.LevelLabel())
	}
	_ = msg.Reply.SendText(ctx, b.String())
	e.cmdSkillsList(ctx, msg)
}

func (e *Engine) cmdSkillsList(ctx context.Context, msg channel.Message) {
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}
	list, err := mgr.List()
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Error: "+err.Error())
		return
	}
	var b strings.Builder
	b.WriteString("🧩 Agent Skills\n\n")
	b.WriteString("On disk: /data/skills/{name}/SKILL.md\n")
	b.WriteString("(+ optional references/, scripts/, assets/ per skill)\n")
	b.WriteString("Durable: each package is zipped into SQLite (hash-checked) and follows cloud DB sync.\n\n")
	if len(list) == 0 {
		b.WriteString("None installed. Tap Add, upload a skill .zip / .md, or run /skills add <url>.")
	} else {
		for _, s := range list {
			fmt.Fprintf(&b, "• %s — %s\n  %s\n", s.Name, s.LevelLabel(), s.Description)
		}
	}
	rows := [][]channel.Button{
		{{Text: "➕ Add skill", CallbackData: "ui:skilladd"}},
		{{Text: "🏪 Marketplace", CallbackData: "ui:skillmarket"}},
		{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
	}
	for _, s := range list {
		rows = append(rows, []channel.Button{{
			Text:         "📦 " + s.Name,
			CallbackData: "ui:skill:" + s.Name,
		}})
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) cmdSkillDetail(ctx context.Context, msg channel.Message, name string) {
	mgr := e.skillMgr()
	if mgr == nil {
		return
	}
	sk, ok, err := mgr.Get(name)
	if err != nil || !ok {
		_ = msg.Reply.SendText(ctx, "Skill not found: "+name)
		e.cmdSkillsList(ctx, msg)
		return
	}
	tree, _ := mgr.Tree(name)
	var b strings.Builder
	fmt.Fprintf(&b, "📦 %s\nLevel: %s\n%s\n\nPath: %s\n", sk.Name, sk.LevelLabel(), sk.Description, sk.Dir)
	if tree != "" {
		b.WriteString("\nFiles:\n")
		b.WriteString(tree)
	}
	rows := [][]channel.Button{
		{{Text: "📄 View SKILL.md", CallbackData: "ui:skillview:" + sk.Name}},
		{{Text: "🗑 Remove", CallbackData: "ui:skillrem:" + sk.Name}},
		{{Text: "⬅ Skills", CallbackData: "ui:skills"}},
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) cmdSkillView(ctx context.Context, msg channel.Message, name string) {
	if e.store == nil {
		return
	}
	body, err := e.store.ReadSkill(name)
	if err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	if len(body) > 3500 {
		body = body[:3500] + "\n…(truncated)"
	}
	_ = msg.Reply.SendText(ctx, "SKILL.md — "+name+"\n\n"+body)
	e.cmdSkillDetail(ctx, msg, name)
}

func (e *Engine) uiStartAddSkill(ctx context.Context, msg channel.Message) {
	e.ui.set(&uiWizard{Kind: "skill_add", Step: "name", Draft: map[string]string{}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: "➕ Add skill\n\n" +
			"Paste a skill URL (GitHub, skills.sh, smithery.ai, …) to install it directly,\nor upload now (document) / continue the wizard:\n\n" +
			"• Single SKILL.md or {name}.md (L1)\n" +
			"• Zip with SKILL.md at root (one skill)\n" +
			"• Zip with my-skill/SKILL.md + refs/scripts/assets\n" +
			"• Zip with multiple */SKILL.md folders (multi-install)\n" +
			"• Zip with skills/foo and skills/bar\n\n" +
			"Format: Agent Skills style — one folder per skill, SKILL.md required.\n\n" +
			"Wizard Step 1/3 — Name (e.g. pdf-processing):",
		Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:skills"}}},
	})
}

func (e *Engine) wizardAddSkill(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	text = strings.TrimSpace(text)
	switch w.Step {
	case "name":
		if text == "" {
			_ = msg.Reply.SendText(ctx, "Send a skill name.")
			return true
		}
		// Pasted URL / `npx skills add <url>` → install from the source instead.
		if strings.Contains(text, "://") {
			src, opts, err := skillpkg.ParseAddArgs(strings.Fields(text))
			if err != nil {
				_ = msg.Reply.SendText(ctx, err.Error())
				return true
			}
			e.ui.clear()
			e.installSkillsFromSource(ctx, msg, src, opts)
			return true
		}
		e.ui.setDraft("name", text)
		e.ui.setStep("description")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Step 2/3 — Description\n\nOne line: when should the agent use this skill?",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:skills"}}},
		})
		return true
	case "description":
		e.ui.setDraft("description", text)
		e.ui.setStep("body")
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "Step 3/3 — Instructions\n\nSend the SKILL.md body (markdown). For L2/L3 packages, cancel and upload a .zip instead.",
			Rows: [][]channel.Button{{{Text: "❌ Cancel", CallbackData: "ui:skills"}}},
		})
		return true
	case "body":
		name := w.Draft["name"]
		desc := w.Draft["description"]
		e.ui.clear()
		mgr := e.skillMgr()
		if mgr == nil {
			_ = msg.Reply.SendText(ctx, "Skills not available.")
			return true
		}
		sk, err := mgr.InstallMarkdown(name, desc, text)
		if err != nil {
			_ = msg.Reply.SendText(ctx, "Create failed: "+err.Error())
			return true
		}
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Skill «%s» created (L1). Package synced to DB.", sk.Name))
		e.cmdSkillDetail(ctx, msg, sk.Name)
		return true
	}
	return false
}

func (e *Engine) installSkillPackageForced(ctx context.Context, msg channel.Message, path, name string) {
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}
	list, err := mgr.InstallPath(path, name)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Install failed: "+err.Error())
		return
	}
	if len(list) == 1 {
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Installed «%s» (%s). Package synced to DB.", list[0].Name, list[0].LevelLabel()))
		e.cmdSkillDetail(ctx, msg, list[0].Name)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Installed %d skills (packages synced to DB):\n", len(list))
	for _, sk := range list {
		fmt.Fprintf(&b, "• %s (%s)\n", sk.Name, sk.LevelLabel())
	}
	_ = msg.Reply.SendText(ctx, b.String())
	e.cmdSkillsList(ctx, msg)
}

func zipLooksLikeSkill(path string) bool {
	r, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, f := range r.File {
		name := strings.ToLower(strings.ReplaceAll(f.Name, "\\", "/"))
		base := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			base = name[i+1:]
		}
		if base == "skill.md" {
			return true
		}
	}
	return false
}

// trySkillPackageMessage installs an uploaded skill .zip/.md when appropriate.
func (e *Engine) trySkillPackageMessage(ctx context.Context, msg channel.Message) bool {
	if len(msg.Attachments) == 0 {
		return false
	}
	w := e.ui.get()
	force := w != nil && w.Kind == "skill_add"
	text := strings.ToLower(strings.TrimSpace(msg.Text))
	if strings.HasPrefix(text, "/skill_install") || strings.Contains(text, "install skill") {
		force = true
	}
	preferred := ""
	if w != nil && w.Kind == "skill_add" {
		preferred = w.Draft["name"]
	}

	for _, att := range msg.Attachments {
		fname := strings.ToLower(att.Filename)
		if !strings.HasSuffix(fname, ".zip") && !strings.HasSuffix(fname, ".md") {
			continue
		}
		pattern := "awaki-skill-upload-*.zip"
		if strings.HasSuffix(fname, ".md") {
			pattern = "awaki-skill-upload-*.md"
		}
		tmp, err := os.CreateTemp("", pattern)
		if err != nil {
			continue
		}
		path := tmp.Name()
		_, _ = tmp.Write(att.Data)
		_ = tmp.Close()

		if strings.HasSuffix(fname, ".zip") {
			looks := zipLooksLikeSkill(path)
			if !force && !looks {
				_ = os.Remove(path)
				continue
			}
			if !looks {
				_ = os.Remove(path)
				_ = msg.Reply.SendText(ctx, "Zip has no SKILL.md — not an Agent Skill package.")
				return true
			}
		} else if !force && !strings.Contains(fname, "skill") {
			_ = os.Remove(path)
			continue
		}

		e.ui.clear()
		e.installSkillPackageForced(ctx, msg, path, preferred)
		_ = os.Remove(path)
		return true
	}
	return false
}

func (e *Engine) cmdPlugin(ctx context.Context, msg channel.Message) {
	args := ""
	if msg.Command != nil && msg.Command.Args != nil {
		args = strings.TrimSpace(msg.Command.Args["raw"])
	}
	if args == "" {
		// fallback: strip /plugin[@bot]
		text := strings.TrimSpace(msg.Text)
		fields := strings.Fields(text)
		if len(fields) > 1 {
			args = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		}
	}
	e.handlePluginArgs(ctx, msg, args)
}

func (e *Engine) handlePluginArgs(ctx context.Context, msg channel.Message, args string) {
	args = strings.TrimSpace(args)
	fields := strings.Fields(args)
	if len(fields) == 0 {
		e.cmdSkillMarket(ctx, msg)
		return
	}
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}

	// /plugin marketplace add|list|remove ...
	if fields[0] == "marketplace" || fields[0] == "market" {
		if len(fields) == 1 || fields[1] == "list" {
			e.cmdSkillMarket(ctx, msg)
			return
		}
		if fields[1] == "add" && len(fields) >= 3 {
			src := fields[2]
			_ = msg.Reply.SendText(ctx, "Downloading marketplace "+src+"…")
			mk, err := mgr.AddMarketplace(ctx, src)
			if err != nil {
				_ = msg.Reply.SendText(ctx, "Marketplace add failed: "+err.Error())
				return
			}
			var b strings.Builder
			fmt.Fprintf(&b, "✅ Marketplace registered\n\nID: %s\nSource: %s\n%s\n\nPlugins:\n", mk.ID, mk.Source, mk.Description)
			for _, p := range mk.Plugins {
				fmt.Fprintf(&b, "• %s — %s\n  install: /plugin install %s@%s\n", p.Name, p.Description, p.Name, mk.ID)
			}
			b.WriteString("\nInstall all: /plugin install all@" + mk.ID)
			_ = msg.Reply.SendText(ctx, b.String())
			return
		}
		if fields[1] == "remove" && len(fields) >= 3 {
			if err := mgr.RemoveMarketplace(fields[2]); err != nil {
				_ = msg.Reply.SendText(ctx, err.Error())
				return
			}
			_ = msg.Reply.SendText(ctx, "Removed marketplace: "+fields[2])
			return
		}
		_ = msg.Reply.SendText(ctx, "Usage:\n/plugin marketplace add owner/repo\n/plugin marketplace list\n/plugin marketplace remove <id>")
		return
	}

	// /plugin install document-skills@anthropic-agent-skills
	if fields[0] == "install" {
		if len(fields) < 2 {
			_ = msg.Reply.SendText(ctx, "Usage: /plugin install document-skills@anthropic-agent-skills\nOr: /plugin install all@anthropic-agent-skills\nOr: /plugin install anthropics/skills")
			return
		}
		spec := fields[1]
		_ = msg.Reply.SendText(ctx, "Installing "+spec+"…")
		list, err := mgr.InstallPlugin(ctx, spec)
		if err != nil {
			_ = msg.Reply.SendText(ctx, "Install failed: "+err.Error())
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "✅ Installed %d skill(s) from %s:\n", len(list), spec)
		for _, sk := range list {
			fmt.Fprintf(&b, "• %s (%s)\n", sk.Name, sk.LevelLabel())
		}
		_ = msg.Reply.SendText(ctx, b.String())
		e.cmdSkillsList(ctx, msg)
		return
	}

	_ = msg.Reply.SendText(ctx, "Usage:\n/plugin marketplace add anthropics/skills\n/plugin install document-skills@anthropic-agent-skills\n/plugin install all@anthropic-agent-skills")
}

func (e *Engine) cmdSkillMarket(ctx context.Context, msg channel.Message) {
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}
	markets := mgr.ListMarketplaces()
	var b strings.Builder
	b.WriteString("🏪 Skill marketplaces (Claude-compatible)\n\n")
	b.WriteString("Add: /plugin marketplace add anthropics/skills\n")
	b.WriteString("Install: /plugin install document-skills@anthropic-agent-skills\n")
	b.WriteString("Install all: /plugin install all@anthropic-agent-skills\n")
	b.WriteString("Single skill from any URL: /skills add <url> (GitHub, skills.sh, smithery.ai, …)\n\n")
	if len(markets) == 0 {
		b.WriteString("No marketplaces registered yet.")
	} else {
		for _, mk := range markets {
			fmt.Fprintf(&b, "• %s (%s)\n  %s\n", mk.ID, mk.Source, mk.Description)
			for _, p := range mk.Plugins {
				fmt.Fprintf(&b, "  – %s (%d skills)\n", p.Name, len(p.Skills))
			}
		}
	}
	rows := [][]channel.Button{
		{{Text: "➕ anthropics/skills", CallbackData: "ui:marketadd:anthropics/skills"}},
		{{Text: "📚 Skills list", CallbackData: "ui:skills"}},
		{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
	}
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}
