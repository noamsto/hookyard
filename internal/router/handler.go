package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/verdict"
)

// waitDelay bounds the copier once the handler is dead. Without it a killed
// child's grandchildren keep the stdout pipe open and Wait never returns,
// which would hold a fan-out goroutine past every budget in §5.
const waitDelay = 100 * time.Millisecond

// HandlerResult is one handler's run, start to classification. Outcome is the
// record's vocabulary (§6) rather than the lattice's; Verdict is the lattice's,
// and the two differ exactly where the point of the record lies — a defect
// consolidates as abstain while staying recorded as error or timeout.
//
// Delivered is deliberately absent: whether advice reached the model is
// decided by the render, not here.
type HandlerResult struct {
	ID      string
	Outcome string
	Elapsed time.Duration
	Verdict verdict.Verdict
	Reason  string
	Advice  string
	Message string
}

// runOne is the seam the panic guarantee is tested through. A handler is a
// subprocess and cannot panic the router's own Go frame, so substituting this
// variable is the only way to reach the recover in Run's goroutines; Run
// dispatches through it for that reason and never calls runHandler directly.
var runOne = runHandler

func runHandler(ctx context.Context, h manifest.Handler, payload []byte, budget time.Duration) HandlerResult {
	// The branch sits behind runOne rather than beside it so the lane keeps the
	// panic guard, the table ordering and the record path the verdict lane has.
	// ctx and budget are unused past here by construction (§4): the router
	// neither waits for this child nor lets its deadline reach it.
	if h.FireAndForget() {
		return dispatch(h, payload)
	}
	started := time.Now()
	// cancel doubles as the capped writer's kill: exec.CommandContext already
	// watches this context, and the ctx.Err() a cap kill leaves behind —
	// Canceled, not DeadlineExceeded — keeps the two kinds of kill apart.
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	out := &capWriter{limit: MaxHandlerOutput, kill: cancel}
	cmd := exec.CommandContext(ctx, h.Exec)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = out
	// Stderr stays nil, so the handler's own stderr goes to /dev/null: §8 keeps
	// it out of the record deliberately, and message is hookyard's own words.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return HandlerResult{
			ID:      h.ID,
			Outcome: record.OutcomeError,
			Elapsed: time.Since(started),
			Verdict: verdict.Abstain,
			Message: fmt.Sprintf("failed to start: %v", err),
		}
	}
	waitErr := cmd.Wait()
	res := HandlerResult{ID: h.ID, Elapsed: time.Since(started), Verdict: verdict.Abstain}
	stdout, overflowed := out.collected()

	// §5's precedence, and the order below is it: cap overflow, start failure
	// (returned above), deadline expiry, exit status, parse.
	switch {
	case overflowed:
		res.Outcome = record.OutcomeError
		res.Message = fmt.Sprintf("output exceeded the %d byte cap", MaxHandlerOutput)
	case ctx.Err() != nil:
		// Cap overflow owns the only other cancel of this context, and it was
		// checked first, so a done context here is a budget overrun — the
		// handler's own sub-budget or the router's deadline above it.
		res.Outcome = record.OutcomeTimeout
	case waitErr != nil:
		res.Outcome = record.OutcomeAbstain
	default:
		res.Outcome, res.Verdict, res.Reason, res.Advice = classify(stdout)
	}
	return res
}

// handlerOutput is the handler wire protocol (§7): the same hookSpecificOutput
// wrapper hookyard itself renders back to Claude Code and Codex.
type handlerOutput struct {
	HookSpecificOutput struct {
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		AdditionalContext        string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// classify reads one handler's stdout. Every shape it cannot read is abstain,
// never error — including a decision outside the three the lattice holds, such
// as Claude Code's own defer. That is a handler saying something with no
// lattice slot, not a broken handler, and only a defect earns error.
//
// A handler emitting both arms contributes its verdict and its advice: the
// outcome records the verdict, since recording advise would lose the decision.
func classify(stdout []byte) (outcome string, v verdict.Verdict, reason, advice string) {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return record.OutcomeAbstain, verdict.Abstain, "", ""
	}
	var out handlerOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return record.OutcomeAbstain, verdict.Abstain, "", ""
	}
	arm := out.HookSpecificOutput
	switch decision := verdict.Verdict(arm.PermissionDecision); decision {
	case verdict.Allow, verdict.Ask, verdict.Deny:
		return outcomeFor(decision), decision, arm.PermissionDecisionReason, arm.AdditionalContext
	}
	if arm.AdditionalContext != "" {
		return record.OutcomeAdvise, verdict.Abstain, "", arm.AdditionalContext
	}
	return record.OutcomeAbstain, verdict.Abstain, "", ""
}

// outcomeFor names a verdict in the record's outcome vocabulary. The two
// vocabularies are kept separate on purpose (record §6), so they are mapped
// rather than cast into each other.
func outcomeFor(v verdict.Verdict) string {
	switch v {
	case verdict.Allow:
		return record.OutcomeAllow
	case verdict.Ask:
		return record.OutcomeAsk
	case verdict.Deny:
		return record.OutcomeDeny
	}
	return record.OutcomeAbstain
}
