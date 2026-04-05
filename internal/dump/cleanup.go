package dump

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CleanupOldFiles deletes .dbbat-dump files older than the retention period.
// Returns the number of files deleted.
func CleanupOldFiles(dir string, retention time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dump dir: %w", err)
	}

	cutoff := time.Now().Add(-retention)
	deleted := 0

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != FileExt {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				deleted++
			}
		}
	}

	return deleted, nil
}
