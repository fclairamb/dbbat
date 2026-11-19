package oracle

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
)

// TestStartCreatesPrivateDumpDir checks the dump directory is created 0700:
// captures hold plaintext statements and rows.
func TestStartCreatesPrivateDumpDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "dumps")

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.DumpConfig{Dir: dir}, slog.New(slog.DiscardHandler))

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Start("127.0.0.1:0") }()

	require.Eventually(t, func() bool {
		info, err := os.Stat(dir)

		return err == nil && info.IsDir()
	}, 5*time.Second, 10*time.Millisecond)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, dump.DirPerm, info.Mode().Perm())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}
