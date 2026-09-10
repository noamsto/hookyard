package record

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// retentionDays is §6's "only housekeeping hookyard does": a router that
// writes the first record of a new day removes stream files older than this.
const retentionDays = 14

// DefaultStateDir mirrors doctor.DefaultPaths's env-then-fallback style.
func DefaultStateDir() (string, error) {
	if dir := os.Getenv("HOOKYARD_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "hookyard"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "hookyard"), nil
}

// Writer appends records to the stream. Now nil means time.Now; tests inject
// a fixed or advancing clock so rotation is deterministic.
type Writer struct {
	StateDir string
	Now      func() time.Time
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Append writes one record to today's stream file, creating the state and
// stream directories on first use and sweeping expired files on the first
// write of a new day. Per §6's ordering rule, the caller must already have
// delivered the verdict before calling this — a failed append must never
// change what the engine already read.
func (w *Writer) Append(e Event) error {
	now := w.now()

	streamDir := filepath.Join(w.StateDir, "stream")
	if err := ensureDir(w.StateDir); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if err := ensureDir(streamDir); err != nil {
		return fmt.Errorf("record: %w", err)
	}

	path := StreamPath(w.StateDir, now)
	_, statErr := os.Stat(path)
	newDay := os.IsNotExist(statErr)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if newDay {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = f.Close()
			return fmt.Errorf("record: %w", err)
		}
		sweep(streamDir, now)
	}

	key := computeKey(os.Getenv("TMUX_PANE"), e.Engine, e.SessionID)
	line, err := buildLine(toRecord(e, now, key))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("record: %w", err)
	}

	_, writeErr := f.Write(line)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	return nil
}

// ensureDir creates dir at 0700 if missing, and always Chmods it to 0700
// afterward — whether it was just created or already existed — so a
// pre-existing directory left looser by an earlier tool, an earlier run under
// a permissive umask, or manual creation is tightened rather than trusted.
func ensureDir(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.Chmod(dir, 0o700)
}

// sweep removes stream files whose date is more than retentionDays before
// now's UTC date. It is best-effort bookkeeping, not the write path: a
// malformed filename or a file already gone is skipped, not fatal.
func sweep(streamDir string, now time.Time) {
	entries, err := os.ReadDir(streamDir)
	if err != nil {
		return
	}
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -retentionDays)
	for _, entry := range entries {
		name := entry.Name()
		date, ok := strings.CutSuffix(name, ".jsonl")
		if !ok {
			continue
		}
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(streamDir, name))
		}
	}
}
