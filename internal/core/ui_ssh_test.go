package core

import (
	"strings"
	"testing"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/sshcreds"
)

// sshHarness builds a harness with an SSH store wired in.
func sshHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.e.ssh = sshcreds.New(t.TempDir())
	return h
}

// kbButtons flattens the last keyboard into "Text|CallbackData" pairs.
func kbButtons(km channel.KeyboardMessage) string {
	var b []string
	for _, row := range km.Rows {
		for _, btn := range row {
			b = append(b, btn.Text+"|"+btn.CallbackData)
		}
	}
	return strings.Join(b, "  ")
}

// typeText feeds free text to the active wizard through the real dispatcher.
func (h *harness) typeText(text string) {
	h.t.Helper()
	m := h.msg()
	m.Text = text
	if !h.e.handleUICallback(h.ctx, m) {
		h.t.Fatalf("wizard text %q was not consumed", text)
	}
}

// TestSSHBoardHasButtons proves "🔑 SSH / Git credentials" opens a board whose
// Add/Refresh/Settings buttons carry the callbacks the dispatcher routes.
func TestSSHBoardHasButtons(t *testing.T) {
	h := sshHarness(t)
	h.tap("ui:ssh")

	got := kbButtons(h.ch.last())
	for _, want := range []string{"ui:sshadd", "ui:ssh", "ui:settings"} {
		if !strings.Contains(got, want) {
			t.Errorf("board missing button %q; got: %s", want, got)
		}
	}
}

// TestSSHAddIdentityFlow drives the full wizard, using the Skip buttons on the
// optional steps, and asserts the identity is saved.
func TestSSHAddIdentityFlow(t *testing.T) {
	h := sshHarness(t)

	h.tap("ui:sshadd")
	if w := h.e.ui.get(); w == nil || w.Kind != "ssh_add" || w.Step != "host" {
		t.Fatalf("ui:sshadd did not start the wizard: %+v", w)
	}

	h.typeText("github.com-gemechisdev") // host
	h.typeText("-")                      // hostname → github.com
	h.typeText("-")                      // user → git
	h.typeText("-----BEGIN OPENSSH PRIVATE KEY-----\nZZZ\n-----END OPENSSH PRIVATE KEY-----")

	if w := h.e.ui.get(); w == nil || w.Step != "public_key" {
		t.Fatalf("expected public_key step, got %+v", w)
	}
	// Public key is optional — leave it and Skip.
	h.tap("ui:sshskip")
	if w := h.e.ui.get(); w == nil || w.Step != "meta" {
		t.Fatalf("Skip on public_key should reach the meta step, got %+v", w)
	}
	// Metadata is optional — Skip saves the identity.
	h.tap("ui:sshskip")
	if h.e.ui.get() != nil {
		t.Fatal("wizard still active after final Skip")
	}

	list, err := h.e.ssh.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 saved identity, got %d", len(list))
	}
	if list[0].Host != "github.com-gemechisdev" || list[0].HostName != "github.com" || list[0].User != "git" {
		t.Errorf("identity saved with wrong fields: %+v", list[0])
	}
}

// TestSSHEveryStepOffersCancel proves the user can abandon the flow at any
// point — every step must carry a Cancel button.
func TestSSHEveryStepOffersCancel(t *testing.T) {
	h := sshHarness(t)
	h.tap("ui:sshadd")

	steps := []string{
		"github.com-gemechisdev", // host  → hostname
		"-",                      // hostname → user
		"-",                      // user → private_key
		"-----BEGIN OPENSSH PRIVATE KEY-----\nZZZ\n-----END OPENSSH PRIVATE KEY-----", // → public_key
	}
	check := func(where string) {
		t.Helper()
		if got := kbButtons(h.ch.last()); !strings.Contains(got, "❌ Cancel") {
			t.Errorf("%s: step has no Cancel button: %s", where, got)
		}
	}
	check("host")
	for _, in := range steps {
		h.typeText(in)
		check("step after " + in)
	}
}

// TestSSHCancelMidFlowIsSafe proves tapping Cancel (ui:ssh) mid-wizard clears
// the wizard and returns to the board instead of answering a step.
func TestSSHCancelMidFlowIsSafe(t *testing.T) {
	h := sshHarness(t)
	h.tap("ui:sshadd")
	h.typeText("github.com")

	h.tap("ui:ssh") // the Cancel button
	if h.e.ui.get() != nil {
		t.Fatal("Cancel did not clear the active wizard")
	}
	if got := kbButtons(h.ch.last()); !strings.Contains(got, "ui:sshadd") {
		t.Errorf("Cancel did not return to the SSH board: %s", got)
	}
}

// TestButtonTapNeverAnswersWizardStep guards the routing bug where an inline
// "ui:" callback could be swallowed as free text by an active wizard.
func TestButtonTapNeverAnswersWizardStep(t *testing.T) {
	h := sshHarness(t)
	h.tap("ui:sshadd") // wizard active, awaiting the host step

	// A ui: callback for another screen must be routed as a callback, never
	// stored as the host value.
	h.tap("ui:settings")

	if h.e.ui.get() != nil {
		t.Fatal("a ui: callback while a wizard is active was swallowed by the wizard")
	}
}
