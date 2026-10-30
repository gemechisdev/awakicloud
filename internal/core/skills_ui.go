package core

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	if len(list) == 0 {
		b.WriteString("No skills installed yet.\nSend a skill .zip, or create L1 via the agent / wizard.")
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
		Text: "➕ Add skill\n\nStep 1/3 — Name\n\nSend a short name (e.g. pdf-processing), or send a .zip skill package as a document and skip the wizard.",
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
			Text: "Step 3/3 — Instructions\n\nSend the SKILL.md body (markdown procedure). For full L2/L3 packages, cancel and upload a .zip instead.",
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
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Skill «%s» created (L1).", sk.Name))
		e.cmdSkillDetail(ctx, msg, sk.Name)
		return true
	}
	return false
}

// TryInstallSkillZip installs an uploaded zip if it looks like an Agent Skill package.
// Returns true if handled.
func (e *Engine) TryInstallSkillZip(ctx context.Context, msg channel.Message, zipPath string) bool {
	mgr := e.skillMgr()
	if mgr == nil {
		return false
	}
	// Peek for SKILL.md without full install failure noise for random zips
	if !zipLooksLikeSkill(zipPath) {
		return false
	}
	sk, err := mgr.InstallZip(zipPath, "")
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Skill zip install failed: "+err.Error())
		return true
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Installed skill «%s» (%s).", sk.Name, sk.LevelLabel()))
	e.cmdSkillDetail(ctx, msg, sk.Name)
	return true
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

func (e *Engine) installSkillZipForced(ctx context.Context, msg channel.Message, zipPath, name string) {
	mgr := e.skillMgr()
	if mgr == nil {
		_ = msg.Reply.SendText(ctx, "Skills not available.")
		return
	}
	sk, err := mgr.InstallZip(zipPath, name)
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Install failed: "+err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, fmt.Sprintf("✅ Installed skill «%s» (%s).", sk.Name, sk.LevelLabel()))
	e.cmdSkillDetail(ctx, msg, sk.Name)
}

// ensure skill path exists for empty installs
func ensureSkillsDir(dir string) {
	_ = os.MkdirAll(dir, 0o755)
}

// trySkillZipMessage installs an uploaded Agent Skill zip when the user is in
// the skill_add wizard, or the caption/text is /skill_install, or the zip
// clearly contains SKILL.md and text mentions install skill.
func (e *Engine) trySkillZipMessage(ctx context.Context, msg channel.Message) bool {
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
		name := strings.ToLower(att.Filename)
		if !strings.HasSuffix(name, ".zip") {
			continue
		}
		tmp, err := os.CreateTemp("", "awaki-skill-upload-*.zip")
		if err != nil {
			continue
		}
		path := tmp.Name()
		_, _ = tmp.Write(att.Data)
		_ = tmp.Close()

		looks := zipLooksLikeSkill(path)
		if !force && !looks {
			_ = os.Remove(path)
			continue
		}
		if !looks {
			_ = os.Remove(path)
			_ = msg.Reply.SendText(ctx, "Zip does not contain SKILL.md — not an Agent Skill package.")
			return true
		}
		e.ui.clear()
		e.installSkillZipForced(ctx, msg, path, preferred)
		_ = os.Remove(path)
		return true
	}
	return false
}
