package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/vocab"
)

func TestToRecordBuildLineShape(t *testing.T) {
	no := false
	e := Event{
		Engine:         vocab.Codex,
		SessionID:      "sess-1",
		CanonicalEvent: "pre_tool",
		NativeEvent:    "PreToolUse",
		CWD:            "/home/noams/nix-config",
		ToolName:       "Bash",
		Verdict:        "deny",
		Enforced:       false,
		Reason:         "on default branch main; branch first",
		Router:         RouterOK,
		RouterElapsed:  4312 * time.Millisecond,
		Handlers: []HandlerOutcome{
			{Name: "git-default-branch-guard", Outcome: OutcomeDeny, Elapsed: 11 * time.Millisecond},
			{Name: "secret-read-guard", Outcome: OutcomeAbstain, Elapsed: 9 * time.Millisecond},
			{
				Name: "git-commit-autostage-guard", Outcome: OutcomeAdvise, Elapsed: 8 * time.Millisecond,
				Advice: "Unstaged tracked changes present alongside staged changes", Delivered: &no,
			},
			{Name: "nix-stage-guard", Outcome: OutcomeTimeout, Elapsed: 4300 * time.Millisecond},
		},
	}

	now := time.Date(2026, 9, 9, 11, 4, 22, 481932000, time.UTC)
	line, err := buildLine(toRecord(e, now, "%21"))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantKeys := []string{
		"v", "ts", "key", "engine", "session_id", "canonical_event", "native_event",
		"cwd", "tool_name", "verdict", "enforced", "reason", "router", "router_ms", "handlers",
	}
	if len(top) != len(wantKeys) {
		t.Fatalf("key count mismatch: got %d keys %v, want %d keys %v", len(top), keysOf(top), len(wantKeys), wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := top[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}

	assertJSONString(t, top, "v", "1")
	assertJSONString(t, top, "ts", `"2026-09-09T11:04:22.481932Z"`)
	assertJSONString(t, top, "key", `"%21"`)
	assertJSONString(t, top, "engine", `"codex"`)
	assertJSONString(t, top, "session_id", `"sess-1"`)
	assertJSONString(t, top, "canonical_event", `"pre_tool"`)
	assertJSONString(t, top, "native_event", `"PreToolUse"`)
	assertJSONString(t, top, "cwd", `"/home/noams/nix-config"`)
	assertJSONString(t, top, "tool_name", `"Bash"`)
	assertJSONString(t, top, "verdict", `"deny"`)
	assertJSONString(t, top, "enforced", "false")
	assertJSONString(t, top, "reason", `"on default branch main; branch first"`)
	assertJSONString(t, top, "router", `"ok"`)
	assertJSONString(t, top, "router_ms", "4312")

	var handlers []map[string]json.RawMessage
	if err := json.Unmarshal(top["handlers"], &handlers); err != nil {
		t.Fatalf("unmarshal handlers: %v", err)
	}
	if len(handlers) != 4 {
		t.Fatalf("got %d handlers, want 4", len(handlers))
	}
	for i, h := range handlers {
		_, hasAdvice := h["advice"]
		_, hasDelivered := h["delivered"]
		wantAdvise := i == 2
		if hasAdvice != wantAdvise {
			t.Errorf("handler %d: advice present=%v, want %v", i, hasAdvice, wantAdvise)
		}
		if hasDelivered != wantAdvise {
			t.Errorf("handler %d: delivered present=%v, want %v", i, hasDelivered, wantAdvise)
		}
	}
	if string(handlers[2]["delivered"]) != "false" {
		t.Errorf("handlers[2].delivered = %s, want false", handlers[2]["delivered"])
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func assertJSONString(t *testing.T, top map[string]json.RawMessage, key, want string) {
	t.Helper()
	got, ok := top[key]
	if !ok {
		t.Errorf("missing key %q", key)
		return
	}
	if string(got) != want {
		t.Errorf("%s = %s, want %s", key, got, want)
	}
}

func TestComputeKey(t *testing.T) {
	if got := computeKey("%21", vocab.Codex, "sess-1"); got != "%21" {
		t.Errorf("pane branch: got %q, want %%21", got)
	}
	if got := computeKey("", vocab.Codex, "sess-1"); got != "codex/sess-1" {
		t.Errorf("fallback branch: got %q, want codex/sess-1", got)
	}
}

func TestReasonTruncation(t *testing.T) {
	// "é" is 2 bytes; repeat past 512 bytes so a naive byte-slice would split
	// the last rune.
	reason := strings.Repeat("é", 300)
	e := Event{Engine: vocab.Codex, SessionID: "s", Verdict: "allow", Router: RouterOK, Reason: reason}
	rec := toRecord(e, time.Now(), "k")
	if len(rec.Reason) > maxReasonBytes {
		t.Fatalf("reason not truncated: %d bytes", len(rec.Reason))
	}
	if !strings.HasPrefix(reason, rec.Reason) {
		t.Fatalf("truncated reason is not a prefix of the original")
	}
	if !utf8ValidTail(rec.Reason) {
		t.Fatalf("truncated reason splits a rune: %q", rec.Reason)
	}
}

func utf8ValidTail(s string) bool {
	return strings.ToValidUTF8(s, "") == s
}

func TestBuildLineTruncatesOversizeHandlers(t *testing.T) {
	handlers := make([]HandlerOutcome, 1000)
	for i := range handlers {
		handlers[i] = HandlerOutcome{Name: strings.Repeat("x", 40), Outcome: OutcomeAbstain, Elapsed: time.Millisecond}
	}
	e := Event{
		Engine: vocab.Codex, SessionID: "s", Verdict: "allow", Router: RouterOK,
		Reason: strings.Repeat("r", maxReasonBytes), Handlers: handlers,
	}
	line, err := buildLine(toRecord(e, time.Now(), "k"))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}
	if len(line) > maxRecordBytes {
		t.Fatalf("line is %d bytes, want <= %d", len(line), maxRecordBytes)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if string(decoded["truncated"]) != "true" {
		t.Errorf("truncated = %s, want true", decoded["truncated"])
	}
}

func TestBuildLineFallsBackToMinimalRecord(t *testing.T) {
	// Key is unbounded until the minimal fallback, so an oversize Key (not
	// SessionID, which the cascade truncates earlier) is what forces this
	// path.
	e := Event{
		Engine:    vocab.Codex,
		SessionID: strings.Repeat("s", 300_000),
		Verdict:   "allow",
		Router:    RouterOK,
	}
	line, err := buildLine(toRecord(e, time.Now(), strings.Repeat("k", 300_000)))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}
	if len(line) > maxRecordBytes {
		t.Fatalf("line is %d bytes, want <= %d", len(line), maxRecordBytes)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if string(decoded["truncated"]) != "true" {
		t.Errorf("truncated = %s, want true", decoded["truncated"])
	}
	var sessionID string
	if err := json.Unmarshal(decoded["session_id"], &sessionID); err != nil {
		t.Fatalf("unmarshal session_id: %v", err)
	}
	if len(sessionID) != maxFieldBytes {
		t.Errorf("session_id is %d bytes, want %d (truncated)", len(sessionID), maxFieldBytes)
	}
	if _, ok := decoded["handlers"]; ok {
		t.Errorf("minimal fallback must not include handlers")
	}
}

func TestBuildLineAdviceBoundPreservesSiblings(t *testing.T) {
	yes := true
	e := Event{
		Engine: vocab.Codex, SessionID: "s", Verdict: "ask", Router: RouterOK,
		Handlers: []HandlerOutcome{
			{
				Name: "advisory-guard", Outcome: OutcomeAdvise, Elapsed: time.Millisecond,
				Advice: strings.Repeat("a", 64*1024), Delivered: &yes,
			},
			{Name: "error-guard", Outcome: OutcomeError, Elapsed: time.Millisecond, Message: "exec: no such file or directory"},
			{Name: "timeout-guard", Outcome: OutcomeTimeout, Elapsed: 5 * time.Second},
			{Name: "abstain-guard", Outcome: OutcomeAbstain, Elapsed: time.Millisecond},
		},
	}
	line, err := buildLine(toRecord(e, time.Now(), "k"))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	var handlers []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["handlers"], &handlers); err != nil {
		t.Fatalf("unmarshal handlers: %v", err)
	}
	if len(handlers) != 4 {
		t.Fatalf("got %d handlers, want 4 (advice bound should prevent eviction)", len(handlers))
	}

	var advice string
	if err := json.Unmarshal(handlers[0]["advice"], &advice); err != nil {
		t.Fatalf("unmarshal advice: %v", err)
	}
	if len(advice) != maxReasonBytes {
		t.Errorf("advice is %d bytes, want %d (truncated)", len(advice), maxReasonBytes)
	}

	wantOutcomes := []string{OutcomeAdvise, OutcomeError, OutcomeTimeout, OutcomeAbstain}
	for i, want := range wantOutcomes {
		var outcome string
		if err := json.Unmarshal(handlers[i]["outcome"], &outcome); err != nil {
			t.Fatalf("unmarshal handlers[%d].outcome: %v", i, err)
		}
		if outcome != want {
			t.Errorf("handlers[%d].outcome = %q, want %q", i, outcome, want)
		}
	}
}

func TestHandlerMessageRoundTrip(t *testing.T) {
	e := Event{
		Engine: vocab.Codex, SessionID: "s", Verdict: "allow", Router: RouterOK,
		Handlers: []HandlerOutcome{
			{Name: "crashy-guard", Outcome: OutcomeError, Elapsed: time.Millisecond, Message: "exec: no such file or directory"},
			{Name: "quiet-guard", Outcome: OutcomeAbstain, Elapsed: time.Millisecond},
		},
	}
	line, err := buildLine(toRecord(e, time.Now(), "k"))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	var handlers []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["handlers"], &handlers); err != nil {
		t.Fatalf("unmarshal handlers: %v", err)
	}

	var msg string
	if err := json.Unmarshal(handlers[0]["message"], &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg != "exec: no such file or directory" {
		t.Errorf("message = %q, want %q", msg, "exec: no such file or directory")
	}
	if _, ok := handlers[1]["message"]; ok {
		t.Errorf("handlers[1] has message key, want omitted for empty Message")
	}
}

func TestNonAdviseHandlerNoDeliveredKey(t *testing.T) {
	// Confirms this still holds now that toRecord also truncates Advice and
	// Message on every handler, not just Advise ones.
	yes := true
	e := Event{
		Engine: vocab.Codex, SessionID: "s", Verdict: "deny", Router: RouterOK,
		Handlers: []HandlerOutcome{
			{Name: "deny-guard", Outcome: OutcomeDeny, Elapsed: time.Millisecond, Delivered: &yes},
			// dispatched is the fire-and-forget lane's outcome; toRecord clears
			// Delivered for any non-advise outcome rather than special-casing
			// it, so this pins that the new outcome inherits that behaviour
			// too.
			{Name: "notify-guard", Outcome: OutcomeDispatched, Elapsed: time.Millisecond, Delivered: &yes},
		},
	}
	line, err := buildLine(toRecord(e, time.Now(), "k"))
	if err != nil {
		t.Fatalf("buildLine: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	var handlers []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["handlers"], &handlers); err != nil {
		t.Fatalf("unmarshal handlers: %v", err)
	}
	if _, ok := handlers[0]["delivered"]; ok {
		t.Errorf("non-advise handler has delivered key, want omitted")
	}

	if string(handlers[1]["outcome"]) != `"dispatched"` {
		t.Errorf("handlers[1].outcome = %s, want \"dispatched\"", handlers[1]["outcome"])
	}
	if _, ok := handlers[1]["delivered"]; ok {
		t.Errorf("dispatched handler has delivered key, want omitted")
	}
	if _, ok := handlers[1]["advice"]; ok {
		t.Errorf("dispatched handler has advice key, want omitted")
	}
}
