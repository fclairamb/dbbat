package store

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPgdriverLoggerRoutesToSlogDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	l := slogPgdriver{logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	l.Printf(context.Background(), "bun: discarding bad listener connection: %s", "boom")

	out := buf.String()
	assert.Contains(t, out, `"level":"DEBUG"`)
	assert.Contains(t, out, "discarding bad listener connection: boom")
}
