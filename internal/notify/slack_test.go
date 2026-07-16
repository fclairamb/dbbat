package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

type fakePersister struct {
	mu       sync.Mutex
	channel  string
	ts       string
	uid      uuid.UUID
	called   bool
	failNext error
}

func (f *fakePersister) SetGrantRequestSlackMessage(_ context.Context, uid uuid.UUID, channel, ts string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.called = true
	f.uid = uid
	f.channel = channel
	f.ts = ts

	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil

		return err
	}

	return nil
}

func nopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fakeSlack stands up an httptest server that imitates the subset of
// Slack's web API the notifier touches: chat.postMessage, chat.update.
// Each call records what was sent and returns a canned response.
type fakeSlack struct {
	t *testing.T

	mu sync.Mutex

	postCalls   []url.Values
	updateCalls []url.Values

	postResp   func() string
	updateResp func() string

	srv *httptest.Server
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()

	f := &fakeSlack{
		t: t,
		postResp: func() string {
			return `{"ok":true,"channel":"C123","ts":"1.2"}`
		},
		updateResp: func() string {
			return `{"ok":true,"channel":"C123","ts":"1.2"}`
		},
	}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		f.mu.Lock()
		defer f.mu.Unlock()

		switch r.URL.Path {
		case "/chat.postMessage":
			f.postCalls = append(f.postCalls, r.Form)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, f.postResp())
		case "/chat.update":
			f.updateCalls = append(f.updateCalls, r.Form)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, f.updateResp())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeSlack) client() *slack.Client {
	// trailing slash matters for slack-go
	return slack.New("xoxb-test", slack.OptionAPIURL(f.srv.URL+"/"))
}

func (f *fakeSlack) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.postCalls)
}

func (f *fakeSlack) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.updateCalls)
}

func sampleEvent(action GrantAction) GrantRequestEvent {
	uid := uuid.New()
	channel := "C123"
	ts := "1.2"

	return GrantRequestEvent{
		Action: action,
		Request: &store.GrantRequest{
			UID:            uid,
			Justification:  "investigating bug 1234",
			SlackChannel:   &channel,
			SlackMessageTS: &ts,
		},
		Definition: &store.GrantDefinition{
			Name:            "Read-only 1h",
			DurationSeconds: 3600,
		},
		Database: &store.Database{Name: "prod-db"},
		Requester: &store.User{
			UID:      uuid.New(),
			Username: "alice",
		},
	}
}

func TestNotifier_DisabledIsNoOp(t *testing.T) {
	t.Parallel()

	n, err := NewSlackNotifier(config.SlackNotifyConfig{}, "https://example.com", nil, nopLogger())
	if err != nil {
		t.Fatalf("NewSlackNotifier disabled: %v", err)
	}

	if n != nil {
		t.Fatal("expected nil notifier when disabled")
	}

	// Calling on a nil notifier should be a no-op.
	(*SlackNotifier)(nil).NotifyGrantRequest(context.Background(), sampleEvent(GrantActionCreated))
}

func TestNotifier_MissingChannelFails(t *testing.T) {
	t.Parallel()

	_, err := NewSlackNotifier(
		config.SlackNotifyConfig{BotToken: "xoxb-test"},
		"https://example.com",
		nil, nopLogger(),
	)
	if err == nil {
		t.Fatal("expected error when channel missing")
	}
}

func TestNotifier_MissingPublicURLFails(t *testing.T) {
	t.Parallel()

	_, err := NewSlackNotifier(
		config.SlackNotifyConfig{BotToken: "xoxb-test", Channel: "#dbbat"},
		"",
		nil, nopLogger(),
	)
	if err == nil {
		t.Fatal("expected error when public URL missing")
	}
}

func TestNotifier_CreatedPostsAndPersists(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	per := &fakePersister{}

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		store:     per,
		log:       nopLogger(),
	}

	ev := sampleEvent(GrantActionCreated)
	// Clear the prior message coords on the request — created path must
	// post fresh and persist what comes back.
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil

	n.NotifyGrantRequest(context.Background(), ev)

	if got := fake.postCount(); got != 1 {
		t.Fatalf("post calls = %d, want 1", got)
	}

	if got := fake.updateCount(); got != 0 {
		t.Fatalf("update calls = %d, want 0", got)
	}

	if !per.called || per.channel != "C123" || per.ts != "1.2" {
		t.Errorf("persister: called=%v channel=%q ts=%q", per.called, per.channel, per.ts)
	}

	// Sanity: the posted blocks should reference the event content.
	body := fake.postCalls[0].Get("blocks")
	if body == "" {
		t.Fatal("blocks form field empty")
	}

	if !strings.Contains(body, "alice") || !strings.Contains(body, "prod-db") {
		t.Errorf("blocks JSON missing expected fields: %s", body)
	}

	// Block list should parse as JSON array.
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Errorf("blocks JSON invalid: %v", err)
	}
}

func TestNotifier_DecidedUpdatesExistingMessage(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}

	n.NotifyGrantRequest(context.Background(), sampleEvent(GrantActionApproved))

	if got := fake.updateCount(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}

	if got := fake.postCount(); got != 0 {
		t.Fatalf("post calls = %d, want 0", got)
	}
}

func TestNotifier_DecidedFallsBackToPostWhenNoTS(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}

	ev := sampleEvent(GrantActionDenied)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil

	n.NotifyGrantRequest(context.Background(), ev)

	if got := fake.postCount(); got != 1 {
		t.Fatalf("post calls = %d, want 1", got)
	}

	if got := fake.updateCount(); got != 0 {
		t.Fatalf("update calls = %d, want 0", got)
	}
}

func TestNotifier_SigningSecretWithoutBotTokenFails(t *testing.T) {
	t.Parallel()

	_, err := NewSlackNotifier(
		config.SlackNotifyConfig{Channel: "#dbbat", SigningSecret: "shhh"},
		"https://example.com",
		nil, nopLogger(),
	)
	if !errors.Is(err, ErrSigningSecretWithoutBotToken) {
		t.Fatalf("expected ErrSigningSecretWithoutBotToken, got %v", err)
	}
}

func TestNotifier_AppTokenWithoutBotTokenFails(t *testing.T) {
	t.Parallel()

	_, err := NewSlackNotifier(
		config.SlackNotifyConfig{Channel: "#dbbat", AppToken: "xapp-shhh"},
		"https://example.com",
		nil, nopLogger(),
	)
	if !errors.Is(err, ErrAppTokenWithoutBotToken) {
		t.Fatalf("expected ErrAppTokenWithoutBotToken, got %v", err)
	}
}

func TestNotifier_InteractiveFlagViaAppToken(t *testing.T) {
	t.Parallel()

	n, err := NewSlackNotifier(
		config.SlackNotifyConfig{BotToken: "xoxb-test", Channel: "#dbbat", AppToken: "xapp-test"},
		"https://example.com",
		nil, nopLogger(),
	)
	if err != nil {
		t.Fatalf("NewSlackNotifier: %v", err)
	}

	if !n.Interactive() {
		t.Error("expected Interactive() true when app token + bot token set (Socket Mode)")
	}
}

func TestNotifier_InteractiveFlag(t *testing.T) {
	t.Parallel()

	n, err := NewSlackNotifier(
		config.SlackNotifyConfig{BotToken: "xoxb-test", Channel: "#dbbat", SigningSecret: "shhh"},
		"https://example.com",
		nil, nopLogger(),
	)
	if err != nil {
		t.Fatalf("NewSlackNotifier: %v", err)
	}

	if !n.Interactive() {
		t.Error("expected Interactive() true when signing secret + bot token set")
	}

	if n.SigningSecret() != "shhh" {
		t.Errorf("SigningSecret() = %q, want %q", n.SigningSecret(), "shhh")
	}

	// A nil notifier is never interactive and has no secret.
	if (*SlackNotifier)(nil).Interactive() {
		t.Error("nil notifier should not be interactive")
	}

	if (*SlackNotifier)(nil).SigningSecret() != "" {
		t.Error("nil notifier should have empty signing secret")
	}
}

func TestNotifier_CreatedInteractiveRendersButtonsAndMentions(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:      fake.client(),
		channel:     "#dbbat",
		publicURL:   "https://example.com",
		interactive: true,
		log:         nopLogger(),
	}

	ev := sampleEvent(GrantActionCreated)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil
	ev.Interactive = true
	ev.RequesterSlackID = "U0REQ"
	ev.AdminSlackIDs = []string{"U0ADM1", "U0ADM2"}

	n.NotifyGrantRequest(context.Background(), ev)

	if got := fake.postCount(); got != 1 {
		t.Fatalf("post calls = %d, want 1", got)
	}

	blocks := fake.postCalls[0].Get("blocks")
	// Mention line: requester + both admins. The `<@ID>` wrappers are
	// JSON-escaped (<…>) in the serialized blocks, so assert on
	// the raw IDs, which appear only inside the mention wrappers.
	for _, want := range []string{"@U0REQ", "@U0ADM1", "@U0ADM2", "please approve or deny"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("blocks missing %q: %s", want, blocks)
		}
	}
	// Action buttons present with our action IDs.
	for _, want := range []string{ActionApprove, ActionDeny} {
		if !strings.Contains(blocks, want) {
			t.Errorf("blocks missing action_id %q: %s", want, blocks)
		}
	}
}

func TestNotifier_CreatedNonInteractiveHasNoButtons(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}

	ev := sampleEvent(GrantActionCreated)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil
	// Interactive not set.

	n.NotifyGrantRequest(context.Background(), ev)

	blocks := fake.postCalls[0].Get("blocks")
	if strings.Contains(blocks, ActionApprove) || strings.Contains(blocks, ActionDeny) {
		t.Errorf("non-interactive created message should have no buttons: %s", blocks)
	}
}

func TestNotifier_DecidedRenderDropsButtons(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:      fake.client(),
		channel:     "#dbbat",
		publicURL:   "https://example.com",
		interactive: true,
		log:         nopLogger(),
	}

	ev := sampleEvent(GrantActionApproved)
	ev.Interactive = true // even interactive, decided renders must drop buttons
	ev.Decider = &store.User{Username: "carol"}

	n.NotifyGrantRequest(context.Background(), ev)

	if got := fake.updateCount(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}

	blocks := fake.updateCalls[0].Get("blocks")
	if strings.Contains(blocks, ActionApprove) || strings.Contains(blocks, ActionDeny) {
		t.Errorf("decided render should drop buttons: %s", blocks)
	}
}

func TestNotifier_AutoApprovedRenderHasNoButtonsOrDeciderLine(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:      fake.client(),
		channel:     "#dbbat",
		publicURL:   "https://example.com",
		interactive: true,
		log:         nopLogger(),
	}

	ev := sampleEvent(GrantActionApproved)
	ev.Interactive = true
	// Decider left nil: auto-approved by the definition's policy, not a
	// human — this is the code path handleCreateGrantRequest exercises when
	// def.AutoApprove is set.

	n.NotifyGrantRequest(context.Background(), ev)

	if got := fake.updateCount(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}

	blocks := fake.updateCalls[0].Get("blocks")
	if strings.Contains(blocks, ActionApprove) || strings.Contains(blocks, ActionDeny) {
		t.Errorf("auto-approved render should have no action buttons: %s", blocks)
	}

	if strings.Contains(blocks, "Approved by") {
		t.Errorf("auto-approved render should not claim a human decider: %s", blocks)
	}

	if !strings.Contains(blocks, "auto-approved") {
		t.Errorf("auto-approved render should say so in the status label: %s", blocks)
	}
}

func TestNotifier_MentionLineFallsBackToUsername(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:      fake.client(),
		channel:     "#dbbat",
		publicURL:   "https://example.com",
		interactive: true,
		log:         nopLogger(),
	}

	ev := sampleEvent(GrantActionCreated)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil
	ev.Interactive = true
	// No RequesterSlackID and no AdminSlackIDs: requester renders as plain
	// username, and the second sentence is dropped.

	n.NotifyGrantRequest(context.Background(), ev)

	blocks := fake.postCalls[0].Get("blocks")
	if !strings.Contains(blocks, "alice requested access") {
		t.Errorf("expected plain username fallback in mention line: %s", blocks)
	}

	if strings.Contains(blocks, "please approve or deny") {
		t.Errorf("second sentence should be dropped when no admins linked: %s", blocks)
	}
}

func TestNotifier_PostThreadReply(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}

	n.PostThreadReply(context.Background(), "C123", "1.2", "✅ approved by <@U0ADM1>")

	if got := fake.postCount(); got != 1 {
		t.Fatalf("post calls = %d, want 1", got)
	}

	form := fake.postCalls[0]
	if form.Get("thread_ts") != "1.2" {
		t.Errorf("thread_ts = %q, want %q", form.Get("thread_ts"), "1.2")
	}

	if !strings.Contains(form.Get("text"), "approved") {
		t.Errorf("thread reply text missing: %q", form.Get("text"))
	}

	// Missing coordinates → no-op.
	n.PostThreadReply(context.Background(), "", "", "ignored")
	if fake.postCount() != 1 {
		t.Errorf("expected no extra post for empty coords, got %d", fake.postCount())
	}

	// nil notifier → no-op, no panic.
	(*SlackNotifier)(nil).PostThreadReply(context.Background(), "C123", "1.2", "x")
}

func TestNotifier_SlackErrorIsLoggedNotReturned(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.postResp = func() string {
		return `{"ok":false,"error":"channel_not_found"}`
	}

	n := &SlackNotifier{
		client:    fake.client(),
		channel:   "#missing",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}

	// Should not panic; should swallow the error.
	ev := sampleEvent(GrantActionCreated)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil

	n.NotifyGrantRequest(context.Background(), ev)

	if fake.postCount() != 1 {
		t.Errorf("expected one post attempt, got %d", fake.postCount())
	}
}
