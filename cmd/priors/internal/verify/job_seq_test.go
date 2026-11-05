package verify

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

const seqName = "h"

func seqFact(name string) []byte { return []byte("fact " + name + "\n") }

func seqPath(name string) string { return "facts/" + name + ".md" }

func seqEnt(f *fixture, key attesttest.Key, name string, seq uint64, op string) []byte {
	return f.entry(key, name, seqPath(name), seqFact(name), seq, op)
}

func seqJunk(f *fixture, name string, seq uint64) []byte {
	return f.junk(name, seqPath(name), seqFact(name), seq)
}

func seqFiles(name string, ent []byte) map[string][]byte {
	return map[string][]byte{seqPath(name): seqFact(name), ".attest/" + name: ent}
}

// seqPut commits ent for name on the current branch and returns the commit.
func seqPut(f *fixture, name string, ent []byte) string {
	return f.commit(seqFiles(name, ent))
}

// seqPushMain pushes the work clone's HEAD to the remote's main, whatever
// branch the clone is on.
func seqPushMain(f *fixture) {
	f.t.Helper()
	gitIn(f.t, f.work, "", "push", "-q", f.remote, "HEAD:refs/heads/main")
}

func seqGit(f *fixture, args ...string) string {
	f.t.Helper()
	return gitIn(f.t, f.work, "", args...)
}

// seqRun runs j and requires an ok verdict file.
func seqRun(f *fixture, j *Job) {
	f.t.Helper()
	if res := f.run(j); res.Status != StatusOK || !res.Wrote {
		f.t.Fatalf("run: %+v, want ok and written", res)
	}
}

// seqWant asserts ent's line: want "" means no line, and a superseded line
// must carry high.
func seqWant(f *fixture, ent []byte, want attest.State, high uint64) {
	f.t.Helper()
	v := f.verdicts()
	d := digest(ent)
	got, ok := v.States[d]
	if want == "" {
		if ok {
			f.t.Errorf("%s: line %q, want none", d[:8], got)
		}
		return
	}
	if !ok || got != want {
		f.t.Errorf("%s: state %q (line %v), want %q", d[:8], got, ok, want)
		return
	}
	if want == attest.StateSuperseded && v.Superseded[d] != high {
		f.t.Errorf("%s: superseded %d, want %d", d[:8], v.Superseded[d], high)
	}
}

func TestSeqForwardReplayAfterRevoke(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	r2 := seqEnt(f, f.A, seqName, 2, "revoke")
	seqPut(f, seqName, e1)
	seqPut(f, seqName, r2)
	f.push()
	j := f.job()
	seqRun(f, j)
	seqWant(f, r2, attest.StateRevoked, 0)

	seqPut(f, seqName, e1)
	f.push()
	seqRun(f, j)
	seqWant(f, e1, attest.StateSuperseded, 2)
	seqWant(f, r2, attest.StateRevoked, 0)
}

func TestSeqForwardReplayAfterReattest(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	e2 := seqEnt(f, f.A, seqName, 2, "attest")
	seqPut(f, seqName, e1)
	seqPut(f, seqName, e2)
	seqPut(f, seqName, e1)
	f.push()
	seqRun(f, f.job())
	seqWant(f, e1, attest.StateSuperseded, 2)
	seqWant(f, e2, "", 0)
}

func TestSeqMergeReplay(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	r2 := seqEnt(f, f.A, seqName, 2, "revoke")
	seqPut(f, seqName, e1)
	f.push()
	seqGit(f, "checkout", "-q", "-b", "side")
	f.commit(map[string][]byte{"side.txt": []byte("side\n")})
	seqGit(f, "checkout", "-q", "main")
	seqPut(f, seqName, r2)
	f.push()

	// The side branch still holds entry 1; -s ours merges main in and keeps it.
	seqGit(f, "checkout", "-q", "side")
	seqGit(f, "merge", "-q", "-s", "ours", "-m", "merge", "main")
	seqPushMain(f)
	seqRun(f, f.job())
	seqWant(f, e1, attest.StateSuperseded, 2)
	seqWant(f, r2, attest.StateRevoked, 0)
}

func TestSeqTie(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string][]byte{seqPath(seqName): seqFact(seqName)})
	f.push()
	tieA := seqEnt(f, f.A, seqName, 3, "attest")
	tieB := seqEnt(f, f.B, seqName, 3, "attest")
	seqGit(f, "checkout", "-q", "-b", "side", base)
	seqPut(f, seqName, tieB)
	seqGit(f, "checkout", "-q", "main")
	seqPut(f, seqName, tieA)
	seqGit(f, "merge", "-q", "-X", "ours", "-m", "merge", "side")
	f.push()
	seqRun(f, f.job())
	seqWant(f, tieA, attest.StateSuperseded, 3)
	seqWant(f, tieB, attest.StateSuperseded, 3)
}

func TestSeqUnobservedEntry(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	e2 := seqEnt(f, f.A, seqName, 2, "attest")
	seqPut(f, seqName, e1)
	f.push()
	seqPut(f, seqName, e2)
	seqRun(f, f.job())
	seqWant(f, e1, attest.StateReviewed, 0)
	seqWant(f, e2, "", 0)
}

func TestSeqOutOfRangeSequenceIgnored(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	var bad [][]byte
	for _, seq := range []uint64{0, 1 << 63} {
		bad = append(bad, seqEnt(f, f.A, seqName, seq, "attest"))
	}
	seqPut(f, seqName, e1)
	for _, b := range bad {
		seqPut(f, seqName, b)
	}
	seqPut(f, seqName, e1)
	f.push()
	seqRun(f, f.job())
	seqWant(f, e1, attest.StateReviewed, 0)
	for _, b := range bad {
		seqWant(f, b, "", 0)
	}
}

func TestSeqJunkEntryAtHighSequence(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	junk := seqJunk(f, seqName, 50)
	seqPut(f, seqName, e1)
	f.push()
	j := f.job()
	seqRun(f, j)
	seqWant(f, e1, attest.StateReviewed, 0)

	seqPut(f, seqName, junk)
	f.push()
	seqRun(f, j)
	seqWant(f, e1, attest.StateSuperseded, 50)
	seqWant(f, junk, "", 0)
}

func TestSeqRevokeLatestThenReattest(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	r2 := seqEnt(f, f.A, seqName, 2, "revoke")
	e3 := seqEnt(f, f.A, seqName, 3, "attest")
	seqPut(f, seqName, e1)
	seqPut(f, seqName, r2)
	f.push()
	j := f.job()
	seqRun(f, j)
	seqWant(f, r2, attest.StateRevoked, 0)
	seqWant(f, e1, "", 0)

	seqPut(f, seqName, e3)
	f.push()
	seqRun(f, j)
	seqWant(f, e3, attest.StateReviewed, 0)
	seqWant(f, r2, "", 0)
	seqWant(f, e1, "", 0)
}

func TestSeqKeyRemoval(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	r2 := seqEnt(f, f.B, seqName, 2, "revoke")
	seqPut(f, seqName, e1)
	seqPut(f, seqName, r2)
	f.push()
	seqRun(f, f.job())
	seqWant(f, r2, attest.StateRevoked, 0)

	seqRun(f, f.job(f.A))
	seqWant(f, r2, "", 0)
	seqWant(f, e1, attest.StateSuperseded, 2)
}

// TestSeqRewriteBelowObservedAttest walks the H@10 scenario of §7: H attested
// at 10 and observed, then the remote rewritten under it.
func TestSeqRewriteBelowObservedAttest(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string][]byte{seqPath(seqName): seqFact(seqName)})
	h := seqEnt(f, f.A, seqName, 10, "attest")
	junk3 := seqJunk(f, seqName, 3)
	r4 := seqEnt(f, f.A, seqName, 4, "revoke")
	r11 := seqEnt(f, f.A, seqName, 11, "revoke")
	seqPut(f, seqName, h)
	f.push()
	j := f.job()
	seqRun(f, j)
	seqWant(f, h, attest.StateReviewed, 0)

	f.forcePush(base)
	seqPut(f, seqName, junk3)
	f.push()
	seqRun(f, j)
	seqWant(f, h, "", 0)
	seqWant(f, junk3, "", 0)

	seqPut(f, seqName, r4)
	f.push()
	seqRun(f, j)
	seqWant(f, r4, attest.StateSuperseded, 10)
	seqWant(f, h, "", 0)

	seqPut(f, seqName, r11)
	f.push()
	seqRun(f, j)
	seqWant(f, r11, attest.StateRevoked, 0)

	seqPut(f, seqName, h)
	f.push()
	seqRun(f, j)
	seqWant(f, h, attest.StateSuperseded, 11)
	seqWant(f, r11, attest.StateRevoked, 0)
	seqWant(f, r4, attest.StateSuperseded, 11)
}

func TestSeqRevokeUnderJunk(t *testing.T) {
	f := newFixture(t)
	e1 := seqEnt(f, f.A, seqName, 1, "attest")
	r12 := seqEnt(f, f.A, seqName, 12, "revoke")
	junk5 := seqJunk(f, seqName, 5)
	junk6 := seqJunk(f, seqName, 6)
	seqPut(f, seqName, e1)
	seqPut(f, seqName, r12)
	f.push()
	j := f.job()
	seqRun(f, j)

	seqPut(f, seqName, junk5)
	f.push()
	seqRun(f, j)
	seqWant(f, r12, attest.StateRevoked, 0)
	seqWant(f, e1, attest.StateSuperseded, 12)
	seqWant(f, junk5, "", 0)

	seqPut(f, seqName, junk6)
	f.push()
	seqRun(f, j)
	seqWant(f, r12, attest.StateRevoked, 0)
	seqWant(f, e1, attest.StateSuperseded, 12)
	seqWant(f, junk6, "", 0)
}

func TestSeqRevokeSupersededAcrossMergeSides(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string][]byte{seqPath(seqName): seqFact(seqName)})
	f.push()
	r2 := seqEnt(f, f.A, seqName, 2, "revoke")
	e3 := seqEnt(f, f.A, seqName, 3, "attest")
	junk1 := seqJunk(f, seqName, 1)

	seqGit(f, "checkout", "-q", "-b", "side")
	seqPut(f, seqName, r2)
	seqPushMain(f)
	j := f.job()
	seqRun(f, j)
	seqWant(f, r2, attest.StateRevoked, 0)

	seqGit(f, "checkout", "-q", "-b", "other", base)
	seqPut(f, seqName, e3)
	seqGit(f, "merge", "-q", "-X", "ours", "-m", "merge", "side")
	seqPut(f, seqName, junk1)
	seqPushMain(f)
	seqRun(f, j)

	r, _, err := loadRecord(filepath.Join(f.state, "private", "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Superseded[digest(r2)]; !ok {
		t.Errorf("record Superseded %v lacks the revoke", r.Superseded)
	}
	seqWant(f, r2, attest.StateSuperseded, 3)
	seqWant(f, e3, "", 0)
	seqWant(f, junk1, "", 0)
}

func TestSeqRecordDropRetry(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string][]byte{seqPath(seqName): seqFact(seqName)})
	e10 := seqEnt(f, f.A, seqName, 10, "attest")
	e5 := seqEnt(f, f.A, seqName, 5, "attest")
	e11 := seqEnt(f, f.A, seqName, 11, "attest")
	seqPut(f, seqName, e10)
	f.push()
	j := f.job()
	seqRun(f, j)

	f.forcePush(base)
	seqPut(f, seqName, e5)
	f.push()
	seqRun(f, j)
	seqWant(f, e5, attest.StateSuperseded, 10)

	seqPut(f, seqName, e11)
	f.push()
	seqRun(f, j)
	seqWant(f, e11, attest.StateReviewed, 0)
}

func TestSeqEvidenceRejectedKeyHasNoLine(t *testing.T) {
	f := newFixture(t)
	old := CheckEvidence
	t.Cleanup(func() { CheckEvidence = old })
	CheckEvidence = func(k config.AttestKey) error {
		if string(k.Key.Marshal()) == string(f.A.PublicKey().Marshal()) {
			return errors.New("rejected")
		}
		return nil
	}
	byA := seqEnt(f, f.A, "a", 1, "attest")
	byB := seqEnt(f, f.B, "b", 1, "attest")
	f.commit(map[string][]byte{
		seqPath("a"): seqFact("a"), ".attest/a": byA,
		seqPath("b"): seqFact("b"), ".attest/b": byB,
	})
	f.push()
	j := f.job()
	if len(j.Keys) != 1 {
		t.Fatalf("Keys = %d, want only B", len(j.Keys))
	}
	seqRun(f, j)
	seqWant(f, byA, "", 0)
	seqWant(f, byB, attest.StateReviewed, 0)
}
