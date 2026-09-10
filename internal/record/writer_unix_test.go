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

func TestAppendPermissionBits(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	stateDir := filepath.Join(t.TempDir(), "hookyard")
	w := &Writer{StateDir: stateDir}
	if err := w.Append(testEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	assertMode(stateDir, 0o700)
	assertMode(filepath.Join(stateDir, "stream"), 0o700)
	assertMode(StreamPath(stateDir, w.now()), 0o600)
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
	for i := 0; i < appends; i++ {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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
