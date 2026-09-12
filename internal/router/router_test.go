package router

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/verdict"
	"github.com/noamsto/hookyard/internal/vocab"
)

// scaledBudget keeps every test but the margin and constants tests in
// milliseconds, so the suite stays fast while exercising the same code paths.
var scaledBudget = Budget{Deadline: 2 * time.Second, Handler: time.Second}

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
// once: the handlers sleep 400/300/200 ms, so completion order is the reverse
// of table order, and a sequential run would take 900 ms.
func TestRunFansOutConcurrentlyInTableOrder(t *testing.T) {
	dir := t.TempDir()
	advises := func(id, sleep string) manifest.Handler {
		return writeHandler(t, dir, id,
			"sleep "+sleep+`; printf '%s' '{"hookSpecificOutput":{"additionalContext":"`+id+`"}}'`)
	}
	handlers := []manifest.Handler{advises("a", "0.4"), advises("b", "0.3"), advises("c", "0.2")}

	start := time.Now()
	res := Run(context.Background(), handlers, testEnvelope(), scaledBudget)
	elapsed := time.Since(start)

	if elapsed >= 700*time.Millisecond {
		t.Errorf("fan-out took %v, want well under the 900ms a sequential run costs", elapsed)
	}
	var ids []string
	for _, h := range res.Handlers {
		ids = append(ids, h.ID)
		if h.Outcome != record.OutcomeAdvise {
			t.Errorf("handler %s outcome = %q, want %q", h.ID, h.Outcome, record.OutcomeAdvise)
		}
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("results in order %v, want table order [a b c]", ids)
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
		Budget{Deadline: 2 * time.Second, Handler: 200 * time.Millisecond})

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
	h := writeHandler(t, dir, "slow", "exec sleep 5")
	h.TimeoutMS = 150

	start := time.Now()
	res := runSingle(t, h, Budget{Deadline: 3 * time.Second, Handler: 2 * time.Second})
	elapsed := time.Since(start)

	if res.Outcome != record.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", res.Outcome, record.OutcomeTimeout)
	}
	if elapsed >= time.Second {
		t.Errorf("handler ran %v, want it killed at its own 150ms override", elapsed)
	}
}

// TestRouterDeadlineKeepsDenyThatReturned covers §6's rule that a router
// overrun consolidates what it has: a deny that came back is a computed
// verdict, and discarding it would be the permissive choice.
func TestRouterDeadlineKeepsDenyThatReturned(t *testing.T) {
	dir := t.TempDir()
	handlers := []manifest.Handler{
		decides(t, dir, "denier", "deny", "path is protected"),
		writeHandler(t, dir, "hangs", "exec sleep 5"),
	}

	start := time.Now()
	res := Run(context.Background(), handlers, testEnvelope(),
		Budget{Deadline: 300 * time.Millisecond, Handler: 5 * time.Second})
	elapsed := time.Since(start)

	if elapsed >= time.Second {
		t.Errorf("Run took %v, want it to return at its own 300ms deadline", elapsed)
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

	res := Run(context.Background(), handlers, testEnvelope(), scaledBudget)

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
// then sleeps 5x the whole run budget before it ever touches the sentinel, so
// Run returning well inside that budget is only possible if it never waited.
// The deny going unheard (not overridden) is the point: stdout was wired to
// /dev/null, so there was nothing to disbelieve.
func TestFireAndForgetHandlerReturnsWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	h := writeHandler(t, dir, "detached",
		"printf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"unheard\"}}'\n"+
			"sleep 1\n"+
			"cat > '"+sentinel+"'")
	h.Lane = manifest.LaneFireAndForget

	budget := Budget{Deadline: 200 * time.Millisecond, Handler: 200 * time.Millisecond}
	env := testEnvelope()

	start := time.Now()
	res := Run(context.Background(), []manifest.Handler{h}, env, budget)
	elapsed := time.Since(start)

	if elapsed >= budget.Deadline {
		t.Errorf("Run took %v, want well inside the %v deadline", elapsed, budget.Deadline)
	}
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
	got := pollSentinel(t, sentinel, want, 5*time.Second)
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

	res := Run(context.Background(), []manifest.Handler{h}, env,
		Budget{Deadline: 2 * time.Second, Handler: time.Second})
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

	res := runSingle(t, h, scaledBudget)

	if res.Outcome != record.OutcomeError {
		t.Errorf("outcome = %q, want %q", res.Outcome, record.OutcomeError)
	}
}
