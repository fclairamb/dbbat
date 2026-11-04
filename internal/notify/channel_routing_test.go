package notify

import (
	"context"
	"testing"

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
