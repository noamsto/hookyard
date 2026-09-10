package record

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/vocab"
)

func testEvent() Event {
	return Event{Engine: vocab.Codex, SessionID: "sess-1", Verdict: "allow", Router: RouterOK}
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		n++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return n
}

func TestAppendSweepsExpiredFiles(t *testing.T) {
	stateDir := t.TempDir()
	streamDir := filepath.Join(stateDir, "stream")
	if err := os.MkdirAll(streamDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	seed := func(daysAgo int) string {
		date := now.AddDate(0, 0, -daysAgo).Format("2006-01-02")
		path := filepath.Join(streamDir, date+".jsonl")
		if err := os.WriteFile(path, []byte(`{"v":1}`+"\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		return path
	}
	keep13 := seed(13)
	keep14 := seed(14)
	gone15 := seed(15)
	gone20 := seed(20)

	w := &Writer{StateDir: stateDir, Now: func() time.Time { return now }}
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, p := range []string{keep13, keep14} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to survive sweep: %v", p, err)
		}
	}
	for _, p := range []string{gone15, gone20} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be swept, stat err = %v", p, err)
		}
	}

	todayPath := StreamPath(stateDir, now)
	if n := lineCount(t, todayPath); n != 1 {
		t.Errorf("today's file has %d lines, want 1", n)
	}
}

func TestAppendRotatesAcrossUTCMidnight(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 23, 59, 0, 0, time.UTC)
	w := &Writer{StateDir: stateDir, Now: func() time.Time { return now }}

	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	firstPath := StreamPath(stateDir, now)
	if n := lineCount(t, firstPath); n != 1 {
		t.Fatalf("first file has %d lines, want 1", n)
	}

	now = now.Add(2 * time.Minute) // 2026-09-11T00:01:00Z
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("second Append: %v", err)
	}
	secondPath := StreamPath(stateDir, now)
	if firstPath == secondPath {
		t.Fatalf("expected rotation to a new path, got the same path %s twice", firstPath)
	}
	if n := lineCount(t, firstPath); n != 1 {
		t.Errorf("first file has %d lines after rotation, want 1 (untouched)", n)
	}
	if n := lineCount(t, secondPath); n != 1 {
		t.Errorf("second file has %d lines, want 1", n)
	}
}

func TestAppendFailsWhenStateDirComponentIsAFile(t *testing.T) {
	tmp := t.TempDir()
	notADir := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	w := &Writer{StateDir: filepath.Join(notADir, "hookyard")}
	if err := w.Append(testEvent()); err == nil {
		t.Fatal("expected Append to fail when a path component is a regular file")
	}
}
