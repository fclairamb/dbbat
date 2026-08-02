package dump

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// legacyFileExt is the pre-pcapng capture extension. The reader no longer
// understands this format; this constant exists only so the retention sweep
// can reap leftover files from before the switch to pcapng. Remove it again
// after a release or two, once operational dump directories have cycled.
const legacyFileExt = ".dbbat-dump"

// CleanupOldFiles deletes .pcapng capture files older than the retention period.
// It also reaps leftover legacyFileExt files from before the pcapng switch,
// since they are otherwise unreadable and invisible to this sweep.
// Returns the number of files deleted.
func CleanupOldFiles(dir string, retention time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dump dir: %w", err)
	}

	cutoff := time.Now().Add(-retention)
	deleted := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != FileExt && ext != legacyFileExt {
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
