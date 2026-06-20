// Package main runs `go build` on file changes and atomically swaps the
// running binary. Replaces `air` for the backend dev loop because air kills
// the running process before building, forcing a multi-second downtime
// window on every save. devloop builds first, then signals the old child to
// exit before starting the new one — the dead window is bounded by graceful
// shutdown, not by build time.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	debounceWindow = 300 * time.Millisecond
	graceTimeout   = 2 * time.Second
	binPath        = "./tmp/dbbat"
	nextBinPath    = "./tmp/dbbat.next"
	binArg         = "serve"
	rootDir        = "."
)

func excludedDirNames() map[string]struct{} {
	return map[string]struct{}{
		"tmp":      {},
		"vendor":   {},
		".git":     {},
		"testdata": {},
		"front":    {},
		"website":  {},
	}
}

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("[devloop] ")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := run(ctx)
	cancel()
	if err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if err := os.MkdirAll("tmp", 0o755); err != nil {
		return fmt.Errorf("mkdir tmp: %w", err)
	}

	if err := build(ctx, binPath); err != nil {
		return fmt.Errorf("initial build failed: %w", err)
	}

	supervisor := newSupervisor()
	if err := supervisor.start(ctx); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := addWatches(watcher, rootDir); err != nil {
		return fmt.Errorf("watch setup: %w", err)
	}
	log.Printf("watching repo for .go changes")

	rebuilds := make(chan struct{}, 1)
	go debounce(ctx, watcher, rebuilds)

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			supervisor.stop()
			return nil
		case <-rebuilds:
			handleRebuild(ctx, supervisor)
		}
	}
}

func handleRebuild(ctx context.Context, sup *supervisor) {
	if err := build(ctx, nextBinPath); err != nil {
		log.Printf("build failed (server still running):\n%v", err)
		_ = os.Remove(nextBinPath)
		return
	}
	sup.stop()
	if err := os.Rename(nextBinPath, binPath); err != nil {
		log.Printf("rename: %v", err)
		return
	}
	if err := sup.start(ctx); err != nil {
		log.Printf("start child: %v", err)
	}
}

func build(ctx context.Context, out string) error {
	// -s -w strips debug symbols; cuts per-rebuild link time with no effect on
	// runtime behavior. dlv attaches to source, not the binary symbols.
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-s -w", "-o", out, ".")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, combined)
	}
	if len(combined) > 0 {
		if _, werr := os.Stdout.Write(combined); werr != nil {
			return fmt.Errorf("write build output: %w", werr)
		}
	}
	return nil
}

type supervisor struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

func newSupervisor() *supervisor {
	return &supervisor{}
}

func (s *supervisor) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cmd := exec.CommandContext(ctx, binPath, binArg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

func (s *supervisor) stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(graceTimeout):
		log.Printf("child did not exit within %s, killing", graceTimeout)
		_ = cmd.Process.Kill()
		<-done
	}
}

func debounce(ctx context.Context, watcher *fsnotify.Watcher, out chan<- struct{}) {
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			handleEvent(watcher, event)
			if !shouldRebuild(event) {
				continue
			}
			timer, timerC = resetTimer(timer)
		case <-timerC:
			timer = nil
			timerC = nil
			select {
			case out <- struct{}{}:
			default:
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

func handleEvent(watcher *fsnotify.Watcher, event fsnotify.Event) {
	if !event.Has(fsnotify.Create) {
		return
	}
	info, err := os.Stat(event.Name)
	if err != nil || !info.IsDir() || isExcludedDir(event.Name) {
		return
	}
	_ = watcher.Add(event.Name)
}

func resetTimer(timer *time.Timer) (*time.Timer, <-chan time.Time) {
	if timer == nil {
		newTimer := time.NewTimer(debounceWindow)
		return newTimer, newTimer.C
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(debounceWindow)
	return timer, timer.C
}

func shouldRebuild(event fsnotify.Event) bool {
	const watchedOps = fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename
	if event.Op&watchedOps == 0 {
		return false
	}
	name := filepath.Base(event.Name)
	if !strings.HasSuffix(name, ".go") {
		return false
	}
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	return !isExcludedDir(filepath.Dir(event.Name))
}

func addWatches(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if !dirEntry.IsDir() {
			return nil
		}
		if isExcludedDir(path) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

func isExcludedDir(path string) bool {
	excluded := excludedDirNames()
	clean := filepath.Clean(path)
	parts := strings.Split(clean, string(filepath.Separator))
	for _, p := range parts {
		if _, ok := excluded[p]; ok {
			return true
		}
	}
	return false
}
