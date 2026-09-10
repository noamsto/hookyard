// Package verdict holds hookyard's decision lattice and the outbound
// renderings that carry a decision back to an engine. It is pure: nothing here
// reads a file, spawns a process, or touches the environment, so the one place
// that decides whether a tool call is blocked can be exercised entirely from a
// table (docs/design/hookyard.md §4).
package verdict

import "strings"

// Verdict is the lattice handler outcomes consolidate over, ordered
// abstain < allow < ask < deny.
type Verdict string

const (
	Abstain Verdict = "abstain"
	Allow   Verdict = "allow"
	Ask     Verdict = "ask"
	Deny    Verdict = "deny"
)

// rank orders the lattice. A value outside the four ranks with Abstain, the
// identity: a handler saying something this lattice has no slot for — Claude
// Code's own defer, say — is a handler that has not objected, not a broken one.
func rank(v Verdict) int {
	switch v {
	case Allow:
		return 1
	case Ask:
		return 2
	case Deny:
		return 3
	default:
		return 0
	}
}

// Consolidate is the maximum over the lattice, with Abstain as the identity
// element, so an empty set and an all-abstain set both yield Abstain.
//
// Deliberately not an if/else chain over "any deny, else any ask, else allow":
// that shape yields allow for the all-abstain case, which would make hookyard
// emit an explicit permissionDecision on every tool call on the machine — and
// on Claude Code an explicit allow bypasses the permission system,
// auto-approving every call every guard declined to comment on.
func Consolidate(verdicts []Verdict) Verdict {
	winner := Abstain
	for _, v := range verdicts {
		if rank(v) > rank(winner) {
			winner = v
		}
	}
	return winner
}

// Contribution is one handler's part of a consolidation. A handler that
// emitted both arms carries both: its verdict decides, its advice rides along.
type Contribution struct {
	Verdict Verdict
	Reason  string
	Advice  string
}

// Fold is the single place a set of contributions becomes a verdict, a reason
// and an advice string; nothing else builds those strings. Contributions
// arrive in manifest table order, which both joins preserve.
//
// The reason joins only the handlers that voted for the winning verdict — a
// loser's reason would explain a decision that was not taken. The advice joins
// every handler that produced one whatever its verdict, since advice never
// enters the lattice and attaches to whichever verdict wins.
func Fold(contributions []Contribution) (Verdict, string, string) {
	verdicts := make([]Verdict, len(contributions))
	for i, c := range contributions {
		verdicts[i] = c.Verdict
	}
	winner := Consolidate(verdicts)

	var reasons, advice []string
	for _, c := range contributions {
		if c.Verdict == winner && c.Reason != "" {
			reasons = append(reasons, c.Reason)
		}
		if c.Advice != "" {
			advice = append(advice, c.Advice)
		}
	}
	return winner, strings.Join(reasons, "; "), strings.Join(advice, "\n\n")
}
