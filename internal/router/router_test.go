package router

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/verdict"
	"github.com/noamsto/hookyard/internal/vocab"
)

// loadBudget is for tests whose proof is an outcome or a structure rather than
// a duration: no sub-budget can expire on a host slow enough to delay a process
// start. A test that must not hang carries its own guard, and one that needs a
// sub-budget to expire quickly states its own.
var loadBudget = Budget{Deadline: 30 * time.Second, Handler: 20 * time.Second}

func writeHandler(t *testing.T, dir, id, body string) manifest.Handler {
	t.Helper()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write handler %s: %v", id, err)
	}
	return manifest.Handler{ID: id, Exec: path}
}

func decides(t *testing.T, dir, id, decision, reason string) manifest.Handler {
	t.Helper()
	out := `{"hookSpecificOutput":{"permissionDecision":"` + decision +
		`","permissionDecisionReason":"` + reason + `"}}`
	return writeHandler(t, dir, id, "printf '%s' '"+out+"'")
}

func testEnvelope() *envelope.Envelope {
	return &envelope.Envelope{
		Engine:         vocab.ClaudeCode,
		CanonicalEvent: vocab.PreTool,
		NativeEvent:    "PreToolUse",
		SessionID:      "sess-1",
		ToolName:       "Bash",
		ToolInput:      json.RawMessage(`{"command":"ls"}`),
	}
}

func runSingle(t *testing.T, h manifest.Handler, budget Budget) HandlerResult {
	t.Helper()
	res := Run(context.Background(), []manifest.Handler{h}, testEnvelope(), budget)
	if len(res.Handlers) != 1 {
		t.Fatalf("got %d handler results, want 1", len(res.Handlers))
	}
	return res.Handlers[0]
}

func TestSelectAppliesEngineEventAndTool(t *testing.T) {
	env := testEnvelope()
	cases := []struct {
		name    string
		handler manifest.Handler
		want    bool
	}{
		{"canonical event", manifest.Handler{ID: "a", Engines: []string{"claude-code"}, Events: []string{"pre_tool"}}, true},
		{"engine-scoped native event", manifest.Handler{ID: "b", Engines: []string{"claude-code"}, Events: []string{"claude-code:PreToolUse"}}, true},
		{"another engine's scoped event", manifest.Handler{ID: "c", Engines: []string{"claude-code"}, Events: []string{"cursor:PreToolUse"}}, false},
		{"other engine", manifest.Handler{ID: "d", Engines: []string{"cursor"}, Events: []string{"pre_tool"}}, false},
		{"other event", manifest.Handler{ID: "e", Engines: []string{"claude-code"}, Events: []string{"post_tool"}}, false},
		{"matching tool", manifest.Handler{ID: "f", Engines: []string{"claude-code"}, Events: []string{"pre_tool"}, Match: []string{"Read", "Bash"}}, true},
		{"other tool", manifest.Handler{ID: "g", Engines: []string{"claude-code"}, Events: []string{"pre_tool"}, Match: []string{"Read"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Select([]manifest.Handler{c.handler}, env)
			if (len(got) == 1) != c.want {
				t.Errorf("Select selected %d handlers, want selected=%v", len(got), c.want)
			}
		})
	}
}

func TestSelectPreservesInputOrder(t *testing.T) {
	applies := func(id string) manifest.Handler {
		return manifest.Handler{ID: id, Engines: []string{"claude-code"}, Events: []string{"pre_tool"}}
	}
	handlers := []manifest.Handler{
		applies("a"),
		{ID: "skipped", Engines: []string{"cursor"}, Events: []string{"pre_tool"}},
		applies("b"),
		applies("c"),
	}
	var ids []string
	for _, h := range Select(handlers, testEnvelope()) {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("selected %v, want [a b c]", ids)
	}
}

// TestRunFansOutConcurrentlyInTableOrder proves both halves of §5's promise at
// once. Each handler waits until all three have started, so a sequential run
// never gets past the first and it times out; then they finish in reverse of
// table order (c, b, a), so results come back in table order only because Run
// places them by index, not by arrival.
func TestRunFansOutConcurrentlyInTableOrder(t *testing.T) {
	dir := t.TempDir()
	marker := func(name string) string { return filepath.Join(dir, name) }
	wait := func(name string) string {
		return "while [ ! -e '" + marker(name) + "' ]; do sleep 0.02; done\n"
	}
	ids := []string{"a", "b", "c"}
	advises := func(id, after string) manifest.Handler {
		body := "touch '" + marker("started-"+id) + "'\n"
		for _, other := range ids {
			body += wait("started-" + other)
		}
		if after != "" {
			body += wait(after)
		}
		body += "touch '" + marker("done-"+id) + "'\n" +
			`printf '%s' '{"hookSpecificOutput":{"additionalContext":"` + id + `"}}'`
		return writeHandler(t, dir, id, body)
	}
	handlers := []manifest.Handler{advises("a", "done-b"), advises("b", "done-c"), advises("c", "")}

	res := Run(context.Background(), handlers, testEnvelope(), loadBudget)

	var got []string
	for _, h := range res.Handlers {
		got = append(got, h.ID)
		if h.Outcome != record.OutcomeAdvise {
			t.Errorf("handler %s outcome = %q, want %q", h.ID, h.Outcome, record.OutcomeAdvise)
		}
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("results in order %v, want table order [a b c]", got)
	}
	if res.Advice != "a\n\nb\n\nc" {
		t.Errorf("advice = %q, want it joined in table order", res.Advice)
	}
	if res.Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q: advice never enters the lattice", res.Verdict, verdict.Abstain)
	}
	if res.DeadlineExpired {
		t.Error("DeadlineExpired = true, want false")
	}
}

// TestRunReturnsInsideTheEmittedTimeout is the 500 ms margin itself, at real
// budgets: a handler that hangs must not hold the engine past the timeout
// hookyard registered with it.
func TestRunReturnsInsideTheEmittedTimeout(t *testing.T) {
	dir := t.TempDir()
	h := writeHandler(t, dir, "hangs", "exec sleep 30")

	start := time.Now()
	res := Run(context.Background(), []manifest.Handler{h}, testEnvelope(), DefaultBudget())
	elapsed := time.Since(start)

	emitted := render.EmittedTimeoutSeconds * time.Second
	if elapsed >= emitted {
		t.Errorf("Run took %v, want under the emitted timeout of %v", elapsed, emitted)
	}
	t.Logf("Run returned in %v, emitted timeout %v", elapsed, emitted)
	// At real budgets the sub-budget binds first by construction — that is what
	// the 200 ms consolidation margin below the router deadline is for — so the
	// router never has to fall back on its own deadline here. A run that did
	// report one expired would mean the sub-budget had stopped being applied.
	if res.DeadlineExpired {
		t.Error("DeadlineExpired = true, want false: the handler's sub-budget expires 200ms first")
	}
	if res.Handlers[0].Outcome != record.OutcomeTimeout {
		t.Errorf("handler outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeTimeout)
	}
	if res.Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Abstain)
	}
}

func TestDefaultBudgetMargins(t *testing.T) {
	emittedMS := render.EmittedTimeoutSeconds * 1000
	if got := emittedMS - DeadlineMS; got != 500 {
		t.Errorf("emitted (%d) - router deadline (%d) = %d ms, want the 500 ms losing-gracefully margin",
			emittedMS, DeadlineMS, got)
	}
	if got := DeadlineMS - manifest.MaxHandlerTimeoutMS; got != 200 {
		t.Errorf("router deadline (%d) - handler sub-budget (%d) = %d ms, want the 200 ms consolidation margin",
			DeadlineMS, manifest.MaxHandlerTimeoutMS, got)
	}
	b := DefaultBudget()
	if b.Deadline != DeadlineMS*time.Millisecond {
		t.Errorf("DefaultBudget().Deadline = %v, want %v", b.Deadline, DeadlineMS*time.Millisecond)
	}
	if b.Handler != manifest.MaxHandlerTimeoutMS*time.Millisecond {
		t.Errorf("DefaultBudget().Handler = %v, want %v", b.Handler, manifest.MaxHandlerTimeoutMS*time.Millisecond)
	}
}

func TestHandlerOverSubBudgetIsTimeoutWhileRouterIsNot(t *testing.T) {
	dir := t.TempDir()
	h := writeHandler(t, dir, "slow", "exec sleep 5")

	res := Run(context.Background(), []manifest.Handler{h}, testEnvelope(),
		Budget{Deadline: 30 * time.Second, Handler: 200 * time.Millisecond})

	if res.Handlers[0].Outcome != record.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeTimeout)
	}
	if res.DeadlineExpired {
		t.Error("DeadlineExpired = true, want false: the sub-budget expired, not the router's")
	}
	if res.Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Abstain)
	}
}

func TestHandlerTimeoutOverrideNarrowsTheSubBudget(t *testing.T) {
	dir := t.TempDir()
	h := writeHandler(t, dir, "slow", "exec sleep 60")
	h.TimeoutMS = 150

	start := time.Now()
	res := runSingle(t, h, loadBudget)
	elapsed := time.Since(start)

	if res.Outcome != record.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", res.Outcome, record.OutcomeTimeout)
	}
	if elapsed >= 10*time.Second {
		t.Errorf("handler ran %v, want it killed at its own 150ms override, not the %v sub-budget", elapsed, loadBudget.Handler)
	}
}

// TestRouterDeadlineKeepsDenyThatReturned covers §6's rule that a router
// overrun consolidates what it has: a deny that came back is a computed
// verdict, and discarding it would be the permissive choice.
func TestRouterDeadlineKeepsDenyThatReturned(t *testing.T) {
	handlers := []manifest.Handler{{ID: "denier"}, {ID: "hangs"}}

	original := runOne
	release := make(chan struct{})
	var running sync.WaitGroup
	running.Add(len(handlers))
	// Restoring runOne only after both handler goroutines have returned keeps
	// the write from racing their reads of it.
	t.Cleanup(func() {
		close(release)
		running.Wait()
		runOne = original
	})
	runOne = func(_ context.Context, h manifest.Handler, _ []byte, _ time.Duration) HandlerResult {
		defer running.Done()
		if h.ID == "denier" {
			return HandlerResult{
				ID:      h.ID,
				Outcome: record.OutcomeDeny,
				Verdict: verdict.Deny,
				Reason:  "path is protected",
			}
		}
		<-release
		return HandlerResult{ID: h.ID}
	}

	done := make(chan Result, 1)
	go func() {
		done <- Run(context.Background(), handlers, testEnvelope(),
			Budget{Deadline: time.Second, Handler: 5 * time.Second})
	}()
	var res Result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run ignored its own deadline")
	}

	if !res.DeadlineExpired {
		t.Error("DeadlineExpired = false, want true")
	}
	if res.Verdict != verdict.Deny {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Deny)
	}
	if res.Reason != "path is protected" {
		t.Errorf("reason = %q, want the denier's reason", res.Reason)
	}
	if res.Handlers[0].Outcome != record.OutcomeDeny {
		t.Errorf("denier outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeDeny)
	}
	if res.Handlers[1].Outcome != record.OutcomeTimeout {
		t.Errorf("hanging handler outcome = %q, want %q", res.Handlers[1].Outcome, record.OutcomeTimeout)
	}
}

// TestPanicInHandlerGoroutineIsRecordedAsError reaches the recover through the
// runOne seam: a real handler is a subprocess and cannot panic this frame.
func TestPanicInHandlerGoroutineIsRecordedAsError(t *testing.T) {
	dir := t.TempDir()
	handlers := []manifest.Handler{
		writeHandler(t, dir, "boom", "exit 0"),
		decides(t, dir, "denier", "deny", "path is protected"),
	}

	original := runOne
	t.Cleanup(func() { runOne = original })
	runOne = func(ctx context.Context, h manifest.Handler, payload []byte, budget time.Duration) HandlerResult {
		if h.ID == "boom" {
			panic("handler exploded")
		}
		return original(ctx, h, payload, budget)
	}

	res := Run(context.Background(), handlers, testEnvelope(), loadBudget)

	boom := res.Handlers[0]
	if boom.Outcome != record.OutcomeError {
		t.Errorf("panicking handler outcome = %q, want %q", boom.Outcome, record.OutcomeError)
	}
	if !strings.Contains(boom.Message, "handler exploded") {
		t.Errorf("message = %q, want hookyard's description of the panic", boom.Message)
	}
	if boom.Verdict != verdict.Abstain {
		t.Errorf("panicking handler verdict = %q, want %q", boom.Verdict, verdict.Abstain)
	}
	if res.Handlers[1].Outcome != record.OutcomeDeny {
		t.Errorf("sibling outcome = %q, want %q: one broken handler is not a broken run",
			res.Handlers[1].Outcome, record.OutcomeDeny)
	}
	if res.Verdict != verdict.Deny {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Deny)
	}
	if res.DeadlineExpired {
		t.Error("DeadlineExpired = true, want false: a handler panic is not a router timeout")
	}
}

// bigEnvelope pads ToolInput so json.Marshal(env) exceeds the 64 KiB pipe
// buffer stageStdin's unlinked-temp-file staging exists for.
func bigEnvelope() *envelope.Envelope {
	env := testEnvelope()
	env.ToolInput = json.RawMessage(`{"command":"` + strings.Repeat("x", 70000) + `"}`)
	return env
}

// pollSentinel waits until path holds at least len(want) bytes, then returns
// what it read. cat > path creates the file at open and fills it after, so a
// loop that stops at the first non-empty read can still catch a partial
// write; waiting for the full length keeps the equality assertion below the
// loop meaningful.
func pollSentinel(t *testing.T, path string, want []byte, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) >= len(want) {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sentinel %s did not receive %d bytes within %v", path, len(want), timeout)
	return nil
}

// TestFireAndForgetHandlerReturnsWithoutWaiting proves the router does not
// wait for a fire-and-forget handler: the handler prints a well-formed deny,
// then blocks on a release file the test only creates after Run has returned,
// so Run returning at all is only possible if it never waited on the child.
// The deny going unheard (not overridden) is the point: stdout was wired to
// /dev/null, so there was nothing to disbelieve.
func TestFireAndForgetHandlerReturnsWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	release := filepath.Join(dir, "release")
	releaseHandler := func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("write release: %v", err)
		}
	}
	t.Cleanup(releaseHandler)
	h := writeHandler(t, dir, "detached",
		"printf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"unheard\"}}'\n"+
			"while [ ! -e '"+release+"' ]; do sleep 0.02; done\n"+
			"cat > '"+sentinel+"'")
	h.Lane = manifest.LaneFireAndForget

	env := testEnvelope()

	done := make(chan Result, 1)
	go func() { done <- Run(context.Background(), []manifest.Handler{h}, env, loadBudget) }()
	var res Result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run waited for the handler")
	}
	releaseHandler()

	if len(res.Handlers) != 1 {
		t.Fatalf("got %d handler results, want 1", len(res.Handlers))
	}
	if res.Handlers[0].Outcome != record.OutcomeDispatched {
		t.Errorf("outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeDispatched)
	}
	if res.Handlers[0].Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q: the deny on stdout is unheard, not disbelieved", res.Handlers[0].Verdict, verdict.Abstain)
	}
	if res.DeadlineExpired {
		t.Error("DeadlineExpired = true, want false")
	}

	want, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got := pollSentinel(t, sentinel, want, 30*time.Second)
	if !bytes.Equal(got, want) {
		t.Errorf("sentinel bytes = %q, want the marshaled envelope %q", got, want)
	}
}

// TestFireAndForgetHandlerDeliversPayloadPastPipeBuffer covers delivery of a
// payload larger than the 64 KiB pipe buffer. It does not discriminate
// stageStdin's staging from a pipe — in-process the test binary never exits,
// so a pipe's copier goroutine always finishes. Only a router that exits can
// tell those apart, which is what the cmd/hookyard e2e of the same name does.
func TestFireAndForgetHandlerDeliversPayloadPastPipeBuffer(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	h := writeHandler(t, dir, "detached", "cat > '"+sentinel+"'")
	h.Lane = manifest.LaneFireAndForget

	env := bigEnvelope()
	want, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if len(want) <= 65536 {
		t.Fatalf("marshaled payload is %d bytes, want over 65536 so this test still exercises the pipe-buffer boundary", len(want))
	}

	res := Run(context.Background(), []manifest.Handler{h}, env, loadBudget)
	if len(res.Handlers) != 1 {
		t.Fatalf("got %d handler results, want 1", len(res.Handlers))
	}
	if res.Handlers[0].Outcome != record.OutcomeDispatched {
		t.Fatalf("outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeDispatched)
	}

	got := pollSentinel(t, sentinel, want, 5*time.Second)
	if !bytes.Equal(got, want) {
		t.Errorf("sentinel bytes (%d) != marshaled envelope bytes (%d)", len(got), len(want))
	}
}

// TestFireAndForgetHandlerWithMissingExecIsError pins the lane's other
// outcome: a start failure is a failure hookyard witnessed, so it is recorded
// as error, never as the dispatched success it never reached.
func TestFireAndForgetHandlerWithMissingExecIsError(t *testing.T) {
	h := manifest.Handler{
		ID:   "missing",
		Exec: filepath.Join(t.TempDir(), "does-not-exist"),
		Lane: manifest.LaneFireAndForget,
	}

	res := runSingle(t, h, loadBudget)

	if res.Outcome != record.OutcomeError {
		t.Errorf("outcome = %q, want %q", res.Outcome, record.OutcomeError)
	}
}
