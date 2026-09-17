package serve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

// tailClock is read by the tailer's goroutine and written by the test's, so
// every rollover test would be a data race without the lock.
type tailClock struct {
	mu sync.Mutex
	at time.Time
}

func newTailClock(t *testing.T, stamp string) *tailClock {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("parse %q: %v", stamp, err)
	}
	return &tailClock{at: at}
}

func (c *tailClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *tailClock) set(t *testing.T, stamp string) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("parse %q: %v", stamp, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func tailPath(stateDir, day string) string {
	return filepath.Join(record.StreamDir(stateDir), day+".jsonl")
}

func tailAppend(t *testing.T, stateDir, day, content string) {
	t.Helper()
	if err := os.MkdirAll(record.StreamDir(stateDir), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file, err := os.OpenFile(tailPath(stateDir, day), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// tailOverwrite rewrites the day's file from offset 0 without truncating, so
// its size only ever grows — a same-inode rewrite no size check can notice.
func tailOverwrite(t *testing.T, stateDir, day, content string) {
	t.Helper()
	file, err := os.OpenFile(tailPath(stateDir, day), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteAt([]byte(content), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func startTailer(t *testing.T, tl *Tailer) (<-chan TailEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan TailEvent, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tl.Run(ctx, out)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("tailer did not return after cancel")
		}
	})
	return out, cancel
}

func nextTail(t *testing.T, out <-chan TailEvent) TailEvent {
	t.Helper()
	select {
	case ev, ok := <-out:
		if !ok {
			t.Fatal("tail channel closed before the expected event")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a tail event")
	}
	return TailEvent{}
}

func nextEntry(t *testing.T, out <-chan TailEvent) *Entry {
	t.Helper()
	ev := nextTail(t, out)
	if ev.Entry == nil {
		t.Fatalf("want a record, got %+v", ev)
	}
	return ev.Entry
}

func TestTailAppendsInOrderWithAbsoluteOffsets(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	var want int64
	for _, id := range []string{"s0", "s1", "s2"} {
		line := recLine(t, record.Record{Engine: "codex", SessionID: id})
		tailAppend(t, stateDir, day, line)
		want += int64(len(line))

		entry := nextEntry(t, out)
		if entry.Rec.SessionID != id {
			t.Fatalf("SessionID = %q, want %q", entry.Rec.SessionID, id)
		}
		if entry.Day != day {
			t.Errorf("Day = %q, want %q", entry.Day, day)
		}
		if entry.Offset != want {
			t.Errorf("Offset = %d, want %d", entry.Offset, want)
		}
	}
}

func TestTailFileAppearingLaterIsPickedUp(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T00:00:00Z")
	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	// Several ticks against a state dir that has no stream dir at all — the
	// normal state of a fresh machine at 00:00 UTC.
	time.Sleep(20 * time.Millisecond)

	tailAppend(t, stateDir, day, recLine(t, record.Record{SessionID: "late"}))
	if got := nextEntry(t, out).Rec.SessionID; got != "late" {
		t.Fatalf("SessionID = %q, want %q", got, "late")
	}
}

func TestTailRolloverDrainsOldDayBeforeNewDay(t *testing.T) {
	stateDir := t.TempDir()
	dayA, dayB := "2026-09-10", "2026-09-11"
	clock := newTailClock(t, "2026-09-10T23:59:59Z")
	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	tailAppend(t, stateDir, dayA, recLine(t, record.Record{SessionID: "a1"}))
	if got := nextEntry(t, out).Rec.SessionID; got != "a1" {
		t.Fatalf("SessionID = %q, want %q", got, "a1")
	}

	// Written just before midnight and possibly not yet read: whether it
	// arrives on a last pre-rollover tick or on the rollover's drain, it must
	// arrive before the new day is announced.
	tailAppend(t, stateDir, dayA, recLine(t, record.Record{SessionID: "a2"}))
	clock.set(t, "2026-09-11T00:00:00Z")
	tailAppend(t, stateDir, dayB, recLine(t, record.Record{SessionID: "b1"}))

	last := nextEntry(t, out)
	if last.Rec.SessionID != "a2" || last.Day != dayA {
		t.Fatalf("got %q on day %q, want a2 on %q", last.Rec.SessionID, last.Day, dayA)
	}
	if ev := nextTail(t, out); ev.NewDay != dayB {
		t.Fatalf("want NewDay %q, got %+v", dayB, ev)
	}
	first := nextEntry(t, out)
	if first.Rec.SessionID != "b1" || first.Day != dayB {
		t.Fatalf("got %q on day %q, want b1 on %q", first.Rec.SessionID, first.Day, dayB)
	}
}

func TestTailTruncationRestartsBeforeReReading(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	tailAppend(t, stateDir, day, recLine(t, record.Record{SessionID: "old1"}))
	tailAppend(t, stateDir, day, recLine(t, record.Record{SessionID: "old2"}))

	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})
	for _, id := range []string{"old1", "old2"} {
		if got := nextEntry(t, out).Rec.SessionID; got != id {
			t.Fatalf("SessionID = %q, want %q", got, id)
		}
	}

	if err := os.Truncate(tailPath(stateDir, day), 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	fresh := recLine(t, record.Record{SessionID: "fresh"})
	tailAppend(t, stateDir, day, fresh)

	if ev := nextTail(t, out); ev.Restart == "" {
		t.Fatalf("want a restart, got %+v", ev)
	}
	entry := nextEntry(t, out)
	if entry.Rec.SessionID != "fresh" {
		t.Fatalf("SessionID = %q, want %q", entry.Rec.SessionID, "fresh")
	}
	if entry.Offset != int64(len(fresh)) {
		t.Errorf("Offset = %d, want %d (re-read from 0)", entry.Offset, len(fresh))
	}
}

func TestTailRapidTruncateAndRegrowDetectsRewriteBeforeReReading(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	old1 := recLine(t, record.Record{SessionID: "old1"})
	old2 := recLine(t, record.Record{SessionID: "old2"})
	tailAppend(t, stateDir, day, old1)
	tailAppend(t, stateDir, day, old2)

	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})
	for _, id := range []string{"old1", "old2"} {
		if got := nextEntry(t, out).Rec.SessionID; got != id {
			t.Fatalf("SessionID = %q, want %q", got, id)
		}
	}

	// A copytruncate that regrows back past the stale offset (len(old1)+
	// len(old2)) before the tailer's next poll: a plain size check never
	// observes the file shorter than the cursor, so only content-identity
	// detection catches it.
	padded := strings.Repeat("f", len(old1)+len(old2))
	fresh := recLine(t, record.Record{SessionID: padded})
	if len(fresh) < len(old1)+len(old2) {
		t.Fatalf("test setup: fresh (%d bytes) must be >= the old offset (%d bytes)", len(fresh), len(old1)+len(old2))
	}
	if err := os.Truncate(tailPath(stateDir, day), 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	tailAppend(t, stateDir, day, fresh)

	ev := nextTail(t, out)
	if ev.Restart == "" {
		t.Fatalf("want a restart when a same-inode rewrite regrows past the stale offset, got %+v", ev)
	}
	entry := nextEntry(t, out)
	if entry.Rec.SessionID != padded {
		t.Fatalf("SessionID = %q, want the rewritten record", entry.Rec.SessionID)
	}
	if entry.Offset != int64(len(fresh)) {
		t.Errorf("Offset = %d, want %d (re-read from 0)", entry.Offset, len(fresh))
	}
}

func TestTailFingerprintSeededFromDiskAtStartup(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	old1 := recLine(t, record.Record{SessionID: "old1"})
	old2 := recLine(t, record.Record{SessionID: "old2"})
	tailAppend(t, stateDir, day, old1+old2)

	// Start the tailer already past both records, as Hub.Seed would leave it
	// — it never reads old1/old2 itself, so its rewrite fingerprint must come
	// from disk at open time, not from its own reads.
	out, _ := startTailer(t, &Tailer{
		StateDir: stateDir,
		Poll:     time.Millisecond,
		Now:      clock.now,
		Start:    map[string]int64{day: int64(len(old1) + len(old2))},
	})

	// Ticks that open the file and emit nothing, the quiet period between
	// `hookyard serve` starting and the first new call — the window in which
	// the fingerprint has to come from disk, since no read will fill it.
	time.Sleep(20 * time.Millisecond)

	// Rewrite in place over the old bytes rather than truncating first, so the
	// file is never observed shorter than the cursor even for an instant: a
	// size check cannot catch this at any tick, only the fingerprint can.
	padded := strings.Repeat("f", len(old1)+len(old2))
	fresh := recLine(t, record.Record{SessionID: padded})
	if len(fresh) < len(old1)+len(old2) {
		t.Fatalf("test setup: fresh (%d bytes) must be >= the old offset (%d bytes)", len(fresh), len(old1)+len(old2))
	}
	tailOverwrite(t, stateDir, day, fresh)

	ev := nextTail(t, out)
	if ev.Restart == "" {
		t.Fatalf("want a restart when a rewrite happens before the tailer's own first read, got %+v", ev)
	}
	entry := nextEntry(t, out)
	if entry.Rec.SessionID != padded {
		t.Fatalf("SessionID = %q, want the rewritten record", entry.Rec.SessionID)
	}
}

func TestTailReplacementRestartsBeforeReReading(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	tailAppend(t, stateDir, day, recLine(t, record.Record{SessionID: "before"}))

	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})
	if got := nextEntry(t, out).Rec.SessionID; got != "before" {
		t.Fatalf("SessionID = %q, want %q", got, "before")
	}

	replacement := filepath.Join(record.StreamDir(stateDir), "replacement")
	if err := os.WriteFile(replacement, []byte(recLine(t, record.Record{SessionID: "after"})), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, tailPath(stateDir, day)); err != nil {
		t.Fatalf("rename: %v", err)
	}

	ev := nextTail(t, out)
	if ev.Restart != "stream file replaced" {
		t.Fatalf("want a replacement restart, got %+v", ev)
	}
	if got := nextEntry(t, out).Rec.SessionID; got != "after" {
		t.Fatalf("SessionID = %q, want %q", got, "after")
	}
}

func TestTailTornLineEmittedOnceWhenComplete(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	line := recLine(t, record.Record{Engine: "cursor", SessionID: "torn"})
	half := len(line) / 2
	tailAppend(t, stateDir, day, line[:half])
	time.Sleep(50 * time.Millisecond) // ticks that must emit nothing
	tailAppend(t, stateDir, day, line[half:])

	entry := nextEntry(t, out)
	if entry.Rec.SessionID != "torn" {
		t.Fatalf("SessionID = %q, want %q", entry.Rec.SessionID, "torn")
	}
	if entry.Offset != int64(len(line)) {
		t.Errorf("Offset = %d, want %d", entry.Offset, len(line))
	}
	select {
	case ev := <-out:
		t.Fatalf("the torn line was emitted twice: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTailOversizedCarryIsDroppedAndResyncs(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	out, _ := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	// No newline separates the runt from the record after it, so the record
	// only arrives if the runt was dropped rather than kept as carry — the
	// sleep is what puts the two on separate reads.
	runt := string(bytes.Repeat([]byte("x"), maxCarry+1))
	tailAppend(t, stateDir, day, runt)
	time.Sleep(100 * time.Millisecond)
	line := recLine(t, record.Record{SessionID: "resync"})
	tailAppend(t, stateDir, day, line)

	entry := nextEntry(t, out)
	if entry.Rec.SessionID != "resync" {
		t.Fatalf("SessionID = %q, want %q", entry.Rec.SessionID, "resync")
	}
	if want := int64(len(runt) + len(line)); entry.Offset != want {
		t.Errorf("Offset = %d, want %d", entry.Offset, want)
	}
}

func TestTailCancelClosesOut(t *testing.T) {
	stateDir := t.TempDir()
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	out, cancel := startTailer(t, &Tailer{StateDir: stateDir, Poll: time.Millisecond, Now: clock.now})

	cancel()
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("out was not closed after cancel")
		}
	}
}
