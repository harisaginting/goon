package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
)

// fakeTelegram is a minimal stand-in for api.telegram.org used in tests.
// It records every sendMessage call so assertions can inspect what the bot
// sent back to which chat. It also captures the JSON payload of the most
// recent setMyCommands so the registration test can verify it.
type fakeTelegram struct {
	mu               sync.Mutex
	server           *httptest.Server
	sent             []sentMsg
	tokenSeg         string
	commandsRegistered string
}

type sentMsg struct {
	ChatID string
	Text   string
}

func newFakeTelegram(t *testing.T, token string) *fakeTelegram {
	t.Helper()
	ft := &fakeTelegram{tokenSeg: "/bot" + token + "/"}
	ft.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip "/bot<token>/<method>"
		path := strings.TrimPrefix(r.URL.Path, ft.tokenSeg)
		switch path {
		case "getMe":
			io.WriteString(w, `{"ok":true,"result":{"username":"goon_test_bot"}}`)
		case "getUpdates":
			io.WriteString(w, `{"ok":true,"result":[]}`)
		case "setMyCommands":
			body, _ := io.ReadAll(r.Body)
			ft.mu.Lock()
			ft.commandsRegistered = string(body)
			ft.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			ft.mu.Lock()
			ft.sent = append(ft.sent, sentMsg{
				ChatID: form.Get("chat_id"),
				Text:   form.Get("text"),
			})
			ft.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ft.server.Close)
	return ft
}

func (f *fakeTelegram) lastSent() (sentMsg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMsg{}, false
	}
	return f.sent[len(f.sent)-1], true
}

func (f *fakeTelegram) all() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMsg, len(f.sent))
	copy(out, f.sent)
	return out
}

// newTestBot wires a Bot against the fake telegram server. Returns the bot,
// the fake server, the memory instance, and the mock LLM (handy for tests
// that exercise chat or /run).
func newTestBot(t *testing.T, secret string, replies []string) (*Bot, *fakeTelegram, *memory.Memory, *llm.Mock) {
	t.Helper()
	ft := newFakeTelegram(t, "TEST-TOKEN")
	mem := memory.Disabled()
	mock := llm.NewMock(replies)
	var out bytes.Buffer
	b, err := New(Options{
		Token:      "TEST-TOKEN",
		Secret:     secret,
		APIBaseURL: ft.server.URL,
		Memory:     mem,
		LLM:        mock,
		Stdout:     &out,
		Stderr:     &out,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, ft, mem, mock
}

// makeUpdate builds a fake Telegram Update for tests.
func makeUpdate(chatID int64, text string) Update {
	return Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 1,
			Text:      text,
			Chat:      Chat{ID: chatID, Type: "private"},
			From:      User{ID: chatID, Username: "harisa", FirstName: "Harisa"},
		},
	}
}

func TestBot_RejectsUnauthorized(t *testing.T) {
	b, ft, _, _ := newTestBot(t, "shh", nil)
	b.handleUpdate(context.Background(), makeUpdate(42, "/status"))
	got, ok := ft.lastSent()
	if !ok {
		t.Fatal("expected a reply")
	}
	if !strings.Contains(got.Text, "not authorized") {
		t.Errorf("text = %q", got.Text)
	}
}

func TestBot_AuthWrongSecret(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "right-secret", nil)
	b.handleUpdate(context.Background(), makeUpdate(42, "/auth wrong-secret"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "wrong secret") {
		t.Errorf("text = %q", got.Text)
	}
	if mem.IsChatAuthorized(42) {
		t.Error("chat should not be authorized after wrong secret")
	}
}

func TestBot_AuthCorrectSecret(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "correct-horse", nil)
	b.handleUpdate(context.Background(), makeUpdate(42, "/auth correct-horse"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "authenticated") {
		t.Errorf("text = %q", got.Text)
	}
	if !mem.IsChatAuthorized(42) {
		t.Error("chat should be authorized")
	}
}

func TestBot_StatusCommand(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	mem.SetStatus(memory.DaemonStatus{Running: true, PID: 4242, BoardName: "jira"})
	b.handleUpdate(context.Background(), makeUpdate(42, "/status"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "running:") || !strings.Contains(got.Text, "jira") {
		t.Errorf("status text:\n%s", got.Text)
	}
}

func TestBot_QueueAndAnswer(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	qid := mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "Approve?"})

	b.handleUpdate(context.Background(), makeUpdate(42, "/queue"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, qid) {
		t.Errorf("queue should mention %s:\n%s", qid, got.Text)
	}

	b.handleUpdate(context.Background(), makeUpdate(42, "/answer "+qid+" yes please"))
	if len(mem.PendingQuestions()) != 0 {
		t.Errorf("question still pending after /answer")
	}
}

func TestBot_LogoutRevokes(t *testing.T) {
	b, _, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "/logout"))
	if mem.IsChatAuthorized(42) {
		t.Error("chat should not be authorized after /logout")
	}
}

func TestBot_PlainChatHitsLLM(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", []string{"sure thing!"})
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "hey what's the deal with goroutines"))
	got, _ := ft.lastSent()
	if got.Text != "sure thing!" {
		t.Errorf("expected llm reply, got %q", got.Text)
	}
}

func TestBot_PRsCommandLists(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")

	host := githost.NewMock()
	host.OpenPRs = []githost.PR{
		{Number: 7, Title: "Fix login", URL: "https://x/7", Author: "alice", Repo: "o/r"},
		{Number: 9, Title: "Add metrics", URL: "https://x/9", Author: "bob", Repo: "o/r"},
	}
	b.opts.Host = host

	b.handleUpdate(context.Background(), makeUpdate(42, "/prs o/r"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "#7") || !strings.Contains(got.Text, "#9") {
		t.Errorf("prs text should list PRs:\n%s", got.Text)
	}
}

func TestBot_ApprovePRWithMockHost(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")

	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 7, Title: "x", Repo: "o/r"}}
	b.opts.Host = host

	b.handleUpdate(context.Background(), makeUpdate(42, "/approve o/r 7 looks good"))
	if len(host.Approved) != 1 || host.Approved[0] != 7 {
		t.Errorf("expected approve recorded, got %v", host.Approved)
	}
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "approved") {
		t.Errorf("text: %q", got.Text)
	}
}

func TestBot_PRCommandsRefuseWhenNoHost(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.opts.Host = nil
	b.handleUpdate(context.Background(), makeUpdate(42, "/prs"))
	got, _ := ft.lastSent()
	for _, want := range []string{"GOON_GIT_HOST", "github", "bitbucket"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("expected %q in diagnostic message, got:\n%s", want, got.Text)
		}
	}
}

// hostWithoutReview is a Host that intentionally does NOT implement
// PRReviewer — github, gitlab and bitbucket all do now, so this is a
// synthetic adapter. Used to verify the bot's "not yet implemented"
// branch fires with a clear name for any host that lacks review support.
type hostWithoutReview struct{}

func (hostWithoutReview) Name() string { return "fake-no-review" }
func (hostWithoutReview) OpenPR(_ context.Context, _ githost.CreateOptions) (githost.PR, error) {
	return githost.PR{}, nil
}

func TestBot_PRCommandsRefuseWhenHostDoesntImplementReview(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.opts.Host = hostWithoutReview{}
	b.handleUpdate(context.Background(), makeUpdate(42, "/prs"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "fake-no-review") {
		t.Errorf("expected adapter name in message, got: %q", got.Text)
	}
	if !strings.Contains(got.Text, "not yet implemented") {
		t.Errorf("expected 'not yet implemented', got: %q", got.Text)
	}
}

func TestBot_DisallowedCommandRejected(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "/uninstall"))
	got, _ := ft.lastSent()
	// Cycle 1 reworded this from "not allowed" to "not available over
	// Telegram" so the user knows where the limitation lives.
	if !strings.Contains(got.Text, "not available over Telegram") {
		t.Errorf("text: %q", got.Text)
	}
}

// TestBot_StartIsFriendlyForAuthenticated covers the cycle-1 fix:
// Telegram's `/start` convention should NOT trip the disallowed-CLI
// path for already-authenticated users — they tap it as a habit and
// the response should be a friendly greeting, not a refusal.
func TestBot_StartIsFriendlyForAuthenticated(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "/start"))
	got, _ := ft.lastSent()
	if strings.Contains(got.Text, "✗") || strings.Contains(got.Text, "not available") {
		t.Errorf("/start should produce a friendly greeting for auth'd users, got: %q", got.Text)
	}
	for _, want := range []string{"authenticated", "/help"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("expected %q in /start reply, got: %q", want, got.Text)
		}
	}
}

// TestBot_StartStillRequiresAuth: an UNAUTHENTICATED chat sending
// /start must hit the auth gate, not the friendly greeting (otherwise
// /start could be used to probe whether a chat is authorized).
func TestBot_StartStillRequiresAuth(t *testing.T) {
	b, ft, _, _ := newTestBot(t, "s", nil)
	b.handleUpdate(context.Background(), makeUpdate(42, "/start"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "not authorized") {
		t.Errorf("unauth /start should hit auth gate, got: %q", got.Text)
	}
}

// TestPollIntervalLabel covers the GOON_POLL_SECONDS humanizer used by
// /answer to tell users when to expect daemon resume.
func TestPollIntervalLabel(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "5m"},
		{"30", "30s"},
		{"60", "1m"},
		{"600", "10m"},
		{"abc", "5m"}, // bad value
		{"0", "5m"},   // zero
	}
	for _, c := range cases {
		t.Setenv("GOON_POLL_SECONDS", c.env)
		got := pollIntervalLabel()
		if got != c.want {
			t.Errorf("pollIntervalLabel(env=%q) = %q, want %q", c.env, got, c.want)
		}
	}
}

// TestScrubEnv ensures the runGoonCLI scrub list strips every secret
// before passing env to a subcommand. Regression-guards token leakage
// via the bot's CLI passthrough (the cycle-1 security fix).
func TestScrubEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"TELEGRAM_BOT_TOKEN=should-be-stripped",
		"GOON_TELEGRAM_SECRET=also-stripped",
		"HOME=/home/me",
	}
	out := scrubEnv(in)
	for _, e := range out {
		if strings.HasPrefix(e, "TELEGRAM_BOT_TOKEN=") || strings.HasPrefix(e, "GOON_TELEGRAM_SECRET=") {
			t.Errorf("scrubEnv leaked: %q", e)
		}
	}
	// Other entries must survive.
	hasPath := false
	for _, e := range out {
		if e == "PATH=/usr/bin" {
			hasPath = true
		}
	}
	if !hasPath {
		t.Error("scrubEnv dropped non-secret entries")
	}
}

// TestSendChunked_RuneAware guards the UTF-8 split fix: long input
// containing multi-byte runes (emoji / CJK) must produce chunks that
// each parse as valid UTF-8. Without rune-aware cutting Telegram
// returns 400.
func TestSendChunked_RuneAware(t *testing.T) {
	// Build a string just over 4000 bytes that's all 4-byte runes
	// (emoji). Each rune is 4 bytes, so 1100 runes = 4400 bytes.
	rune4 := "🤖"
	if len(rune4) != 4 {
		t.Fatalf("test precondition: expected 4-byte rune, got %d", len(rune4))
	}
	var sb strings.Builder
	for i := 0; i < 1100; i++ {
		sb.WriteString(rune4)
	}
	body := sb.String()

	b, ft, _, _ := newTestBot(t, "s", nil)
	b.SendChunked(context.Background(), 7, body)
	all := ft.all()
	if len(all) < 2 {
		t.Fatalf("expected ≥2 chunks for 4400-byte input, got %d", len(all))
	}
	for i, m := range all {
		if !utf8Valid(m.Text) {
			t.Errorf("chunk %d is invalid UTF-8 (length %d)", i, len(m.Text))
		}
	}
}

// utf8Valid wraps utf8.ValidString so the test reads cleanly.
func utf8Valid(s string) bool { return utf8.ValidString(s) }

func TestBot_HelpListsCommands(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "/help"))
	got, _ := ft.lastSent()
	for _, want := range []string{"/status", "/run", "/auth", "/prs", "/review"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("help missing %s", want)
		}
	}
}

func TestParsePRRef(t *testing.T) {
	cases := []struct {
		in       []string
		wantRepo string
		wantNum  int
		wantErr  bool
	}{
		{[]string{"o/r", "7"}, "o/r", 7, false},
		{[]string{"o/r"}, "", 0, true},
		{[]string{"badrepo", "7"}, "", 0, true},
		{[]string{"o/r", "abc"}, "", 0, true},
		{[]string{"o/r", "0"}, "", 0, true},
	}
	for _, c := range cases {
		repo, num, err := parsePRRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePRRef(%v) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePRRef(%v): %v", c.in, err)
			continue
		}
		if repo != c.wantRepo || num != c.wantNum {
			t.Errorf("parsePRRef(%v) = (%q,%d) want (%q,%d)",
				c.in, repo, num, c.wantRepo, c.wantNum)
		}
	}
}

// Sanity: bot Start must not panic when getUpdates returns 409 (another
// process polling the same token). It logs and continues until ctx ends.
func TestBot_Handles409Gracefully(t *testing.T) {
	mem := memory.Disabled()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			io.WriteString(w, `{"ok":true,"result":{"username":"x"}}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"ok":false,"description":"Conflict"}`)
	}))
	t.Cleanup(server.Close)
	var out bytes.Buffer
	b, err := New(Options{
		Token: "T", Secret: "s", APIBaseURL: server.URL,
		Memory: mem, Stdout: &out, Stderr: &out, PollTimeout: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Start(ctx)
		close(done)
	}()
	// Wait for at least one error log to land, then cancel.
	for i := 0; i < 50; i++ {
		if strings.Contains(out.String(), "409") {
			break
		}
	}
	cancel()
	<-done
}

func TestBot_RegisterCommandsPublishesToTelegram(t *testing.T) {
	b, ft, _, _ := newTestBot(t, "s", nil)
	if err := b.registerCommands(context.Background()); err != nil {
		t.Fatalf("registerCommands: %v", err)
	}
	ft.mu.Lock()
	got := ft.commandsRegistered
	ft.mu.Unlock()
	if got == "" {
		t.Fatal("setMyCommands was not called")
	}
	// Must include at least the headline commands.
	for _, want := range []string{`"help"`, `"status"`, `"prs"`, `"run"`, `"auth"`} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing %s:\n%s", want, got)
		}
	}
}

// TestBot_PauseResumeFlipsMemoryFlag covers /pause and /resume — the
// bot is one of three control surfaces (CLI, web, telegram) that drive
// Memory.Status.Paused. All three must agree, so this test asserts the
// bot really flips the same bit `goon pause` writes.
func TestBot_PauseResumeFlipsMemoryFlag(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")

	b.handleUpdate(context.Background(), makeUpdate(42, "/pause"))
	if !mem.IsPaused() {
		t.Fatal("/pause should set Memory.Paused = true")
	}
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "paused") {
		t.Errorf("expected 'paused' in reply, got %q", got.Text)
	}

	b.handleUpdate(context.Background(), makeUpdate(42, "/resume"))
	if mem.IsPaused() {
		t.Fatal("/resume should clear Memory.Paused")
	}
	got, _ = ft.lastSent()
	if !strings.Contains(got.Text, "resumed") {
		t.Errorf("expected 'resumed' in reply, got %q", got.Text)
	}
}

// TestBot_StatusShowsPaused covers the surfacing of the Paused flag in
// /status output. Without this, a paused daemon looks "running" to a
// confused user staring at /status wondering why goon isn't picking up
// new tickets.
func TestBot_StatusShowsPaused(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	mem.SetStatus(memory.DaemonStatus{Running: true, Paused: true, BoardName: "jira"})

	b.handleUpdate(context.Background(), makeUpdate(42, "/status"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "paused:") {
		t.Errorf("expected 'paused:' line in /status when daemon is paused, got:\n%s", got.Text)
	}
}

// TestBot_ListTicketsShowsTicketsAndStatus exercises the /tickets path
// end-to-end: seed two tickets + one paused workflow, verify the
// rendered text contains both keys, the paused-stage indicator, and the
// /answer hint pointing at the right question id.
func TestBot_ListTicketsShowsTicketsAndStatus(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")

	mem.SeenTicket(memory.TicketSnapshot{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Status: "open",
	})
	mem.SeenTicket(memory.TicketSnapshot{
		ID: "ENG-2", Source: "jira", Key: "ENG-2",
		Title: "Logout bug", Status: "in_progress",
	})
	mem.UpsertWorkflow(memory.Workflow{
		ID: "wf-1", TicketID: "ENG-1", TicketKey: "ENG-1",
		State: memory.WFAwaitingApproval, Stage: "approve_plan",
		PendingQuestionID: "q-7",
	})

	b.handleUpdate(context.Background(), makeUpdate(42, "/tickets"))
	got, _ := ft.lastSent()
	for _, want := range []string{"ENG-1", "ENG-2", "Add login", "Logout bug",
		"paused at approve_plan", "q-7"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("expected %q in /tickets output, got:\n%s", want, got.Text)
		}
	}
}

// TestBot_TicketDetailRendersPlanAndApprovals covers /ticket showing the
// plan checklist (✓/✗), recorded approvals, and the pending-question
// reply hint. This is the highest-information density command and the
// regression risk if the rendering ever drifts is high.
func TestBot_TicketDetailRendersPlanAndApprovals(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	mem.SeenTicket(memory.TicketSnapshot{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Status: "in_progress", URL: "https://x/ENG-1",
	})
	mem.UpsertWorkflow(memory.Workflow{
		ID: "wf-7", TicketID: "ENG-1", TicketKey: "ENG-1", Title: "Add login",
		State: memory.WFAwaitingApproval, Stage: "approve_plan",
		PendingQuestionID: "q-9",
		Repo:              "/r/eng", Branch: "goon/eng-1", PRURL: "",
		Approvals: map[string]string{"confirm_repo": "yes"},
		Plan: []memory.PlanStep{
			{Index: 0, Title: "wire OAuth", Done: true},
			{Index: 1, Title: "add /login", Done: true},
			{Index: 2, Title: "add /logout", Done: false},
		},
	})

	b.handleUpdate(context.Background(), makeUpdate(42, "/ticket ENG-1"))
	got, _ := ft.lastSent()
	for _, want := range []string{
		"ENG-1", "Add login",
		"https://x/ENG-1",
		"approve_plan",
		"goon/eng-1",
		"confirm_repo: yes",
		"✓ wire OAuth",
		"✓ add /login",
		"✗ add /logout",
		"2/3 steps done",
		"/answer q-9",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("expected %q in /ticket output, got:\n%s", want, got.Text)
		}
	}
}

// TestBot_TicketDetailNotFound returns a friendly error rather than
// crashing when the user types an unknown id.
func TestBot_TicketDetailNotFound(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "/ticket NOPE-99"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "ticket not found") {
		t.Errorf("expected 'ticket not found', got: %q", got.Text)
	}
}

// TestBot_ListTicketsFilter narrows the list by substring match.
func TestBot_ListTicketsFilter(t *testing.T) {
	b, ft, mem, _ := newTestBot(t, "s", nil)
	mem.AuthorizeChat(42, "harisa", "Harisa")
	mem.SeenTicket(memory.TicketSnapshot{ID: "ENG-1", Key: "ENG-1", Title: "Add login"})
	mem.SeenTicket(memory.TicketSnapshot{ID: "WEB-1", Key: "WEB-1", Title: "Refactor API"})

	b.handleUpdate(context.Background(), makeUpdate(42, "/tickets login"))
	got, _ := ft.lastSent()
	if !strings.Contains(got.Text, "ENG-1") || strings.Contains(got.Text, "WEB-1") {
		t.Errorf("filter should keep ENG-1 only, got:\n%s", got.Text)
	}
}

// TestBot_ChatInjectsLiveContext is the cycle-7 regression test: the
// chat handler must include the current goon state in the LLM prompt
// so the user asking "list tickets" gets a real answer instead of
// "I'm just an AI without access to your project." This test seeds
// memory with a ticket, a workflow, a pending question, and a learned
// repo, then sends a plain-text chat message and asserts the recorded
// LLM messages contain each of those facts.
func TestBot_ChatInjectsLiveContext(t *testing.T) {
	b, _, mem, mock := newTestBot(t, "s", []string{"sure thing"})
	mem.AuthorizeChat(42, "harisa", "Harisa")
	mem.SetStatus(memory.DaemonStatus{Running: true, BoardName: "jira", HostName: "bitbucket"})
	mem.SeenTicket(memory.TicketSnapshot{
		ID: "ENG-7", Source: "jira", Key: "ENG-7",
		Title: "Add login flow", Status: "open",
	})
	mem.UpsertWorkflow(memory.Workflow{
		ID: "wf-x", TicketID: "ENG-7", TicketKey: "ENG-7", Title: "Add login flow",
		State: memory.WFAwaitingApproval, Stage: "approve_plan",
		PendingQuestionID: "q-9",
	})
	mem.AskQuestion(memory.Question{ID: "q-9", TicketID: "ENG-7", Question: "Approve plan?"})
	mem.RecordRepoChoice("ENG", "/r/eng")

	b.handleUpdate(context.Background(), makeUpdate(42, "give me list open ticket"))

	// Inspect the LLM's recorded messages. The state block lives in
	// LastMsgs as a system message; the user's text is the last entry.
	combined := ""
	for _, m := range mock.LastMsgs {
		combined += m.Content + "\n"
	}
	for _, want := range []string{
		"GOON STATE",
		"ENG-7",
		"Add login flow",
		"q-9",
		"jira",
		"bitbucket",
		"/r/eng",
		"/tickets", // command pointer in the system prompt
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("expected live-context injection to include %q; messages were:\n%s",
				want, combined)
		}
	}
}

// TestBot_ChatNoMemoryDoesntPanic guards the chat path against a
// Disabled memory or one with no data — chat should still work, just
// with an "operating without context" note.
func TestBot_ChatNoMemoryDoesntPanic(t *testing.T) {
	b, _, mem, mock := newTestBot(t, "s", []string{"hi there"})
	mem.AuthorizeChat(42, "harisa", "Harisa")
	// No state seeded — fresh disabled memory.
	b.handleUpdate(context.Background(), makeUpdate(42, "hello"))
	combined := ""
	for _, m := range mock.LastMsgs {
		combined += m.Content + "\n"
	}
	if !strings.Contains(combined, "GOON STATE") {
		t.Errorf("expected state block even on empty memory; got:\n%s", combined)
	}
	if !strings.Contains(combined, "[tickets: none") {
		t.Errorf("expected explicit 'no tickets' note in state; got:\n%s", combined)
	}
}

// TestBot_ChatInjectsActiveKnowledge covers the cycle-7 follow-up: the
// chat handler must also surface the ACTIVE markdown notes (SOUL.md
// and the topic-note index) — that's goon's persistent knowledge
// layer. Without it the LLM can answer "what tickets exist" but not
// "what do you know about our auth flow."
func TestBot_ChatInjectsActiveKnowledge(t *testing.T) {
	// Isolated notes dir per test so we don't trample real state.
	memDir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", memDir)

	store, err := notes.New(memDir)
	if err != nil {
		t.Fatalf("notes.New: %v", err)
	}
	if err := store.Write(notes.SoulFilename,
		"# Conventions\n- branch prefix is goon/\n- always run goimports before PR"); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	if err := store.Write("learnings/oauth-flow.md",
		"# OAuth flow\nThe service uses PKCE + state cookie."); err != nil {
		t.Fatalf("write topic: %v", err)
	}

	b, _, mem, mock := newTestBot(t, "s", []string{"sure"})
	mem.AuthorizeChat(42, "harisa", "Harisa")
	b.handleUpdate(context.Background(), makeUpdate(42, "what do you know"))

	combined := ""
	for _, m := range mock.LastMsgs {
		combined += m.Content + "\n"
	}
	for _, want := range []string{
		"SOUL.md",
		"branch prefix is goon/",       // body content from soul
		"learnings/oauth-flow.md",      // topic note name
		"OAuth flow",                   // headline of topic note
		"/memory read",                 // command pointer
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("knowledge block missing %q; messages:\n%s", want, combined)
		}
	}
}

// TestBot_ChatHistoryRolls10TurnsDownTo6 is the regression test for the
// rolling chat-history cap. The chat helper promises a 6-turn rolling
// history (12 messages); send 10 user/assistant pairs and assert only the
// last 6 pairs survive, with the oldest evicted FIFO. Without this test a
// silent regression in trimHistory could let history grow unbounded per
// chat and slowly leak memory in long-lived sessions.
func TestBot_ChatHistoryRolls10TurnsDownTo6(t *testing.T) {
	b, _, _, _ := newTestBot(t, "s", nil)
	chatID := int64(99)
	for i := 0; i < 10; i++ {
		userMsg := fmt.Sprintf("u%d", i)
		assistantMsg := fmt.Sprintf("a%d", i)
		b.appendUserTurn(chatID, userMsg)
		b.appendAssistantTurn(chatID, assistantMsg)
	}
	b.chatHistMu.Lock()
	hist := b.chatHist[chatID]
	b.chatHistMu.Unlock()
	if len(hist) != 12 {
		t.Fatalf("history length = %d, want 12 (= 6 turns * 2)", len(hist))
	}
	// Must contain only the last 6 turns: u4..u9 / a4..a9.
	want := []string{"u4", "a4", "u5", "a5", "u6", "a6", "u7", "a7", "u8", "a8", "u9", "a9"}
	for i, msg := range hist {
		if msg.Content != want[i] {
			t.Errorf("hist[%d] = %q, want %q (oldest should be evicted FIFO)",
				i, msg.Content, want[i])
		}
	}
}

// TestBot_ChatHistoryIsolatedPerChat ensures two chats don't share the
// rolling history (a leak between two unrelated users would be a real
// privacy bug).
func TestBot_ChatHistoryIsolatedPerChat(t *testing.T) {
	b, _, _, _ := newTestBot(t, "s", nil)
	b.appendUserTurn(1, "alice-only")
	b.appendUserTurn(2, "bob-only")
	b.chatHistMu.Lock()
	defer b.chatHistMu.Unlock()
	if len(b.chatHist[1]) != 1 || b.chatHist[1][0].Content != "alice-only" {
		t.Errorf("chat 1: %+v", b.chatHist[1])
	}
	if len(b.chatHist[2]) != 1 || b.chatHist[2][0].Content != "bob-only" {
		t.Errorf("chat 2: %+v", b.chatHist[2])
	}
}

// json.Marshal sanity for Update parsing — guards the field tags.
func TestUpdate_JSONRoundtrip(t *testing.T) {
	in := `{"update_id":1,"message":{"message_id":2,"text":"hi","chat":{"id":3,"type":"private"},"from":{"id":4,"username":"u","first_name":"F"}}}`
	var u Update
	if err := json.Unmarshal([]byte(in), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.UpdateID != 1 || u.Message == nil || u.Message.Text != "hi" || u.Message.Chat.ID != 3 || u.Message.From.Username != "u" {
		t.Errorf("got: %+v", u)
	}
}
