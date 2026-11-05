package attestcmd

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const (
	textInEffect    = "\nin effect\n"
	textNotInEffect = "observed but not in effect"
	textPending     = "not yet observed"
)

// replay is a store whose fact h the owner attested at sequence 1 (the entry
// the owner's checkout keeps in its history), with the verdict job not yet
// run again.
type replay struct {
	*fixture
	reviewed []byte
	c1       string
}

func newReplay(t *testing.T) *replay {
	t.Helper()
	f := newFixture(t)
	f.jobOnTrigger = true
	if code := f.run(f.opts(), attest.OpAttest); code != 0 {
		t.Fatalf("seed attest exit = %d\n%s", code, f.all())
	}
	f.jobOnTrigger = false
	return &replay{fixture: f, reviewed: f.remoteBlob("main", "facts/h.md"), c1: f.remoteMain()}
}

// pullWork moves the agent's work clone to the remote's tip.
func (f *fixture) pullWork() {
	f.t.Helper()
	gitOut(f.t, f.work, "", "fetch", "-q", f.remote, "main")
	gitOut(f.t, f.work, "", "reset", "-q", "--hard", "FETCH_HEAD")
}

// attestH is the entry that attests the reviewed bytes at sequence 10.
func (r *replay) attestH() []byte {
	return r.signedEntry(r.A, 10, "attest", "facts/h.md", r.reviewed)
}

// pushH pushes H at sequence 10 on top of the remote and runs the job, so H is
// observed and reviewed.
func (r *replay) pushH() (digest string) {
	r.t.Helper()
	r.pullWork()
	h := r.attestH()
	r.commitPush(map[string][]byte{".attest/h": h})
	r.runJob()
	digest = hexSum(h)
	if got := r.verdicts().States[digest]; got != attest.StateReviewed {
		r.t.Fatalf("H state = %q, want reviewed", got)
	}
	return digest
}

// rewrite force-pushes the owner's first attest with junk at sequence 3 on top
// ("junk") or with its entry deleted ("none"), dropping H from the remote, and
// brings the owner's checkout up to the new tip.
func (r *replay) rewrite(kind string) {
	r.t.Helper()
	r.forcePush(r.c1)
	switch kind {
	case "junk":
		r.commitPush(map[string][]byte{".attest/h": r.junkEntry(3)})
	case "none":
		gitOut(r.t, r.work, "", "rm", "-q", ".attest/h")
		r.commitPush(nil)
	default:
		r.t.Fatalf("unknown rewrite %q", kind)
	}
	r.syncStore()
}

// restoreH pushes H's fact bytes and entry on top of the remote and runs the
// job; it returns the commit.
func (r *replay) restoreH() string {
	r.t.Helper()
	r.pullWork()
	commit := r.commitPush(map[string][]byte{"facts/h.md": r.reviewed, ".attest/h": r.attestH()})
	r.runJob()
	return commit
}

// plantTree unpacks the checkout's packs to loose objects, then overwrites the
// loose root tree of victim with the bytes of donor's root tree, keeping
// victim's tree oid. It returns a func that puts the real tree back.
func (f *fixture) plantTree(victim, donor string) (undo func()) {
	f.t.Helper()
	packDir := filepath.Join(f.root, ".git", "objects", "pack")
	held := f.t.TempDir()
	packs, err := filepath.Glob(filepath.Join(packDir, "*.pack"))
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range packs {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			src := strings.TrimSuffix(p, ".pack") + ext
			if err := os.Rename(src, filepath.Join(held, filepath.Base(src))); err != nil && !os.IsNotExist(err) {
				f.t.Fatal(err)
			}
		}
	}
	for _, p := range packs {
		raw, err := os.ReadFile(filepath.Join(held, filepath.Base(p)))
		if err != nil {
			f.t.Fatal(err)
		}
		gitOut(f.t, f.root, string(raw), "unpack-objects", "-q")
	}

	victimTree := gitOut(f.t, f.root, "", "rev-parse", victim+"^{tree}")
	donorTree := gitOut(f.t, f.root, "", "rev-parse", donor+"^{tree}")
	cmd := tools.Command(context.Background(), tools.Git, "cat-file", "tree", donorTree)
	cmd.Dir = f.root
	raw, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("cat-file tree %s: %v", donorTree, err)
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := fmt.Fprintf(zw, "tree %d\x00%s", len(raw), raw); err != nil {
		f.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		f.t.Fatal(err)
	}
	loose := filepath.Join(f.root, ".git", "objects", victimTree[:2], victimTree[2:])
	orig, err := os.ReadFile(loose)
	if err != nil {
		f.t.Fatalf("revoke tree is not loose: %v", err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(loose, z.Bytes(), 0o444); err != nil {
		f.t.Fatal(err)
	}
	// Plain cat-file re-hashes a loose object and refuses it; the batch reader
	// the command uses does not, so it serves the planted bytes.
	batch := tools.Command(context.Background(), tools.Git, "cat-file", "--batch")
	batch.Dir = f.root
	batch.Stdin = strings.NewReader(victimTree + "\n")
	out, err := batch.Output()
	if err != nil || !bytes.Contains(out, raw) {
		f.t.Fatalf("the plant did not take effect: err=%v\n%q", err, out)
	}
	return func() {
		f.t.Helper()
		if err := os.Chmod(loose, 0o644); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(loose, orig, 0o444); err != nil {
			f.t.Fatal(err)
		}
	}
}

// pendingOpts waits too briefly for a verdict, as a job still running would.
func (f *fixture) pendingOpts() Options {
	o := f.opts()
	o.Wait = 200 * time.Millisecond
	return o
}

// step runs op and checks the exit code, the printed sequence and the outcome
// text; it returns the printed entry digest and commit.
func (f *fixture) step(o Options, op attest.Op, seq uint64, outcome string) (sha, commit string) {
	f.t.Helper()
	want := 1
	if outcome == textInEffect {
		want = 0
	}
	if code := f.run(o, op); code != want {
		f.t.Fatalf("exit = %d, want %d\n%s", code, want, f.all())
	}
	if !strings.Contains(f.out.String(), fmt.Sprintf("sequence: %d\n", seq)) {
		f.t.Errorf("stdout lacks sequence %d:\n%s", seq, f.all())
	}
	f.wantOutcome(outcome)
	return f.result()
}

func (f *fixture) wantOutcome(outcome string) {
	f.t.Helper()
	out := "\n" + f.out.String()
	if !strings.Contains(out, outcome) {
		f.t.Errorf("stdout lacks %q:\n%s", outcome, f.all())
	}
	if outcome == textInEffect && strings.Contains(out, "not in effect") {
		f.t.Errorf("reported not in effect:\n%s", f.all())
	}
}

func (f *fixture) wantState(digest string, state attest.State, superseded uint64) {
	f.t.Helper()
	v := f.verdicts()
	if got := v.States[digest]; got != state {
		f.t.Errorf("state of %s = %q, want %q", digest, got, state)
	}
	if got := v.Superseded[digest]; got != superseded {
		f.t.Errorf("superseded of %s = %d, want %d", digest, got, superseded)
	}
}

func (f *fixture) wantNotReviewed(digest string) {
	f.t.Helper()
	if got := f.verdicts().States[digest]; got == attest.StateReviewed {
		f.t.Errorf("%s reads reviewed after a restore", digest)
	}
}

func TestReplay(t *testing.T) {
	t.Run("a: rewritten remote and a human revoke", func(t *testing.T) {
		for _, tc := range []struct {
			kind string
			seq  uint64
		}{{"junk", 4}, {"none", 2}} {
			t.Run(tc.kind, func(t *testing.T) {
				r := newReplay(t)
				h := r.pushH()
				r.rewrite(tc.kind)
				r.jobOnTrigger = true

				sha, _ := r.step(r.opts(), attest.OpRevoke, tc.seq, textNotInEffect)
				r.wantState(sha, attest.StateSuperseded, 10)

				sha, _ = r.step(r.opts(), attest.OpRevoke, 11, textInEffect)
				r.wantState(sha, attest.StateRevoked, 0)

				r.restoreH()
				r.wantNotReviewed(h)
			})
		}
	})

	t.Run("b: junk on top of the superseded revoke", func(t *testing.T) {
		r := newReplay(t)
		h := r.pushH()
		r.rewrite("junk")
		r.jobOnTrigger = true
		sha, _ := r.step(r.opts(), attest.OpRevoke, 4, textNotInEffect)
		r.wantState(sha, attest.StateSuperseded, 10)

		r.pullWork()
		r.commitPush(map[string][]byte{".attest/h": r.junkEntry(5)})
		r.syncStore()

		sha, _ = r.step(r.opts(), attest.OpRevoke, 11, textInEffect)
		r.wantState(sha, attest.StateRevoked, 0)

		r.restoreH()
		r.wantNotReviewed(h)
	})

	t.Run("c: revoke walked first under junk after a rewrite", func(t *testing.T) {
		r := newReplay(t)
		h := r.pushH()
		r.rewrite("junk")

		sha, commit := r.step(r.pendingOpts(), attest.OpRevoke, 4, textPending)
		r.pullWork()
		r.commitPush(map[string][]byte{".attest/h": r.junkEntry(5)})
		r.runJob()
		r.wantState(sha, attest.StateSuperseded, 10)

		if code := r.check(r.opts(), sha, commit); code != 1 {
			t.Fatalf("check exit = %d, want 1\n%s", code, r.all())
		}
		r.wantOutcome(textNotInEffect)

		r.syncStore()
		r.jobOnTrigger = true
		sha, _ = r.step(r.opts(), attest.OpRevoke, 11, textInEffect)
		r.wantState(sha, attest.StateRevoked, 0)

		r.restoreH()
		r.wantNotReviewed(h)
	})

	t.Run("d: a record's highest under junk", func(t *testing.T) {
		t.Run("revoke is in effect", func(t *testing.T) {
			r := newReplay(t)
			sha, commit := r.step(r.pendingOpts(), attest.OpRevoke, 2, textPending)
			r.pullWork()
			r.commitPush(map[string][]byte{".attest/h": r.junkEntry(1)})
			r.runJob()
			r.wantState(sha, attest.StateRevoked, 0)

			for range 2 {
				if code := r.check(r.opts(), sha, commit); code != 0 {
					t.Fatalf("check exit = %d, want 0\n%s", code, r.all())
				}
				for _, part := range []string{"op: revoke sequence: 2", "in effect"} {
					if !strings.Contains(r.out.String(), part) {
						t.Errorf("stdout lacks %q:\n%s", part, r.out.String())
					}
				}
				r.wantOutcome(textInEffect)

				r.pullWork()
				r.commitPush(map[string][]byte{".attest/h": r.junkEntry(1)})
				r.runJob()
				r.wantState(sha, attest.StateRevoked, 0)
			}
		})

		t.Run("attest is not in effect", func(t *testing.T) {
			r := newReplay(t)
			sha, commit := r.step(r.pendingOpts(), attest.OpAttest, 2, textPending)
			r.pullWork()
			r.commitPush(map[string][]byte{".attest/h": r.junkEntry(1)})
			r.runJob()
			if v := r.verdicts(); v.Status != "ok" {
				t.Fatalf("verdict status = %q, want ok", v.Status)
			} else if state, ok := v.States[sha]; ok {
				t.Errorf("attest has a line %q, want none", state)
			}

			if code := r.check(r.opts(), sha, commit); code != 1 {
				t.Fatalf("check exit = %d, want 1\n%s", code, r.all())
			}
			r.wantOutcome(textNotInEffect)

			r.syncStore()
			r.jobOnTrigger = true
			sha, _ = r.step(r.opts(), attest.OpAttest, 3, textInEffect)
			r.wantState(sha, attest.StateReviewed, 0)
		})
	})

	t.Run("e: job slower than the wait", func(t *testing.T) {
		r := newReplay(t)
		h := r.pushH()
		r.rewrite("junk")

		sha, commit := r.step(r.pendingOpts(), attest.OpRevoke, 4, textPending)
		r.pullWork()
		r.commitPush(map[string][]byte{".attest/h": r.junkEntry(5)})
		restore := r.restoreH()
		r.syncStore()
		if got := r.verdicts().States[sha]; got == attest.StateRevoked {
			t.Error("revoke under a restored H reads revoked")
		}

		wantReport := func(t *testing.T) {
			t.Helper()
			if code := r.check(r.opts(), sha, commit); code != 1 {
				t.Fatalf("check exit = %d, want 1\n%s", code, r.all())
			}
			for _, part := range []string{"op: revoke sequence: 4", textNotInEffect} {
				if !strings.Contains(r.out.String(), part) {
					t.Errorf("stdout lacks %q:\n%s", part, r.out.String())
				}
			}
			if !strings.Contains(r.out.String(), "h "+sha+" ") {
				t.Errorf("stdout lacks the name and digest:\n%s", r.out.String())
			}
		}
		wantReport(t)

		gitOut(t, r.root, "", "replace", "-f", commit, restore)
		wantReport(t)
		gitOut(t, r.root, "", "replace", "-d", commit)

		// The entry is packed by now, so a rewritten tree cannot be planted as a
		// loose object; an earlier attest's digest stands in for what it serves.
		if code := r.check(r.opts(), hexSum(r.attestH()), commit); code != 1 {
			t.Fatalf("check with another entry's digest: exit = %d, want 1\n%s", code, r.all())
		}
		if !strings.Contains(r.errb.String(), "does not hash to") {
			t.Errorf("stderr lacks the digest mismatch:\n%s", r.errb.String())
		}

		undo := r.plantTree(commit, restore)
		if code := r.check(r.opts(), sha, commit); code != 1 {
			t.Fatalf("check on a rewritten tree: exit = %d, want 1\n%s", code, r.all())
		}
		if !strings.Contains(r.errb.String(), "does not hash to") {
			t.Errorf("stderr lacks the digest mismatch:\n%s", r.errb.String())
		}
		undo()

		r.jobOnTrigger = true
		sha, _ = r.step(r.opts(), attest.OpRevoke, 11, textInEffect)
		r.wantState(sha, attest.StateRevoked, 0)
		r.wantNotReviewed(h)
	})

	t.Run("f: remote rewritten after the job's last fetch", func(t *testing.T) {
		f := newFixture(t)
		c0 := f.remoteMain()
		f.jobOnTrigger = true
		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("seed attest exit = %d\n%s", code, f.all())
		}
		reviewed := f.remoteBlob("main", "facts/h.md")
		f.jobOnTrigger = false
		f.pullWork()
		h := f.signedEntry(f.A, 10, "attest", "facts/h.md", reviewed)
		f.commitPush(map[string][]byte{".attest/h": h})
		f.runJob()
		if got := f.verdicts().States[hexSum(h)]; got != attest.StateReviewed {
			t.Fatalf("H state = %q, want reviewed", got)
		}

		f.forcePush(c0)
		f.jobOnTrigger = true
		sha, _ := f.step(f.opts(), attest.OpAttest, 2, textNotInEffect)
		f.wantState(sha, attest.StateSuperseded, 10)
		if !strings.Contains(f.out.String(), "superseded") {
			t.Errorf("stdout does not say superseded:\n%s", f.out.String())
		}

		sha, _ = f.step(f.opts(), attest.OpAttest, 11, textInEffect)
		f.wantState(sha, attest.StateReviewed, 0)
	})
}
