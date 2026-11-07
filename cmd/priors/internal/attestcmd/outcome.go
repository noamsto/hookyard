package attestcmd

import (
	"context"
	"fmt"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

type outcome int

const (
	inEffect outcome = iota
	notInEffect
	pending
	notPushed
)

type report struct {
	outcome outcome
	text    string
}

// classify maps the verdict read for an entry to an outcome. descends is
// consulted only when the job reports ok but has no line for the entry.
func classify(op attest.Op, digest string, v attest.Verdicts, verr error, descends func(tip string) bool) report {
	if verr != nil {
		return report{pending, verr.Error()}
	}
	retry := "re-run the same command"
	if op == attest.OpRevoke {
		retry = "retry the revoke"
	}
	switch state, ok := v.States[digest]; {
	case !ok:
	case state == attest.StateReviewed && op == attest.OpAttest, state == attest.StateRevoked && op == attest.OpRevoke:
		return report{inEffect, ""}
	case state == attest.StateSuperseded:
		text := fmt.Sprintf("superseded (highest sequence %d): ", v.Superseded[digest])
		if op == attest.OpRevoke {
			return report{notInEffect, text + retry}
		}
		return report{notInEffect, text + retry + " under the same name; an attest still superseded after that retry is re-attested under a new name"}
	case state == attest.StateLapsed:
		return report{notInEffect, "lapsed: re-attest"}
	default:
		return report{notInEffect, fmt.Sprintf("state %s for this %s: retry", state, op)}
	}
	if v.Status != "ok" {
		return report{pending, "status " + v.Status}
	}
	if descends(v.Tip) {
		return report{notInEffect, "observed but not in effect: the job walked the entry but an entry pushed on top hides it; re-run the same command"}
	}
	return report{pending, "the verdict tip does not include the entry yet"}
}

// wait reads immediately, then every poll, until a read is decisive or the
// deadline passes; it then returns the last pending report.
func wait(ctx context.Context, deadline, poll time.Duration, read func() report) report {
	end := time.NewTimer(deadline)
	defer end.Stop()
	for {
		r := read()
		if r.outcome != pending {
			return r
		}
		tick := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			tick.Stop()
			return r
		case <-end.C:
			tick.Stop()
			return r
		case <-tick.C:
		}
	}
}
