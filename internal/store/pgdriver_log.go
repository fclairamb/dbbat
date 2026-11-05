package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/uptrace/bun/driver/pgdriver"
)

// slogPgdriver routes bun's pgdriver diagnostics into slog. The driver writes
// through the standard log package to stderr as plain text, which an
// aggregator cannot parse. Its one recurring line is the LISTEN connection
// being discarded when the storage database's idle_session_timeout closes it
// (the listener reconnects by itself), so everything goes out at DEBUG.
type slogPgdriver struct{ logger *slog.Logger }

func (l slogPgdriver) Printf(ctx context.Context, format string, v ...any) {
	logger := l.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.DebugContext(ctx, fmt.Sprintf(format, v...), slog.String("component", "pgdriver"))
}

var routePgdriverOnce sync.Once

// routePgdriverLogsToSlog replaces pgdriver's package-level logger, once:
// store.New runs concurrently in tests and the assignment is a global write.
// The logger resolves slog.Default() at log time.
func routePgdriverLogsToSlog() {
	routePgdriverOnce.Do(func() { pgdriver.Logger = slogPgdriver{} })
}
