package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

// monoStore is a store with one fact "a" attested at sequence 1 and revoked at
// sequence 2, each in its own commit and pushed.
type monoStore struct {
	fact           []byte
	attest, revoke []byte
	c1, c2         string
}

func monoRevoked(f *fixture) monoStore {
	f.t.Helper()
	s := monoStore{fact: []byte("fact a\n")}
	s.attest = f.entry(f.A, "a", "facts/a.md", s.fact, 1, "attest")
	s.revoke = f.entry(f.A, "a", "facts/a.md", s.fact, 2, "revoke")
	s.c1 = f.commit(map[string][]byte{"facts/a.md": s.fact, ".attest/a": s.attest})
	s.c2 = f.commit(map[string][]byte{".attest/a": s.revoke})
	f.push()
	return s
}

// monoWant asserts the verdict file's state for one entry digest, and its
// superseded sequence when the state is superseded.
func monoWant(t *testing.T, f *fixture, ent []byte, state attest.State, seq uint64) {
	t.Helper()
	v := f.verdicts()
	d := digest(ent)
	got, ok := v.States[d]
	if !ok || got != state {
		t.Fatalf("state of %s = %q (present %v), want %s; states %v", d[:7], got, ok, state, v.States)
	}
	if state == attest.StateSuperseded && v.Superseded[d] != seq {
		t.Fatalf("superseded sequence of %s = %d, want %d", d[:7], v.Superseded[d], seq)
	}
}

func monoOK(t *testing.T, f *fixture, j *Job) {
	t.Helper()
	if res := f.run(j); res.Status != StatusOK || !res.Wrote {
		t.Fatalf("run: %+v, want ok and written", res)
	}
}

func TestMonotonicForcePushDropsRevoke(t *testing.T) {
	f := newFixture(t)
	s := monoRevoked(f)
	j := f.job()

	monoOK(t, f, j)
	monoWant(t, f, s.revoke, attest.StateRevoked, 0)

	f.forcePush(s.c1)
	monoOK(t, f, j)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)
	monoWant(t, f, s.revoke, attest.StateRevoked, 0)
}

func TestMonotonicDefaultBranchSwitch(t *testing.T) {
	f := newFixture(t)
	s := monoRevoked(f)
	gitIn(t, f.work, "", "push", "-q", f.remote, s.c1+":refs/heads/other")
	j := f.job()

	monoOK(t, f, j)
	monoWant(t, f, s.revoke, attest.StateRevoked, 0)

	f.setHead("other")
	monoOK(t, f, j)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)
	monoWant(t, f, s.revoke, attest.StateRevoked, 0)
}

func TestMonotonicForcePushDropsHigherEntry(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	ent1 := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	ent2 := f.entry(f.A, "a", "facts/a.md", fact, 2, "attest")
	c1 := f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": ent1})
	f.commit(map[string][]byte{".attest/a": ent2})
	f.push()
	j := f.job()

	monoOK(t, f, j)
	monoWant(t, f, ent2, attest.StateReviewed, 0)

	f.forcePush(c1)
	monoOK(t, f, j)
	monoWant(t, f, ent1, attest.StateSuperseded, 2)
	if st, ok := f.verdicts().States[digest(ent2)]; ok {
		t.Fatalf("dropped entry 2 has a line %q, want none", st)
	}
}

func TestMonotonicForcePushDropsLapsingCommit(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	ent := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	c1 := f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": ent})
	f.commit(map[string][]byte{"facts/a.md": []byte("edited\n")})
	f.push()
	j := f.job()

	monoOK(t, f, j)
	monoWant(t, f, ent, attest.StateLapsed, 0)

	f.forcePush(c1)
	monoOK(t, f, j)
	monoWant(t, f, ent, attest.StateLapsed, 0)
}

func TestMonotonicMirrorResetKeepsRecord(t *testing.T) {
	f := newFixture(t)
	s := monoRevoked(f)
	j := f.job()

	monoOK(t, f, j)
	f.forcePush(s.c1)
	if err := os.RemoveAll(filepath.Join(f.state, "private", "mirror.git")); err != nil {
		t.Fatal(err)
	}
	monoOK(t, f, j)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)
}

func TestMonotonicRecordHighestRevokeKeepsLine(t *testing.T) {
	f := newFixture(t)
	s := monoRevoked(f)
	j := f.job()

	monoOK(t, f, j)
	f.forcePush(s.c1)
	other := []byte("fact b\n")
	otherEnt := f.entry(f.A, "b", "facts/b.md", other, 1, "attest")
	f.commit(map[string][]byte{"facts/b.md": other, ".attest/b": otherEnt})
	f.push()
	if err := os.RemoveAll(filepath.Join(f.state, "private", "mirror.git")); err != nil {
		t.Fatal(err)
	}

	monoOK(t, f, j)
	monoWant(t, f, s.revoke, attest.StateRevoked, 0)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)
	monoWant(t, f, otherEnt, attest.StateReviewed, 0)
}

func TestMonotonicForcePushToAncestorWhileBehind(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	ent := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	revoke := f.entry(f.A, "a", "facts/a.md", fact, 2, "revoke")
	f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": ent})
	c2 := f.commit(nil)
	for range 4 {
		f.commit(nil)
	}
	f.commit(map[string][]byte{".attest/a": revoke})
	f.push()
	j := f.job()
	j.Budget.Commits = 2

	if res := f.run(j); res.Status != StatusBehind {
		t.Fatalf("run 1: %+v, want behind", res)
	}

	f.forcePush(c2)
	if res := f.run(j); res.Status != StatusBehind || !res.Wrote {
		t.Fatalf("run 2: %+v, want behind and written", res)
	}
	v := f.verdicts()
	if v.Status != string(StatusBehind) || v.Tip != c2 || len(v.States) != 0 || len(v.Superseded) != 0 {
		t.Fatalf("run 2 verdicts %+v, want behind at %s with no lines", v, c2)
	}

	j.Budget.Commits = DefaultBudget.Commits
	monoOK(t, f, j)
	monoWant(t, f, ent, attest.StateSuperseded, 2)
	monoWant(t, f, revoke, attest.StateRevoked, 0)
}

func TestMonotonicStatePermissions(t *testing.T) {
	f := newFixture(t)
	monoRevoked(f)
	monoOK(t, f, f.job())

	private := filepath.Join(f.state, "private")
	for path, want := range map[string]os.FileMode{private: 0o700, filepath.Join(private, "record.json"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, fi.Mode().Perm(), want)
		}
	}
}

func TestMonotonicFreshStateDirTakesCurrentHistory(t *testing.T) {
	f := newFixture(t)
	s := monoRevoked(f)
	j := f.job()
	monoOK(t, f, j)
	f.forcePush(s.c1)
	monoOK(t, f, j)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)

	fresh := *f
	fresh.root = t.TempDir()
	fresh.state = filepath.Join(fresh.root, "var", f.id)
	if err := os.MkdirAll(fresh.state, 0o755); err != nil {
		t.Fatal(err)
	}
	monoOK(t, &fresh, fresh.job())
	monoWant(t, &fresh, s.attest, attest.StateReviewed, 0)
	monoWant(t, f, s.attest, attest.StateSuperseded, 2)
}
