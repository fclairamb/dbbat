package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// sampleTerminationEvent is a fully populated statement-timeout event, the
// shape the statement-timeout watchdog produces.
func sampleTerminationEvent() store.TerminationEvent {
	return store.TerminationEvent{
		Connection: &store.Connection{UID: uuid.New()},
		User:       &store.User{UID: uuid.New(), Username: "florent"},
		Database:   &store.Server{Name: "prod-datalake-ro"},
		Grant: &store.AccessGrant{
			Definition: &store.GrantDefinition{Name: "Diagnostic PARIS_HABITAT"},
		},
		Reason:    store.TerminationStatementTimeout,
		QueryHead: "SELECT count(*) FROM data d JOIN properties p ON p.data_id = d.id WHERE d.deleted_at IS NULL",
		Limit:     30 * time.Second,
		Ran:       32100 * time.Millisecond,
	}
}

func newTestTerminationNotifier(f *fakeSlack, includeSQL bool, window time.Duration) *SlackNotifier {
	return &SlackNotifier{
		client:             f.client(),
		channel:            "C123",
		publicURL:          "https://example.com",
		log:                nopLogger(),
		terminationSQL:     includeSQL,
		terminationWindow:  window,
		terminationPending: make(map[terminationCoalesceKey]*terminationCoalesce),
	}
}

// renderBlocksToString round-trips a rendered block slice through JSON so
// tests can assert on its content by substring rather than reaching into
// slack-go's block types field by field. HTML-escaping is disabled: Slack's
// own `<url|text>` link syntax uses literal angle brackets, and the default
// encoder would otherwise turn them into </> and break every
// substring assertion involving a mention or a link.
func renderBlocksToString(t *testing.T, blocks []slack.Block) string {
	t.Helper()

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(blocks))

	return buf.String()
}

func TestBuildTerminationBlocks_StatementTimeoutWithSQL(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	blocks := buildTerminationBlocks(ev, "https://example.com", true)

	text := renderBlocksToString(t, blocks)

	require.Contains(t, text, "prod-datalake-ro")
	require.Contains(t, text, "florent")
	require.Contains(t, text, "Diagnostic PARIS_HABITAT")
	require.Contains(t, text, "30s")
	require.Contains(t, text, "32.1s")
	require.Contains(t, text, "SELECT count(*)")
	require.Contains(t, text, ev.Connection.UID.String())
}

func TestBuildTerminationBlocks_SQLOmittedWhenDisabled(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	blocks := buildTerminationBlocks(ev, "https://example.com", false)

	text := renderBlocksToString(t, blocks)

	require.NotContains(t, text, "SELECT count(*)")
}

func TestBuildTerminationBlocks_SQLOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	ev.QueryHead = ""
	blocks := buildTerminationBlocks(ev, "https://example.com", true)

	text := renderBlocksToString(t, blocks)

	require.NotContains(t, text, "```")
}

func TestBuildTerminationBlocks_AdminTerminatedWording(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	ev.Reason = store.TerminationAdminTerminated
	ev.TerminatedBy = "alice"
	ev.Detail = "runaway report"
	ev.Limit = 0
	ev.Ran = 0

	text := renderBlocksToString(t, buildTerminationBlocks(ev, "https://example.com", true))

	require.Contains(t, text, "Terminated by alice: runaway report")
	require.NotContains(t, text, "limit", "the admin wording must not borrow the timeout phrasing")
}

func TestBuildTerminationBlocks_AdminTerminatedNoDetail(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	ev.Reason = store.TerminationAdminTerminated
	ev.TerminatedBy = "alice"
	ev.Detail = ""
	ev.Limit = 0
	ev.Ran = 0

	text := renderBlocksToString(t, buildTerminationBlocks(ev, "https://example.com", true))

	require.Contains(t, text, "Terminated by alice")
	require.NotContains(t, text, "Terminated by alice:")
}

func TestBuildTerminationBlocks_QuotaExceededWording(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	ev.Reason = store.TerminationQuotaExceeded
	ev.Limit = 0
	ev.Ran = 0

	text := renderBlocksToString(t, buildTerminationBlocks(ev, "https://example.com", true))

	require.Contains(t, text, "quota")
}

func TestBuildTerminationBlocks_UserMentionUsesLinkedSlackID(t *testing.T) {
	t.Parallel()

	ev := sampleTerminationEvent()
	ev.UserSlackID = "U123456"

	text := renderBlocksToString(t, buildTerminationBlocks(ev, "https://example.com", true))

	require.Contains(t, text, "<@U123456>")
	require.NotContains(t, text, "florent")
}

func TestNotifyTermination_NilNotifierIsNoOp(t *testing.T) {
	t.Parallel()

	// Must not panic.
	(*SlackNotifier)(nil).NotifyTermination(context.Background(), sampleTerminationEvent())
}

// TestNotifyTermination_Coalesces is the spec's rate-limiting case: repeated
// terminations for the same (user, database, reason) inside the window fold
// into one follow-up, not one post each. 8 events in a window -> 2 posts (the
// first immediate, the rest coalesced into the follow-up once the window
// closes).
func TestNotifyTermination_Coalesces(t *testing.T) {
	t.Parallel()

	f := newFakeSlack(t)
	n := newTestTerminationNotifier(f, true, 30*time.Millisecond)

	ev := sampleTerminationEvent()

	for range 8 {
		n.NotifyTermination(context.Background(), ev)
	}

	require.Equal(t, 1, f.postCount(), "only the first of the burst posts immediately")

	require.Eventually(t, func() bool {
		return f.postCount() == 2
	}, time.Second, 5*time.Millisecond, "the coalesced follow-up posts once the window closes")

	// No third post: the window closing with nothing further queued must not
	// post an empty follow-up.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 2, f.postCount())
}

// TestNotifyTermination_DistinctKeysDoNotCoalesce checks the grouping key
// itself: a different reason (or user, or database) must never be folded
// into another key's window.
func TestNotifyTermination_DistinctKeysDoNotCoalesce(t *testing.T) {
	t.Parallel()

	f := newFakeSlack(t)
	n := newTestTerminationNotifier(f, true, time.Minute)

	timeout := sampleTerminationEvent()

	quota := sampleTerminationEvent()
	quota.Reason = store.TerminationQuotaExceeded

	n.NotifyTermination(context.Background(), timeout)
	n.NotifyTermination(context.Background(), quota)

	require.Equal(t, 2, f.postCount(), "distinct reasons must each post immediately")
}
