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
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

type wheelGroups struct{}

func (wheelGroups) Gids() ([]int, error)         { return []int{0}, nil }
func (wheelGroups) Name(gid int) (string, error) { return "wheel", nil }

// run resets the captured output and runs the command for fact h.
func (f *fixture) run(o Options, op attest.Op) int {
	f.t.Helper()
	f.out.Reset()
	f.errb.Reset()
	return Attest(context.Background(), o, Request{Name: "h", Handle: f.handle, Op: op})
}

func (f *fixture) check(o Options, sha, commit string) int {
	f.t.Helper()
	f.out.Reset()
	f.errb.Reset()
	return Check(context.Background(), o, "h", "", sha, commit)
}

// all is everything the last run printed.
func (f *fixture) all() string { return f.out.String() + f.errb.String() }

func (f *fixture) remoteMain() string { return gitOut(f.t, f.remote, "", "rev-parse", "main") }

// remoteBlob is the file at rev:path in the remote, bytes exact.
func (f *fixture) remoteBlob(rev, path string) []byte {
	f.t.Helper()
	cmd := tools.Command(context.Background(), tools.Git, "cat-file", "blob", rev+":"+path)
	cmd.Dir = f.remote
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("cat-file %s:%s: %v", rev, path, err)
	}
	return out
}

func (f *fixture) factRaw() []byte {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "facts", "h.md"))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// setFact commits raw as the fact in the store checkout and pushes it, so the
// checkout and the work clone's remote agree; the work clone is left behind.
func (f *fixture) setFact(raw []byte) {
	f.t.Helper()
	f.writeFile(filepath.Join(f.root, "facts", "h.md"), string(raw), 0o644)
	gitOut(f.t, f.root, "", "commit", "-q", "-am", "set fact")
	gitOut(f.t, f.root, "", "push", "-q", f.remote, "HEAD:main")
	gitOut(f.t, f.work, "", "pull", "-q", "--ff-only", f.remote, "main")
}

// result is the sha and commit of the `h <sha> <commit>` line the last run printed.
func (f *fixture) result() (sha, commit string) {
	f.t.Helper()
	for line := range strings.SplitSeq(f.out.String(), "\n") {
		w := strings.Fields(line)
		if len(w) == 3 && w[0] == "h" && len(w[1]) == 64 && len(w[2]) == 40 {
			return w[1], w[2]
		}
	}
	f.t.Fatalf("no result line in:\n%s", f.all())
	return "", ""
}

func (f *fixture) writeVerdicts(v attest.Verdicts) {
	f.t.Helper()
	b, err := attest.FormatVerdicts(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.writeFile(attest.VerdictPath(fixtureStoreID), string(b), 0o644)
}

// verdictTrigger is a trigger that writes an ok verdict file for the remote's
// tip, then lets mut shape the line for the entry there.
func (f *fixture) verdictTrigger(mut func(v *attest.Verdicts, digest string)) func(string) error {
	return func(string) error {
		f.triggers++
		tip := f.remoteMain()
		v := attest.Verdicts{
			Store: fixtureStoreID, Trust: f.cfg.TrustDigest, Tip: tip, Fetched: time.Now(), Status: "ok",
			States: map[string]attest.State{}, Superseded: map[string]uint64{},
		}
		mut(&v, hexSum(f.remoteBlob(tip, ".attest/h")))
		f.writeVerdicts(v)
		return nil
	}
}

func (f *fixture) requireClean(o Options, op attest.Op, calls int) {
	f.t.Helper()
	if code := f.run(o, op); code != 1 {
		f.t.Fatalf("exit = %d, want 1\n%s", code, f.all())
	}
	if f.calls != calls {
		f.t.Errorf("signer calls = %d, want %d", f.calls, calls)
	}
}

func TestAttest(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := newFixture(t)
		f.jobOnTrigger = true
		raw := f.factRaw()
		want, err := fact.WithConfidence(raw, "reviewed")
		if err != nil {
			t.Fatal(err)
		}

		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, f.all())
		}
		sha, commit := f.result()
		if commit != f.remoteMain() {
			t.Errorf("printed commit %s, remote main %s", commit, f.remoteMain())
		}
		entry := f.remoteBlob("main", ".attest/h")
		if hexSum(entry) != sha {
			t.Errorf("printed digest %s, committed entry %s", sha, hexSum(entry))
		}
		if got := f.remoteBlob("main", "facts/h.md"); !bytes.Equal(got, want) {
			t.Errorf("committed fact =\n%s\nwant\n%s", got, want)
		}
		if !strings.Contains(string(entry), "\nsha256: "+hexSum(want)+"\n") {
			t.Errorf("entry does not bind the committed fact:\n%s", entry)
		}
		out := f.out.String()
		for _, part := range []string{"priors-attest v1", "name: h", "op: attest", "in effect"} {
			if !strings.Contains(out, part) {
				t.Errorf("stdout lacks %q:\n%s", part, out)
			}
		}
		if strings.Contains(f.all(), "not in effect") {
			t.Errorf("reported not in effect:\n%s", f.all())
		}
		if f.triggers != 1 {
			t.Errorf("triggers = %d, want 1", f.triggers)
		}
	})

	t.Run("render escapes", func(t *testing.T) {
		f := newFixture(t)
		f.jobOnTrigger = true
		raw := bytes.Replace(f.factRaw(), []byte("body of h"), []byte("body \u202e \x1b[31m \U000E0041 of h"), 1)
		f.setFact(raw)

		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, f.all())
		}
		out := f.out.String()
		for _, r := range []rune{'\u202e', '\x1b', '\U000E0041'} {
			if strings.ContainsRune(f.all(), r) {
				t.Errorf("raw rune %U reached the output", r)
			}
		}
		for _, esc := range []string{`\u{202E}`, `\u{001B}`, `\u{E0041}`} {
			if !strings.Contains(out, esc) {
				t.Errorf("stdout lacks %s:\n%s", esc, out)
			}
		}
	})

	t.Run("fact changed after signing", func(t *testing.T) {
		f := newFixture(t)
		before, remote := f.head(f.root), f.remoteMain()
		edited := append(f.factRaw(), "edited\n"...)
		o := f.opts()
		o.afterSign = func() { f.writeFile(filepath.Join(f.root, "facts", "h.md"), string(edited), 0o644) }

		if code := f.run(o, attest.OpAttest); code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, f.all())
		}
		if !strings.Contains(f.all(), "changed since it was read") {
			t.Errorf("output = %s", f.all())
		}
		if !bytes.Equal(f.factRaw(), edited) {
			t.Error("fact was overwritten")
		}
		if _, err := os.Lstat(filepath.Join(f.root, ".attest", "h")); !os.IsNotExist(err) {
			t.Errorf(".attest/h exists: %v", err)
		}
		if f.head(f.root) != before || f.remoteMain() != remote {
			t.Error("HEAD or remote moved")
		}
	})

	t.Run("working tree swap after entry write", func(t *testing.T) {
		f := newFixture(t)
		f.jobOnTrigger = true
		swapped := f.signedEntry(f.A, 99, "attest", "facts/h.md", []byte("old"))
		o := f.opts()
		o.afterEntryWrite = func() { f.writeFile(filepath.Join(f.root, ".attest", "h"), string(swapped), 0o644) }

		if code := f.run(o, attest.OpAttest); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, f.all())
		}
		sha, _ := f.result()
		committed := f.remoteBlob("main", ".attest/h")
		if hexSum(committed) != sha || sha == hexSum(swapped) {
			t.Errorf("printed %s, committed %s, swapped %s", sha, hexSum(committed), hexSum(swapped))
		}
		if !strings.Contains(string(committed), "\nsequence: 1\n") {
			t.Errorf("committed entry is not the signed one:\n%s", committed)
		}
	})

	t.Run("planted loose object", func(t *testing.T) {
		f := newFixture(t)
		want, err := fact.WithConfidence(f.factRaw(), "reviewed")
		if err != nil {
			t.Fatal(err)
		}
		payload := attesttest.Entry(fixtureStoreID, "h", "facts/h.md", hexSum(want), 1, "attest")
		sig := f.A.SignSSHSIG(payload, attest.Namespace, 0x05)
		if !bytes.Equal(sig, f.A.SignSSHSIG(payload, attest.Namespace, 0x05)) {
			t.Fatal("signatures are not deterministic")
		}
		entry := append(bytes.Clone(payload), sig...)
		oid := gitOut(t, f.root, string(entry), "hash-object", "--stdin")
		bad := []byte("planted bytes, not the signed entry\n")
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := fmt.Fprintf(zw, "blob %d\x00%s", len(bad), bad); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(f.root, ".git", "objects", oid[:2])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, oid[2:]), z.Bytes(), 0o444); err != nil {
			t.Fatal(err)
		}
		remote := f.remoteMain()

		if code := f.run(f.opts(), attest.OpAttest); code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, f.all())
		}
		if !strings.Contains(f.all(), "not pushed") {
			t.Errorf("output = %s", f.all())
		}
		if f.remoteMain() != remote {
			t.Error("remote moved")
		}
		if strings.Contains(f.all(), hexSum(bad)) {
			t.Error("the planted bytes' digest was printed")
		}
	})

	t.Run("pushes to the default branch from another branch", func(t *testing.T) {
		f := newFixture(t)
		f.jobOnTrigger = true
		gitOut(t, f.root, "", "checkout", "-q", "-b", "side")
		gitOut(t, f.root, "", "config", "remote.origin.push", "refs/heads/side:refs/heads/side")
		gitOut(t, f.root, "", "config", "push.default", "nothing")

		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, f.all())
		}
		_, commit := f.result()
		if f.remoteMain() != commit {
			t.Errorf("remote main %s, want %s", f.remoteMain(), commit)
		}
		if got := gitOut(t, f.remote, "", "branch", "--list", "side"); got != "" {
			t.Errorf("remote has side: %q", got)
		}
	})

	t.Run("remote moves before the push", func(t *testing.T) {
		f := newFixture(t)
		o := f.opts()
		o.beforePush = func() { f.commitPush(nil) }

		if code := f.run(o, attest.OpAttest); code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, f.all())
		}
		if !strings.Contains(f.all(), "not pushed") {
			t.Errorf("output = %s", f.all())
		}
		if f.triggers != 0 {
			t.Errorf("triggers = %d, want 0", f.triggers)
		}
	})

	t.Run("head not descending from origin", func(t *testing.T) {
		f := newFixture(t)
		f.commitPush(nil)
		gitOut(t, f.root, "", "commit", "-q", "--allow-empty", "-m", "local")
		f.requireClean(f.opts(), attest.OpAttest, 0)
		if !strings.Contains(f.all(), "descend") {
			t.Errorf("output = %s", f.all())
		}
	})

	t.Run("numbering", func(t *testing.T) {
		t.Run("second attest", func(t *testing.T) {
			f := newFixture(t)
			f.jobOnTrigger = true
			for _, want := range []string{"sequence: 1\n", "sequence: 2\n"} {
				if code := f.run(f.opts(), attest.OpAttest); code != 0 {
					t.Fatalf("exit = %d, want 0\n%s", code, f.all())
				}
				if !strings.Contains(f.out.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, f.out.String())
				}
			}
		})

		t.Run("above a superseded line", func(t *testing.T) {
			f := newFixture(t)
			old := f.signedEntry(f.A, 3, "attest", "facts/h.md", f.factRaw())
			f.commitPush(map[string][]byte{".attest/h": old})
			f.syncStore()
			f.writeVerdicts(attest.Verdicts{
				Store: fixtureStoreID, Trust: f.cfg.TrustDigest, Tip: f.remoteMain(), Fetched: time.Now(), Status: "ok",
				States:     map[string]attest.State{hexSum(old): attest.StateSuperseded},
				Superseded: map[string]uint64{hexSum(old): 7},
			})
			o := f.opts()
			o.Wait = 200 * time.Millisecond

			f.run(o, attest.OpAttest)
			if !strings.Contains(f.out.String(), "sequence: 8\n") {
				t.Errorf("stdout lacks sequence 8:\n%s", f.all())
			}
		})

		t.Run("revoke copies the signed entry", func(t *testing.T) {
			f := newFixture(t)
			f.jobOnTrigger = true
			if code := f.run(f.opts(), attest.OpAttest); code != 0 {
				t.Fatalf("attest exit = %d\n%s", code, f.all())
			}
			reviewed := f.factRaw()
			if code := f.run(f.opts(), attest.OpRevoke); code != 0 {
				t.Fatalf("revoke exit = %d, want 0\n%s", code, f.all())
			}
			out := f.out.String()
			for _, part := range []string{"op: revoke\n", "sequence: 2\n", "path: facts/h.md\n", "sha256: " + hexSum(reviewed) + "\n"} {
				if !strings.Contains(out, part) {
					t.Errorf("stdout lacks %q:\n%s", part, out)
				}
			}
			if !bytes.Equal(f.factRaw(), reviewed) {
				t.Error("revoke touched the fact")
			}
		})
	})

	t.Run("outcomes", func(t *testing.T) {
		t.Run("superseded", func(t *testing.T) {
			f := newFixture(t)
			o := f.opts()
			o.Trigger = f.verdictTrigger(func(v *attest.Verdicts, d string) {
				v.States[d], v.Superseded[d] = attest.StateSuperseded, 7
			})
			if code := f.run(o, attest.OpAttest); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			for _, part := range []string{"not in effect", "superseded (highest sequence 7)"} {
				if !strings.Contains(f.all(), part) {
					t.Errorf("output lacks %q:\n%s", part, f.all())
				}
			}
		})

		t.Run("lapsed", func(t *testing.T) {
			f := newFixture(t)
			o := f.opts()
			o.Trigger = func(string) error {
				f.triggers++
				gitOut(t, f.work, "", "pull", "-q", "--ff-only", f.remote, "main")
				edited, err := os.ReadFile(filepath.Join(f.work, "facts", "h.md"))
				if err != nil {
					t.Fatal(err)
				}
				f.commitPush(map[string][]byte{"facts/h.md": bytes.Replace(edited, []byte("body of h"), []byte("body edited"), 1)})
				f.runJob()
				return nil
			}
			if code := f.run(o, attest.OpAttest); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			for _, part := range []string{"not in effect", "lapsed"} {
				if !strings.Contains(f.all(), part) {
					t.Errorf("output lacks %q:\n%s", part, f.all())
				}
			}
		})

		t.Run("ok tip without a line", func(t *testing.T) {
			f := newFixture(t)
			o := f.opts()
			o.Trigger = f.verdictTrigger(func(*attest.Verdicts, string) {})
			if code := f.run(o, attest.OpAttest); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			if !strings.Contains(f.all(), "not in effect") {
				t.Errorf("output = %s", f.all())
			}
		})

		t.Run("pending at the deadline", func(t *testing.T) {
			f := newFixture(t)
			o := f.opts()
			o.Wait = 300 * time.Millisecond
			if code := f.run(o, attest.OpAttest); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			sha, commit := f.result()
			for _, part := range []string{"not yet observed", "priors attest --check h " + sha + " " + commit} {
				if !strings.Contains(f.all(), part) {
					t.Errorf("output lacks %q:\n%s", part, f.all())
				}
			}
		})

		t.Run("not pushed", func(t *testing.T) {
			f := newFixture(t)
			remote := f.remoteMain()
			o := f.opts()
			o.beforePush = func() { f.sshMode("hostkey") }
			if code := f.run(o, attest.OpAttest); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			if !strings.Contains(f.all(), "not pushed") {
				t.Errorf("output = %s", f.all())
			}
			if f.remoteMain() != remote {
				t.Error("remote moved")
			}
		})
	})

	t.Run("check", func(t *testing.T) {
		attested := func(t *testing.T) (f *fixture, sha, commit string) {
			f = newFixture(t)
			f.jobOnTrigger = true
			if code := f.run(f.opts(), attest.OpAttest); code != 0 {
				t.Fatalf("attest exit = %d\n%s", code, f.all())
			}
			sha, commit = f.result()
			return f, sha, commit
		}

		t.Run("reports", func(t *testing.T) {
			f, sha, commit := attested(t)
			calls, head, remote := f.calls, f.head(f.root), f.remoteMain()
			if code := f.check(f.opts(), sha, commit); code != 0 {
				t.Fatalf("exit = %d, want 0\n%s", code, f.all())
			}
			for _, part := range []string{"h " + sha + " op: attest sequence: 1", "in effect"} {
				if !strings.Contains(f.out.String(), part) {
					t.Errorf("stdout lacks %q:\n%s", part, f.out.String())
				}
			}
			if f.calls != calls || f.head(f.root) != head || f.remoteMain() != remote {
				t.Error("check signed, committed or pushed")
			}
		})

		t.Run("digest mismatch", func(t *testing.T) {
			f, _, commit := attested(t)
			if code := f.check(f.opts(), hexSum([]byte("x")), commit); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			if !strings.Contains(f.errb.String(), "does not hash to") {
				t.Errorf("stderr = %s", f.errb.String())
			}
		})

		t.Run("another name's entry", func(t *testing.T) {
			f := newFixture(t)
			other := f.signedEntry(f.A, 1, "attest", "facts/other.md", []byte("x"))
			commit := f.commitPush(map[string][]byte{".attest/h": other})
			f.syncStore()
			calls := f.calls
			if code := f.check(f.opts(), hexSum(other), commit); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, f.all())
			}
			if !strings.Contains(f.errb.String(), "check 6") {
				t.Errorf("stderr = %s", f.errb.String())
			}
			if f.calls != calls {
				t.Error("check signed")
			}
		})
	})

	t.Run("host off", func(t *testing.T) {
		tests := []struct {
			name string
			set  func(o *Options)
			want string
		}{
			{"owner-admin", func(o *Options) { o.Config.TrustRoot, o.Config.TrustRootLabel = "owner-admin", "owner-admin" }, "attestation off: trust_root owner-admin"},
			{"wheel", func(o *Options) { o.Groups = wheelGroups{} }, "attestation off: group wheel"},
		}
		for _, tt := range tests {
			for _, op := range []attest.Op{attest.OpAttest, attest.OpRevoke} {
				t.Run(tt.name+" "+string(op), func(t *testing.T) {
					f := newFixture(t)
					remote := f.remoteMain()
					o := f.opts()
					tt.set(&o)
					f.requireClean(o, op, 0)
					if !strings.Contains(f.errb.String(), tt.want) {
						t.Errorf("stderr = %s, want %q", f.errb.String(), tt.want)
					}
					if f.sshLog() != "" {
						t.Errorf("ssh ran: %s", f.sshLog())
					}
					if got := gitOut(t, f.root, "", "status", "--porcelain"); got != "" {
						t.Errorf("checkout dirty: %s", got)
					}
					if f.remoteMain() != remote {
						t.Error("remote moved")
					}
				})
			}
		}
	})

	t.Run("pin is not ssh", func(t *testing.T) {
		for _, url := range []string{"", "PATH", "file://PATH", "ext::sh -c true"} {
			t.Run(url, func(t *testing.T) {
				f := newFixture(t)
				o := f.opts()
				o.Config.PersonalRemote = strings.ReplaceAll(url, "PATH", f.remote)
				f.requireClean(o, attest.OpAttest, 0)
				if f.sshLog() != "" {
					t.Errorf("ssh ran: %s", f.sshLog())
				}
			})
		}
	})

	t.Run("handle not allowlisted", func(t *testing.T) {
		f := newFixture(t)
		o := f.opts()
		f.writeFile(f.handle, string(attesttest.NewSKEd25519(t).Handle(5)), 0o600)
		f.requireClean(o, attest.OpAttest, 0)
	})

	t.Run("loader variable", func(t *testing.T) {
		f := newFixture(t)
		o := f.opts()
		o.Env = []string{"PATH=/usr/bin", "LD_PRELOAD=/x"}
		f.requireClean(o, attest.OpAttest, 0)
		f.requireClean(o, attest.OpRevoke, 0)
		if code := f.check(o, strings.Repeat("a", 64), strings.Repeat("b", 40)); code != 1 {
			t.Errorf("check exit = %d, want 1", code)
		}
		if f.sshLog() != "" {
			t.Errorf("ssh ran: %s", f.sshLog())
		}
	})

	t.Run("gate refusal precedes render", func(t *testing.T) {
		f := newFixture(t)
		fc, err := fact.Parse(f.factRaw())
		if err != nil {
			t.Fatal(err)
		}
		fc.Metadata.Flags = []string{"suspicious"}
		raw, err := fc.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		f.setFact(raw)
		f.requireClean(f.opts(), attest.OpAttest, 0)
		if !strings.Contains(f.errb.String(), "not gated for the store") {
			t.Errorf("stderr = %s", f.errb.String())
		}
		if f.out.Len() != 0 {
			t.Errorf("rendered before the gate refused:\n%s", f.out.String())
		}
	})

	t.Run("head moves before the commit", func(t *testing.T) {
		f := newFixture(t)
		remote := f.remoteMain()
		o := f.opts()
		o.beforeCommit = func() { gitOut(t, f.root, "", "commit", "-q", "--allow-empty", "-m", "moved") }
		if code := f.run(o, attest.OpAttest); code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, f.all())
		}
		if !strings.Contains(f.all(), "not committed") {
			t.Errorf("output = %s", f.all())
		}
		if f.remoteMain() != remote {
			t.Error("remote moved")
		}
	})

	t.Run("grafts and shallow", func(t *testing.T) {
		layouts := []struct {
			name  string
			setup func(f *fixture, o *Options)
		}{
			{"grafts", func(f *fixture, _ *Options) { f.writeFile(filepath.Join(f.root, ".git", "info", "grafts"), "", 0o644) }},
			{"shallow", func(f *fixture, _ *Options) { f.writeFile(filepath.Join(f.root, ".git", "shallow"), "", 0o644) }},
			{"shallow in the common dir", func(f *fixture, o *Options) {
				wt := filepath.Join(f.dir, "wt")
				gitOut(f.t, f.root, "", "worktree", "add", "-q", "-b", "wtb", wt)
				f.writeFile(filepath.Join(f.root, ".git", "shallow"), "", 0o644)
				o.Config.PersonalStore = wt
			}},
		}
		for _, l := range layouts {
			t.Run(l.name, func(t *testing.T) {
				f := newFixture(t)
				o := f.opts()
				l.setup(f, &o)
				f.requireClean(o, attest.OpAttest, 0)
				f.requireClean(o, attest.OpRevoke, 0)
				if code := f.check(o, strings.Repeat("a", 64), strings.Repeat("b", 40)); code != 1 {
					t.Errorf("check exit = %d, want 1\n%s", code, f.all())
				}
				if f.sshLog() != "" {
					t.Errorf("ssh ran: %s", f.sshLog())
				}
			})
		}
	})

	t.Run("replace ref and commit-graph are ignored", func(t *testing.T) {
		f := newFixture(t)
		f.jobOnTrigger = true
		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("first attest exit = %d\n%s", code, f.all())
		}
		c1 := f.remoteMain()

		// The history the plant hides: C2 drops the entry, C3 sits on C2
		// unpushed, and a naive walk of C3 would see neither the entry nor C2.
		gitOut(t, f.root, "", "rm", "-q", ".attest/h")
		gitOut(t, f.root, "", "commit", "-q", "-m", "drop the entry")
		gitOut(t, f.root, "", "push", "-q", f.remote, "HEAD:main")
		gitOut(t, f.root, "", "commit", "-q", "--allow-empty", "-m", "local")
		c3 := f.head(f.root)
		plantGraph(t, f.root, c3)
		gitOut(t, f.root, "", "replace", "--graft", c3)
		if got := gitOut(t, f.root, "", "rev-list", "--count", "HEAD"); got != "1" {
			t.Fatalf("planted history has %s commits, want 1", got)
		}

		if code := f.run(f.opts(), attest.OpAttest); code != 0 {
			t.Fatalf("second attest exit = %d, want 0\n%s", code, f.all())
		}
		if !strings.Contains(f.out.String(), "sequence: 2\n") {
			t.Errorf("number was shaped by the plant:\n%s", f.out.String())
		}

		sha, commit := f.result()
		gitOut(t, f.root, "", "replace", "-f", commit, c1)
		if code := f.check(f.opts(), sha, commit); code != 0 {
			t.Fatalf("check exit = %d, want 0\n%s", code, f.all())
		}
		if !strings.Contains(f.out.String(), "op: attest sequence: 2") {
			t.Errorf("check read the replacement:\n%s", f.out.String())
		}
	})
}
