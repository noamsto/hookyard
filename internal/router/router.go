// Package router selects the handlers a hook event applies to, fans them out
// concurrently under one deadline, and consolidates what they say into a
// single verdict (docs/design/hookyard.md §4, §5).
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/verdict"
)

// DeadlineMS is §5's router deadline: the timeout hookyard registers with each
// engine, less the 500 ms it needs to lose gracefully. The router must hit its
// own deadline first, so that a stuck handler produces a recorded partial
// consolidation rather than whatever an engine does to a hook it gave up on.
const DeadlineMS = 4500

// Budget bounds one run. Tests scale it to milliseconds; the binary runs with
// DefaultBudget.
type Budget struct {
	Deadline time.Duration
	Handler  time.Duration
}

// DefaultBudget reads the sub-budget from manifest rather than restating it:
// manifest.MaxHandlerTimeoutMS is the bound every table entry was validated
// against, and a second constant here could disagree with it.
func DefaultBudget() Budget {
	return Budget{
		Deadline: DeadlineMS * time.Millisecond,
		Handler:  manifest.MaxHandlerTimeoutMS * time.Millisecond,
	}
}

// Result is one fan-out, consolidated. Handlers is in table order whatever
// order they finished in, so reasons, advice and the record read the same way
// on every run.
type Result struct {
	Handlers        []HandlerResult
	Verdict         verdict.Verdict
	Reason          string
	Advice          string
	DeadlineExpired bool
}

// Select returns the handlers a payload applies to, in input order (§4).
// Selection is driven by the payload rather than by the event hookyard was
// invoked for: on the cross-registration path the two disagree by
// construction, and the payload is what actually happened.
func Select(handlers []manifest.Handler, env *envelope.Envelope) []manifest.Handler {
	var selected []manifest.Handler
	for _, h := range handlers {
		if !slices.Contains(h.Engines, string(env.Engine)) {
			continue
		}
		if !matchesEvent(h, env) {
			continue
		}
		// Match is validated into the same normalized vocabulary the envelope's
		// tool name is already in (§7), so this is set membership rather than
		// another translation.
		if len(h.Match) > 0 && !slices.Contains(h.Match, env.ToolName) {
			continue
		}
		selected = append(selected, h)
	}
	return selected
}

func matchesEvent(h manifest.Handler, env *envelope.Envelope) bool {
	scoped := string(env.Engine) + ":" + env.NativeEvent
	for _, event := range h.Events {
		if env.CanonicalEvent != "" && event == env.CanonicalEvent {
			return true
		}
		if env.NativeEvent != "" && event == scoped {
			return true
		}
	}
	return false
}

type indexedResult struct {
	index  int
	result HandlerResult
}

// Run fans the selected handlers out under one deadline and folds what comes
// back. It has no error return: §5's fail-open leaves every failure mode as a
// recorded handler outcome that consolidates as abstain.
func Run(ctx context.Context, handlers []manifest.Handler, env *envelope.Envelope, budget Budget) Result {
	// Cannot fail for an envelope that came from envelope.Decode: every field
	// is a string or a json.RawMessage that was itself decoded. Were it ever to
	// fail, handlers read an empty stdin and abstain, which is the direction §5
	// wants this path to fail in.
	payload, _ := json.Marshal(env)

	ctx, cancel := context.WithTimeout(ctx, budget.Deadline)
	defer cancel()
	deadline, _ := ctx.Deadline()

	started := time.Now()
	done := make(chan indexedResult, len(handlers))
	for i, h := range handlers {
		sub := handlerBudget(h, budget, time.Until(deadline))
		go func() {
			// A panic here cannot be recovered by Run's frame: the runtime would
			// tear the process down with exit status 2, which is Cursor's block
			// shape — failing closed on the one path §5 promises fails open. One
			// broken handler is a handler defect, so it is recorded as that and
			// its siblings keep running.
			defer func() {
				if r := recover(); r != nil {
					done <- indexedResult{i, HandlerResult{
						ID:      h.ID,
						Outcome: record.OutcomeError,
						Elapsed: time.Since(started),
						Verdict: verdict.Abstain,
						Message: fmt.Sprintf("hookyard panicked running this handler: %v", r),
					}}
				}
			}()
			done <- indexedResult{i, runOne(ctx, h, payload, sub)}
		}()
	}

	outcomes := make([]HandlerResult, len(handlers))
	filled := make([]bool, len(handlers))
	collected := 0
	store := func(r indexedResult) {
		outcomes[r.index] = r.result
		filled[r.index] = true
		collected++
	}

collect:
	for collected < len(handlers) {
		select {
		case r := <-done:
			store(r)
		case <-ctx.Done():
			// A result that landed in the same instant the deadline fired is a
			// result, not a timeout — select picks at random between two ready
			// cases — so take what is already in hand before writing the rest off.
			for collected < len(handlers) {
				select {
				case r := <-done:
					store(r)
				default:
					break collect
				}
			}
		}
	}

	// The run keeps a deny that came back before the overrun (§4): §5's
	// fail-open covers a router that cannot produce a verdict, and one that has
	// already computed a deny has produced one. Discarding it would be the
	// permissive choice on a security path.
	elapsed := time.Since(started)
	for i := range outcomes {
		if filled[i] {
			continue
		}
		outcomes[i] = HandlerResult{
			ID:      handlers[i].ID,
			Outcome: record.OutcomeTimeout,
			Elapsed: elapsed,
			Verdict: verdict.Abstain,
		}
	}

	contributions := make([]verdict.Contribution, len(outcomes))
	for i, o := range outcomes {
		contributions[i] = verdict.Contribution{Verdict: o.Verdict, Reason: o.Reason, Advice: o.Advice}
	}
	consolidated, reason, advice := verdict.Fold(contributions)
	return Result{
		Handlers:        outcomes,
		Verdict:         consolidated,
		Reason:          reason,
		Advice:          advice,
		DeadlineExpired: collected < len(handlers),
	}
}

// handlerBudget is §5's min(override, remaining router budget). An override
// above the sub-budget cannot be honoured and never reaches here — manifest
// refuses it on the way in — but the remaining budget still binds, since a
// handler outliving the router's own deadline buys nothing.
func handlerBudget(h manifest.Handler, budget Budget, remaining time.Duration) time.Duration {
	sub := budget.Handler
	if h.TimeoutMS > 0 {
		sub = time.Duration(h.TimeoutMS) * time.Millisecond
	}
	return min(sub, remaining)
}
