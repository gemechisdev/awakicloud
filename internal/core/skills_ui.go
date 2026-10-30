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
		b.WriteString("None installed. Tap Add or upload a skill .zip / .md.")
	} else {
		for _, s := range list {
			fmt.Fprintf(&b, "• %s — %s\n  %s\n", s.Name, s.LevelLabel(), s.Description)
		}
	}
	rows := [][]channel.Button{
		{{Text: "➕ Add skill", CallbackData: "ui:skilladd"}},
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
			"Upload now (document) or continue the wizard:\n\n" +
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
