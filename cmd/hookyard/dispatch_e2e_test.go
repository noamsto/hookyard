package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

// dispatchEvent is the canonical event this file routes on. post_tool is what
// the committed PostToolUse fixture decodes to, and it carries no decision
// slot — the only kind of event a fire-and-forget handler may register for.
const dispatchEvent = "post_tool"

// dispatchSleep is the handler's own runtime, and dispatchRouterBound the wall
// clock the router is held to. The gap between them is what keeps the
// sentinel-absent assertion off a race on a loaded box: the child cannot have
// written anything yet when the router is reaped.
const (
	dispatchSleep       = 2 * time.Second
	dispatchRouterBound = time.Second
)

// dispatchArgs is this file's own argv, spelled out rather than shared with
// routeArgs: every helper in route_e2e_test.go builds pre_tool handlers.
func dispatchArgs(engine, stateDir string) []string {
	return []string{"route", "--registered-for", engine, "--event", dispatchEvent, "--state-dir", stateDir}
}

// dispatchHandler writes the detachment probe: it records its own session id,
// outlives the router by sleeping, and only then writes its stdin to the
// sentinel. Recording the session id first means it lands whether or not the
// process survives long enough to reach the sentinel.
func dispatchHandler(t *testing.T, dir, sidPath, sentinelPath string) manifest.Handler {
	t.Helper()
	const id = "detach-probe"
	path := filepath.Join(dir, id)
	body := "#!/bin/sh\n" +
		"ps -o sid= -p $$ > \"" + sidPath + "\"\n" +
		"sleep " + strconv.Itoa(int(dispatchSleep/time.Second)) + "\n" +
		"cat > \"" + sentinelPath + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return manifest.Handler{
		ID:      id,
		Exec:    path,
		Events:  []string{dispatchEvent},
		Engines: []string{"claude-code"},
		Lane:    manifest.LaneFireAndForget,
	}
}

// dispatchWaitForContent polls on content rather than existence: `cat > file`
// creates the file the moment it opens it and fills it afterwards, so a poll
// that stops at os.Stat can read zero bytes off a file that is about to hold
// the payload.
func dispatchWaitForContent(t *testing.T, path string, within time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("sentinel %s never gained content within %s", path, within)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// dispatchHandlerEntry returns the sole handler entry of the single record the
// run appended, decoded as raw keys. The delivered assertion is about a key
// being absent from the serialised line, which a typed *bool cannot tell apart
// from an explicit null.
func dispatchHandlerEntry(t *testing.T, stateDir string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(record.StreamPath(stateDir, time.Now()))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d records, want 1: %s", len(lines), raw)
	}
	var rec struct {
		Handlers []map[string]json.RawMessage `json:"handlers"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if len(rec.Handlers) != 1 {
		t.Fatalf("want 1 handler entry, got %d: %s", len(rec.Handlers), lines[0])
	}
	return rec.Handlers[0]
}

// dispatchPipeBufferBytes is the kernel pipe buffer size stageStdin's own
// comment cites: the size a bytes.Reader child's stdin copier goroutine can
// fill before it needs draining by Wait.
const dispatchPipeBufferBytes = 65536

// dispatchPaddingBytes comfortably clears dispatchPipeBufferBytes rather than
// merely exceeding it, so the margin survives incidental payload growth (a
// new envelope field, say) without needing to be retuned.
const dispatchPaddingBytes = 200 * 1024

// dispatchEchoHandler writes its stdin straight to the sentinel and exits.
// Unlike dispatchHandler it needs no sleep or session probing: the payload
// size is the point here, and the router has already exited by the time this
// handler is even scheduled, regardless of how quickly the handler runs.
func dispatchEchoHandler(t *testing.T, dir, sentinelPath string) manifest.Handler {
	t.Helper()
	const id = "large-payload-probe"
	path := filepath.Join(dir, id)
	body := "#!/bin/sh\ncat > \"" + sentinelPath + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return manifest.Handler{
		ID:      id,
		Exec:    path,
		Events:  []string{dispatchEvent},
		Engines: []string{"claude-code"},
		Lane:    manifest.LaneFireAndForget,
	}
}

// dispatchEnlargeFixture pads fixture's tool_input with a long string field
// so the marshaled envelope hookyard hands the child clears
// dispatchPipeBufferBytes, and returns the padding value alongside so the
// caller can check it round-trips rather than just counting bytes.
func dispatchEnlargeFixture(t *testing.T, fixture string) (enlarged []byte, padding string) {
	t.Helper()
	var native map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture), &native); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var toolInput map[string]json.RawMessage
	if err := json.Unmarshal(native["tool_input"], &toolInput); err != nil {
		t.Fatalf("decode tool_input: %v", err)
	}

	padding = strings.Repeat("x", dispatchPaddingBytes)
	paddingJSON, err := json.Marshal(padding)
	if err != nil {
		t.Fatalf("marshal padding: %v", err)
	}
	toolInput["padding"] = paddingJSON

	toolInputJSON, err := json.Marshal(toolInput)
	if err != nil {
		t.Fatalf("marshal tool_input: %v", err)
	}
	native["tool_input"] = toolInputJSON

	enlarged, err = json.Marshal(native)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	// If this stops holding, the test below would pass by never reaching the
	// pipe buffer at all, not by the router surviving it — fail loudly
	// instead of silently testing nothing.
	if len(enlarged) <= dispatchPipeBufferBytes {
		t.Fatalf("enlarged fixture is only %d bytes, want more than %d — padding is no longer enough",
			len(enlarged), dispatchPipeBufferBytes)
	}
	return enlarged, padding
}

// TestDispatchedHandlerReceivesPayloadPastThePipeBuffer is the discriminator
// for stageStdin's choice of an unlinked temp file over
// cmd.Stdin = bytes.NewReader(payload): that form makes os/exec spawn a
// copier goroutine that only Wait drains, and this lane never calls Wait, so
// a router that exits moments after Start truncates any payload past the
// 64 KiB pipe buffer. internal/router/router_test.go already has a
// large-payload case, but it runs in-process — the test binary never exits,
// so the copier always finishes and a bytes.Reader implementation would pass
// it too.
func TestDispatchedHandlerReceivesPayloadPastThePipeBuffer(t *testing.T) {
	bin := buildRouteBinary(t)

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	sentinelPath := filepath.Join(dir, "sentinel.json")
	writeTable(t, stateDir, dispatchEchoHandler(t, dir, sentinelPath))

	enlarged, padding := dispatchEnlargeFixture(t, readFixture(t, "claude-PostToolUse.json"))

	cmd := exec.Command(bin, dispatchArgs("claude-code", stateDir)...)
	cmd.Stdin = bytes.NewReader(enlarged)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run hookyard: %v (stderr: %s)", err, stderr.String())
	}

	delivered := dispatchWaitForContent(t, sentinelPath, 15*time.Second)
	if len(delivered) <= dispatchPipeBufferBytes {
		t.Fatalf("delivered payload is %d bytes, want more than %d — looks truncated at the pipe buffer",
			len(delivered), dispatchPipeBufferBytes)
	}

	var env struct {
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(delivered, &env); err != nil {
		preview := delivered
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("delivered payload is not JSON, likely truncated: %v\nfirst bytes: %s", err, preview)
	}
	var toolInput struct {
		Padding string `json:"padding"`
	}
	if err := json.Unmarshal(env.ToolInput, &toolInput); err != nil {
		t.Fatalf("delivered tool_input is not JSON, likely truncated: %v", err)
	}
	if toolInput.Padding != padding {
		t.Errorf("delivered padding round-trip mismatch: got %d bytes, want %d — truncated somewhere in between",
			len(toolInput.Padding), len(padding))
	}
}

// dispatchSessionID asks ps for pid's session id, the same way the handler
// script asks for its own. Linux-only by construction: -o sid= is a procps
// keyword with no BSD equivalent.
func dispatchSessionID(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "sid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps -o sid= -p %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}

// TestDispatchedHandlerSurvivesTheRouterProcessGroup is acceptance criterion 2:
// the one property nothing else can prove, that a dispatched child outlives its
// parent being killed the way an engine kills a timed-out hook.
func TestDispatchedHandlerSurvivesTheRouterProcessGroup(t *testing.T) {
	bin := buildRouteBinary(t)

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	sidPath := filepath.Join(dir, "handler.sid")
	sentinelPath := filepath.Join(dir, "sentinel.json")
	writeTable(t, stateDir, dispatchHandler(t, dir, sidPath, sentinelPath))

	fixture := readFixture(t, "claude-PostToolUse.json")

	// Started here rather than through runRouteBinary: that helper uses
	// exec.CommandContext and gives no way to ask for a process group, and the
	// group is the whole experiment. Setpgid, not Setsid — the router must be
	// killable as a group or the kill below proves nothing.
	cmd := exec.Command(bin, dispatchArgs("claude-code", stateDir)...)
	cmd.Stdin = strings.NewReader(fixture)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hookyard: %v", err)
	}
	pgid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("run hookyard: %v (stderr: %s)", err, stderr.String())
	}
	elapsed := time.Since(started)

	// 1. The router did not wait for the handler: this is the "does not count
	//    against the emitted timeout budget" half of the criterion.
	if elapsed >= dispatchRouterBound {
		t.Fatalf("router took %s, want under %s against the handler's %s sleep",
			elapsed, dispatchRouterBound, dispatchSleep)
	}

	// 2. Without this the test passes vacuously whenever the child happens to
	//    finish first — including for an implementation with no detachment at
	//    all, whose child would simply have run to completion inline.
	if _, err := os.Stat(sentinelPath); err == nil {
		t.Fatalf("sentinel already exists %s into a %s sleep — the router waited for the handler",
			elapsed, dispatchSleep)
	}

	// 3. The kill deliberately comes AFTER Wait, and its ordering is the test:
	//    moving it earlier would destroy assertion 2's meaning, because the
	//    sentinel's absence would then only say the router had not finished.
	//    ESRCH is the SUCCESS signal, not a failure — once the router is reaped
	//    a correctly detached child has left the group empty, so "no such
	//    process" is exactly what a working implementation produces.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("SIGKILL the router's process group %d: %v", pgid, err)
	}

	// 4. The child receives what the router marshals from its decoded envelope,
	//    not the fixture bytes an engine wrote to the router's stdin, so the
	//    expectation is derived from the fixture rather than compared to it:
	//    the sentinel must parse as JSON and carry the session id the fixture
	//    supplied alongside the canonical event this argv routed on.
	payload := dispatchWaitForContent(t, sentinelPath, 15*time.Second)
	var delivered struct {
		SessionID      string `json:"session_id"`
		CanonicalEvent string `json:"canonical_event"`
	}
	if err := json.Unmarshal(payload, &delivered); err != nil {
		t.Fatalf("sentinel is not JSON: %v\n%s", err, payload)
	}
	var source struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(fixture), &source); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if delivered.SessionID != source.SessionID {
		t.Errorf("delivered session_id = %q, want the fixture's %q", delivered.SessionID, source.SessionID)
	}
	if delivered.CanonicalEvent != dispatchEvent {
		t.Errorf("delivered canonical_event = %q, want %q", delivered.CanonicalEvent, dispatchEvent)
	}

	// 5. The router emits the outcome and the writer serialises it; this is the
	//    only assertion that proves the two are joined on the real path.
	entry := dispatchHandlerEntry(t, stateDir)
	var outcome string
	if err := json.Unmarshal(entry["outcome"], &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome != record.OutcomeDispatched {
		t.Errorf("outcome = %q, want %q", outcome, record.OutcomeDispatched)
	}
	if _, ok := entry["delivered"]; ok {
		t.Errorf("delivered is an advise-only key, but the record line carries it: %v", entry)
	}

	// 6. The group kill alone would also pass for a plain Setpgid child, which
	//    dies with the engine in production. A session of its own is what
	//    actually survives, so this is what pins the Setsid decision.
	t.Run("the handler runs in its own session", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("ps -o sid= is a procps keyword with no BSD equivalent")
		}
		raw, err := os.ReadFile(sidPath)
		if err != nil {
			t.Fatalf("read handler session id: %v", err)
		}
		handlerSID := strings.TrimSpace(string(raw))
		if handlerSID == "" {
			t.Fatalf("handler recorded no session id")
		}
		if own := dispatchSessionID(t, os.Getpid()); handlerSID == own {
			t.Errorf("handler session id %s equals the test process's own — the child was never detached", own)
		}
	})
}
