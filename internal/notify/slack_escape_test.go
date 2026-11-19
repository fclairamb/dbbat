package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

const hostileText = "<!channel> <https://evilco.example|x> & <@U999>"

// assertNoLiveMrkdwn fails when a hostile control sequence survived.
func assertNoLiveMrkdwn(t *testing.T, text string) {
	t.Helper()

	for _, raw := range []string{"<!channel>", "<https://evilco.example|x>", "<@U999>"} {
		require.NotContains(t, text, raw)
	}

	require.Contains(t, text, "&lt;!channel&gt; &lt;https://evilco.example|x&gt; &amp; &lt;@U999&gt;")
}

func TestEscapeSlackText(t *testing.T) {
	t.Parallel()

	require.Equal(t, "a &amp; b &lt;c&gt;", EscapeSlackText("a & b <c>"))
	require.Equal(t, "need prod access for incident 1234", EscapeSlackText("need prod access for incident 1234"))
	require.Empty(t, EscapeSlackText(""))
}

func renewalEvent(justification string) GrantRequestEvent {
	return GrantRequestEvent{
		Action:     GrantActionApproved,
		Request:    &store.GrantRequest{UID: uuid.New(), Justification: justification},
		Definition: &store.GrantDefinition{Name: "ro", DurationSeconds: 3600},
		Server:     &store.Server{Name: "prod"},
		Requester:  &store.User{Username: "alice"},
	}
}

func TestRenewalText_EscapesJustification(t *testing.T) {
	t.Parallel()

	text := renewalText(renewalEvent(hostileText), "https://dbbat.example")
	assertNoLiveMrkdwn(t, text)
	// The dbbat-generated link stays live.
	require.Contains(t, text, "<https://dbbat.example/app/grant-requests|request>")
}

func TestRenewalText_OrdinaryJustificationUnchanged(t *testing.T) {
	t.Parallel()

	text := renewalText(renewalEvent("need prod access for incident 1234"), "https://dbbat.example")
	require.Contains(t, text, `— "need prod access for incident 1234"`)

	text = renewalText(renewalEvent("R&D access"), "https://dbbat.example")
	require.Contains(t, text, `— "R&amp;D access"`)
}

func TestRenewalText_EscapesUnlinkedUsername(t *testing.T) {
	t.Parallel()

	ev := renewalEvent("x")
	ev.Requester = &store.User{Username: "<!here>"}

	text := renewalText(ev, "https://dbbat.example")
	require.NotContains(t, text, "<!here>")
	require.Contains(t, text, "&lt;!here&gt;")
}

func TestMainSectionText_EscapesUserSuppliedFields(t *testing.T) {
	t.Parallel()

	reason := hostileText
	ev := renewalEvent(hostileText)
	ev.Action = GrantActionDenied
	ev.Decider = &store.User{Username: "<!here>"}
	ev.Request.DecisionReason = &reason

	text := mainSectionText(ev)
	require.Equal(t, 2, strings.Count(text, "&lt;!channel&gt;"))
	assertNoLiveMrkdwn(t, text)
	require.NotContains(t, text, "<!here>")
}

func TestMentionLine_EscapesUsernameFallback(t *testing.T) {
	t.Parallel()

	ev := renewalEvent("")
	ev.Action = GrantActionCreated
	ev.Requester = &store.User{Username: hostileText}

	assertNoLiveMrkdwn(t, mentionLine(ev))
}

func TestBuildHoldBlocks_EscapesSQLAndReason(t *testing.T) {
	t.Parallel()

	e := &ApprovalEscalator{includeSQL: true, publicURL: "https://dbbat.example"}
	hold := ApprovalHold{
		QueryUID:     uuid.New(),
		Username:     "alice",
		DatabaseName: "prod",
		SQL:          "SELECT '" + hostileText + "'",
		Pattern:      "DROP",
		StartedAt:    time.Now(),
	}

	blocks := e.buildHoldBlocks(hold, "denied", "<!here>", hostileText)

	body, ok := blocks[1].(*slack.SectionBlock)
	require.True(t, ok)
	assertNoLiveMrkdwn(t, body.Text.Text)
	require.NotContains(t, body.Text.Text, "<!here>")

	sql, ok := blocks[2].(*slack.SectionBlock)
	require.True(t, ok)
	assertNoLiveMrkdwn(t, sql.Text.Text)
}

func TestTermination_EscapesUserSuppliedFields(t *testing.T) {
	t.Parallel()

	ev := store.TerminationEvent{
		User:         &store.User{Username: "alice"},
		Database:     &store.Server{Name: "prod"},
		Grant:        &store.AccessGrant{Definition: &store.GrantDefinition{Name: "ro"}},
		Reason:       store.TerminationAdminTerminated,
		TerminatedBy: "<!here>",
		Detail:       hostileText,
		QueryHead:    hostileText,
	}

	text := terminationMainText(ev)
	assertNoLiveMrkdwn(t, text)
	require.NotContains(t, text, "<!here>")

	blocks := buildTerminationBlocks(ev, "", true)
	sql, ok := blocks[2].(*slack.SectionBlock)
	require.True(t, ok)
	assertNoLiveMrkdwn(t, sql.Text.Text)
}
