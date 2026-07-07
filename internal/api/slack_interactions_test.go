package api

import (
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
	fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)

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

// newSignedSlackRequest builds a gin context + recorder for a signed
// interaction POST with a valid current timestamp.
func newSignedSlackRequest(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	ts := fmt.Sprintf("%d", time.Now().Unix())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request.Header.Set("X-Slack-Request-Timestamp", ts)
	c.Request.Header.Set("X-Slack-Signature", signSlackBody(testSigningSecret, ts, body))

	return c, rec
}

// stubDecider records calls and returns canned results, standing in for
// *Server's store/notify-backed decision behavior.
type stubDecider struct {
	mu sync.Mutex

	user    *store.User
	userErr error

	approveOutcome *decideOutcome
	approveErr     error
	denyOutcome    *decideOutcome
	denyErr        error

	approveCalled  bool
	denyCalled     bool
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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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

func TestSlackInteraction_UnlinkedUserEphemeralNoStore(t *testing.T) {
	t.Parallel()

	eph := newEphemeralRecorder(t)

	dec := newStubDecider()
	dec.userErr = store.ErrIdentityNotFound

	body := slackInteractionBody(t, notify.ActionApprove, uuid.NewString(), "U0GHOST", eph.srv.URL)
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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
	c, _ := newSignedSlackRequest(t, body)

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

	if got := threadReplyText(notify.GrantActionCancelled, "", nil); !strings.Contains(got, "cancelled") {
		t.Errorf("cancelled reply = %q", got)
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
