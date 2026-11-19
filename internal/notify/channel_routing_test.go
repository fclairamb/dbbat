package notify

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

func strPtr(s string) *string { return &s }

func newRoutingNotifier(fake *fakeSlack, per SlackPersister) *SlackNotifier {
	n := newTestTerminationNotifier(fake, false, 0)
	n.channel = "#dbbat"
	n.channels = map[string]string{"prod": "#dbbat-prod"}
	n.store = per

	return n
}

func TestChannelFor(t *testing.T) {
	t.Parallel()

	n := newRoutingNotifier(newFakeSlack(t), nil)

	ch, post := n.channelFor(context.Background(), nil)
	assert.Equal(t, "#dbbat", ch)
	assert.True(t, post)

	ch, post = n.channelFor(context.Background(), &store.GrantDefinition{})
	assert.Equal(t, "#dbbat", ch)
	assert.True(t, post)

	ch, post = n.channelFor(context.Background(), &store.GrantDefinition{NotifyChannel: strPtr("prod")})
	assert.Equal(t, "#dbbat-prod", ch)
	assert.True(t, post)

	_, post = n.channelFor(context.Background(), &store.GrantDefinition{NotifyChannel: strPtr("none")})
	assert.False(t, post)

	// A key removed from the config falls back to the default channel.
	ch, post = n.channelFor(context.Background(), &store.GrantDefinition{NotifyChannel: strPtr("gone")})
	assert.Equal(t, "#dbbat", ch)
	assert.True(t, post)

	// A hold never goes unannounced: "none" falls back to the default.
	assert.Equal(t, "#dbbat", n.holdChannelFor(context.Background(), &store.GrantDefinition{NotifyChannel: strPtr("none")}))
	assert.Equal(t, "#dbbat-prod", n.holdChannelFor(context.Background(), &store.GrantDefinition{NotifyChannel: strPtr("prod")}))
}

func TestNotifyGrantRequest_ChannelRouting(t *testing.T) {
	t.Parallel()

	t.Run("default channel", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		newRoutingNotifier(fake, &fakePersister{}).NotifyGrantRequest(context.Background(), sampleEvent(GrantActionCreated))

		require.Equal(t, 1, fake.postCount())
		assert.Equal(t, "#dbbat", fake.postCalls[0].Get("channel"))
	})

	t.Run("named key", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		ev := sampleEvent(GrantActionCreated)
		ev.Definition.NotifyChannel = strPtr("prod")
		newRoutingNotifier(fake, &fakePersister{}).NotifyGrantRequest(context.Background(), ev)

		require.Equal(t, 1, fake.postCount())
		assert.Equal(t, "#dbbat-prod", fake.postCalls[0].Get("channel"))
	})

	t.Run("unknown key falls back", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		ev := sampleEvent(GrantActionCreated)
		ev.Definition.NotifyChannel = strPtr("gone")
		newRoutingNotifier(fake, &fakePersister{}).NotifyGrantRequest(context.Background(), ev)

		require.Equal(t, 1, fake.postCount())
		assert.Equal(t, "#dbbat", fake.postCalls[0].Get("channel"))
	})

	t.Run("none suppresses creation and updates", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{}
		n := newRoutingNotifier(fake, per)

		for _, action := range []GrantAction{GrantActionCreated, GrantActionApproved, GrantActionDenied} {
			ev := sampleEvent(action)
			ev.Definition.NotifyChannel = strPtr("none")
			n.NotifyGrantRequest(context.Background(), ev)

			// Also with no stored message: no fallback post either.
			ev.Request.SlackChannel, ev.Request.SlackMessageTS = nil, nil
			n.NotifyGrantRequest(context.Background(), ev)
		}

		assert.Equal(t, 0, fake.postCount())
		assert.Equal(t, 0, fake.updateCount())
		assert.False(t, per.called)
	})
}

func TestNotifyTermination_ChannelRouting(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	n := newRoutingNotifier(fake, nil)

	ev := sampleTerminationEvent()
	ev.Grant.Definition.NotifyChannel = strPtr("prod")
	n.NotifyTermination(context.Background(), ev)

	require.Equal(t, 1, fake.postCount())
	assert.Equal(t, "#dbbat-prod", fake.postCalls[0].Get("channel"))

	none := sampleTerminationEvent()
	none.Grant.Definition.NotifyChannel = strPtr("none")
	n.NotifyTermination(context.Background(), none)
	assert.Equal(t, 1, fake.postCount(), "none posts nothing")
}

func TestNotifyGrantBytes_ChannelRouting(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	n := newRoutingNotifier(fake, nil)

	ev := sampleGrantBytesEvent()
	ev.Grant.Definition = &store.GrantDefinition{NotifyChannel: strPtr("prod")}
	n.NotifyGrantBytes(context.Background(), ev)

	require.Equal(t, 1, fake.postCount())
	assert.Equal(t, "#dbbat-prod", fake.postCalls[0].Get("channel"))

	ev.Grant.Definition = &store.GrantDefinition{NotifyChannel: strPtr("none")}
	n.NotifyGrantBytes(context.Background(), ev)
	assert.Equal(t, 1, fake.postCount(), "none posts nothing")
}

func TestApprovalEscalator_HoldChannel(t *testing.T) {
	t.Parallel()

	n := newRoutingNotifier(newFakeSlack(t), nil)
	e := newApprovalEscalator(nil, "#dbbat", "https://example.com", false, 0, false, nopLogger())

	assert.Equal(t, "#dbbat", e.holdChannel(context.Background(), ApprovalHold{}), "no resolver: default channel")

	e.channelFor = n.holdChannelFor

	assert.Equal(t, "#dbbat-prod",
		e.holdChannel(context.Background(), ApprovalHold{Definition: &store.GrantDefinition{NotifyChannel: strPtr("prod")}}))
	assert.Equal(t, "#dbbat",
		e.holdChannel(context.Background(), ApprovalHold{Definition: &store.GrantDefinition{NotifyChannel: strPtr("none")}}))
}

func autoApprovedEvent() GrantRequestEvent {
	ev := sampleEvent(GrantActionApproved)
	ev.Request.SlackChannel = nil
	ev.Request.SlackMessageTS = nil
	ev.Request.UserID = uuid.New()
	ev.Request.DatabaseID = uuid.New()
	ev.Definition.LineageUID = uuid.New()
	ev.Decider = nil

	return ev
}

func TestNotifyGrantRequest_FallbackPostPersistsCoordinates(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	per := &fakePersister{}
	ev := autoApprovedEvent()

	newRoutingNotifier(fake, per).NotifyGrantRequest(context.Background(), ev)

	require.Equal(t, 1, fake.postCount())
	assert.True(t, per.called)
	assert.Equal(t, ev.Request.UID, per.uid)
	assert.Equal(t, "C123", per.channel)
	assert.Equal(t, "1.2", per.ts)
}

func TestNotifyGrantRequest_RenewalThreading(t *testing.T) {
	t.Parallel()

	rootUID := uuid.New()
	root := &store.GrantRequest{UID: rootUID, SlackChannel: strPtr("C123"), SlackMessageTS: strPtr("9.9")}

	t.Run("first request is top-level", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{}
		n := newRoutingNotifier(fake, per)
		n.renewalWindow = time.Hour
		n.NotifyGrantRequest(context.Background(), autoApprovedEvent())

		require.Equal(t, 1, fake.postCount())
		assert.Empty(t, fake.postCalls[0].Get("thread_ts"))
		assert.Equal(t, uuid.Nil, per.threadRoot)
	})

	t.Run("renewal is a threaded reply with root coordinates", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{root: root}
		n := newRoutingNotifier(fake, per)
		n.renewalWindow = time.Hour
		n.channelIDs.Store("#dbbat", "C123") // learned from an earlier post
		ev := autoApprovedEvent()
		n.NotifyGrantRequest(context.Background(), ev)

		require.Equal(t, 1, fake.postCount())
		assert.Equal(t, "9.9", fake.postCalls[0].Get("thread_ts"))
		assert.Contains(t, fake.postCalls[0].Get("text"), "renewed by alice")
		assert.Equal(t, ev.Request.UID, per.threadUID)
		assert.Equal(t, rootUID, per.threadRoot)
		assert.Equal(t, "C123", per.channel)
		assert.Equal(t, "9.9", per.ts)
		assert.False(t, per.called, "a reply must not be stored as the request's own message")
	})

	t.Run("window 0 is always top-level", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{root: root}
		newRoutingNotifier(fake, per).NotifyGrantRequest(context.Background(), autoApprovedEvent())

		require.Equal(t, 1, fake.postCount())
		assert.Empty(t, fake.postCalls[0].Get("thread_ts"))
		assert.True(t, per.called)
	})

	t.Run("manual approval is never threaded", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{root: root}
		n := newRoutingNotifier(fake, per)
		n.renewalWindow = time.Hour
		ev := autoApprovedEvent()
		ev.Decider = &store.User{UID: uuid.New(), Username: "bob"}
		n.NotifyGrantRequest(context.Background(), ev)

		require.Equal(t, 1, fake.postCount())
		assert.Empty(t, fake.postCalls[0].Get("thread_ts"))
	})

	t.Run("lookup error falls back to top-level", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{rootErr: assert.AnError}
		n := newRoutingNotifier(fake, per)
		n.renewalWindow = time.Hour
		n.NotifyGrantRequest(context.Background(), autoApprovedEvent())

		require.Equal(t, 1, fake.postCount())
		assert.Empty(t, fake.postCalls[0].Get("thread_ts"))
	})

	t.Run("root in another channel is not used", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		per := &fakePersister{root: &store.GrantRequest{
			UID: rootUID, SlackChannel: strPtr("C999"), SlackMessageTS: strPtr("9.9"),
		}}
		n := newRoutingNotifier(fake, per)
		n.renewalWindow = time.Hour
		n.NotifyGrantRequest(context.Background(), autoApprovedEvent())

		require.Equal(t, 1, fake.postCount())
		assert.Empty(t, fake.postCalls[0].Get("thread_ts"))
	})

	t.Run("a threaded row is never chat.updated", func(t *testing.T) {
		t.Parallel()

		fake := newFakeSlack(t)
		n := newRoutingNotifier(fake, &fakePersister{})
		ev := sampleEvent(GrantActionDenied)
		ev.Request.SlackThreadRootUID = &rootUID
		n.NotifyGrantRequest(context.Background(), ev)

		assert.Equal(t, 0, fake.updateCount())
		assert.Equal(t, 0, fake.postCount())
	})
}
