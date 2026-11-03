package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/core/chat"
	"github.com/gemechischala/awaki/internal/domain"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/store/local"
	"github.com/gemechischala/awaki/internal/transport"
)

// ---- fakes -----------------------------------------------------------------

type fakeReplier struct {
	mu    sync.Mutex
	texts []string
}

func (r *fakeReplier) SendText(_ context.Context, t string) error {
	r.mu.Lock()
	r.texts = append(r.texts, t)
	r.mu.Unlock()
	return nil
}
func (r *fakeReplier) SendFile(context.Context, string, string) error { return nil }
func (r *fakeReplier) SendTyping(context.Context) error               { return nil }
func (r *fakeReplier) SendAsk(context.Context, string, transport.AskKind, []transport.AskOption) error {
	return nil
}
func (r *fakeReplier) ConsumeEvents(context.Context, <-chan transport.Event, <-chan string) string {
	return ""
}

type fakeChannel struct {
	rep   *fakeReplier
	mu    sync.Mutex
	all   []channel.KeyboardMessage
	edits []int
}

func (f *fakeChannel) Name() string                               { return "fake" }
func (f *fakeChannel) OwnerReplier() transport.Replier            { return f.rep }
func (f *fakeChannel) Run(context.Context, channel.Handler) error { return nil }
func (f *fakeChannel) SendKeyboardToOwner(_ context.Context, km channel.KeyboardMessage) {
	f.mu.Lock()
	f.all = append(f.all, km)
	f.edits = append(f.edits, 0)
	f.mu.Unlock()
}
func (f *fakeChannel) SendOrEditKeyboardToOwner(_ context.Context, id int, km channel.KeyboardMessage) {
	f.mu.Lock()
	f.all = append(f.all, km)
	f.edits = append(f.edits, id)
	f.mu.Unlock()
}
func (f *fakeChannel) last() channel.KeyboardMessage { return f.all[len(f.all)-1] }

// ---- harness ---------------------------------------------------------------

type harness struct {
	t   *testing.T
	e   *Engine
	ch  *fakeChannel
	rep *fakeReplier
	st  store.Store
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	rep := &fakeReplier{}
	fc := &fakeChannel{rep: rep}
	e := &Engine{
		ch:       fc,
		store:    st,
		chats:    chat.NewManager(st, nil),
		projects: project.NewManager(dir, nil),
	}
	return &harness{t: t, e: e, ch: fc, rep: rep, st: st, ctx: context.Background()}
}

func (h *harness) msg() channel.Message { return channel.Message{Reply: h.rep, EditMessageID: 77} }

// tap simulates pressing an inline button / reply-keyboard label.
func (h *harness) tap(data string) {
	h.t.Helper()
	m := h.msg()
	m.Text = data
	if !h.e.handleUICallback(h.ctx, m) {
		h.t.Fatalf("callback %q was not handled", data)
	}
}

// cmd simulates a typed slash command.
func (h *harness) cmd(name, raw string) *channel.Command {
	h.t.Helper()
	c := &channel.Command{Name: name, Args: map[string]string{"raw": raw}, Result: &channel.CommandResult{}}
	h.e.handleCommand(h.ctx, channel.Message{Reply: h.rep, Command: c})
	return c
}

// say records a user turn in the active chat (what the agent sink persists).
func (h *harness) say(text string) *store.Chat {
	h.t.Helper()
	ch, err := h.e.chats.Resolve()
	if err != nil {
		h.t.Fatal(err)
	}
	ch.AppendMessage(domain.Turn{Role: "user", Content: []domain.ContentBlock{domain.TextBlock(text)}})
	ch.AppendMessage(domain.Turn{Role: "assistant", Content: []domain.ContentBlock{domain.TextBlock("reply to " + text)}})
	ch.Title = text
	ch.Summary = "summary"
	if err := ch.Save(); err != nil {
		h.t.Fatal(err)
	}
	return ch
}

func (h *harness) active() *store.Chat {
	h.t.Helper()
	ch, err := h.e.chats.Get(h.e.chats.ActiveID())
	if err != nil {
		h.t.Fatalf("no active chat: %v", err)
	}
	return ch
}

func (h *harness) mkProject(name string) string {
	h.t.Helper()
	h.e.createProject(h.ctx, h.msg(), name, "view", "")
	return project.Slugify(name)
}

func buttons(km channel.KeyboardMessage) []channel.Button {
	var out []channel.Button
	for _, r := range km.Rows {
		out = append(out, r...)
	}
	return out
}

func hasCallback(km channel.KeyboardMessage, prefix string) bool {
	for _, b := range buttons(km) {
		if strings.HasPrefix(b.CallbackData, prefix) {
			return true
		}
	}
	return false
}

func callbackFor(t *testing.T, km channel.KeyboardMessage, prefix string) string {
	t.Helper()
	for _, b := range buttons(km) {
		if strings.HasPrefix(b.CallbackData, prefix) {
			return b.CallbackData
		}
	}
	t.Fatalf("no button with callback prefix %q in %+v", prefix, buttons(km))
	return ""
}

// assertCallbackLimits fails if any button ever sent exceeds Telegram's 64-byte limit.
func (h *harness) assertCallbackLimits() {
	h.t.Helper()
	for _, km := range h.ch.all {
		for _, b := range buttons(km) {
			if b.URL != "" {
				continue
			}
			if b.CallbackData == "" || len(b.CallbackData) > 64 || b.CallbackData == "ui:noop" && !strings.Contains(b.Text, "/") {
				if len(b.CallbackData) > 64 || b.CallbackData == "" {
					h.t.Errorf("bad callback %q (%d bytes) on button %q", b.CallbackData, len(b.CallbackData), b.Text)
				}
			}
		}
	}
}

// ---- tests -----------------------------------------------------------------

func TestNewChatPickerListsProjectsAndNone(t *testing.T) {
	h := newHarness(t)
	h.tap("ui:new") // no projects yet → starts a general chat immediately
	if !strings.Contains(h.ch.last().Text, "General") || h.active().ProjectSlug != "" {
		t.Fatalf("expected immediate general chat, got %q", h.ch.last().Text)
	}

	api := h.mkProject("API v2")
	pay := h.mkProject("payments")
	h.tap("ui:new")
	km := h.ch.last()
	if !strings.Contains(km.Text, "Where should it live") {
		t.Fatalf("picker text: %q", km.Text)
	}
	for _, want := range []string{"ui:nc:-", "ui:nc:" + api, "ui:nc:" + pay, "ui:ncp"} {
		if !hasCallback(km, want) {
			t.Errorf("picker missing %s; have %+v", want, buttons(km))
		}
	}
	h.assertCallbackLimits()
}

func TestStartChatInProjectBindsAndReusesEmptyChat(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	before := h.e.chats.ActiveID() // may be ""
	h.tap("ui:nc:" + api)
	ch := h.active()
	if ch.Mode != "project" || ch.ProjectSlug != api {
		t.Fatalf("chat not bound: %+v", ch)
	}
	if before != "" && ch.ID != before {
		t.Fatalf("empty chat should be reused")
	}
	if !strings.Contains(h.ch.last().Text, "New chat") || !strings.Contains(h.ch.last().Text, api) {
		t.Fatalf("confirmation: %q", h.ch.last().Text)
	}
	// tapping New chat again on the still-empty chat must not create a second one
	id := ch.ID
	h.tap("ui:nc:-")
	if h.active().ID != id || h.active().ProjectSlug != "" {
		t.Fatalf("empty chat should be reused and unbound: %+v", h.active())
	}
	infos, _ := h.st.ListChats()
	if len(infos) != 1 {
		t.Fatalf("expected exactly 1 chat in store, got %d", len(infos))
	}
}

func TestChatsHubShowsBadgesFiltersAndPages(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.tap("ui:nc:" + api)
	h.say("Bugfix auth flow")
	h.tap("ui:nc:-")
	h.say("Quick question about rates")

	// typed /chats with no args must list everything (was: "Usage: /chats <slug>")
	c := h.cmd(channel.CmdChatsByProject, "")
	if !c.Result.Handled || c.Result.Error != nil {
		t.Fatalf("command result: %+v", c.Result)
	}
	km := h.ch.last()
	for _, want := range []string{"Bugfix auth flow", "Quick question about rates", "📁 `" + api + "`", "💬 General"} {
		if !strings.Contains(km.Text, want) {
			t.Errorf("hub text missing %q:\n%s", want, km.Text)
		}
	}
	if !hasCallback(km, "ui:co:") || !hasCallback(km, "ui:cm:") || !hasCallback(km, "ui:new") {
		t.Errorf("hub buttons: %+v", buttons(km))
	}

	// scope filters
	h.tap("ui:chats:p=" + api + ":0")
	if txt := h.ch.last().Text; !strings.Contains(txt, "Bugfix auth flow") || strings.Contains(txt, "Quick question") {
		t.Errorf("project scope wrong:\n%s", txt)
	}
	if !hasCallback(h.ch.last(), "ui:nc:"+api) {
		t.Error("project scope must offer New chat here")
	}
	h.tap("ui:chats:gen:0")
	if txt := h.ch.last().Text; strings.Contains(txt, "Bugfix auth flow") || !strings.Contains(txt, "Quick question") {
		t.Errorf("general scope wrong:\n%s", txt)
	}

	// /chats <project> and /chats <search words>
	h.cmd(channel.CmdChatsByProject, "API v2")
	if txt := h.ch.last().Text; !strings.Contains(txt, "Bugfix auth flow") || strings.Contains(txt, "Quick question") {
		t.Errorf("/chats <project>:\n%s", txt)
	}
	h.cmd(channel.CmdChatsByProject, "rates")
	if txt := h.ch.last().Text; !strings.Contains(txt, "Quick question") || strings.Contains(txt, "Bugfix") {
		t.Errorf("search:\n%s", txt)
	}
	h.cmd(channel.CmdChatsByProject, "zzz-nothing")
	if !strings.Contains(h.ch.last().Text, "No chats match") {
		t.Errorf("empty search: %q", h.ch.last().Text)
	}

	// legacy /history alias and old callback still work
	h.cmd(channel.CmdHistory, "")
	h.tap("ui:hist:0")
	h.assertCallbackLimits()

	// pagination: 9 more chats → 2 pages
	for i := 0; i < 9; i++ {
		h.tap("ui:nc:-")
		h.say("filler chat " + string(rune('a'+i)))
	}
	h.tap("ui:chats:all:0")
	if !strings.Contains(h.ch.last().Text, "page 1/") || !hasCallback(h.ch.last(), "ui:chats:all:1") {
		t.Errorf("expected pagination:\n%s", h.ch.last().Text)
	}
	h.tap("ui:chats:all:999") // out-of-range page clamps instead of failing
	h.assertCallbackLimits()
}

func TestOpenChatSwitchesActiveChatAndWorkspace(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.tap("ui:nc:" + api)
	projChat := h.say("Bugfix auth flow")
	h.tap("ui:nc:-")
	general := h.say("Quick question")
	if h.active().ID != general.ID {
		t.Fatal("setup: general should be active")
	}

	h.tap("ui:co:" + chatRef(projChat.ID))
	cur := h.active()
	if cur.ID != projChat.ID || cur.ProjectSlug != api || cur.Status != "active" {
		t.Fatalf("resume failed: %+v", cur)
	}
	txt := h.ch.last().Text
	for _, want := range []string{"Resumed", "Bugfix auth flow", "**You:**", "**Assistant:**"} {
		if !strings.Contains(txt, want) {
			t.Errorf("resume card missing %q:\n%s", want, txt)
		}
	}
	prev, _ := h.st.LoadChat(general.ID)
	if prev.Status != "closed" {
		t.Errorf("previous chat should be closed, got %q", prev.Status)
	}
	// opening the already-current chat shows its card, no switch
	h.tap("ui:co:" + chatRef(projChat.ID))
	if !strings.Contains(h.ch.last().Text, "already your current chat") {
		t.Errorf("already-current: %q", h.ch.last().Text)
	}
	// legacy callbacks with full ids still resolve
	h.tap("ui:open:" + general.ID)
	if h.active().ID != general.ID {
		t.Error("legacy ui:open:<fullid> must switch")
	}
	h.tap("ui:chatopen:" + projChat.ID)
	if h.active().ID != projChat.ID {
		t.Error("legacy ui:chatopen:<fullid> must switch")
	}
}

func TestMoveChatBetweenProjectsAndOut(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	pay := h.mkProject("payments")
	h.say("Some work") // general chat with content
	ref := chatRef(h.active().ID)

	h.tap("ui:cmv:" + ref + ":0")
	km := h.ch.last()
	for _, want := range []string{"ui:cmt:" + ref + ":" + api, "ui:cmt:" + ref + ":" + pay, "ui:pnm:" + ref} {
		if !hasCallback(km, want) {
			t.Errorf("move picker missing %s: %+v", want, buttons(km))
		}
	}
	if hasCallback(km, "ui:cmt:"+ref+":-") {
		t.Error("general chat must not offer 'remove from project'")
	}

	h.tap("ui:cmt:" + ref + ":" + api)
	if ch := h.active(); ch.Mode != "project" || ch.ProjectSlug != api {
		t.Fatalf("move into project failed: %+v", ch)
	}
	if !strings.Contains(h.ch.last().Text, "Moved to") {
		t.Errorf("confirmation: %q", h.ch.last().Text)
	}
	// same project again → no-op message
	h.tap("ui:cmt:" + ref + ":" + api)
	if !strings.Contains(h.ch.last().Text, "Already there") {
		t.Errorf("noop: %q", h.ch.last().Text)
	}
	// /move <project> by name, then /move - to leave
	h.cmd(channel.CmdMove, "payments")
	if h.active().ProjectSlug != pay {
		t.Fatalf("/move payments: %+v", h.active())
	}
	h.cmd(channel.CmdChatLink, api) // legacy alias
	if h.active().ProjectSlug != api {
		t.Fatalf("/chatlink alias: %+v", h.active())
	}
	h.cmd(channel.CmdMove, "-")
	if ch := h.active(); ch.Mode != "chat" || ch.ProjectSlug != "" || ch.ProjectID != "" {
		t.Fatalf("remove from project failed: %+v", ch)
	}
	h.assertCallbackLimits()
}

func TestMoveNonCurrentChatDoesNotTouchActiveChat(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.tap("ui:nc:-")
	first := h.say("first")
	h.tap("ui:nc:-")
	second := h.say("second")

	h.tap("ui:cmt:" + chatRef(first.ID) + ":" + api)
	got, _ := h.e.chats.Get(first.ID)
	if got.ProjectSlug != api {
		t.Fatalf("first not moved: %+v", got)
	}
	if h.e.chats.ActiveID() != second.ID {
		t.Fatal("active chat must not change when moving another chat")
	}
}

func TestRenameChatViaWizardAndCommand(t *testing.T) {
	h := newHarness(t)
	h.say("original title")
	ref := chatRef(h.active().ID)

	h.tap("ui:cren:" + ref)
	if h.e.ui.get() == nil || h.e.ui.get().Kind != "rename_chat" {
		t.Fatal("rename wizard not started")
	}
	m := h.msg()
	m.Text = "Brand new title"
	if !h.e.handleUICallback(h.ctx, m) {
		t.Fatal("wizard input not consumed")
	}
	if got := h.active(); got.Title != "Brand new title" || !got.TitleLocked {
		t.Fatalf("rename failed: %+v", got)
	}
	h.cmd(channel.CmdRename, "Via command")
	if h.active().Title != "Via command" {
		t.Fatalf("/rename failed: %+v", h.active())
	}
}

func TestProjectsListAndDetail(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.mkProject("research notes")
	h.tap("ui:nc:" + api)
	h.say("Bugfix auth flow")
	h.tap("ui:nc:" + api)
	h.say("Add rate limiting")

	h.cmd(channel.CmdProjects, "")
	km := h.ch.last()
	if !strings.Contains(km.Text, "API v2") || !strings.Contains(km.Text, "2 chats") || !strings.Contains(km.Text, "0 chats") {
		t.Errorf("projects list should show chat counts:\n%s", km.Text)
	}
	if !hasCallback(km, "ui:pd:"+api) || !hasCallback(km, "ui:projnew") {
		t.Errorf("projects buttons: %+v", buttons(km))
	}

	h.tap("ui:pd:" + api)
	km = h.ch.last()
	for _, want := range []string{"API v2", "Bugfix auth flow", "Add rate limiting"} {
		if !strings.Contains(km.Text, want) {
			t.Errorf("detail missing %q:\n%s", want, km.Text)
		}
	}
	for _, want := range []string{"ui:nc:" + api, "ui:co:", "ui:chats:p=" + api + ":0", "ui:pmv:" + api, "ui:pren:" + api, "ui:psync:" + api} {
		if !hasCallback(km, want) {
			t.Errorf("detail missing button %s: %+v", want, buttons(km))
		}
	}
	// viewing a project must not rebind the active chat or change anything
	before := h.active()
	h.tap("ui:pd:research-notes")
	if after := h.active(); after.ID != before.ID || after.ProjectSlug != before.ProjectSlug {
		t.Fatal("viewing a project must not change the active chat")
	}
	h.tap("ui:projopen:" + api) // legacy
	h.tap("ui:pd:does-not-exist")
	if !strings.Contains(h.ch.last().Text, "not found") {
		t.Errorf("missing project: %q", h.ch.last().Text)
	}
}

func TestProjectCreateWizardThenChatAndRename(t *testing.T) {
	h := newHarness(t)
	h.mkProject("Existing")
	h.tap("ui:ncp") // New project & start
	if w := h.e.ui.get(); w == nil || w.Kind != "new_project" || w.Draft["then"] != "chat" {
		t.Fatalf("wizard: %+v", h.e.ui.get())
	}
	m := h.msg()
	m.Text = "Telebirr Mini App"
	h.e.handleUICallback(h.ctx, m)
	ch := h.active()
	if ch.ProjectSlug != "telebirr-mini-app" || ch.Mode != "project" {
		t.Fatalf("chat should be started inside new project: %+v", ch)
	}

	h.tap("ui:pren:telebirr-mini-app")
	m.Text = "Telebirr v2"
	h.e.handleUICallback(h.ctx, m)
	if !strings.Contains(h.ch.last().Text, "Telebirr v2") {
		t.Errorf("rename project card: %q", h.ch.last().Text)
	}
	meta, err := h.e.projects.ReadMeta("telebirr-mini-app")
	if err != nil || meta.Name != "Telebirr v2" || meta.Slug != "telebirr-mini-app" {
		t.Fatalf("meta after rename: %+v %v", meta, err)
	}

	// "new project & move here" from the move picker
	h.tap("ui:nc:-")
	h.say("loose chat")
	ref := chatRef(h.active().ID)
	h.tap("ui:pnm:" + ref)
	m.Text = "Brand New"
	h.e.handleUICallback(h.ctx, m)
	if got := h.active(); got.ProjectSlug != "brand-new" {
		t.Fatalf("chat should have moved into new project: %+v", got)
	}
}

func TestBusyAgentBlocksSwitching(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.say("work")
	id := h.active().ID
	h.e.active = &activeRun{}

	h.tap("ui:nc:" + api)
	if h.active().ID != id || h.active().ProjectSlug != "" {
		t.Fatal("new chat must be refused while agent runs")
	}
	if len(h.rep.texts) == 0 || !strings.Contains(h.rep.texts[len(h.rep.texts)-1], "/stop") {
		t.Fatalf("expected busy notice, got %v", h.rep.texts)
	}
	h.tap("ui:cmt:" + chatRef(id) + ":" + api)
	if h.active().ProjectSlug != "" {
		t.Fatal("move of the running chat must be refused")
	}
	h.e.active = nil
	h.tap("ui:cmt:" + chatRef(id) + ":" + api)
	if h.active().ProjectSlug != api {
		t.Fatal("move should work once idle")
	}
}

func TestLongNamesAndUnicodeStayWithinLimits(t *testing.T) {
	h := newHarness(t)
	long := "A very long project name that definitely exceeds forty characters in length"
	slug := h.mkProject(long)
	if len(slug) > 40 {
		t.Fatalf("slug too long for callbacks: %d", len(slug))
	}
	h.tap("ui:nc:" + slug)
	h.say("ፈጣን ዕጣ — የውይይት ርዕስ በጣም ረጅም የሆነ ርዕስ ለመፈተሽ የተጻፈ ነው ይህ ረጅም ነው")
	h.tap("ui:nc:-")
	h.say("Afaan Oromoo: Haasaa dheeraa dha kun ")
	for _, d := range []string{"ui:chats:all:0", "ui:chats:p=" + slug + ":0", "ui:projects:0", "ui:pd:" + slug, "ui:new", "ui:cur", "ui:cfp:0", "ui:home"} {
		h.tap(d)
	}
	ref := chatRef(h.active().ID)
	h.tap("ui:cmv:" + ref + ":0")
	h.tap("ui:cm:" + ref)
	h.assertCallbackLimits()
	for _, km := range h.ch.all {
		if len(km.Text) > 3500 {
			t.Errorf("keyboard text too long (%d) — Telegram limit risk", len(km.Text))
		}
	}
}

func TestHomeShowsCurrentChatAndNewReplyKeyboard(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.tap("ui:nc:" + api)
	h.say("Bugfix auth flow")
	h.cmd(channel.CmdStart, "")
	km := h.ch.last()
	if !strings.Contains(km.Text, "Now:") || !strings.Contains(km.Text, api) || !strings.Contains(km.Text, "Bugfix auth flow") {
		t.Errorf("home should show the current location:\n%s", km.Text)
	}
	flat := ""
	for _, r := range km.ReplyKeyboard {
		flat += strings.Join(r, "|") + "\n"
	}
	if !strings.Contains(flat, navChats) || !strings.Contains(flat, navNewChat) || !strings.Contains(flat, navProjects) {
		t.Errorf("reply keyboard: %q", flat)
	}
	// reply-keyboard labels route to the new screens (incl. the legacy History label)
	h.tap(navChats)
	if !strings.Contains(h.ch.last().Text, "Chats") {
		t.Errorf("navChats: %q", h.ch.last().Text)
	}
	h.tap(navHistory)
	h.tap(navProjects)
	h.tap(navNewChat)
	if !strings.Contains(h.ch.last().Text, "Where should it live") {
		t.Errorf("navNewChat should open the picker: %q", h.ch.last().Text)
	}
	h.assertCallbackLimits()
}

func TestSlashNewVariants(t *testing.T) {
	h := newHarness(t)
	api := h.mkProject("API v2")
	h.say("existing content")

	h.cmd(channel.CmdNewChat, "") // picker
	if !strings.Contains(h.ch.last().Text, "Where should it live") {
		t.Fatalf("/new should open the picker: %q", h.ch.last().Text)
	}
	h.cmd(channel.CmdNewChat, "API") // unique prefix match
	if h.active().ProjectSlug != api {
		t.Fatalf("/new API: %+v", h.active())
	}
	h.say("more content")
	h.cmd(channel.CmdNewChat, "-")
	if h.active().ProjectSlug != "" {
		t.Fatalf("/new - should start general: %+v", h.active())
	}
	h.say("third")
	h.cmd(channel.CmdNewChat, "nonexistent-project")
	if !strings.Contains(h.ch.last().Text, "Where should it live") {
		t.Fatalf("unknown project should fall back to picker")
	}
}

func TestCbGuardDegradesOversizedPayload(t *testing.T) {
	if got := cb("cmt", chatRef("0123456789abcdef"), strings.Repeat("x", 80)); got != "ui:noop" {
		t.Fatalf("oversized callback must degrade to noop, got %q", got)
	}
	if got := cb("co", "abcd1234"); got != "ui:co:abcd1234" {
		t.Fatalf("got %q", got)
	}
}

func TestHelpers(t *testing.T) {
	if s := parseScope("p=my-proj"); s.Kind != "proj" || s.Slug != "my-proj" || s.token() != "p=my-proj" {
		t.Errorf("scope: %+v", s)
	}
	if parseScope("garbage").Kind != "all" || parseScope("gen").Kind != "gen" {
		t.Error("scope parse")
	}
	if got := mdSafe("a_b *c* `d` [e]"); strings.ContainsAny(got, "_*`[]") {
		t.Errorf("mdSafe left markdown metachars: %q", got)
	}
	if got := truncRunes("ፈጣን ዕጣ ፈጣን ዕጣ", 5); !strings.HasSuffix(got, "…") || []rune(got)[0] != 'ፈ' {
		t.Errorf("truncRunes must be rune-safe: %q", got)
	}
	start, end, pages, page := pageBounds(13, 6, 99)
	if start != 12 || end != 13 || pages != 3 || page != 2 {
		t.Errorf("pageBounds: %d %d %d %d", start, end, pages, page)
	}
	if s, e, p, pg := pageBounds(0, 6, 3); s != 0 || e != 0 || p != 1 || pg != 0 {
		t.Errorf("empty pageBounds: %d %d %d %d", s, e, p, pg)
	}
}
