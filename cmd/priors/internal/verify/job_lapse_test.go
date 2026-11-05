package verify

import (
	"bytes"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
)

const lapsePath = "facts/a.md"

var lapseFact = []byte("fact a\n")

// lapseAttested commits and pushes the fact with a seq 1 attest, returning the
// entry and its commit.
func lapseAttested(f *fixture) (ent []byte, oid string) {
	f.t.Helper()
	ent = f.entry(f.A, "a", lapsePath, lapseFact, 1, "attest")
	oid = f.commit(map[string][]byte{lapsePath: lapseFact, ".attest/a": ent})
	f.push()
	return ent, oid
}

// lapseRun runs the job and requires an ok verdict.
func lapseRun(f *fixture, j *Job) attest.Verdicts {
	f.t.Helper()
	if res := f.run(j); res.Status != StatusOK || !res.Wrote {
		f.t.Fatalf("run: %+v, want ok and written", res)
	}
	return f.verdicts()
}

func lapseWant(t *testing.T, v attest.Verdicts, ent []byte, want attest.State) {
	t.Helper()
	if got := v.States[digest(ent)]; got != want {
		t.Fatalf("state of %s = %q, want %q (states %v)", digest(ent), got, want, v.States)
	}
}

// lapseBreaks are the ways a commit after the attest leaves the fact without
// the attested bytes.
var lapseBreaks = map[string]func(f *fixture){
	"archive": func(f *fixture) {
		f.commit(map[string][]byte{"_archive/a.md": lapseFact}, lapsePath)
	},
	"delete": func(f *fixture) {
		f.commit(nil, lapsePath)
	},
	"edit": func(f *fixture) {
		f.commit(map[string][]byte{lapsePath: []byte("fact a, edited\n")})
	},
}

func TestLapseRestoreStaysLapsed(t *testing.T) {
	for name, brk := range lapseBreaks {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ent, _ := lapseAttested(f)
			brk(f)
			f.commit(map[string][]byte{lapsePath: lapseFact}, lapseRestoreDel(name)...)
			f.push()

			v := lapseRun(f, f.job())
			lapseWant(t, v, ent, attest.StateLapsed)
			if len(v.States) != 1 {
				t.Fatalf("states %v, want only the lapsed entry", v.States)
			}
		})
	}
}

// lapseRestoreDel names the files the restoring commit removes: an archive
// restore moves the fact back out of _archive/.
func lapseRestoreDel(name string) []string {
	if name == "archive" {
		return []string{"_archive/a.md"}
	}
	return nil
}

func TestLapseWithoutRestore(t *testing.T) {
	for name, brk := range lapseBreaks {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ent, _ := lapseAttested(f)
			brk(f)
			f.push()

			lapseWant(t, lapseRun(f, f.job()), ent, attest.StateLapsed)
		})
	}
}

// TestLapseFactOverSizeCap: a fact over gate 4's cap is never read whole, so
// it does not hold even when its bytes match the attested hash.
func TestLapseFactOverSizeCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), gate.MaxFactBytes+1)
	t.Run("attested", func(t *testing.T) {
		f := newFixture(t)
		ent := f.entry(f.A, "a", lapsePath, big, 1, "attest")
		f.commit(map[string][]byte{lapsePath: big, ".attest/a": ent})
		f.push()
		lapseWant(t, lapseRun(f, f.job()), ent, attest.StateLapsed)
	})
	t.Run("replaced", func(t *testing.T) {
		f := newFixture(t)
		ent, _ := lapseAttested(f)
		f.commit(map[string][]byte{lapsePath: big})
		f.push()
		lapseWant(t, lapseRun(f, f.job()), ent, attest.StateLapsed)
	})
}

func TestLapseUnreachableLapsingCommit(t *testing.T) {
	f := newFixture(t)
	ent, attested := lapseAttested(f)
	j := f.job()
	lapseWant(t, lapseRun(f, j), ent, attest.StateReviewed)

	f.commit(map[string][]byte{lapsePath: []byte("fact a, edited\n")})
	f.push()
	lapseWant(t, lapseRun(f, j), ent, attest.StateLapsed)

	f.forcePush(attested)
	v := lapseRun(f, j)
	if v.Tip != attested {
		t.Fatalf("tip %s, want the attested commit %s", v.Tip, attested)
	}
	lapseWant(t, v, ent, attest.StateLapsed)
}

func TestLapseAcrossRuns(t *testing.T) {
	f := newFixture(t)
	ent, _ := lapseAttested(f)
	j := f.job()
	lapseWant(t, lapseRun(f, j), ent, attest.StateReviewed)

	f.commit(map[string][]byte{lapsePath: []byte("fact a, edited\n")})
	f.push()
	lapseWant(t, lapseRun(f, j), ent, attest.StateLapsed)

	f.commit(map[string][]byte{lapsePath: lapseFact})
	f.push()
	lapseWant(t, lapseRun(f, j), ent, attest.StateLapsed)
}

func TestLapseReattestReviews(t *testing.T) {
	for name, brk := range lapseBreaks {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ent, _ := lapseAttested(f)
			brk(f)
			f.commit(map[string][]byte{lapsePath: lapseFact}, lapseRestoreDel(name)...)
			f.push()
			j := f.job()
			lapseWant(t, lapseRun(f, j), ent, attest.StateLapsed)

			again := f.entry(f.A, "a", lapsePath, lapseFact, 2, "attest")
			f.commit(map[string][]byte{".attest/a": again})
			f.push()

			v := lapseRun(f, j)
			lapseWant(t, v, again, attest.StateReviewed)
			if _, ok := v.States[digest(ent)]; ok {
				t.Fatalf("old entry still has a line: %v", v.States)
			}
		})
	}
}
