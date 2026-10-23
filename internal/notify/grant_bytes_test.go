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

func sampleGrantBytesEvent() store.GrantBytesEvent {
	return store.GrantBytesEvent{
		Grant:       &store.AccessGrant{UID: uuid.MustParse("11111111-2222-3333-4444-555555555555")},
		User:        &store.User{Username: "florent"},
		UserSlackID: "U0USER1",
		Database:    &store.Server{Name: "prod-datalake-ro"},
		Step:        1 << 30,
		Steps:       3,
		Total:       3 << 30,
		Delta:       1 << 30,
		Since:       time.Now().Add(-12*time.Minute - 10*time.Second),
	}
}

func newGrantBytesNotifier(fake *fakeSlack) *SlackNotifier {
	return &SlackNotifier{
		client:    fake.client(),
		channel:   "#dbbat",
		publicURL: "https://example.com",
		log:       nopLogger(),
	}
}

func TestNotifyGrantBytes_RepliesInRequestThread(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	ev := sampleGrantBytesEvent()
	ev.RequestSlackChannel = "C777"
	ev.RequestSlackTS = "1700000000.000100"

	newGrantBytesNotifier(fake).NotifyGrantBytes(context.Background(), ev)

	require.Equal(t, 1, fake.postCount())

	form := fake.postCalls[0]
	assert.Equal(t, "C777", form.Get("channel"))
	assert.Equal(t, "1700000000.000100", form.Get("thread_ts"))

	text := form.Get("text")
	assert.Contains(t, text, "11111111-2222-3333-4444-555555555555")
	assert.Contains(t, text, "<@U0USER1>")
	assert.Contains(t, text, "prod-datalake-ro")
	assert.Contains(t, text, "3.0 GiB so far")
	assert.Contains(t, text, "last 1.0 GiB in 12 min")
	assert.Contains(t, text, "The traffic is not blocked.")
	assert.Contains(t, text, "https://example.com/app/grants")
}

func TestNotifyGrantBytes_FallsBackToChannel(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	ev := sampleGrantBytesEvent()
	ev.UserSlackID = ""

	newGrantBytesNotifier(fake).NotifyGrantBytes(context.Background(), ev)

	require.Equal(t, 1, fake.postCount())

	form := fake.postCalls[0]
	assert.Equal(t, "#dbbat", form.Get("channel"))
	assert.Empty(t, form.Get("thread_ts"))
	assert.Contains(t, form.Get("text"), "florent", "no Slack id: the username stands in for the mention")

	// nil notifier: no-op, no panic.
	(*SlackNotifier)(nil).NotifyGrantBytes(context.Background(), ev)
}

func TestFormatBytesHuman(t *testing.T) {
	t.Parallel()

	for n, want := range map[int64]string{
		900:                    "900 B",
		1536:                   "1.5 KiB",
		512 << 20:              "512.0 MiB",
		11*(1<<30) + (1<<30)/2: "11.5 GiB",
	} {
		assert.Equal(t, want, formatBytesHuman(n))
	}
}
