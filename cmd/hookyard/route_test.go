package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

// claudePreTool is a Claude Code PreToolUse payload trimmed to the fields
// envelope.Decode reads: prompt_id is what detects the engine, the rest is
// what the envelope, selection and the record are asserted on.
const claudePreTool = `{"prompt_id":"p1","session_id":"sess-1","hook_event_name":"PreToolUse",` +
	`"tool_name":"Bash","cwd":"/work","tool_input":{"command":"ls"}}`

// denyStdout is what verdict.Render prints for that payload consolidated to a
// deny whose reason is "A".
const denyStdout = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
	`"permissionDecisionReason":"A"}}` + "\n"

func routeOpts(stateDir string) routeOptions {
	return routeOptions{
		start:         time.Now(),
		registeredFor: "claude-code",
		event:         "PreToolUse",
		stateDir:      stateDir,
	}
}

// runPipeline drives runRoute the way route does, returning what it printed.
func runPipeline(t *testing.T, opts routeOptions, stdin string) string {
	t.Helper()
	var out bytes.Buffer
	runRoute(context.Background(), opts, strings.NewReader(stdin), &out)
	return out.String()
}

func handlerScript(t *testing.T, dir, id, body string) manifest.Handler {
	t.Helper()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write handler %s: %v", id, err)
	}
	return manifest.Handler{
		ID:      id,
		Exec:    path,
		Events:  []string{"pre_tool"},
		Engines: []string{"claude-code"},
	}
}

func decides(t *testing.T, dir, id, decision, reason string) manifest.Handler {
	t.Helper()
	out := `{"hookSpecificOutput":{"permissionDecision":"` + decision +
		`","permissionDecisionReason":"` + reason + `"}}`
	return handlerScript(t, dir, id, "printf '%s' '"+out+"'")
}

func writeTable(t *testing.T, stateDir string, handlers ...manifest.Handler) {
	t.Helper()
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := manifest.WriteTable(filepath.Join(stateDir, "table.json"), handlers); err != nil {
		t.Fatalf("write table: %v", err)
	}
}

// readRecord reads the single record the run under test appended.
func readRecord(t *testing.T, stateDir string) record.Record {
	t.Helper()
	raw, err := os.ReadFile(record.StreamPath(stateDir, time.Now()))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d records, want 1: %s", len(lines), raw)
	}
	var rec record.Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return rec
}

// wantRouterError is the shape §5 gives every failure before fan-out: nothing
// printed, and a record identifying the call from argv alone.
func wantRouterError(t *testing.T, printed string, rec record.Record) {
	t.Helper()
	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	if rec.Router != record.RouterError {
		t.Errorf("want router %q, got %q", record.RouterError, rec.Router)
	}
	if rec.Verdict != "abstain" {
		t.Errorf("want the fail-open verdict abstain, got %q", rec.Verdict)
	}
	if rec.Engine != "claude-code" {
		t.Errorf("want engine from --registered-for, got %q", rec.Engine)
	}
	if rec.NativeEvent != "PreToolUse" {
		t.Errorf("want native_event from --event, got %q", rec.NativeEvent)
	}
}

// An unknown flag must reach neither flag's own os.Exit(2) nor main's
// os.Exit(1): both are non-zero, and Cursor reads a non-zero exit as a block.
func TestRouteReturnsNilOnAnUnknownFlag(t *testing.T) {
	stateDir := t.TempDir()

	err := route([]string{"--registered-for", "claude-code", "--event", "PreToolUse",
		"--state-dir", stateDir, "--bogus"})
	if err != nil {
		t.Fatalf("want nil from route, got %v", err)
	}

	wantRouterError(t, "", readRecord(t, stateDir))
}

// The payload is the thing that failed on all three of these, so argv is all
// that is left to identify the call (§8).
func TestRunRouteRecordsDecodeFailuresAsRouterErrors(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
	}{
		{"over the 1 MiB cap", strings.Repeat("a", 1<<20+1)},
		{"not JSON", "definitely not json"},
		{"no engine discriminator", `{"hook_event_name":"PreToolUse"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stateDir := t.TempDir()
			printed := runPipeline(t, routeOpts(stateDir), c.stdin)
			wantRouterError(t, printed, readRecord(t, stateDir))
		})
	}
}

func TestRunRouteRecordsTableFailuresAsRouterErrors(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		stateDir := t.TempDir()
		printed := runPipeline(t, routeOpts(stateDir), claudePreTool)
		wantRouterError(t, printed, readRecord(t, stateDir))
	})

	t.Run("invalid", func(t *testing.T) {
		stateDir := t.TempDir()
		// An id ReadTable's own validation rejects: a stale or hand-edited
		// table is not trusted on the critical path.
		table := `{"handlers":[{"id":"bad id","exec":"/bin/true","events":["pre_tool"],"engines":["claude-code"]}]}`
		if err := os.WriteFile(filepath.Join(stateDir, "table.json"), []byte(table), 0o600); err != nil {
			t.Fatalf("write table: %v", err)
		}

		printed := runPipeline(t, routeOpts(stateDir), claudePreTool)
		wantRouterError(t, printed, readRecord(t, stateDir))
	})
}

// A stale config entry carries no --state-dir. The run still happens, and the
// record still says where it went — after the deny reason, never in place of it.
func TestRunRouteFallsBackToTheDefaultStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	t.Setenv("HOOKYARD_STATE_DIR", stateDir)
	writeTable(t, stateDir, decides(t, dir, "deny-a", "deny", "A"))

	printed := runPipeline(t, routeOpts(""), claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Router != record.RouterOK {
		t.Errorf("want router %q, got %q", record.RouterOK, rec.Router)
	}
	if !strings.HasPrefix(rec.Reason, "A; ") {
		t.Errorf("want the deny reason first, got %q", rec.Reason)
	}
	if !strings.Contains(rec.Reason, stateDir) {
		t.Errorf("want the fallback directory named in the reason, got %q", rec.Reason)
	}
}

func TestRunRouteRecordsTheEnvelopeAndTheHandlerItRan(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, decides(t, dir, "deny-a", "deny", "A"))

	printed := runPipeline(t, routeOpts(stateDir), claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
		t.Errorf("want an enforced deny, got %q enforced=%v", rec.Verdict, rec.Enforced)
	}
	if rec.Reason != "A" {
		t.Errorf("want reason %q, got %q", "A", rec.Reason)
	}
	if rec.SessionID != "sess-1" || rec.CWD != "/work" || rec.ToolName != "Bash" ||
		rec.CanonicalEvent != "pre_tool" {
		t.Errorf("want the envelope's fields on the record, got %+v", rec)
	}
	if len(rec.Handlers) != 1 || rec.Handlers[0].Name != "deny-a" ||
		rec.Handlers[0].Outcome != record.OutcomeDeny {
		t.Errorf("want one deny handler entry, got %+v", rec.Handlers)
	}
}

// Whether advice reached the model is the render's answer, not the handler's,
// and only an advisory entry has anywhere to put it.
func TestRunRouteRecordsDeliveryOnAdvisoryEntriesAlone(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	advisory := handlerScript(t, dir, "advisor",
		`printf '%s' '{"hookSpecificOutput":{"additionalContext":"note"}}'`)
	writeTable(t, stateDir, advisory, handlerScript(t, dir, "quiet", "exit 0"))

	printed := runPipeline(t, routeOpts(stateDir), claudePreTool)

	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"note"}}` + "\n"
	if printed != want {
		t.Errorf("want %q printed, got %q", want, printed)
	}
	rec := readRecord(t, stateDir)
	if len(rec.Handlers) != 2 {
		t.Fatalf("want two handler entries, got %+v", rec.Handlers)
	}
	if rec.Handlers[0].Delivered == nil || !*rec.Handlers[0].Delivered {
		t.Errorf("want the advisory entry marked delivered, got %+v", rec.Handlers[0])
	}
	if rec.Handlers[1].Delivered != nil {
		t.Errorf("want no delivered key on the abstaining entry, got %+v", rec.Handlers[1])
	}
}

// Nothing to run is not a decision: the call proceeds as though no hook had
// fired, and the empty handlers[] is what says so.
func TestRunRouteRecordsAnEmptyFanOutWhenNothingMatches(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	onRead := decides(t, dir, "deny-a", "deny", "A")
	onRead.Match = []string{"Read"}
	writeTable(t, stateDir, onRead)

	printed := runPipeline(t, routeOpts(stateDir), claudePreTool)

	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Router != record.RouterOK {
		t.Errorf("want router %q, got %q", record.RouterOK, rec.Router)
	}
	if rec.Verdict != "abstain" || !rec.Enforced {
		t.Errorf("want an enforced abstain, got %q enforced=%v", rec.Verdict, rec.Enforced)
	}
	if len(rec.Handlers) != 0 {
		t.Errorf("want no handler entries, got %+v", rec.Handlers)
	}
}

func TestRunRouteAllAbstainPrintsNothingAndRecordsEveryHandler(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir,
		handlerScript(t, dir, "quiet", "exit 0"),
		handlerScript(t, dir, "failing", "exit 1"))

	printed := runPipeline(t, routeOpts(stateDir), claudePreTool)

	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Router != record.RouterOK {
		t.Errorf("want router %q, got %q", record.RouterOK, rec.Router)
	}
	if len(rec.Handlers) != 2 {
		t.Fatalf("want two handler entries, got %+v", rec.Handlers)
	}
	for _, h := range rec.Handlers {
		if h.Outcome != record.OutcomeAbstain {
			t.Errorf("want %s abstaining, got %q", h.Name, h.Outcome)
		}
	}
}

// The parent deadline stands in for the router's own 4.5 s budget, so the
// timeout path is proved in milliseconds rather than seconds.
func TestRunRouteRecordsAnExpiredDeadlineAsATimeout(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, handlerScript(t, dir, "slow", "sleep 5"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	runRoute(ctx, routeOpts(stateDir), strings.NewReader(claudePreTool), &out)

	if out.Len() != 0 {
		t.Errorf("want nothing printed, got %q", out.String())
	}
	rec := readRecord(t, stateDir)
	if rec.Router != record.RouterTimeout {
		t.Errorf("want router %q, got %q", record.RouterTimeout, rec.Router)
	}
	if len(rec.Handlers) != 1 || rec.Handlers[0].Outcome != record.OutcomeTimeout {
		t.Errorf("want the handler recorded as a timeout, got %+v", rec.Handlers)
	}
}

// A failed append is bookkeeping: the deny is already on stdout by the time
// the writer is touched, and nothing about it changes.
func TestRunRouteSwallowsAnAppendFailure(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, decides(t, dir, "deny-a", "deny", "A"))
	// A regular file where the stream directory belongs. Permissions alone
	// cannot make the append fail for the user running the test, because
	// record.EnsureStateDir chmods a directory it owns back to 0700.
	stream := filepath.Join(stateDir, "stream")
	if err := os.WriteFile(stream, nil, 0o600); err != nil {
		t.Fatalf("occupy the stream path: %v", err)
	}

	printed := runPipeline(t, routeOpts(stateDir), claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	info, err := os.Stat(stream)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("want the append to have failed, got %d bytes written", info.Size())
	}
}

// A nil stdin panics inside envelope.Decode's reader, standing in for any
// defect on the sequential path: an unrecovered panic exits 2, which is
// Cursor's block shape.
func TestRunRouteRecordsARecoveredPanicAsARouterError(t *testing.T) {
	stateDir := t.TempDir()

	var out bytes.Buffer
	runRoute(context.Background(), routeOpts(stateDir), nil, &out)

	rec := readRecord(t, stateDir)
	wantRouterError(t, out.String(), rec)
	if !strings.Contains(rec.Reason, "panicked") {
		t.Errorf("want the panic named in the reason, got %q", rec.Reason)
	}
}

// The entry was imported into another engine's config, so the payload is not
// this registration's to decide: no handler runs at all.
func TestRunRouteSuppressesAPayloadFromAnotherEngine(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, decides(t, dir, "deny-a", "deny", "A"))
	opts := routeOpts(stateDir)
	opts.registeredFor = "cursor"
	opts.event = "preToolUse"

	printed := runPipeline(t, opts, claudePreTool)

	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Verdict != record.OutcomeSuppressed || !rec.Enforced {
		t.Errorf("want an enforced suppression, got %q enforced=%v", rec.Verdict, rec.Enforced)
	}
	if rec.Router != record.RouterOK {
		t.Errorf("want router %q, got %q", record.RouterOK, rec.Router)
	}
	if rec.Engine != "claude-code" {
		t.Errorf("want the payload's engine on the record, got %q", rec.Engine)
	}
	if len(rec.Handlers) != 0 {
		t.Errorf("want no handler entries, got %+v", rec.Handlers)
	}
}
