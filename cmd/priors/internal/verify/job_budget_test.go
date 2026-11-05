package verify

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

func TestRunFirstFetchOK(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	ent := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": ent})
	f.push()
	tip := gitIn(t, f.remote, "", "rev-parse", "main")
	j := f.job()

	for run := 1; run <= 2; run++ {
		res := f.run(j)
		if res.Status != StatusOK || !res.Wrote {
			t.Fatalf("run %d: %+v, want ok and written", run, res)
		}
		v := f.verdicts()
		if v.Store != "personal" || v.Tip != tip || v.Status != string(StatusOK) || v.Trust != j.TrustDigest {
			t.Fatalf("run %d: header %+v", run, v)
		}
		if len(v.States) != 1 || v.States[digest(ent)] != attest.StateReviewed {
			t.Fatalf("run %d: states %v, want %s reviewed", run, v.States, digest(ent))
		}
	}

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
	if log := f.sshLog(); !strings.Contains(log, "git-upload-pack") {
		t.Errorf("ssh.log = %q, want a git-upload-pack invocation", log)
	}
}

// budSeries is n commits, each attesting one more fact (names a, b, c, ...).
type budSeries struct {
	names []string
	ents  [][]byte
	oids  []string
}

// budPush commits and pushes n attested facts, one per commit.
func budPush(f *fixture, n int) budSeries {
	f.t.Helper()
	var s budSeries
	for i := range n {
		name := string(rune('a' + i))
		fact := []byte("fact " + name + "\n")
		ent := f.entry(f.A, name, "facts/"+name+".md", fact, 1, "attest")
		oid := f.commit(map[string][]byte{"facts/" + name + ".md": fact, ".attest/" + name: ent})
		s.names, s.ents, s.oids = append(s.names, name), append(s.ents, ent), append(s.oids, oid)
	}
	f.push()
	return s
}

func (f *fixture) budMirror() string { return filepath.Join(f.state, "private", "mirror.git") }

// budWalked lists the commits behind the mirror's walked refs.
func budWalked(f *fixture) []string {
	f.t.Helper()
	out := gitIn(f.t, f.budMirror(), "", "for-each-ref", "--format=%(refname:strip=3)", walkedRef)
	got := strings.Fields(out)
	slices.Sort(got)
	return got
}

// budRun runs the job and requires the status and a written verdict file of
// that status.
func budRun(f *fixture, j *Job, want Status) attest.Verdicts {
	f.t.Helper()
	res := f.run(j)
	if res.Status != want || !res.Wrote {
		f.t.Fatalf("run: %+v, want %s and written", res, want)
	}
	v := f.verdicts()
	if v.Status != string(want) {
		f.t.Fatalf("header status %q, want %s", v.Status, want)
	}
	return v
}

func budWant(t *testing.T, v attest.Verdicts, ent []byte, want attest.State) {
	t.Helper()
	if got := v.States[digest(ent)]; got != want {
		t.Fatalf("state of %s = %q, want %q (states %v)", digest(ent), got, want, v.States)
	}
}

func budNoVerdicts(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.state, "verdicts")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("verdict file: stat err %v, want not exist", err)
	}
}

func TestBudgetWalkLimitCatchesUp(t *testing.T) {
	f := newFixture(t)
	s := budPush(f, 10)
	j := f.job()
	j.Budget.Commits = 3

	for run := 1; run <= 3; run++ {
		v := budRun(f, j, StatusBehind)
		if len(v.States) != 0 {
			t.Fatalf("run %d: states %v, want header only", run, v.States)
		}
	}
	v := budRun(f, j, StatusOK)
	if len(v.States) != 10 {
		t.Fatalf("states %v, want 10", v.States)
	}
	for _, ent := range s.ents {
		budWant(t, v, ent, attest.StateReviewed)
	}
}

// The soft deadline cuts the walk after the third commit whatever the fetch
// took, because the hook holds the walk until it fires.
func TestBudgetDeadlineCutResumes(t *testing.T) {
	f := newFixture(t)
	s := budPush(f, 5)
	j := f.job()
	j.Budget.Wall, j.Budget.Reserve = 6*time.Second, 2*time.Second

	t.Cleanup(func() { afterCommit = nil })
	afterCommit = func(soft context.Context, n int) {
		if n == 3 {
			<-soft.Done()
		}
	}
	v := budRun(f, j, StatusBehind)
	if len(v.States) != 0 {
		t.Fatalf("states %v, want header only", v.States)
	}
	if got := budWalked(f); !slices.Equal(got, []string{s.oids[2]}) {
		t.Fatalf("walked refs %v, want [%s]", got, s.oids[2])
	}
	r, exists, err := loadRecord(filepath.Join(f.state, "private", "record.json"))
	if err != nil || !exists {
		t.Fatalf("record: exists %v, err %v", exists, err)
	}
	if len(r.Entries) != 3 {
		t.Fatalf("record entries %d, want 3", len(r.Entries))
	}
	for i, ent := range s.ents {
		if _, ok := r.Entries[digest(ent)]; ok != (i < 3) {
			t.Fatalf("record holds entry %d: %v", i, ok)
		}
	}

	var last int
	afterCommit = func(_ context.Context, n int) { last = n }
	v = budRun(f, j, StatusOK)
	if last != 2 {
		t.Fatalf("second run walked %d commits, want the remaining 2", last)
	}
	if got := budWalked(f); !slices.Equal(got, []string{s.oids[4]}) {
		t.Fatalf("walked refs %v, want [%s]", got, s.oids[4])
	}
	for _, ent := range s.ents {
		budWant(t, v, ent, attest.StateReviewed)
	}
}

func TestBudgetOversizeSkipsFetch(t *testing.T) {
	f := newFixture(t)
	ent := budPush(f, 1).ents[0]
	j := f.job()
	v := budRun(f, j, StatusOK)
	budWant(t, v, ent, attest.StateReviewed)
	log := f.sshLog()

	j.Budget.MirrorBytes = 1
	v = budRun(f, j, StatusOversize)
	if len(v.States) != 0 {
		t.Fatalf("states %v, want header only", v.States)
	}
	if got := f.sshLog(); got != log {
		t.Fatalf("ssh.log gained %q, want no new invocation", strings.TrimPrefix(got, log))
	}
}

// A reset mirror is smaller than the cap again, so one run refetches before
// the cap bites.
func TestBudgetOversizeAfterReset(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	j := f.job()
	budRun(f, j, StatusOK)

	fresh := filepath.Join(t.TempDir(), "fresh.git")
	gitIn(t, f.root, "", "init", "-q", "--bare", "--template=", fresh)
	empty, err := dirSize(fresh)
	if err != nil {
		t.Fatal(err)
	}
	full, err := dirSize(f.budMirror())
	if err != nil {
		t.Fatal(err)
	}
	if full <= empty+1 {
		t.Fatalf("fetched mirror is %d bytes, empty one %d", full, empty)
	}
	j.Budget.MirrorBytes = empty + 1

	if err := os.RemoveAll(f.budMirror()); err != nil {
		t.Fatal(err)
	}
	budRun(f, j, StatusOK)
	budRun(f, j, StatusOversize)
}

func TestBudgetTmpFilesRemoved(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	j := f.job()
	budRun(f, j, StatusOK)

	m := f.budMirror()
	leftovers := []string{
		filepath.Join(m, "objects", "pack", "tmp_pack_x"),
		filepath.Join(m, "objects", "pack", "tmp_idx_x"),
		filepath.Join(m, "objects", "incoming-x", "y"),
		filepath.Join(m, "objects", "ab", "tmp_obj_x"),
	}
	for _, p := range leftovers {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f.write(p, "partial", 0o644)
	}
	budRun(f, j, StatusOK)
	for _, p := range append(leftovers, filepath.Join(m, "objects", "incoming-x")) {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: lstat err %v, want not exist", p, err)
		}
	}
}

func TestBudgetStallEndsBehind(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	j := f.job()
	budRun(f, j, StatusOK)

	j.Budget.Wall, j.Budget.Reserve = 3*time.Second, time.Second
	f.sshMode("stall")
	budRun(f, j, StatusBehind)
}

func TestBudgetPreAuthFailureNoBacklogIsStale(t *testing.T) {
	for _, mode := range []string{"hostkey", "refused"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ent := budPush(f, 1).ents[0]
			j := f.job()
			j.Now = func() time.Time { return time.Unix(1000, 0) }
			first := budRun(f, j, StatusOK)
			budWant(t, first, ent, attest.StateReviewed)

			f.sshMode(mode)
			same := f.job()
			same.Now = func() time.Time { return time.Unix(2000, 0) }
			v := budRun(f, same, StatusStaleRemote)
			budWant(t, v, ent, attest.StateReviewed)

			// The allowlist is read fresh each run, so the line follows it.
			onlyB := f.job(f.B)
			onlyB.Now = same.Now
			v = budRun(f, onlyB, StatusStaleRemote)
			if len(v.States) != 0 {
				t.Fatalf("states %v, want none once the signer is off the allowlist", v.States)
			}
			if v.Fetched.Unix() != 1000 || v.Tip != first.Tip {
				t.Fatalf("fetched %d tip %s, want 1000 and %s", v.Fetched.Unix(), v.Tip, first.Tip)
			}
		})
	}
}

func TestBudgetPreAuthFailureWithBacklogIsBehind(t *testing.T) {
	f := newFixture(t)
	budPush(f, 10)
	j := f.job()
	j.Budget.Commits = 3
	budRun(f, j, StatusBehind)

	f.sshMode("hostkey")
	v := budRun(f, j, StatusBehind)
	if len(v.States) != 0 {
		t.Fatalf("states %v, want header only", v.States)
	}
}

func TestBudgetPreAuthFailureAfterResetIsBehind(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	j := f.job()
	budRun(f, j, StatusOK)

	if err := os.RemoveAll(f.budMirror()); err != nil {
		t.Fatal(err)
	}
	f.sshMode("hostkey")
	v := budRun(f, j, StatusBehind)
	if len(v.States) != 0 {
		t.Fatalf("states %v, want header only", v.States)
	}
}

func TestBudgetVerdictFileHoldsNoFactNameOrPath(t *testing.T) {
	f := newFixture(t)
	fact := []byte("zebra fact body\n")
	ent := f.entry(f.A, "zebra", "facts/zebra-notes.md", fact, 1, "attest")
	f.commit(map[string][]byte{"facts/zebra-notes.md": fact, ".attest/zebra": ent})
	f.push()
	v := budRun(f, f.job(), StatusOK)
	budWant(t, v, ent, attest.StateReviewed)

	b, err := os.ReadFile(filepath.Join(f.state, "verdicts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"zebra", "zebra-notes", "facts/", "fact body"} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("verdict file contains %q:\n%s", s, b)
		}
	}
}

func TestBudgetJunkNameCopyAddsNoLine(t *testing.T) {
	f := newFixture(t)
	ent := f.entry(f.A, "a", "facts/a.md", []byte("fact a\n"), 1, "attest")
	f.commit(map[string][]byte{
		"facts/a.md": []byte("fact a\n"),
		".attest/a":  ent, ".attest/b": ent, ".attest/UPPER": ent, ".attest/a.copy": ent,
	})
	f.push()
	v := budRun(f, f.job(), StatusOK)
	budWant(t, v, ent, attest.StateReviewed)
	if len(v.States) != 1 {
		t.Fatalf("states %v, want only the entry under its own name", v.States)
	}
}

func TestBudgetSupersededLines(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	e1 := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	e2 := f.entry(f.A, "a", "facts/a.md", fact, 2, "attest")
	f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": e1})
	f.commit(map[string][]byte{".attest/a": e2})
	f.push()

	j := f.job()
	v := budRun(f, j, StatusOK)
	budWant(t, v, e2, attest.StateReviewed)
	if len(v.States) != 1 {
		t.Fatalf("states %v, want no line for the superseded entry once the tip wins", v.States)
	}

	junk := f.junk("a", "facts/a.md", fact, 3)
	f.commit(map[string][]byte{".attest/a": junk})
	f.push()
	v = budRun(f, j, StatusOK)
	budWant(t, v, e1, attest.StateSuperseded)
	budWant(t, v, e2, attest.StateSuperseded)
	if len(v.States) != 2 || v.Superseded[digest(e1)] != 3 || v.Superseded[digest(e2)] != 3 {
		t.Fatalf("states %v superseded %v, want e1 and e2 superseded at 3 and no line for the junk", v.States, v.Superseded)
	}

	v = budRun(f, f.job(f.B), StatusOK)
	if len(v.States) != 0 {
		t.Fatalf("states %v, want superseded lines dropped with their signer's key", v.States)
	}
}

func TestBudgetReattestAndRevokeLeaveNoNonTipLine(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	e1 := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	e2 := f.entry(f.A, "a", "facts/a.md", fact, 2, "revoke")
	e3 := f.entry(f.A, "a", "facts/a.md", fact, 3, "attest")
	f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": e1})
	f.commit(map[string][]byte{".attest/a": e2})
	f.commit(map[string][]byte{".attest/a": e3})
	f.push()

	v := budRun(f, f.job(), StatusOK)
	budWant(t, v, e3, attest.StateReviewed)
	if len(v.States) != 1 {
		t.Fatalf("states %v, want only the tip entry", v.States)
	}
}

func TestBudgetNoFetchEverWritesNothing(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	f.sshMode("hostkey")
	res := f.run(f.job())
	if res.Status != StatusBehind || res.Wrote {
		t.Fatalf("run: %+v, want behind and not written", res)
	}
	budNoVerdicts(t, f)
}

func TestBudgetNoTipRemovesStaleVerdicts(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	f.write(filepath.Join(f.state, "verdicts"), "stale\n", 0o644)
	f.sshMode("hostkey")
	res := f.run(f.job())
	if res.Status != StatusBehind || res.Wrote {
		t.Fatalf("run: %+v, want behind and not written", res)
	}
	budNoVerdicts(t, f)
}

func TestBudgetCorruptRecordRemovesVerdicts(t *testing.T) {
	f := newFixture(t)
	budPush(f, 1)
	j := f.job()
	budRun(f, j, StatusOK)

	rec := filepath.Join(f.state, "private", "record.json")
	f.write(rec, "{not json", 0o600)
	res := f.run(j)
	if res.Status != StatusBehind || res.Wrote {
		t.Fatalf("run: %+v, want behind and not written", res)
	}
	budNoVerdicts(t, f)
	if b, err := os.ReadFile(rec); err != nil || string(b) != "{not json" {
		t.Fatalf("record %q, err %v, want it untouched", b, err)
	}
}
