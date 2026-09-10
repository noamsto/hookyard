//go:build unix

package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestAppendPermissionBits(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	stateDir := filepath.Join(t.TempDir(), "hookyard")
	w := &Writer{StateDir: stateDir}
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertFileMode(t, stateDir, 0o700)
	assertFileMode(t, filepath.Join(stateDir, "stream"), 0o700)
	assertFileMode(t, StreamPath(stateDir, w.now()), 0o600)
}

// TestAppendPermissionBitsRestrictiveUmask is §6's "not left to the ambient
// umask" contract exercised in the direction that actually matters: a fully
// restrictive umask would, without the explicit Chmod calls in ensureDir and
// Append, silently produce an under-permissioned (possibly unusable) state
// dir, stream dir, or stream file.
func TestAppendPermissionBitsRestrictiveUmask(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "hookyard")

	old := syscall.Umask(0o777)
	t.Cleanup(func() { syscall.Umask(old) })

	w := &Writer{StateDir: stateDir}
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertFileMode(t, stateDir, 0o700)
	assertFileMode(t, filepath.Join(stateDir, "stream"), 0o700)
	assertFileMode(t, StreamPath(stateDir, w.now()), 0o600)
}

// TestAppendTightensPreexistingDir asserts ensureDir enforces 0700 on a
// directory that already existed at looser permissions — §6 requires this
// unconditionally, not only on first creation.
func TestAppendTightensPreexistingDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "hookyard")
	streamDir := filepath.Join(stateDir, "stream")
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := &Writer{StateDir: stateDir}
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertFileMode(t, stateDir, 0o700)
	assertFileMode(t, streamDir, 0o700)
}

// TestAppendTightensPreexistingStreamFile is the same rule for the file that
// actually holds the data: today's stream file already existing at a looser
// mode must be tightened by the next append, not honoured until tomorrow's
// rotation.
func TestAppendTightensPreexistingStreamFile(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	stateDir := filepath.Join(t.TempDir(), "hookyard")
	w := &Writer{StateDir: stateDir}
	path := StreamPath(stateDir, w.now())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertFileMode(t, path, 0o600)
}

// concurrentAppendWorkerEnv guards TestConcurrentAppendWorker so a normal
// `go test` run skips it: it only does something when re-exec'd as a
// subprocess by TestConcurrentAppendsAcrossProcesses.
const concurrentAppendWorkerEnv = "HOOKYARD_RECORD_CONCURRENT_WORKER"

func TestConcurrentAppendWorker(t *testing.T) {
	if os.Getenv(concurrentAppendWorkerEnv) == "" {
		t.Skip("only runs as a subprocess of TestConcurrentAppendsAcrossProcesses")
	}
	appends, err := strconv.Atoi(os.Getenv("HOOKYARD_RECORD_APPENDS"))
	if err != nil {
		t.Fatalf("bad HOOKYARD_RECORD_APPENDS: %v", err)
	}

	w := &Writer{StateDir: os.Getenv("HOOKYARD_STATE_DIR")}
	for i := range appends {
		e := testEvent()
		if i == 0 {
			e.Handlers = padHandlers(900) // sized near the 64 KiB cap
		} else {
			e.Handlers = padHandlers(40) // sized several KB
		}
		if err := w.Append(e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func padHandlers(n int) []HandlerOutcome {
	handlers := make([]HandlerOutcome, n)
	for i := range handlers {
		handlers[i] = HandlerOutcome{Name: strings.Repeat("h", 40), Outcome: OutcomeAbstain, Elapsed: time.Millisecond}
	}
	return handlers
}

// TestConcurrentAppendsAcrossProcesses is the case §6 line 890 flags as
// untested: that O_APPEND writes from independent processes, not just
// independent goroutines sharing one file descriptor, don't interleave on a
// local filesystem.
func TestConcurrentAppendsAcrossProcesses(t *testing.T) {
	const numProcs = 6
	const appendsPerProc = 5

	stateDir := t.TempDir()
	binary := os.Args[0]

	var wg sync.WaitGroup
	errs := make([]error, numProcs)
	for i := range numProcs {
		wg.Go(func() {
			cmd := exec.Command(binary, "-test.run=^TestConcurrentAppendWorker$")
			cmd.Env = append(os.Environ(),
				concurrentAppendWorkerEnv+"=1",
				"HOOKYARD_STATE_DIR="+stateDir,
				"HOOKYARD_RECORD_APPENDS="+strconv.Itoa(appendsPerProc),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("subprocess %d: %w\n%s", i, err, out)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	path := StreamPath(stateDir, time.Now())
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 128*1024), 128*1024)
	lines := 0
	for scanner.Scan() {
		lines++
		var v map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			t.Fatalf("line %d is not valid, complete JSON: %v\n%s", lines, err, scanner.Bytes())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if want := numProcs * appendsPerProc; lines != want {
		t.Fatalf("got %d lines, want %d", lines, want)
	}
}
