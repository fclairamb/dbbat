package notify

import (
	"strings"
	"testing"
	"time"
)

func TestNewApprovalEscalatorDisabled(t *testing.T) {
	t.Parallel()

	// No Slack client configured — escalation is simply off, and every
	// method on the nil escalator must be a safe no-op.
	if e := NewApprovalEscalator(nil, 30*time.Second, true, nil); e != nil {
		t.Fatal("escalator built without a notifier")
	}

	// A zero delay is the documented "disable escalation" switch.
	if e := NewApprovalEscalator(&SlackNotifier{}, 0, true, nil); e != nil {
		t.Fatal("a zero delay must disable escalation")
	}

	var nilEscalator *ApprovalEscalator
	nilEscalator.Schedule(t.Context(), ApprovalHold{})
	nilEscalator.Resolved(t.Context(), [16]byte{}, "approved", "bob", "")
}

func TestTruncateSQL(t *testing.T) {
	t.Parallel()

	short := "SELECT 1"
	if got := truncateSQL(short, MaxSlackSQLLength); got != short {
		t.Fatalf("short SQL altered: %q", got)
	}

	long := strings.Repeat("x", MaxSlackSQLLength+50)

	got := truncateSQL(long, MaxSlackSQLLength)
	if len(got) <= MaxSlackSQLLength || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("long SQL not truncated: len=%d", len(got))
	}
}

func TestApprovalStatusLabelsAreDistinct(t *testing.T) {
	t.Parallel()

	denied := approvalStatusLabel("denied")
	abandoned := approvalStatusLabel("abandoned")

	if denied == abandoned {
		t.Fatal("denied and abandoned must not read the same")
	}

	// "abandoned" must be unmistakable: nothing ran, and nobody rejected it.
	if !strings.Contains(abandoned, "gave up") || !strings.Contains(abandoned, "nothing ran") {
		t.Fatalf("abandoned reads like a rejection: %q", abandoned)
	}

	if approvalEmoji("denied") == approvalEmoji("abandoned") {
		t.Fatal("denied and abandoned share an emoji")
	}

	if approvalStatusLabel("") == "" {
		t.Fatal("the pending label is empty")
	}
}

func TestFormatHeldForCountsUp(t *testing.T) {
	t.Parallel()

	if got := formatHeldFor(time.Time{}); got != "—" {
		t.Fatalf("zero time = %q", got)
	}

	got := formatHeldFor(time.Now().Add(-90 * time.Second))
	if !strings.Contains(got, "1m30s") {
		t.Fatalf("held-for = %q, want an elapsed duration", got)
	}
}
