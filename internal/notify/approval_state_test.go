package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// fakeSlackPoster records the post/update calls the escalator makes and can hold a
// post open so the "resolved while the post is in flight" race is reproducible
// rather than incidental.
type fakeSlackPoster struct {
	mu         sync.Mutex
	posts      int
	updates    int
	lastTS     string
	lastUpdate []slack.Block

	// releasePost, when non-nil, blocks PostMessageContext until closed.
	releasePost chan struct{}
	// posting is closed the first time PostMessageContext is entered.
	posting     chan struct{}
	postingOnce sync.Once

	postErr error
}

// errSlackDown stands in for a Slack outage in the failed-post test.
var errSlackDown = errors.New("slack is down")

func newFakeSlackPoster() *fakeSlackPoster {
	return &fakeSlackPoster{posting: make(chan struct{})}
}

func (f *fakeSlackPoster) postBlocks(
	_ context.Context, channel string, _ []slack.Block,
) (string, string, error) {
	f.postingOnce.Do(func() { close(f.posting) })

	if f.releasePost != nil {
		<-f.releasePost
	}

	if f.postErr != nil {
		return "", "", f.postErr
	}

	f.mu.Lock()
	f.posts++
	ts := "1700000000.0000" + string(rune('0'+f.posts))
	f.lastTS = ts
	f.mu.Unlock()

	return channel, ts, nil
}

func (f *fakeSlackPoster) updateBlocks(_ context.Context, _, _ string, blocks []slack.Block) error {
	f.mu.Lock()
	f.updates++
	f.lastUpdate = blocks
	f.mu.Unlock()

	return nil
}

// blockText flattens every text object in a Block Kit message so a test can
// assert on what a human would read.
func blockText(blocks []slack.Block) string {
	var b strings.Builder

	for _, block := range blocks {
		switch v := block.(type) {
		case *slack.HeaderBlock:
			if v.Text != nil {
				b.WriteString(v.Text.Text)
				b.WriteString("\n")
			}
		case *slack.SectionBlock:
			if v.Text != nil {
				b.WriteString(v.Text.Text)
				b.WriteString("\n")
			}
		case *slack.ContextBlock:
			if v.ContextElements.Elements != nil {
				for _, el := range v.ContextElements.Elements {
					if txt, ok := el.(*slack.TextBlockObject); ok {
						b.WriteString(txt.Text)
						b.WriteString("\n")
					}
				}
			}
		}
	}

	return b.String()
}

func (f *fakeSlackPoster) update() []slack.Block {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastUpdate
}

func (f *fakeSlackPoster) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.posts, f.updates
}

func (f *fakeSlackPoster) waitFor(t *testing.T, want func(posts, updates int) bool, what string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if posts, updates := f.counts(); want(posts, updates) {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	posts, updates := f.counts()
	t.Fatalf("%s (posts=%d updates=%d)", what, posts, updates)
}

func testHold() ApprovalHold {
	return ApprovalHold{
		QueryUID:      uuid.New(),
		ConnectionUID: uuid.New(),
		Username:      "alice",
		DatabaseName:  "prod",
		SQL:           "DELETE FROM users",
		Pattern:       `(?i)^DELETE`,
		StartedAt:     time.Now(),
	}
}

func testEscalator(poster slackPoster, delay time.Duration) *ApprovalEscalator {
	return newApprovalEscalator(poster, "#dbbat", "https://dbbat.example", true, delay, true, nil)
}

func TestEscalationPostsAfterTheDelay(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	esc := testEscalator(fake, 20*time.Millisecond)

	esc.Schedule(context.Background(), testHold())

	fake.waitFor(t, func(posts, _ int) bool { return posts == 1 },
		"the pending hold never escalated to Slack")
}

func TestResolvingBeforeTheTimerFiresPostsNothing(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	esc := testEscalator(fake, time.Hour)

	hold := testHold()
	esc.Schedule(context.Background(), hold)
	esc.Resolved(context.Background(), hold.QueryUID, "approved", "bob", "")

	time.Sleep(50 * time.Millisecond)

	if posts, updates := fake.counts(); posts != 0 || updates != 0 {
		t.Fatalf("a hold resolved before the delay still talked to Slack: posts=%d updates=%d", posts, updates)
	}

	// And the timer is gone, so nothing fires later either.
	esc.mu.Lock()
	remaining := len(esc.pending)
	esc.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d escalation(s) left armed after resolution", remaining)
	}
}

func TestResolvingAfterThePostUpdatesInPlace(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	esc := testEscalator(fake, 10*time.Millisecond)

	hold := testHold()
	esc.Schedule(context.Background(), hold)

	fake.waitFor(t, func(posts, _ int) bool { return posts == 1 }, "escalation never posted")

	esc.Resolved(context.Background(), hold.QueryUID, "denied", "bob", "prod freeze")

	// Because there is no approval timeout, a posted message would otherwise
	// stay actionable indefinitely. It must be rewritten, not left alone.
	fake.waitFor(t, func(posts, updates int) bool { return posts == 1 && updates == 1 },
		"the resolved hold left a stale, still-actionable Slack message")

	rendered := blockText(fake.update())
	if !strings.Contains(rendered, "denied") || !strings.Contains(rendered, "prod freeze") {
		t.Fatalf("the edit lost the real outcome: %s", rendered)
	}

	if hasActionBlock(fake.update()) {
		t.Fatalf("the edit left a live Approve button: %s", rendered)
	}

	esc.mu.Lock()
	remaining := len(esc.pending)
	esc.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d escalation(s) still tracked after resolution", remaining)
	}
}

func TestResolvingWhileThePostIsInFlightRendersTheRealOutcome(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	fake.releasePost = make(chan struct{})

	esc := testEscalator(fake, 10*time.Millisecond)

	hold := testHold()
	esc.Schedule(context.Background(), hold)

	// Wait until the post is genuinely in flight, then resolve — the narrow
	// window where the message does not exist yet but is about to. Getting
	// this wrong leaves a live Approve button on a hold nobody is waiting for.
	select {
	case <-fake.posting:
	case <-time.After(3 * time.Second):
		t.Fatal("the escalation timer never fired")
	}

	// Deliberately an *approval*: the message must say so. Publishing a
	// placeholder outcome here would assert that an approved statement was
	// abandoned — wrong in the direction that matters.
	esc.Resolved(context.Background(), hold.QueryUID, "approved", "bob", "on-call fix")

	close(fake.releasePost)

	fake.waitFor(t, func(posts, updates int) bool { return posts == 1 && updates == 1 },
		"a hold resolved mid-post left its Slack message un-updated")

	rendered := blockText(fake.update())

	if !strings.Contains(rendered, "approved") {
		t.Fatalf("the edit does not say the hold was approved: %s", rendered)
	}

	if strings.Contains(rendered, "abandoned") || strings.Contains(rendered, "gave up") {
		t.Fatalf("the edit claims an outcome that never happened: %s", rendered)
	}

	if !strings.Contains(rendered, "bob") {
		t.Fatalf("the edit does not name the approver: %s", rendered)
	}

	if !strings.Contains(rendered, "on-call fix") {
		t.Fatalf("the edit dropped the resolution reason: %s", rendered)
	}

	// And the buttons are gone, exactly as on the non-racing path.
	if hasActionBlock(fake.update()) {
		t.Fatalf("the edit left a live Approve button: %s", rendered)
	}

	esc.mu.Lock()
	remaining := len(esc.pending)
	esc.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d escalation(s) still tracked after a mid-post resolution", remaining)
	}
}

func TestResolvingMidPostSurvivesAFailedPost(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	fake.releasePost = make(chan struct{})
	fake.postErr = errSlackDown

	esc := testEscalator(fake, 10*time.Millisecond)

	hold := testHold()
	esc.Schedule(context.Background(), hold)

	select {
	case <-fake.posting:
	case <-time.After(3 * time.Second):
		t.Fatal("the escalation timer never fired")
	}

	esc.Resolved(context.Background(), hold.QueryUID, "approved", "bob", "")
	close(fake.releasePost)

	// Nothing was ever posted, so nothing can be edited — and the hold must
	// not stay tracked forever waiting for a message that will never exist.
	time.Sleep(80 * time.Millisecond)

	if _, updates := fake.counts(); updates != 0 {
		t.Fatalf("updated a message that was never posted (%d updates)", updates)
	}

	esc.mu.Lock()
	remaining := len(esc.pending)
	esc.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d escalation(s) leaked after a failed post was resolved mid-flight", remaining)
	}
}

func TestFailedPostLeavesNothingTracked(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	fake.postErr = errSlackDown

	esc := testEscalator(fake, 10*time.Millisecond)

	hold := testHold()
	esc.Schedule(context.Background(), hold)

	select {
	case <-fake.posting:
	case <-time.After(3 * time.Second):
		t.Fatal("the escalation timer never fired")
	}

	// A failed post must not leave the escalator believing a message exists —
	// Resolved would then try to edit a message that was never created.
	esc.Resolved(context.Background(), hold.QueryUID, "approved", "bob", "")

	time.Sleep(50 * time.Millisecond)

	if _, updates := fake.counts(); updates != 0 {
		t.Fatalf("updated a message that was never posted (%d updates)", updates)
	}
}

func TestSchedulingTheSameHoldTwiceArmsOneTimer(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	esc := testEscalator(fake, 20*time.Millisecond)

	hold := testHold()
	esc.Schedule(context.Background(), hold)
	esc.Schedule(context.Background(), hold)

	fake.waitFor(t, func(posts, _ int) bool { return posts >= 1 }, "escalation never posted")

	time.Sleep(80 * time.Millisecond)

	if posts, _ := fake.counts(); posts != 1 {
		t.Fatalf("posted %d times for one hold — exactly one message per hold", posts)
	}
}

func TestPendingRenderCarriesActionsAndResolvedRenderDoesNot(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackPoster()
	esc := testEscalator(fake, time.Hour)

	hold := testHold()

	pending := esc.buildHoldBlocks(hold, "", "", "")
	if !hasActionBlock(pending) {
		t.Fatal("the pending message carries no Approve/Deny buttons")
	}

	for _, status := range []string{"approved", "denied", "abandoned"} {
		resolved := esc.buildHoldBlocks(hold, status, "bob", "because")
		if hasActionBlock(resolved) {
			t.Fatalf("the %s render still carries buttons — a stale Approve that silently no-ops", status)
		}
	}
}

func TestNonInteractiveEscalatorNeverRendersButtons(t *testing.T) {
	t.Parallel()

	// No inbound transport configured: link-through-UI only, so a button
	// would have nothing to call back into.
	esc := newApprovalEscalator(newFakeSlackPoster(), "#dbbat", "https://dbbat.example", false, time.Hour, true, nil)

	if hasActionBlock(esc.buildHoldBlocks(testHold(), "", "", "")) {
		t.Fatal("buttons rendered without an inbound interaction transport")
	}
}

func TestSQLIsOmittedWhenDisabled(t *testing.T) {
	t.Parallel()

	with := newApprovalEscalator(newFakeSlackPoster(), "#c", "https://x", true, time.Hour, true, nil)
	without := newApprovalEscalator(newFakeSlackPoster(), "#c", "https://x", true, time.Hour, false, nil)

	hold := testHold()

	if len(with.buildHoldBlocks(hold, "", "", "")) <= len(without.buildHoldBlocks(hold, "", "", "")) {
		t.Fatal("DBB_APPROVAL_SLACK_SQL=false still ships the SQL block to Slack")
	}
}

func hasActionBlock(blocks []slack.Block) bool {
	for _, b := range blocks {
		if _, ok := b.(*slack.ActionBlock); ok {
			return true
		}
	}

	return false
}
