package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/uptrace/bun/driver/pgdriver"
)

// slogPgdriver routes bun's pgdriver diagnostics into slog. The driver writes
// through the standard log package to stderr as plain text, which an
// aggregator cannot parse. Its one recurring line is the LISTEN connection
// being discarded when the storage database's idle_session_timeout closes it
// (the listener reconnects by itself), so everything goes out at DEBUG.
type slogPgdriver struct{ logger *slog.Logger }

func (l slogPgdriver) Printf(ctx context.Context, format string, v ...any) {
	l.logger.DebugContext(ctx, fmt.Sprintf(format, v...), slog.String("component", "pgdriver"))
}

// routePgdriverLogsToSlog replaces pgdriver's package-level logger.
func routePgdriverLogsToSlog() {
	pgdriver.Logger = slogPgdriver{logger: slog.Default()}
}
