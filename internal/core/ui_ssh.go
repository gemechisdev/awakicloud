package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/sshcreds"
)

func (e *Engine) uiSSHBoard(ctx context.Context, msg channel.Message) {
	e.ui.clear()
	if e.ssh == nil {
		_ = msg.Reply.SendText(ctx, "SSH credentials are not available.")
		return
	}
	list, err := e.ssh.List()
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Error listing SSH identities: "+err.Error())
		return
	}

	var b strings.Builder
	b.WriteString("🔑 SSH / Git identities\n\n")
	b.WriteString("Used for `git clone/push` over SSH (multi-account GitHub).\n")
	b.WriteString("Clone with the Host alias, e.g.\n`git@github.com-gemechisdev:user/repo.git`\n\n")
	if len(list) == 0 {
		b.WriteString("No identities yet. Tap Add to set one up interactively.")
	} else {
		b.WriteString(fmt.Sprintf("%d configured:\n\n", len(list)))
		for _, id := range list {
			label := id.Host
			if id.GitHubUser != "" {
				label += " (@" + id.GitHubUser + ")"
			}
			fmt.Fprintf(&b, "• `%s` → %s as %s\n", id.Host, id.HostName, id.User)
			if id.GitHubUser != "" {
				fmt.Fprintf(&b, "  GitHub: @%s\n", id.GitHubUser)
			}
		}
	}

	rows := [][]channel.Button{
		{{Text: "➕ Add identity", CallbackData: "ui:sshadd"}},
		{{Text: "🔄 Refresh", CallbackData: "ui:ssh"}},
	}
	for _, id := range list {
		rows = append(rows, []channel.Button{
			{Text: "🔑 " + truncateUI(id.Host, 28), CallbackData: "ui:ssh:" + id.ID},
		})
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Settings", CallbackData: "ui:settings"}})

	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) uiSSHDetail(ctx context.Context, msg channel.Message, idKey string) {
	if e.ssh == nil {
		return
	}
	id, ok, err := e.ssh.Get(idKey)
	if err != nil || !ok {
		_ = msg.Reply.SendText(ctx, "Identity not found: "+idKey)
		e.uiSSHBoard(ctx, msg)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔑 `%s`\n\n", id.Host)
	fmt.Fprintf(&b, "HostName: %s\nUser: %s\n", id.HostName, id.User)
	fmt.Fprintf(&b, "Key file: `%s`\n", id.IdentityFile)
	if id.GitHubUser != "" {
		fmt.Fprintf(&b, "GitHub user: @%s\n", id.GitHubUser)
	}
	if id.GitName != "" || id.GitEmail != "" {
		fmt.Fprintf(&b, "Git author: %s <%s>\n", id.GitName, id.GitEmail)
	}
	fmt.Fprintf(&b, "\nClone example:\n`git@%s:OWNER/REPO.git`\n", id.Host)
	if id.PublicKey != "" {
		pub := id.PublicKey
		if len([]rune(pub)) > 400 {
			pub = string([]rune(pub)[:400]) + "…"
		}
		fmt.Fprintf(&b, "\nPublic key (add this on GitHub → SSH keys):\n```\n%s\n```\n", strings.TrimSpace(pub))
	} else {
		b.WriteString("\n(No public key stored — paste one when re-adding, or derive with ssh-keygen -y.)\n")
	}

	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: b.String(),
		Rows: [][]channel.Button{
			{{Text: "🗑 Remove", CallbackData: "ui:sshrm:" + id.ID}},
			{{Text: "📋 All identities", CallbackData: "ui:ssh"}},
			{{Text: "⬅ Settings", CallbackData: "ui:settings"}},
		},
	})
}

func (e *Engine) uiSSHRemove(ctx context.Context, msg channel.Message, idKey string) {
	if e.ssh == nil {
		return
	}
	if err := e.ssh.Remove(idKey); err != nil {
		_ = msg.Reply.SendText(ctx, err.Error())
		return
	}
	_ = msg.Reply.SendText(ctx, "Removed SSH identity: "+idKey)
	e.uiSSHBoard(ctx, msg)
}

func (e *Engine) uiStartAddSSH(ctx context.Context, msg channel.Message) {
	e.ui.set(&uiWizard{Kind: "ssh_add", Step: "host", Draft: map[string]string{}})
	e.sendSSHStep(ctx, msg,
		"🔑 Add SSH identity — Step 1/6\n\n"+
			"Host alias (what you type in git URLs).\n\n"+
			"Examples:\n"+
			"• `github.com` — default account\n"+
			"• `github.com-gemechisdev` — second GitHub account\n"+
			"• `github.com-gemechischala`\n\n"+
			"Send the Host alias, or tap Cancel.",
		nil,
	)
}

// sshKeyRows builds the inline buttons shown while the SSH add wizard runs.
// The Cancel button is on every step so the flow can be abandoned at any point;
// extra holds per-step actions (Skip on the optional steps).
func sshKeyRows(extra []channel.Button) [][]channel.Button {
	rows := [][]channel.Button{}
	if len(extra) > 0 {
		rows = append(rows, extra)
	}
	rows = append(rows, []channel.Button{{Text: "❌ Cancel", CallbackData: "ui:ssh"}})
	return rows
}

// sendSSHStep posts a wizard step with inline buttons so the user never has to
// type "cancel"/"-" to move on.
func (e *Engine) sendSSHStep(ctx context.Context, msg channel.Message, text string, extra []channel.Button) {
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: text, Rows: sshKeyRows(extra)})
}

// wizardAddSSH drives the interactive add flow.
// Steps: host → hostname → user → private_key → public_key → meta → done
func (e *Engine) wizardAddSSH(ctx context.Context, msg channel.Message, w *uiWizard, text string) bool {
	if e.ssh == nil {
		e.ui.clear()
		_ = msg.Reply.SendText(ctx, "SSH credentials are not available.")
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "cancel" || lower == "/cancel" {
		e.ui.clear()
		_ = msg.Reply.SendText(ctx, "Cancelled.")
		e.uiSSHBoard(ctx, msg)
		return true
	}

	// Allow document upload for private/public key steps.
	if (w.Step == "private_key" || w.Step == "public_key") && len(msg.Attachments) > 0 {
		if body, ok := readFirstTextAttachment(msg); ok {
			text = body
		}
	}

	switch w.Step {
	case "host":
		host := strings.TrimSpace(text)
		if host == "" {
			e.sendSSHStep(ctx, msg, "Host alias cannot be empty. Send it again, or tap Cancel.", nil)
			return true
		}
		e.ui.setDraft("host", host)
		e.ui.setStep("hostname")
		e.sendSSHStep(ctx, msg,
			"Step 2/6 — HostName (real server).\n\n"+
				"Usually `github.com`. Send that, or another hostname.\n"+
				"Default if you send `-`: github.com",
			nil,
		)
		return true

	case "hostname":
		hn := strings.TrimSpace(text)
		if hn == "" || hn == "-" {
			hn = "github.com"
		}
		e.ui.setDraft("hostname", hn)
		e.ui.setStep("user")
		e.sendSSHStep(ctx, msg,
			"Step 3/6 — SSH user.\n\n"+
				"For GitHub this is always `git`. Send `git` or `-` for default.",
			nil,
		)
		return true

	case "user":
		user := strings.TrimSpace(text)
		if user == "" || user == "-" {
			user = "git"
		}
		e.ui.setDraft("user", user)
		e.ui.setStep("private_key")
		e.sendSSHStep(ctx, msg,
			"Step 4/6 — Private key.\n\n"+
				"Paste the full private key (including BEGIN/END lines),\n"+
				"or upload the key file as a document.\n\n"+
				"It is stored under the data dir with mode 600 and never sent back.",
			nil,
		)
		return true

	case "private_key":
		priv := strings.TrimSpace(text)
		if priv == "" {
			e.sendSSHStep(ctx, msg, "Private key is empty. Paste the key or upload the file.", nil)
			return true
		}
		e.ui.setDraft("private_key", priv)
		e.ui.setStep("public_key")
		e.sendSSHStep(ctx, msg,
			"Step 5/6 — Public key (optional).\n\n"+
				"Paste the `.pub` line, upload the file, or tap Skip.\n"+
				"Recommended so you can copy it into GitHub → Settings → SSH keys.",
			[]channel.Button{{Text: "⏭ Skip", CallbackData: "ui:sshskip"}},
		)
		return true

	case "public_key":
		pub := strings.TrimSpace(text)
		if pub == "-" {
			pub = ""
		}
		e.ui.setDraft("public_key", pub)
		e.ui.setStep("meta")
		e.sendSSHStep(ctx, msg,
			"Step 6/6 — Optional metadata.\n\n"+
				"Send one line:\n"+
				"`github_user | git name | git email`\n\n"+
				"Examples:\n"+
				"• `gemechisdev | Gemechis Dev | dev@example.com`\n"+
				"• `gemechisdev`\n"+
				"or tap Skip to save without metadata.",
			[]channel.Button{{Text: "⏭ Skip", CallbackData: "ui:sshskip"}},
		)
		return true

	case "meta":
		ghUser, gitName, gitEmail := parseSSHMeta(text)
		// Re-read draft from live wizard
		w2 := e.ui.get()
		if w2 == nil {
			e.ui.clear()
			return true
		}
		in := sshcreds.AddInput{
			Host:       w2.Draft["host"],
			HostName:   w2.Draft["hostname"],
			User:       w2.Draft["user"],
			PrivateKey: w2.Draft["private_key"],
			PublicKey:  w2.Draft["public_key"],
			GitHubUser: ghUser,
			GitName:    gitName,
			GitEmail:   gitEmail,
		}
		e.finishAddSSH(ctx, msg, in)
		return true
	}
	return true
}

// finishAddSSH saves the identity built by the add wizard and reports the result.
// Used both by the final (metadata) step and by the Skip buttons on the
// optional public-key / metadata steps.
func (e *Engine) finishAddSSH(ctx context.Context, msg channel.Message, in sshcreds.AddInput) {
	if e.ssh == nil {
		e.ui.clear()
		_ = msg.Reply.SendText(ctx, "SSH credentials are not available.")
		return
	}
	ident, err := e.ssh.Add(in)
	e.ui.clear()
	if err != nil {
		_ = msg.Reply.SendText(ctx, "Failed to save identity: "+err.Error())
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Saved SSH identity `%s`\n\n", ident.Host)
	fmt.Fprintf(&b, "HostName: %s\nUser: %s\nKey: `%s`\n", ident.HostName, ident.User, ident.IdentityFile)
	if ident.GitHubUser != "" {
		fmt.Fprintf(&b, "GitHub: @%s\n", ident.GitHubUser)
	}
	fmt.Fprintf(&b, "\nUse in clone URLs:\n`git@%s:OWNER/REPO.git`\n", ident.Host)
	if ident.PublicKey != "" {
		fmt.Fprintf(&b, "\nAdd this public key on GitHub:\n```\n%s\n```\n", strings.TrimSpace(ident.PublicKey))
	}
	b.WriteString("\n~/.ssh/config was updated (Awaki managed section).")
	_ = msg.Reply.SendText(ctx, b.String())
	e.uiSSHBoard(ctx, msg)
}

// wizardAddSSHSkip advances the add wizard for the optional steps without
// requiring the user to send "-": it keeps everything gathered so far and
// jumps straight to saving (public key step) or saves as-is (metadata step).
func (e *Engine) wizardAddSSHSkip(ctx context.Context, msg channel.Message) bool {
	switch w := e.ui.get(); {
	case w == nil:
		e.uiSSHBoard(ctx, msg)
		return true
	case w.Step == "public_key":
		e.ui.setStep("meta")
		e.ui.setDraft("public_key", "")
		e.sendSSHStep(ctx, msg,
			"Step 6/6 — Optional metadata.\n\n"+
				"Send one line:\n"+
				"`github_user | git name | git email`\n\n"+
				"Examples:\n"+
				"• `gemechisdev | Gemechis Dev | dev@example.com`\n"+
				"• `gemechisdev`\n"+
				"or tap Skip to save without metadata.",
			[]channel.Button{{Text: "⏭ Skip", CallbackData: "ui:sshskip"}},
		)
		return true
	case w.Kind == "ssh_add" && w.Step == "meta":
		e.finishAddSSH(ctx, msg, sshcreds.AddInput{
			Host:       w.Draft["host"],
			HostName:   w.Draft["hostname"],
			User:       w.Draft["user"],
			PrivateKey: w.Draft["private_key"],
			PublicKey:  w.Draft["public_key"],
		})
		return true
	default:
		// Not an optional SSH step — ignore the tap (defensive).
		return true
	}
}

func parseSSHMeta(text string) (ghUser, gitName, gitEmail string) {
	text = strings.TrimSpace(text)
	if text == "" || text == "-" {
		return "", "", ""
	}
	parts := strings.Split(text, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) >= 1 {
		ghUser = parts[0]
	}
	if len(parts) >= 2 {
		gitName = parts[1]
	}
	if len(parts) >= 3 {
		gitEmail = parts[2]
	}
	// Allow "name <email>" in second field alone
	if gitEmail == "" && gitName != "" {
		if i := strings.Index(gitName, "<"); i >= 0 {
			if j := strings.Index(gitName[i:], ">"); j > 0 {
				gitEmail = strings.TrimSpace(gitName[i+1 : i+j])
				gitName = strings.TrimSpace(gitName[:i])
			}
		}
	}
	return ghUser, gitName, gitEmail
}

// readFirstTextAttachment loads the first non-image attachment as UTF-8 text.
func readFirstTextAttachment(msg channel.Message) (string, bool) {
	for _, att := range msg.Attachments {
		if strings.HasPrefix(att.MimeType, "image/") {
			continue
		}
		if len(att.Data) == 0 || len(att.Data) > 64*1024 {
			continue
		}
		return string(att.Data), true
	}
	return "", false
}
