package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/notify"
	"github.com/fclairamb/dbbat/internal/store"
)

const testSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5"

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// signSlackBody produces the X-Slack-Signature header value for a body at a
// given timestamp, matching slack.NewSecretsVerifier's expectation.
func signSlackBody(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(body)

	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// slackInteractionBody builds a form-encoded body carrying a block_actions
// payload for the given action and value.
func slackInteractionBody(t *testing.T, actionID, value, userID, responseURL string) []byte {
	t.Helper()

	cb := map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": userID},
		"response_url": responseURL,
		"actions": []map[string]any{
			// block_id is required for slack-go to classify this as a
			// block action (vs a legacy attachment action).
			{"action_id": actionID, "block_id": "grant_request_actions", "value": value, "type": "button"},
		},
	}

	payload, err := json.Marshal(cb)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	form := url.Values{}
	form.Set("payload", string(payload))

	return []byte(form.Encode())
}

// newSignedSlackRequest builds a gin context for a signed interaction POST
// with a valid current timestamp. Status is asserted via c.Writer.Status().
func newSignedSlackRequest(t *testing.T, body []byte) *gin.Context {
	t.Helper()

	ts := fmt.Sprintf("%d", time.Now().Unix())

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request.Header.Set("X-Slack-Request-Timestamp", ts)
	c.Request.Header.Set("X-Slack-Signature", signSlackBody(testSigningSecret, ts, body))

	return c
}

// stubDecider records calls and returns canned results, standing in for
// *Server's store/notify-backed decision behavior.
type stubDecider struct {
	mu sync.Mutex

	user    *store.User
	userErr error

	// mayDecide overrides the stub's admin-based authorization answer, so a
	// test can play a non-admin who *is* an access approver.
	mayDecide *bool
	// mayCancel overrides the requester gate for Withdraw clicks.
	mayCancel *bool

	approveOutcome *decideOutcome
	approveErr     error
	denyOutcome    *decideOutcome
	denyErr        error
	cancelOutcome  *decideOutcome
	cancelErr      error

	approveCalled  bool
	denyCalled     bool
	cancelCalled   bool
	rerenderCalled bool
	threadReplied  bool
	lastDeciderUID uuid.UUID

	done chan struct{}
}

func newStubDecider() *stubDecider {
	return &stubDecider{done: make(chan struct{}, 1)}
}

func (s *stubDecider) signalDone() {
	select {
	case s.done <- struct{}{}:
	default:
	}
}

func (s *stubDecider) waitDone(t *testing.T) {
	t.Helper()

	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for decision processing")
	}
}

func (s *stubDecider) userBySlackID(_ context.Context, _ string) (*store.User, error) {
	if s.userErr != nil {
		return nil, s.userErr
	}

	return s.user, nil
}

// mayDecideRequest stands in for the real gate (admin, or an access approver
// resolved off the target server). The stub has no store, so it answers on the
// admin half alone — which is what the Slack tests exercise — unless a test
// overrides it.
func (s *stubDecider) mayDecideRequest(_ context.Context, user *store.User, _ uuid.UUID) bool {
	if s.mayDecide != nil {
		return *s.mayDecide
	}

	return user != nil && user.IsAdmin()
}

func (s *stubDecider) approve(_ context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error) {
	s.mu.Lock()
	s.approveCalled = true
	s.lastDeciderUID = decider.UID
	s.mu.Unlock()

	_ = uid

	return s.approveOutcome, s.approveErr
}

func (s *stubDecider) deny(_ context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error) {
	s.mu.Lock()
	s.denyCalled = true
	s.lastDeciderUID = decider.UID
	s.mu.Unlock()

	_ = uid

	return s.denyOutcome, s.denyErr
}

func (s *stubDecider) mayCancelRequest(_ context.Context, _ *store.User, _ uuid.UUID) bool {
	return s.mayCancel != nil && *s.mayCancel
}

func (s *stubDecider) cancel(_ context.Context, _ uuid.UUID, by *store.User) (*decideOutcome, error) {
	s.mu.Lock()
	s.cancelCalled = true
	s.lastDeciderUID = by.UID
	s.mu.Unlock()

	return s.cancelOutcome, s.cancelErr
}

func (s *stubDecider) rerenderRequest(_ context.Context, _ uuid.UUID) {
	s.mu.Lock()
	s.rerenderCalled = true
	s.mu.Unlock()
}

func (s *stubDecider) postThreadReply(_ context.Context, _ notify.GrantRequestEvent, _ notify.GrantAction) {
	s.mu.Lock()
	s.threadReplied = true
	s.mu.Unlock()

	s.signalDone()
}

// ephemeralRecorder captures ephemeral posts to a fake response_url.
type ephemeralRecorder struct {
	mu    sync.Mutex
	posts []string
	srv   *httptest.Server
	got   chan struct{}
}

func newEphemeralRecorder(t *testing.T) *ephemeralRecorder {
	t.Helper()

	e := &ephemeralRecorder{got: make(chan struct{}, 4)}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)

		e.mu.Lock()
		e.posts = append(e.posts, msg.Text)
		e.mu.Unlock()

		select {
		case e.got <- struct{}{}:
		default:
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(e.srv.Close)

	return e
}

func (e *ephemeralRecorder) wait(t *testing.T) {
	t.Helper()

	select {
	case <-e.got:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ephemeral post")
	}
}

func (e *ephemeralRecorder) lastPost() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.posts) == 0 {
		return ""
	}

	return e.posts[len(e.posts)-1]
}

func testServer() *Server {
	return &Server{
		logger: testLogger(),
		config: &config.Config{PublicURL: "https://dbbat.example.com"},
	}
}

func adminUser() *store.User {
	return &store.User{UID: uuid.New(), Username: "admin", Roles: []string{store.RoleAdmin, store.RoleConnector}}
}

func nonAdminUser() *store.User {
	return &store.User{UID: uuid.New(), Username: "connector", Roles: []string{store.RoleConnector}}
}

func sampleOutcome(action notify.GrantAction) *decideOutcome {
	ch := "C123"
	ts := "1.2"

	return &decideOutcome{
		Request: &store.GrantRequest{UID: uuid.New(), SlackChannel: &ch, SlackMessageTS: &ts},
		Action:  action,
		Event: notify.GrantRequestEvent{
			Request:        &store.GrantRequest{SlackChannel: &ch, SlackMessageTS: &ts},
			DeciderSlackID: "U0ADM1",
		},
	}
}

// --- Signature verification ---

func TestSlackInteraction_ValidSignatureAcks200(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = adminUser()
	dec.approveOutcome = sampleOutcome(notify.GrantActionApproved)

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	dec.waitDone(t)

	if !dec.approveCalled {
		t.Error("expected approve to be called")
	}
}

func TestSlackInteraction_BadSignature401(t *testing.T) {
	t.Parallel()

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0ADM1", "")
	ts := fmt.Sprintf("%d", time.Now().Unix())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(string(body)))
	c.Request.Header.Set("X-Slack-Request-Timestamp", ts)
	c.Request.Header.Set("X-Slack-Signature", "v0=deadbeef") // wrong

	dec := newStubDecider()
	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", c.Writer.Status())
	}

	if dec.approveCalled || dec.denyCalled {
		t.Error("store must be untouched on bad signature")
	}
}

func TestSlackInteraction_StaleTimestamp401(t *testing.T) {
	t.Parallel()

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0ADM1", "")
	staleTS := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(string(body)))
	c.Request.Header.Set("X-Slack-Request-Timestamp", staleTS)
	c.Request.Header.Set("X-Slack-Signature", signSlackBody(testSigningSecret, staleTS, body))

	testServer().serveSlackInteraction(c, testSigningSecret, newStubDecider())

	if c.Writer.Status() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for stale timestamp", c.Writer.Status())
	}
}

func TestSlackInteraction_OversizedBody401(t *testing.T) {
	t.Parallel()

	big := make([]byte, slackInteractionMaxBody+10)
	for i := range big {
		big[i] = 'a'
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(string(big)))
	c.Request.Header.Set("X-Slack-Request-Timestamp", ts)
	c.Request.Header.Set("X-Slack-Signature", signSlackBody(testSigningSecret, ts, big))

	testServer().serveSlackInteraction(c, testSigningSecret, newStubDecider())

	if c.Writer.Status() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for oversized body", c.Writer.Status())
	}
}

func TestSlackInteraction_EmptySigningSecret401(t *testing.T) {
	t.Parallel()

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, "", newStubDecider())

	if c.Writer.Status() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when signing secret empty", c.Writer.Status())
	}
}

// --- Decision processing ---

func TestSlackInteraction_AdminApprove(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = adminUser()
	dec.approveOutcome = sampleOutcome(notify.GrantActionApproved)

	requestUID := uuid.NewString()
	body := slackInteractionBody(t, notify.ActionApprove, requestUID, "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	dec.waitDone(t)

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if !dec.approveCalled {
		t.Error("approve not called")
	}

	if dec.denyCalled {
		t.Error("deny should not be called on approve")
	}

	if dec.lastDeciderUID != dec.user.UID {
		t.Errorf("decider UID = %v, want mapped admin %v", dec.lastDeciderUID, dec.user.UID)
	}

	if !dec.threadReplied {
		t.Error("thread reply not posted on success")
	}
}

func TestSlackInteraction_AdminDeny(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = adminUser()
	dec.denyOutcome = sampleOutcome(notify.GrantActionDenied)

	body := slackInteractionBody(t, notify.ActionDeny, uuid.NewString(), "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	dec.waitDone(t)

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if !dec.denyCalled {
		t.Error("deny not called")
	}

	if dec.approveCalled {
		t.Error("approve should not be called on deny")
	}

	if !dec.threadReplied {
		t.Error("thread reply not posted on success")
	}
}

func TestSlackInteraction_NonAdminEphemeralNoStore(t *testing.T) {
	t.Parallel()

	eph := newEphemeralRecorder(t)

	dec := newStubDecider()
	dec.user = nonAdminUser()

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0USR", eph.srv.URL)
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	eph.wait(t)

	if !strings.Contains(eph.lastPost(), "admins") {
		t.Errorf("ephemeral = %q, want non-admin message", eph.lastPost())
	}

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if dec.approveCalled || dec.denyCalled {
		t.Error("store must be untouched for non-admin")
	}
}

// TestSlackInteraction_RequesterWithdraws: the requester — refused by the
// approve/deny gate — clicks Withdraw and the request is withdrawn, never
// approved or denied.
func TestSlackInteraction_RequesterWithdraws(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = nonAdminUser()
	yes, no := true, false
	dec.mayCancel = &yes
	dec.mayDecide = &no
	dec.cancelOutcome = sampleOutcome(notify.GrantActionCancelled)

	body := slackInteractionBody(t, notify.ActionCancel, uuid.NewString(), "U0REQ", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	dec.waitDone(t)

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if !dec.cancelCalled {
		t.Error("cancel not called")
	}

	if dec.approveCalled || dec.denyCalled {
		t.Error("withdraw must not approve or deny")
	}

	if dec.lastDeciderUID != dec.user.UID {
		t.Errorf("canceller UID = %v, want requester %v", dec.lastDeciderUID, dec.user.UID)
	}

	if !dec.threadReplied {
		t.Error("thread reply not posted on success")
	}
}

// TestSlackInteraction_NonRequesterWithdrawRefused: an approver who may
// decide the request still may not withdraw it on the requester's behalf.
func TestSlackInteraction_NonRequesterWithdrawRefused(t *testing.T) {
	t.Parallel()

	eph := newEphemeralRecorder(t)

	dec := newStubDecider()
	dec.user = nonAdminUser()
	yes, no := true, false
	dec.mayDecide = &yes
	dec.mayCancel = &no

	body := slackInteractionBody(t, notify.ActionCancel, uuid.NewString(), "U0APP", eph.srv.URL)
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	eph.wait(t)

	if got := eph.lastPost(); got != msgNotRequester {
		t.Errorf("ephemeral = %q, want %q", got, msgNotRequester)
	}

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if dec.cancelCalled || dec.approveCalled || dec.denyCalled {
		t.Error("store must be untouched for a non-requester withdraw")
	}
}

func TestMayCancelGrantRequest(t *testing.T) {
	t.Parallel()

	requester := nonAdminUser()
	other := nonAdminUser()
	admin := adminUser()
	req := &store.GrantRequest{UID: uuid.New(), UserID: requester.UID}

	cases := []struct {
		name string
		user *store.User
		want bool
	}{
		{"requester", requester, true},
		{"admin", admin, false},
		{"other user", other, false},
		{"nil user", nil, false},
	}

	for _, tc := range cases {
		if got := mayCancelGrantRequest(tc.user, req); got != tc.want {
			t.Errorf("%s: mayCancelGrantRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSlackInteraction_UnlinkedUserEphemeralNoStore(t *testing.T) {
	t.Parallel()

	eph := newEphemeralRecorder(t)

	dec := newStubDecider()
	dec.userErr = store.ErrIdentityNotFound

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0GHOST", eph.srv.URL)
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	eph.wait(t)

	if !strings.Contains(strings.ToLower(eph.lastPost()), "isn't linked") {
		t.Errorf("ephemeral = %q, want unlinked message", eph.lastPost())
	}

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if dec.approveCalled || dec.denyCalled {
		t.Error("store must be untouched for unlinked user")
	}
}

func TestSlackInteraction_AlreadyDecidedEphemeralAndRerender(t *testing.T) {
	t.Parallel()

	eph := newEphemeralRecorder(t)

	dec := newStubDecider()
	dec.user = adminUser()
	dec.approveErr = store.ErrInvalidTransition

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0ADM1", eph.srv.URL)
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	eph.wait(t)

	if !strings.Contains(strings.ToLower(eph.lastPost()), "no longer pending") {
		t.Errorf("ephemeral = %q, want 'no longer pending'", eph.lastPost())
	}

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if !dec.rerenderCalled {
		t.Error("expected re-render on already-decided click")
	}

	if dec.threadReplied {
		t.Error("no thread reply on failed decision")
	}
}

// --- Ignored interactions (200, no side effects) ---

func TestSlackInteraction_UnknownActionIDNoOp(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = adminUser()

	body := slackInteractionBody(t, "some_other_button", uuid.NewString(), "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	// No goroutine spawned for unknown actions, so nothing to wait on;
	// give any erroneous goroutine a moment, then assert no calls.
	time.Sleep(50 * time.Millisecond)

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if dec.approveCalled || dec.denyCalled {
		t.Error("unknown action_id must not trigger a decision")
	}
}

func TestSlackInteraction_NonUUIDValueNoOp(t *testing.T) {
	t.Parallel()

	dec := newStubDecider()
	dec.user = adminUser()

	body := slackInteractionBody(t, notify.ActionApprove, "not-a-uuid", "U0ADM1", "")
	c := newSignedSlackRequest(t, body)

	testServer().serveSlackInteraction(c, testSigningSecret, dec)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.Writer.Status())
	}

	time.Sleep(50 * time.Millisecond)

	dec.mu.Lock()
	defer dec.mu.Unlock()

	if dec.approveCalled || dec.denyCalled {
		t.Error("non-UUID value must not trigger a decision")
	}
}

// --- Pure-function coverage ---

func TestThreadReplyText(t *testing.T) {
	t.Parallel()

	if got := threadReplyText(notify.GrantActionApproved, "U1", nil); !strings.Contains(got, "<@U1>") || !strings.Contains(got, "approved") {
		t.Errorf("approved reply = %q", got)
	}

	if got := threadReplyText(notify.GrantActionDenied, "", &store.User{Username: "bob"}); !strings.Contains(got, "bob") || !strings.Contains(got, "denied") {
		t.Errorf("denied reply = %q", got)
	}

	if got := threadReplyText(notify.GrantActionCancelled, "", nil); !strings.Contains(got, "🚫") || !strings.Contains(got, "requester") {
		t.Errorf("cancel reply = %q", got)
	}

	if got := threadReplyText(notify.GrantActionCancelled, "U0REQ", nil); !strings.Contains(got, "<@U0REQ>") || !strings.Contains(got, "withdrawn") {
		t.Errorf("withdraw reply = %q", got)
	}
}

func TestThreadReplyText_EscapesUsername(t *testing.T) {
	t.Parallel()

	got := threadReplyText(notify.GrantActionDenied, "", &store.User{Username: "<!channel>"})
	if strings.Contains(got, "<!channel>") || !strings.Contains(got, "&lt;!channel&gt;") {
		t.Errorf("denied reply = %q", got)
	}
}

func TestActionForStatus(t *testing.T) {
	t.Parallel()

	cases := map[store.GrantRequestStatus]notify.GrantAction{
		store.GrantRequestApproved:  notify.GrantActionApproved,
		store.GrantRequestDenied:    notify.GrantActionDenied,
		store.GrantRequestCancelled: notify.GrantActionCancelled,
		store.GrantRequestPending:   notify.GrantActionCreated,
		store.GrantRequestExpired:   notify.GrantActionCreated,
	}

	for status, want := range cases {
		if got := actionForStatus(status); got != want {
			t.Errorf("actionForStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

// --- Startup transport note ---

// transportNoteServer builds a Server with the given Slack notify config whose
// log output is captured in the returned buffer.
func transportNoteServer(sn config.SlackNotifyConfig) (*Server, *bytes.Buffer) {
	var buf bytes.Buffer

	return &Server{
		logger: slog.New(slog.NewTextHandler(&buf, nil)),
		config: &config.Config{PublicURL: "https://dbbat.example.com", SlackNotify: sn},
	}, &buf
}

func TestLogSlackInteractivityTransport(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cfg      config.SlackNotifyConfig
		wantNote bool
	}{
		"signing secret only": {config.SlackNotifyConfig{BotToken: "xoxb-1", SigningSecret: "s3"}, true},
		"socket mode":         {config.SlackNotifyConfig{BotToken: "xoxb-1", AppToken: "xapp-1"}, false},
		"both transports":     {config.SlackNotifyConfig{BotToken: "xoxb-1", SigningSecret: "s3", AppToken: "xapp-1"}, false},
		"notify only":         {config.SlackNotifyConfig{BotToken: "xoxb-1"}, false},
		"disabled":            {config.SlackNotifyConfig{}, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, buf := transportNoteServer(tc.cfg)
			srv.logSlackInteractivityTransport()

			if got := strings.Contains(buf.String(), "DBB_SLACK_NOTIFY_APP_TOKEN"); got != tc.wantNote {
				t.Errorf("reachability note logged = %v, want %v (log: %q)", got, tc.wantNote, buf.String())
			}

			if tc.wantNote && !strings.Contains(buf.String(), "https://dbbat.example.com/api/v1/slack/interactions") {
				t.Errorf("note should carry the endpoint URL (log: %q)", buf.String())
			}
		})
	}
}

func TestLogSlackInteractivityTransport_NilConfig(t *testing.T) {
	t.Parallel()

	srv := &Server{logger: testLogger()}
	srv.logSlackInteractivityTransport() // must not panic on a nil config
}
