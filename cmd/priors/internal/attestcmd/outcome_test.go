package attestcmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

func TestClassify(t *testing.T) {
	const (
		digest = "dig"
		tip    = "tip1"
	)
	verdicts := func(status string, states map[string]attest.State, sup map[string]uint64) attest.Verdicts {
		return attest.Verdicts{Tip: tip, Status: status, States: states, Superseded: sup}
	}
	state := func(s attest.State) attest.Verdicts {
		return verdicts("ok", map[string]attest.State{digest: s}, nil)
	}
	superseded := verdicts("ok", map[string]attest.State{digest: attest.StateSuperseded}, map[string]uint64{digest: 10})
	never := func(string) bool {
		t.Error("descends must not be consulted")
		return false
	}

	tests := []struct {
		name    string
		op      attest.Op
		v       attest.Verdicts
		verr    error
		descend func(string) bool
		want    outcome
		mention []string
	}{
		{"unreadable", attest.OpAttest, attest.Verdicts{}, errors.New("no verdict file"), never, pending, []string{"no verdict file"}},
		{"attest reviewed", attest.OpAttest, state(attest.StateReviewed), nil, never, inEffect, nil},
		{"revoke revoked", attest.OpRevoke, state(attest.StateRevoked), nil, never, inEffect, nil},
		{"attest superseded", attest.OpAttest, superseded, nil, never, notInEffect, []string{"highest sequence 10", "same name"}},
		{"revoke superseded", attest.OpRevoke, superseded, nil, never, notInEffect, []string{"highest sequence 10", "retry the revoke"}},
		{"attest lapsed", attest.OpAttest, state(attest.StateLapsed), nil, never, notInEffect, []string{"re-attest"}},
		{"revoke reviewed", attest.OpRevoke, state(attest.StateReviewed), nil, never, notInEffect, []string{"retry"}},
		{"attest revoked", attest.OpAttest, state(attest.StateRevoked), nil, never, notInEffect, []string{"retry"}},
		{
			"no line, ok, descends", attest.OpAttest, verdicts("ok", nil, nil), nil,
			func(got string) bool { return got == tip }, notInEffect, []string{"not in effect"},
		},
		{
			"no line, ok, does not descend", attest.OpAttest, verdicts("ok", nil, nil), nil,
			func(string) bool { return false }, pending, []string{"does not include the entry yet"},
		},
		{"no line, behind", attest.OpAttest, verdicts("behind", nil, nil), nil, never, pending, []string{"status behind"}},
		{
			"line for another digest, behind", attest.OpAttest,
			verdicts("behind", map[string]attest.State{"x": attest.StateReviewed}, nil), nil, never, pending, []string{"status behind"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.op, digest, tt.v, tt.verr, tt.descend)
			if got.outcome != tt.want {
				t.Fatalf("outcome = %v, want %v (text %q)", got.outcome, tt.want, got.text)
			}
			for _, m := range tt.mention {
				if !strings.Contains(got.text, m) {
					t.Errorf("text %q does not contain %q", got.text, m)
				}
			}
		})
	}
}

func TestWait(t *testing.T) {
	t.Run("returns the first decisive read", func(t *testing.T) {
		calls := 0
		got := wait(context.Background(), time.Second, time.Millisecond, func() report {
			calls++
			if calls < 3 {
				return report{pending, "not yet"}
			}
			return report{inEffect, ""}
		})
		if got.outcome != inEffect || calls != 3 {
			t.Fatalf("got %+v after %d calls, want inEffect after 3", got, calls)
		}
	})
	t.Run("never decisive returns pending at the deadline", func(t *testing.T) {
		got := wait(context.Background(), 20*time.Millisecond, time.Millisecond, func() report {
			return report{pending, "still waiting"}
		})
		if got.outcome != pending || got.text != "still waiting" {
			t.Fatalf("got %+v, want the last pending report", got)
		}
	})
}
