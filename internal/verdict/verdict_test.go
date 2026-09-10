package verdict

import "testing"

var lattice = []Verdict{Abstain, Allow, Ask, Deny}

func TestConsolidateIsTheMaximumOverThePair(t *testing.T) {
	// Written out rather than computed from rank(), so a wrong order cannot
	// agree with the expectation by sharing the code under test.
	want := [4][4]Verdict{
		{Abstain, Allow, Ask, Deny},
		{Allow, Allow, Ask, Deny},
		{Ask, Ask, Ask, Deny},
		{Deny, Deny, Deny, Deny},
	}
	for i, left := range lattice {
		for j, right := range lattice {
			if got := Consolidate([]Verdict{left, right}); got != want[i][j] {
				t.Errorf("Consolidate(%s, %s) = %s, want %s", left, right, got, want[i][j])
			}
		}
	}
}

func TestConsolidateSingletonIsThatVerdict(t *testing.T) {
	for _, v := range lattice {
		if got := Consolidate([]Verdict{v}); got != v {
			t.Errorf("Consolidate(%s) = %s, want %s", v, got, v)
		}
	}
}

func TestConsolidateIdentityCasesYieldAbstain(t *testing.T) {
	cases := map[string][]Verdict{
		"nil":         nil,
		"empty":       {},
		"all abstain": {Abstain, Abstain, Abstain},
	}
	for name, verdicts := range cases {
		if got := Consolidate(verdicts); got != Abstain {
			t.Errorf("Consolidate(%s) = %s, want %s", name, got, Abstain)
		}
	}
}

func TestConsolidateRanksUnknownVerdictWithAbstain(t *testing.T) {
	// Claude Code's own "defer" is a decision this lattice has no slot for: a
	// handler that has not objected, never one that outranks a real verdict.
	if got := Consolidate([]Verdict{"defer"}); got != Abstain {
		t.Errorf("Consolidate(defer) = %s, want %s", got, Abstain)
	}
	if got := Consolidate([]Verdict{"defer", Allow}); got != Allow {
		t.Errorf("Consolidate(defer, allow) = %s, want %s", got, Allow)
	}
}

func checkFold(t *testing.T, contributions []Contribution, wantVerdict Verdict, wantReason, wantAdvice string) {
	t.Helper()
	gotVerdict, gotReason, gotAdvice := Fold(contributions)
	if gotVerdict != wantVerdict {
		t.Errorf("verdict = %s, want %s", gotVerdict, wantVerdict)
	}
	if gotReason != wantReason {
		t.Errorf("reason = %q, want %q", gotReason, wantReason)
	}
	if gotAdvice != wantAdvice {
		t.Errorf("advice = %q, want %q", gotAdvice, wantAdvice)
	}
}

func TestFoldJoinsOnlyTheWinningVerdictsReasons(t *testing.T) {
	checkFold(t, []Contribution{
		{Verdict: Deny, Reason: "A"},
		{Verdict: Ask, Reason: "B", Advice: "watch out"},
		{Verdict: Deny, Reason: "C"},
	}, Deny, "A; C", "watch out")
}

func TestFoldCollectsAdviceRegardlessOfVerdict(t *testing.T) {
	checkFold(t, []Contribution{
		{Verdict: Abstain, Advice: "first"},
		{Verdict: Abstain},
		{Verdict: Abstain, Advice: "second"},
	}, Abstain, "", "first\n\nsecond")
}

func TestFoldEmptyReasonsYieldEmptyReason(t *testing.T) {
	// What makes Codex's mandatory-reason fallback reachable at all.
	checkFold(t, []Contribution{
		{Verdict: Deny},
		{Verdict: Deny},
	}, Deny, "", "")
}

func TestFoldEmptySetIsTheIdentity(t *testing.T) {
	checkFold(t, nil, Abstain, "", "")
}
