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

	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/vocab"
)

// claudePreTool is a Claude Code PreToolUse payload trimmed to the fields
// envelope.Decode reads: prompt_id is what detects the engine, the rest is
// what the envelope, selection and the record are asserted on.
const claudePreTool = `{"prompt_id":"p1","session_id":"sess-1","hook_event_name":"PreToolUse",` +
	`"tool_name":"Bash","cwd":"/work","tool_input":{"command":"ls"}}`

// codexSessionEnd is the discriminator-less payload Codex 0.154.0 sends for
// its SessionEnd hook: exactly the shipped session-end.command.input schema
// (cwd, hook_event_name, reason, session_id, transcript_path), with no turn_id,
// so envelope.Detect cannot place it. Inferred from the shipped binary's
// embedded schema, not captured: codex is not authenticated on this machine.
const codexSessionEnd = `{"cwd":"/work","hook_event_name":"SessionEnd","reason":"other",` +
	`"session_id":"sess-1","transcript_path":null}`

// cursorSessionEnd is the payload cursor-agent 2026.09.18 builds for
// _E.sessionEnd. It carries cursor_version, so envelope.Detect places it; the
// rest is inferred from the shipped bundle, not captured (cursor is not
// authenticated on this machine).
const cursorSessionEnd = `{"cursor_version":"2026.09.18-9a7762b","hook_event_name":"sessionEnd",` +
	`"session_id":"sess-1","conversation_id":"c1","generation_id":"g1","cwd":"",` +
	`"workspace_roots":["/work"],"reason":"completed","duration_ms":1234,` +
	`"is_background_agent":false,"final_status":"completed","model":"gpt-5"}`

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

// The payload is the thing that failed on both of these, so argv is all that
// is left to identify the call (§8). A payload with no engine discriminator is
// covered by TestRunRouteRegisteredForFallbackIsNarrow.
func TestRunRouteRecordsDecodeFailuresAsRouterErrors(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
	}{
		{"over the 1 MiB cap", strings.Repeat("a", 1<<20+1)},
		{"not JSON", "definitely not json"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stateDir := t.TempDir()
			printed := runPipeline(t, routeOpts(stateDir), c.stdin)
			wantRouterError(t, printed, readRecord(t, stateDir))
		})
	}
}

const wantRegisteredForNote = "engine taken from --registered-for claude-code: payload carried no engine discriminator"
const wantRegisteredForCodexNote = "engine taken from --registered-for codex: payload carried no engine discriminator"

// withoutDiscriminator returns a committed fixture with prompt_id and effort
// stripped, the shape Claude Code sends on events that carry neither.
func withoutDiscriminator(t *testing.T, fixture string) string {
	t.Helper()
	var native map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFixture(t, fixture)), &native); err != nil {
		t.Fatalf("decode fixture %s: %v", fixture, err)
	}
	delete(native, "prompt_id")
	delete(native, "effort")
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("encode fixture %s: %v", fixture, err)
	}
	return string(raw)
}

// Every catalog event routes in yard mode whether or not Claude Code sent a
// discriminator, and only the payloads that lacked one say so in the record.
func TestRunRouteRoutesEveryClaudeCatalogEventInYardMode(t *testing.T) {
	const notification = `{"session_id":"s","transcript_path":"/t.jsonl","cwd":"/w","hook_event_name":"Notification",` +
		`"message":"m","title":"t","notification_type":"idle_prompt"}`
	const preCompact = `{"session_id":"s","transcript_path":"/t.jsonl","cwd":"/w","hook_event_name":"PreCompact",` +
		`"trigger":"manual","custom_instructions":""}`

	type payload struct {
		name         string
		stdin        string
		wantFallback bool
	}
	payloads := map[string][]payload{
		"SessionStart":     {{"fixture", readFixture(t, "claude-SessionStart.json"), true}},
		"UserPromptSubmit": {{"fixture", readFixture(t, "claude-UserPromptSubmit.json"), false}},
		"PreToolUse":       {{"fixture", readFixture(t, "claude-PreToolUse.json"), false}},
		"PostToolUse":      {{"fixture", readFixture(t, "claude-PostToolUse.json"), false}},
		"PreCompact":       {{"synthesized", preCompact, true}},
		"Stop":             {{"fixture", readFixture(t, "claude-Stop.json"), false}},
		"Notification":     {{"synthesized", notification, true}},
		"SessionEnd": {
			{"fixture", readFixture(t, "claude-SessionEnd.json"), false},
			{"prompt-less", withoutDiscriminator(t, "claude-SessionEnd.json"), true},
		},
	}

	for _, row := range vocab.ClaudeCodeCatalog {
		cases, ok := payloads[row.Native]
		if !ok {
			t.Fatalf("no payload for catalog event %s", row.Native)
		}
		for _, c := range cases {
			t.Run(row.Native+"/"+c.name, func(t *testing.T) {
				stateDir := t.TempDir()
				writeTable(t, stateDir)
				opts := routeOpts(stateDir)
				opts.event = row.Routed

				printed := runPipeline(t, opts, c.stdin)

				if printed != "" {
					t.Errorf("want nothing printed, got %q", printed)
				}
				rec := readRecord(t, stateDir)
				if rec.Engine != "claude-code" {
					t.Errorf("want engine claude-code, got %q", rec.Engine)
				}
				if rec.NativeEvent != row.Native {
					t.Errorf("want native_event %q, got %q", row.Native, rec.NativeEvent)
				}
				if rec.Router == record.RouterError {
					t.Errorf("want a routed run, got a router error: %q", rec.Reason)
				}
				if len(rec.Handlers) != 0 {
					t.Errorf("want no handler entries, got %+v", rec.Handlers)
				}
				if got := strings.Contains(rec.Reason, wantRegisteredForNote); got != c.wantFallback {
					t.Errorf("reason carries the --registered-for note = %v, want %v (reason %q)", got, c.wantFallback, rec.Reason)
				}
			})
		}
	}
}

// The fallback is scoped to the one surface only Claude Code reads: anything
// else without a discriminator is still a router error, and says nothing
// about --registered-for.
func TestRunRouteRegisteredForFallbackIsNarrow(t *testing.T) {
	sessionStart := readFixture(t, "claude-SessionStart.json")

	wantUnknownEngine := func(t *testing.T, printed string, rec record.Record, engine string) {
		t.Helper()
		if printed != "" {
			t.Errorf("want nothing printed, got %q", printed)
		}
		if rec.Router != record.RouterError {
			t.Errorf("want router %q, got %q", record.RouterError, rec.Router)
		}
		if rec.Engine != engine {
			t.Errorf("want engine from --registered-for %q, got %q", engine, rec.Engine)
		}
		if !strings.Contains(rec.Reason, envelope.ErrUnknownEngine.Error()) {
			t.Errorf("want the unknown-engine error in the reason, got %q", rec.Reason)
		}
		if strings.Contains(rec.Reason, wantRegisteredForNote) {
			t.Errorf("want no --registered-for note on a router error, got %q", rec.Reason)
		}
	}

	yardCases := []struct {
		name          string
		registeredFor string
		event         string
		stdin         string
	}{
		{"registered for codex", "codex", "", sessionStart},
		{"registered for codex, bare PreToolUse", "codex", "", `{"hook_event_name":"PreToolUse"}`},
		{"not an exact catalog event", "claude-code", "", `{"hook_event_name":"sessionStart","session_id":"s","cwd":"/w"}`},
		// An engine-scoped fallback needs the payload's event to match the one
		// argv named for that engine; a mismatched no-discriminator payload
		// stays a router error rather than being claimed as codex.
		{"registered for codex, event named but payload event differs", "codex", "codex:SessionEnd", sessionStart},
	}
	for _, c := range yardCases {
		t.Run(c.name, func(t *testing.T) {
			stateDir := t.TempDir()
			writeTable(t, stateDir)
			opts := routeOpts(stateDir)
			opts.registeredFor = c.registeredFor
			if c.event != "" {
				opts.event = c.event
			}

			printed := runPipeline(t, opts, c.stdin)

			wantUnknownEngine(t, printed, readRecord(t, stateDir), c.registeredFor)
		})
	}

	t.Run("build mode", func(t *testing.T) {
		root := t.TempDir()
		handlersDir := filepath.Join(root, "handlers")
		if err := os.MkdirAll(handlersDir, 0o700); err != nil {
			t.Fatalf("mkdir handlers: %v", err)
		}
		opts, stateDir := pluginOpts(t, root)
		if err := record.EnsureStateDir(stateDir); err != nil {
			t.Fatalf("create state dir: %v", err)
		}
		writePluginTable(t, root, decides(t, handlersDir, "deny-a", "deny", "A"))

		printed := runPipeline(t, opts, sessionStart)

		wantUnknownEngine(t, printed, readRecord(t, stateDir), "claude-code")
	})

	t.Run("build mode codex SessionEnd", func(t *testing.T) {
		root := t.TempDir()
		handlersDir := filepath.Join(root, "handlers")
		if err := os.MkdirAll(handlersDir, 0o700); err != nil {
			t.Fatalf("mkdir handlers: %v", err)
		}
		opts, stateDir := pluginOpts(t, root)
		opts.registeredFor = "codex"
		opts.event = "codex:SessionEnd"
		if err := record.EnsureStateDir(stateDir); err != nil {
			t.Fatalf("create state dir: %v", err)
		}
		writePluginTable(t, root, decides(t, handlersDir, "deny-a", "deny", "A"))

		printed := runPipeline(t, opts, codexSessionEnd)

		wantUnknownEngine(t, printed, readRecord(t, stateDir), "codex")
	})
}

// Codex's SessionEnd payload carries no turn_id, so Detect cannot identify it;
// a yard-mode entry whose --event names codex:SessionEnd is trusted to
// --registered-for and the engine-scoped event routes.
func TestRunRouteCodexSessionEndRoutesViaRegisteredFor(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	h := decides(t, dir, "end", "allow", "")
	h.Events = []string{"codex:SessionEnd"}
	h.Engines = []string{"codex"}
	writeTable(t, stateDir, h)
	opts := routeOpts(stateDir)
	opts.registeredFor = "codex"
	opts.event = "codex:SessionEnd"

	printed := runPipeline(t, opts, codexSessionEnd)

	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Router == record.RouterError {
		t.Fatalf("want a routed run, got a router error: %q", rec.Reason)
	}
	if rec.Engine != "codex" {
		t.Errorf("Engine = %q, want codex", rec.Engine)
	}
	if rec.NativeEvent != "SessionEnd" {
		t.Errorf("NativeEvent = %q, want SessionEnd", rec.NativeEvent)
	}
	if rec.CanonicalEvent != "" {
		t.Errorf("CanonicalEvent = %q, want empty (no canonical session-end)", rec.CanonicalEvent)
	}
	if !strings.Contains(rec.Reason, wantRegisteredForCodexNote) {
		t.Errorf("reason %q does not carry the --registered-for codex note", rec.Reason)
	}
}

// Cursor's SessionEnd carries cursor_version, so Detect places it with no
// fallback: an entry whose --event names cursor:sessionEnd routes normally.
func TestRunRouteCursorSessionEndRoutes(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	h := decides(t, dir, "end", "allow", "")
	h.Events = []string{"cursor:sessionEnd"}
	h.Engines = []string{"cursor"}
	writeTable(t, stateDir, h)
	opts := routeOpts(stateDir)
	opts.registeredFor = "cursor"
	opts.event = "cursor:sessionEnd"

	printed := runPipeline(t, opts, cursorSessionEnd)

	if printed != "" {
		t.Errorf("want nothing printed, got %q", printed)
	}
	rec := readRecord(t, stateDir)
	if rec.Router == record.RouterError {
		t.Fatalf("want a routed run, got a router error: %q", rec.Reason)
	}
	if rec.Engine != "cursor" {
		t.Errorf("Engine = %q, want cursor", rec.Engine)
	}
	if rec.NativeEvent != "sessionEnd" {
		t.Errorf("NativeEvent = %q, want sessionEnd", rec.NativeEvent)
	}
	if rec.CanonicalEvent != "" {
		t.Errorf("CanonicalEvent = %q, want empty", rec.CanonicalEvent)
	}
	if strings.Contains(rec.Reason, "--registered-for") {
		t.Errorf("reason %q should not carry a fallback note; cursor_version detected it", rec.Reason)
	}
}

// The note is joined inside appendRecord, so a failure after the fallback
// still records that the engine came from argv rather than the payload.
func TestRunRouteRegisteredForNoteSurvivesALaterRouterError(t *testing.T) {
	stateDir := t.TempDir()

	printed := runPipeline(t, routeOpts(stateDir), readFixture(t, "claude-SessionStart.json"))

	rec := readRecord(t, stateDir)
	wantRouterError(t, printed, rec)
	if !strings.Contains(rec.Reason, "reading the handler table") {
		t.Errorf("want the table error kept in the reason, got %q", rec.Reason)
	}
	if !strings.Contains(rec.Reason, wantRegisteredForNote) {
		t.Errorf("want the --registered-for note joined into the reason, got %q", rec.Reason)
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

// pluginOpts is routeOpts for build mode: --plugin-root instead of --state-dir. It always
// points HOOKYARD_STATE_DIR and XDG_STATE_HOME at temp paths, so no test can ever reach the
// developer's real ~/.local/state/hookyard. The returned state dir does not exist yet — a
// test that wants a record creates it first with record.EnsureStateDir.
func pluginOpts(t *testing.T, root string) (opts routeOptions, stateDir string) {
	t.Helper()
	stateDir = filepath.Join(t.TempDir(), "state")
	t.Setenv("HOOKYARD_STATE_DIR", stateDir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return routeOptions{
		start:         time.Now(),
		registeredFor: "claude-code",
		event:         "PreToolUse",
		pluginRoot:    root,
	}, stateDir
}

// writePluginTable writes a built plugin's table at root. handlerScript and decides return
// handlers with an absolute exec rooted under the directory the caller passed them, so this
// converts each to the root-relative form a plugin table actually holds.
func writePluginTable(t *testing.T, root string, handlers ...manifest.Handler) {
	t.Helper()
	for i, h := range handlers {
		rel, err := filepath.Rel(root, h.Exec)
		if err != nil {
			t.Fatalf("rel exec: %v", err)
		}
		handlers[i].Exec = rel
	}
	if err := manifest.WritePluginTable(filepath.Join(root, manifest.PluginTablePath), handlers); err != nil {
		t.Fatalf("write plugin table: %v", err)
	}
}

func TestRunRouteBuildModeDeny(t *testing.T) {
	root := t.TempDir()
	handlersDir := filepath.Join(root, "handlers")
	if err := os.MkdirAll(handlersDir, 0o700); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}
	opts, _ := pluginOpts(t, root)
	writePluginTable(t, root, decides(t, handlersDir, "deny-a", "deny", "A"))

	printed := runPipeline(t, opts, claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
}

// A handler whose exec went missing after build is never exec'd, but it must not stop the
// rest of the plugin's handlers from running (§3.1, §5).
func TestRunRouteBuildModeMissingExecStillRendersDeny(t *testing.T) {
	root := t.TempDir()
	handlersDir := filepath.Join(root, "handlers")
	if err := os.MkdirAll(handlersDir, 0o700); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}
	opts, stateDir := pluginOpts(t, root)
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	gone := handlerScript(t, handlersDir, "gone", "exit 0")
	if err := os.Remove(gone.Exec); err != nil {
		t.Fatalf("remove gone script: %v", err)
	}
	writePluginTable(t, root, gone, decides(t, handlersDir, "deny-a", "deny", "A"))

	printed := runPipeline(t, opts, claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	rec := readRecord(t, stateDir)
	if len(rec.Handlers) != 2 {
		t.Fatalf("want two handler entries, got %+v", rec.Handlers)
	}
	if rec.Handlers[0].Name != "gone" || rec.Handlers[0].Outcome != record.OutcomeError {
		t.Errorf("want gone recorded as error first, got %+v", rec.Handlers[0])
	}
	if rec.Handlers[1].Name != "deny-a" || rec.Handlers[1].Outcome != record.OutcomeDeny {
		t.Errorf("want deny-a recorded as deny second, got %+v", rec.Handlers[1])
	}
}

// A symlink inside the root that resolves outside it is exactly what ResolvePluginExec's
// containment check exists to catch; the sibling deny still wins.
func TestRunRouteBuildModeEscapingSymlinkExecErrors(t *testing.T) {
	root := t.TempDir()
	handlersDir := filepath.Join(root, "handlers")
	if err := os.MkdirAll(handlersDir, 0o700); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}
	opts, stateDir := pluginOpts(t, root)
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write outside script: %v", err)
	}
	escaping := filepath.Join(handlersDir, "escaping")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Fatalf("symlink escaping handler: %v", err)
	}
	escapeHandler := manifest.Handler{
		ID:      "escaping",
		Exec:    escaping,
		Events:  []string{"pre_tool"},
		Engines: []string{"claude-code"},
	}
	writePluginTable(t, root, escapeHandler, decides(t, handlersDir, "deny-a", "deny", "A"))

	printed := runPipeline(t, opts, claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	rec := readRecord(t, stateDir)
	if len(rec.Handlers) != 2 {
		t.Fatalf("want two handler entries, got %+v", rec.Handlers)
	}
	if rec.Handlers[0].Name != "escaping" || rec.Handlers[0].Outcome != record.OutcomeError {
		t.Errorf("want escaping recorded as error first, got %+v", rec.Handlers[0])
	}
	if rec.Handlers[1].Name != "deny-a" || rec.Handlers[1].Outcome != record.OutcomeDeny {
		t.Errorf("want deny-a recorded as deny second, got %+v", rec.Handlers[1])
	}
}

// Build mode never creates the record's state directory on an end user's machine (§3.1): it
// only ever appends to one that is already there.
func TestRunRouteBuildModeNonexistentStateDirStaysAbsent(t *testing.T) {
	root := t.TempDir()
	handlersDir := filepath.Join(root, "handlers")
	if err := os.MkdirAll(handlersDir, 0o700); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}
	opts, stateDir := pluginOpts(t, root)
	writePluginTable(t, root, decides(t, handlersDir, "deny-a", "deny", "A"))

	printed := runPipeline(t, opts, claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("want the state dir to stay absent, got err=%v", err)
	}
}

func TestRunRouteBuildModeRelativePluginRootIsARouterError(t *testing.T) {
	opts, stateDir := pluginOpts(t, "relative/root")
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	printed := runPipeline(t, opts, claudePreTool)

	wantRouterError(t, printed, readRecord(t, stateDir))
}

// --plugin-root and --state-dir together are ambiguous about which mode is meant, so the
// call is refused before either is consulted — but the refusal itself is still recorded
// under the build-mode rule, never into --state-dir.
func TestRunRouteBuildModeBothFlagsIsARouterError(t *testing.T) {
	opts, stateDir := pluginOpts(t, t.TempDir())
	opts.stateDir = t.TempDir()
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	printed := runPipeline(t, opts, claudePreTool)

	wantRouterError(t, printed, readRecord(t, stateDir))
}

func TestRunRouteBuildModeMissingTableIsARouterError(t *testing.T) {
	root := t.TempDir()
	opts, stateDir := pluginOpts(t, root)
	if err := record.EnsureStateDir(stateDir); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	printed := runPipeline(t, opts, claudePreTool)

	wantRouterError(t, printed, readRecord(t, stateDir))
}

// A relative HOOKYARD_STATE_DIR would resolve against the agent's own cwd, not a
// trustworthy location, so build mode treats it the same as no state directory at all: the
// verdict still renders, and nothing is created.
func TestRunRouteBuildModeRelativeStateDirEnvCreatesNothing(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("HOOKYARD_STATE_DIR", "rel-state")

	root := t.TempDir()
	handlersDir := filepath.Join(root, "handlers")
	if err := os.MkdirAll(handlersDir, 0o700); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}
	writePluginTable(t, root, decides(t, handlersDir, "deny-a", "deny", "A"))

	opts := routeOptions{
		start:         time.Now(),
		registeredFor: "claude-code",
		event:         "PreToolUse",
		pluginRoot:    root,
	}
	printed := runPipeline(t, opts, claudePreTool)

	if printed != denyStdout {
		t.Errorf("want the deny printed as %q, got %q", denyStdout, printed)
	}
	if _, err := os.Stat(filepath.Join(cwd, "rel-state")); !os.IsNotExist(err) {
		t.Errorf("want no rel-state directory created, got err=%v", err)
	}
}
