package router

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/verdict"
)

// emits writes a handler that prints n bytes on stdout, in 256-byte chunks
// through the shell's own printf so the fixture depends on no external binary.
func emits(t *testing.T, dir, id string, n int) manifest.Handler {
	t.Helper()
	const chunk = 256
	body := "line=" + strings.Repeat("a", chunk) + "\n" +
		"i=0\n" +
		"while [ $i -lt " + strconv.Itoa(n/chunk) + " ]; do printf '%s' \"$line\"; i=$((i+1)); done"
	return writeHandler(t, dir, id, body)
}

func TestOverCapIsErrorWithAMessage(t *testing.T) {
	dir := t.TempDir()
	body := "line=" + strings.Repeat("a", 256) + "\nwhile :; do printf '%s' \"$line\"; done"
	h := writeHandler(t, dir, "runaway", body)

	start := time.Now()
	got := runSingle(t, h, scaledBudget)
	elapsed := time.Since(start)

	if got.Outcome != record.OutcomeError {
		t.Errorf("outcome = %q, want %q", got.Outcome, record.OutcomeError)
	}
	if !strings.Contains(got.Message, strconv.Itoa(MaxHandlerOutput)) {
		t.Errorf("message = %q, want it to name the %d byte cap", got.Message, MaxHandlerOutput)
	}
	if got.Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q: a defect's output is not a verdict", got.Verdict, verdict.Abstain)
	}
	if elapsed >= scaledBudget.Handler {
		t.Errorf("runaway ran %v, want it killed as bytes arrived rather than at its sub-budget", elapsed)
	}
	t.Logf("runaway handler killed after %v, sub-budget %v", elapsed, scaledBudget.Handler)
}

func TestExactlyAtCapIsNotError(t *testing.T) {
	dir := t.TempDir()
	got := runSingle(t, emits(t, dir, "exact", MaxHandlerOutput), scaledBudget)

	if got.Outcome != record.OutcomeAbstain {
		t.Errorf("outcome = %q, want %q: exactly at the cap is not over it", got.Outcome, record.OutcomeAbstain)
	}
	if got.Message != "" {
		t.Errorf("message = %q, want empty", got.Message)
	}
}

// TestOverCapDiscardsAWellFormedDeny is §5's named consequence: a handler that
// denies and then runs past the cap is a defect, so its deny is discarded in
// the permissive direction while the record keeps it visible as error.
func TestOverCapDiscardsAWellFormedDeny(t *testing.T) {
	dir := t.TempDir()
	body := `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}'` +
		"\nline=" + strings.Repeat("a", 256) + "\nwhile :; do printf '%s' \"$line\"; done"
	h := writeHandler(t, dir, "denies-then-floods", body)

	res := Run(context.Background(), []manifest.Handler{h}, testEnvelope(), scaledBudget)

	if res.Handlers[0].Outcome != record.OutcomeError {
		t.Errorf("outcome = %q, want %q", res.Handlers[0].Outcome, record.OutcomeError)
	}
	if res.Verdict != verdict.Abstain {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Abstain)
	}
}

func TestMissingBinaryIsError(t *testing.T) {
	h := manifest.Handler{ID: "gone", Exec: filepath.Join(t.TempDir(), "not-there")}
	got := runSingle(t, h, scaledBudget)

	if got.Outcome != record.OutcomeError {
		t.Errorf("outcome = %q, want %q", got.Outcome, record.OutcomeError)
	}
	if got.Message == "" {
		t.Error("message is empty, want the start failure described")
	}
}

func TestStdoutShapesClassifyPerTheTable(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		body    string
		outcome string
		verdict verdict.Verdict
		reason  string
		advice  string
	}{
		{"allow", `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"allow","permissionDecisionReason":"known good"}}'`,
			record.OutcomeAllow, verdict.Allow, "known good", ""},
		{"ask", `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"confirm"}}'`,
			record.OutcomeAsk, verdict.Ask, "confirm", ""},
		{"deny", `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"protected"}}'`,
			record.OutcomeDeny, verdict.Deny, "protected", ""},
		{"advice only", `printf '%s' '{"hookSpecificOutput":{"additionalContext":"prefer rg"}}'`,
			record.OutcomeAdvise, verdict.Abstain, "", "prefer rg"},
		{"empty stdout", "exit 0", record.OutcomeAbstain, verdict.Abstain, "", ""},
		{"non-zero exit", `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}'` + "\nexit 3",
			record.OutcomeAbstain, verdict.Abstain, "", ""},
		{"malformed stdout", "printf 'not json at all'", record.OutcomeAbstain, verdict.Abstain, "", ""},
		{"unrecognized decision", `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"defer"}}'`,
			record.OutcomeAbstain, verdict.Abstain, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runSingle(t, writeHandler(t, dir, strings.ReplaceAll(c.name, " ", "-"), c.body), scaledBudget)
			if got.Outcome != c.outcome {
				t.Errorf("outcome = %q, want %q", got.Outcome, c.outcome)
			}
			if got.Verdict != c.verdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, c.verdict)
			}
			if got.Reason != c.reason {
				t.Errorf("reason = %q, want %q", got.Reason, c.reason)
			}
			if got.Advice != c.advice {
				t.Errorf("advice = %q, want %q", got.Advice, c.advice)
			}
		})
	}
}

func TestBothArmsContributeVerdictAndAdvice(t *testing.T) {
	dir := t.TempDir()
	h := writeHandler(t, dir, "both", `printf '%s' `+
		`'{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"confirm","additionalContext":"prefer rg"}}'`)

	res := Run(context.Background(), []manifest.Handler{h}, testEnvelope(), scaledBudget)

	if res.Handlers[0].Outcome != record.OutcomeAsk {
		t.Errorf("outcome = %q, want %q: recording advise would lose the verdict",
			res.Handlers[0].Outcome, record.OutcomeAsk)
	}
	if res.Verdict != verdict.Ask {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdict.Ask)
	}
	if res.Reason != "confirm" {
		t.Errorf("reason = %q, want %q", res.Reason, "confirm")
	}
	if res.Advice != "prefer rg" {
		t.Errorf("advice = %q, want %q", res.Advice, "prefer rg")
	}
}

func TestHandlerReadsTheEnvelopeFromStdin(t *testing.T) {
	dir := t.TempDir()
	h := writeHandler(t, dir, "echoes", "payload=$(cat)\n"+
		`case "$payload" in *'"tool_name":"Bash"'*'"command":"ls"'*) `+
		`printf '%s' '{"hookSpecificOutput":{"additionalContext":"saw the envelope"}}' ;; esac`)

	got := runSingle(t, h, scaledBudget)

	if got.Advice != "saw the envelope" {
		t.Errorf("advice = %q, want the handler to have read the marshalled envelope", got.Advice)
	}
}
