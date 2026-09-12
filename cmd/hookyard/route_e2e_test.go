package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

const e2eFixtureDir = "../../docs/design/fixtures/hook-payloads"

// e2eBudget bounds one subprocess run: generous for a loaded CI box, but
// still short enough that a hung binary fails the test instead of the suite.
const e2eBudget = 10 * time.Second

// buildRouteBinary compiles cmd/hookyard once. This file's whole point is
// proving the pipeline through the real executable rather than through
// runRoute called in-process — route_test.go already covers that — so a
// build failure here belongs to the test, not to a helper package.
func buildRouteBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hookyard")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runRouteBinary drives the built binary the way an engine invokes it: argv,
// a payload on stdin, and an exit code, never runRoute's Go-level return.
// env nil inherits the test process's own environment; a non-nil slice
// (including empty) replaces it outright.
func runRouteBinary(t *testing.T, bin string, env []string, stdin string, args ...string) (stdout string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = env
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run hookyard: %v (stderr: %s)", err, stderr.String())
	}
	return out.String(), exitErr.ExitCode()
}

// routeArgs mirrors the argv internal/render actually writes into each
// engine's native config (commandString in internal/render/render.go),
// spelled out here rather than imported so a real invocation is what this
// test drives. event is the canonical event this invocation was registered
// for, mirroring the per-(engine, event) argv internal/render writes.
func routeArgs(engine, event, stateDir string) []string {
	return []string{"route", "--registered-for", engine, "--event", event, "--state-dir", stateDir}
}

// e2eHandler writes an executable handler script that emits body verbatim on
// stdout, plus its table entry for engine on event.
func e2eHandler(t *testing.T, dir, id, engine, event, body string) manifest.Handler {
	t.Helper()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write handler %s: %v", id, err)
	}
	return manifest.Handler{
		ID:      id,
		Exec:    path,
		Events:  []string{event},
		Engines: []string{engine},
	}
}

// e2eDeny, e2eQuiet and e2eAdvise are the three handler shapes a fan-out has
// to survive together: a deny carrying a reason, a silent abstain, and advice
// with no verdict of its own. All three speak the same wire shape regardless
// of engine — classify reads hookSpecificOutput from every handler, and only
// Render translates it per engine (§7).
func e2eDeny(t *testing.T, dir, engine, event, reason string) manifest.Handler {
	t.Helper()
	out := `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"` + reason + `"}}`
	return e2eHandler(t, dir, engine+"-deny", engine, event, "printf '%s' '"+out+"'")
}

func e2eQuiet(t *testing.T, dir, engine, event string) manifest.Handler {
	t.Helper()
	return e2eHandler(t, dir, engine+"-quiet", engine, event, "exit 0")
}

func e2eAdvise(t *testing.T, dir, engine, event, advice string) manifest.Handler {
	t.Helper()
	out := `{"hookSpecificOutput":{"additionalContext":"` + advice + `"}}`
	return e2eHandler(t, dir, engine+"-advise", engine, event, "printf '%s' '"+out+"'")
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e2eFixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

func wantDelivered(t *testing.T, h record.RecordHandler, want bool) {
	t.Helper()
	if h.Delivered == nil || *h.Delivered != want {
		t.Errorf("%s: delivered = %v, want %v", h.Name, h.Delivered, want)
	}
}

func TestRouteAgainstTheRealBinary(t *testing.T) {
	bin := buildRouteBinary(t)

	// One subtest per engine: a deny handler wins the lattice over a silent
	// abstain, and advice rides along per §7's per-engine capability table —
	// delivered on claude-code and cursor, dropped on codex's deny-only slot.

	t.Run("claude-code deny carries advice", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir,
			e2eDeny(t, dir, "claude-code", "pre_tool", "blocked"),
			e2eQuiet(t, dir, "claude-code", "pre_tool"),
			e2eAdvise(t, dir, "claude-code", "pre_tool", "fyi"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "claude-PreToolUse.json"),
			routeArgs("claude-code", "pre_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
			`"permissionDecisionReason":"blocked","additionalContext":"fyi"}}` + "\n"
		if printed != want {
			t.Errorf("printed = %q, want %q", printed, want)
		}

		rec := readRecord(t, stateDir)
		if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
			t.Errorf("want an enforced deny, got %q enforced=%v", rec.Verdict, rec.Enforced)
		}
		if rec.Reason != "blocked" {
			t.Errorf("reason = %q, want %q", rec.Reason, "blocked")
		}
		if len(rec.Handlers) != 3 {
			t.Fatalf("want 3 handler entries, got %+v", rec.Handlers)
		}
		if rec.Handlers[0].Outcome != record.OutcomeDeny || rec.Handlers[1].Outcome != record.OutcomeAbstain ||
			rec.Handlers[2].Outcome != record.OutcomeAdvise {
			t.Errorf("want deny/abstain/advise in table order, got %+v", rec.Handlers)
		}
		wantDelivered(t, rec.Handlers[2], true)
	})

	t.Run("codex deny drops the advice", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir,
			e2eDeny(t, dir, "codex", "pre_tool", "blocked"),
			e2eQuiet(t, dir, "codex", "pre_tool"),
			e2eAdvise(t, dir, "codex", "pre_tool", "fyi"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "codex-pre_tool_use.json"),
			routeArgs("codex", "pre_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
			`"permissionDecisionReason":"blocked"}}` + "\n"
		if printed != want {
			t.Errorf("printed = %q, want %q", printed, want)
		}

		rec := readRecord(t, stateDir)
		if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
			t.Errorf("want an enforced deny, got %q enforced=%v", rec.Verdict, rec.Enforced)
		}
		if len(rec.Handlers) != 3 {
			t.Fatalf("want 3 handler entries, got %+v", rec.Handlers)
		}
		// Codex has no advisory slot at all (§7): the advise handler's own
		// contribution is still recorded, just never delivered.
		wantDelivered(t, rec.Handlers[2], false)
	})

	t.Run("cursor deny joins reason and advice", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir,
			e2eDeny(t, dir, "cursor", "pre_tool", "blocked"),
			e2eQuiet(t, dir, "cursor", "pre_tool"),
			e2eAdvise(t, dir, "cursor", "pre_tool", "fyi"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "cursor-preToolUse.json"),
			routeArgs("cursor", "pre_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		want := `{"permission":"deny","user_message":"blocked\n\nfyi"}` + "\n"
		if printed != want {
			t.Errorf("printed = %q, want %q", printed, want)
		}

		rec := readRecord(t, stateDir)
		if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
			t.Errorf("want an enforced deny, got %q enforced=%v", rec.Verdict, rec.Enforced)
		}
		// The captured fixture carries an empty cwd and tool_name "Shell";
		// the envelope normalizes both (cwd from workspace_roots[0], Shell to
		// the normalized Bash) before the record ever sees them (§7).
		if rec.CWD == "" || rec.ToolName != "Bash" {
			t.Errorf("want the envelope's normalized cwd and tool name on the record, got %+v", rec)
		}
		if len(rec.Handlers) != 3 {
			t.Fatalf("want 3 handler entries, got %+v", rec.Handlers)
		}
		wantDelivered(t, rec.Handlers[2], true)
	})

	// §8's cross-registration suppression: a Cursor payload arriving on a
	// route invocation registered for Claude Code never reaches selection, so
	// even a handler that would otherwise deny never runs.
	t.Run("suppresses a payload from another engine", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir, e2eDeny(t, dir, "claude-code", "pre_tool", "blocked"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "cursor-preToolUse.json"),
			routeArgs("claude-code", "pre_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		if printed != "" {
			t.Errorf("want nothing printed, got %q", printed)
		}

		rec := readRecord(t, stateDir)
		if rec.Verdict != record.OutcomeSuppressed || !rec.Enforced {
			t.Errorf("want a suppressed, enforced record, got %q enforced=%v", rec.Verdict, rec.Enforced)
		}
		if rec.Router != record.RouterOK {
			t.Errorf("want router %q, got %q", record.RouterOK, rec.Router)
		}
		if len(rec.Handlers) != 0 {
			t.Errorf("want no handler entries — nothing ran, got %+v", rec.Handlers)
		}
	})

	// A cleared environment must not stop the record from landing. Every path
	// in this run is absolute (the binary, the handler script, --state-dir),
	// so nothing here needs an inherited environment — that is the point:
	// with --state-dir supplied, resolveStateDir never calls
	// record.DefaultStateDir, whose os.UserHomeDir fallback a cleared HOME
	// would leave unable to resolve anything. Asserting the record on disk,
	// not just a zero exit code, is what would catch a regression where that
	// fallback started running unconditionally and silently wrote nothing.
	t.Run("writes the record with a cleared environment", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir, e2eDeny(t, dir, "claude-code", "pre_tool", "isolated"))

		printed, code := runRouteBinary(t, bin, []string{}, readFixture(t, "claude-PreToolUse.json"),
			routeArgs("claude-code", "pre_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		if printed == "" {
			t.Fatalf("want the deny printed even with a cleared environment, got nothing")
		}

		rec := readRecord(t, stateDir)
		if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
			t.Errorf("want an enforced deny recorded despite the cleared environment, got %q enforced=%v",
				rec.Verdict, rec.Enforced)
		}
		if rec.Reason != "isolated" {
			t.Errorf("reason = %q, want %q", rec.Reason, "isolated")
		}
	})

	// ExitOnError's usage text would itself land on stderr as Cursor's block
	// reason; ContinueOnError plus route's unconditional nil return is what
	// keeps this at exit 0 instead (§5).
	t.Run("an unknown flag still exits 0", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "claude-PreToolUse.json"),
			append(routeArgs("claude-code", "pre_tool", stateDir), "--bogus-flag")...)
		if code != 0 {
			t.Fatalf("want exit 0 on an unknown flag — a non-zero exit is Cursor's block shape, got %d", code)
		}
		if printed != "" {
			t.Errorf("want nothing printed on a flag parse failure, got %q", printed)
		}

		rec := readRecord(t, stateDir)
		if rec.Router != record.RouterError {
			t.Errorf("want router %q, got %q", record.RouterError, rec.Router)
		}
	})
}

// TestRouteDeliversAdvisoryOnAdvisoryOnlyEvents proves through the real
// binary that Claude Code's widened advisory set (§7: pre_tool,
// session_start, post_tool) actually reaches stdout on the two events that
// carry no decision slot, and that a computed deny on those events never
// leaks permissionDecision/Reason into the advisory-only shape.
//
// Each subtest gets its own t.TempDir(): manifest.ReadTable rejects a
// duplicate handler id, and ids here are derived as engine+"-advise"/
// engine+"-deny", so sharing a table across subtests would collide.
func TestRouteDeliversAdvisoryOnAdvisoryOnlyEvents(t *testing.T) {
	bin := buildRouteBinary(t)

	t.Run("session_start advice is delivered", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir, e2eAdvise(t, dir, "claude-code", "session_start", "fyi"))

		// No committed fixture captures a real Claude Code SessionStart
		// payload, so this is a literal inline payload. envelope.Detect
		// identifies Claude Code by prompt_id or effort — never by
		// hook_event_name — so prompt_id must be present or Decode fails with
		// ErrUnknownEngine. vocab.InboundEvent(ClaudeCode, "SessionStart") ->
		// session_start is likewise assumed, not fixture-backed (§7), so this
		// test pins hookyard's own contract on that mapping rather than
		// verifying it against a captured payload.
		payload := `{"hook_event_name":"SessionStart","session_id":"e2e","cwd":"/tmp","prompt_id":"e2e","source":"startup"}`

		printed, code := runRouteBinary(t, bin, nil, payload,
			routeArgs("claude-code", "session_start", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		want := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"fyi"}}` + "\n"
		if printed != want {
			t.Errorf("printed = %q, want %q", printed, want)
		}

		rec := readRecord(t, stateDir)
		if len(rec.Handlers) != 1 {
			t.Fatalf("want 1 handler entry, got %+v", rec.Handlers)
		}
		wantDelivered(t, rec.Handlers[0], true)
	})

	t.Run("post_tool advice is delivered", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		writeTable(t, stateDir, e2eAdvise(t, dir, "claude-code", "post_tool", "fyi"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "claude-PostToolUse.json"),
			routeArgs("claude-code", "post_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		want := `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"fyi"}}` + "\n"
		if printed != want {
			t.Errorf("printed = %q, want %q", printed, want)
		}

		rec := readRecord(t, stateDir)
		if len(rec.Handlers) != 1 {
			t.Fatalf("want 1 handler entry, got %+v", rec.Handlers)
		}
		wantDelivered(t, rec.Handlers[0], true)
	})

	t.Run("post_tool deny does not leak into the advisory-only slot", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		// A deny handler alone emits no additionalContext, so the advise
		// handler has to ride along for there to be anything to assert.
		writeTable(t, stateDir,
			e2eDeny(t, dir, "claude-code", "post_tool", "blocked"),
			e2eAdvise(t, dir, "claude-code", "post_tool", "fyi"))

		printed, code := runRouteBinary(t, bin, nil, readFixture(t, "claude-PostToolUse.json"),
			routeArgs("claude-code", "post_tool", stateDir)...)
		if code != 0 {
			t.Fatalf("want exit 0, got %d", code)
		}
		if !strings.Contains(printed, "additionalContext") {
			t.Errorf("want additionalContext in the output, got %q", printed)
		}
		if strings.Contains(printed, "permissionDecision") {
			t.Errorf("want no permissionDecision on an event with no decision slot, got %q", printed)
		}
		if strings.Contains(printed, "blocked") {
			t.Errorf("want the deny's reason not to leak into the advisory-only output, got %q", printed)
		}

		rec := readRecord(t, stateDir)
		// Deny still wins the lattice (§7's Fold) even though this event has
		// nowhere to render the decision — Verdict reflects what was computed,
		// Enforced (checked at the handler level below) reflects what reached
		// the engine.
		if rec.Verdict != record.OutcomeDeny {
			t.Errorf("want the deny to win the lattice, got %q", rec.Verdict)
		}
		if len(rec.Handlers) != 2 {
			t.Fatalf("want 2 handler entries, got %+v", rec.Handlers)
		}
		if rec.Handlers[0].Outcome != record.OutcomeDeny || rec.Handlers[1].Outcome != record.OutcomeAdvise {
			t.Errorf("want deny/advise in table order, got %+v", rec.Handlers)
		}
		wantDelivered(t, rec.Handlers[1], true)
	})
}
