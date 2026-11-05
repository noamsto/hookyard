package commit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
)

// attestFixture is setup's checkout with a separate trust root and the
// returned key allowlisted; the host it classifies to is on.
func attestFixture(t *testing.T) (fixture, attest.Host, []byte, func(name string) []byte) {
	t.Helper()
	fx := setup(t)
	fx.cfg.TrustRoot, fx.cfg.TrustRootLabel = "separate", "separate"
	fx.cfg.PersonalStoreID = testStoreID
	key := attesttest.NewSKEd25519(t)
	host := attest.Host{Keys: []ssh.PublicKey{key.PublicKey()}}
	raw := marshal(t, reviewedFact("rev-fact"))
	sign := func(name string) []byte { return entry(key, name, revRel, raw, "attest", upUV) }
	return fx, host, raw, sign
}

func TestGateOffHostRefusesReviewedFactDespiteEntry(t *testing.T) {
	fx, _, raw, sign := attestFixture(t)
	fx.putAttest(t, revEntry, sign("rev-fact"))
	fx.commitAll(t, "entry")
	off := attest.Host{Off: "attestation off: group wheel"}

	got := Gate(context.Background(), fx.cfg, fx.root, off, fx.rules, fx.scanner, map[string][]byte{revRel: raw}, false)
	if !strings.Contains(got, revRel+" (unattested-review") {
		t.Errorf("Gate = %q, want unattested-review", got)
	}
}

func TestGatePresign(t *testing.T) {
	flagged := reviewedFact("rev-fact")
	flagged.Metadata.Flags = []string{"suspicious"}
	dirty := reviewedFact("rev-fact")
	dirty.Body = scannerHit + "\n"
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{"clean", marshal(t, reviewedFact("rev-fact")), ""},
		{"flagged", marshal(t, flagged), "flagged"},
		{"scanner hit", marshal(t, dirty), "gate:secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, host, _, _ := attestFixture(t)
			got := Gate(context.Background(), fx.cfg, fx.root, host, fx.rules, fx.scanner, map[string][]byte{revRel: tt.raw}, true)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("Gate = %q, want pass", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("Gate = %q, want it to name %q", got, tt.want)
			}
		})
	}
}

func TestGateChecksTheEntryWhenNotPresigning(t *testing.T) {
	fx, host, raw, sign := attestFixture(t)
	fx.putAttest(t, revEntry, sign("rev-fact"))
	fx.commitAll(t, "entry")
	files := map[string][]byte{revRel: raw}

	if got := Gate(context.Background(), fx.cfg, fx.root, host, fx.rules, fx.scanner, files, false); got != "" {
		t.Fatalf("Gate with a valid entry = %q, want pass", got)
	}

	fx.putAttest(t, revEntry, sign("other-fact"))
	fx.commitAll(t, "entry for another name")
	got := Gate(context.Background(), fx.cfg, fx.root, host, fx.rules, fx.scanner, files, false)
	if !strings.Contains(got, "unattested-review") {
		t.Errorf("Gate with another name's entry = %q, want unattested-review", got)
	}
}

func TestFilesCommitsTheInMemoryBytes(t *testing.T) {
	fx := setup(t)
	committest.PinPush(t)
	rel := "_global/good-fact.md"
	want := marshal(t, cleanFact("good-fact"))
	base := head(t, fx.dir())
	beforeStage = func() { writeFile(t, filepath.Join(fx.dir(), rel), marshal(t, linkyFact("good-fact"))) }
	t.Cleanup(func() { beforeStage = func() {} })

	c, w := Files(context.Background(), fx.dir(), base, map[string][]byte{rel: want}, "attest: x #1")
	if w != "" || c == "" {
		t.Fatalf("Files = %q, %q", c, w)
	}
	if got := head(t, fx.dir()); got != c {
		t.Errorf("HEAD = %s, want %s", got, c)
	}
	if got := strings.Fields(git(t, fx.dir(), "diff", "--name-only", base, c)); len(got) != 1 || got[0] != rel {
		t.Errorf("commit changed %v, want only %s", got, rel)
	}
	if got := headBlob(t, fx.dir(), rel); got != string(want) {
		t.Errorf("committed %s =\n%s\nwant the in-memory bytes\n%s", rel, got, want)
	}
	for line := range strings.SplitSeq(git(t, fx.dir(), "status", "--porcelain", "--", rel), "\n") {
		if line != "" && line[0] != '?' && line[0] != ' ' {
			t.Errorf("staged change for %s: %q", rel, line)
		}
	}
	if s := staged(t, fx.dir()); s != "" {
		t.Errorf("staged %q, want nothing", s)
	}
}

func TestFilesStaleParentCommitsNothing(t *testing.T) {
	fx := setup(t)
	committest.PinPush(t)
	stale := head(t, fx.dir())
	fx.put(t, "_global/moved.md", cleanFact("moved"))
	fx.commitAll(t, "moves HEAD")
	moved := head(t, fx.dir())

	c, w := Files(context.Background(), fx.dir(), stale, map[string][]byte{"_global/good-fact.md": marshal(t, cleanFact("good-fact"))}, "attest: x #1")
	if c != "" || w == "" {
		t.Errorf("Files = %q, %q, want a warning and no commit", c, w)
	}
	if got := head(t, fx.dir()); got != moved {
		t.Errorf("HEAD moved: %s -> %s", moved, got)
	}
}

func TestFilesNeverSigns(t *testing.T) {
	fx := setup(t)
	committest.PinPush(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	marker := filepath.Join(t.TempDir(), "signer-ran")
	signer := filepath.Join(t.TempDir(), "signer")
	writeFile(t, signer, []byte("#!"+sh+"\ntouch '"+marker+"'\nexit 1\n"))
	if err := os.Chmod(signer, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	git(t, fx.dir(), "config", "commit.gpgsign", "true")
	git(t, fx.dir(), "config", "gpg.format", "ssh")
	git(t, fx.dir(), "config", "gpg.ssh.program", signer)
	git(t, fx.dir(), "config", "user.signingkey", signer)
	base := head(t, fx.dir())

	c, w := Files(context.Background(), fx.dir(), base, map[string][]byte{"_global/good-fact.md": marshal(t, cleanFact("good-fact"))}, "attest: x #1")
	if c == "" || w != "" {
		t.Fatalf("Files = %q, %q, want an unsigned commit", c, w)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Errorf("the checkout's signing program ran: %v", err)
	}
	if obj := git(t, fx.dir(), "cat-file", "commit", c); strings.Contains(obj, "\ngpgsig") {
		t.Errorf("commit is signed:\n%s", obj)
	}
}

func TestFilesWithoutIdentityCommitsNothing(t *testing.T) {
	fx := setup(t)
	committest.PinPush(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	base := head(t, fx.dir())

	c, w := Files(context.Background(), fx.dir(), base, map[string][]byte{"_global/good-fact.md": marshal(t, cleanFact("good-fact"))}, "attest: x #1")
	if c != "" || !strings.Contains(w, "no git identity (user.name/user.email) configured for the store") {
		t.Errorf("Files = %q, %q, want the missing identity named", c, w)
	}
	if got := head(t, fx.dir()); got != base {
		t.Errorf("HEAD moved: %s -> %s", base, got)
	}
}

// replaceHead plants refs/replace/<HEAD> pointing at a commit whose tree is
// HEAD's top-level entries passed through edit.
func replaceHead(t *testing.T, dir string, edit func(entries []string) []string) {
	t.Helper()
	base := head(t, dir)
	entries := strings.Split(strings.TrimRight(git(t, dir, "ls-tree", base), "\n"), "\n")
	cmd := exec.Command("git", "-C", dir, "mktree")
	cmd.Stdin = strings.NewReader(strings.Join(edit(entries), "\n") + "\n")
	tree, err := cmd.Output()
	if err != nil {
		t.Fatalf("git mktree: %v", err)
	}
	git(t, dir, "replace", base, orphanCommit(t, dir, strings.TrimSpace(string(tree))))
}

func TestFilesIgnoresReplaceRefs(t *testing.T) {
	rel := "_global/good-fact.md"
	tests := []struct {
		name string
		edit func(t *testing.T, dir string, entries []string) []string
	}{
		{"adds a path", func(t *testing.T, dir string, entries []string) []string {
			oid := strings.TrimSpace(git(t, dir, "hash-object", "-w", "--stdin"))
			return append(entries, "100644 blob "+oid+"\tevil.md")
		}},
		{"drops a path", func(_ *testing.T, _ string, entries []string) []string {
			return slices.DeleteFunc(entries, func(e string) bool { return strings.HasSuffix(e, "\t.gitignore") })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t)
			committest.PinPush(t)
			base := head(t, fx.dir())
			replaceHead(t, fx.dir(), func(entries []string) []string { return tt.edit(t, fx.dir(), entries) })
			raw := marshal(t, cleanFact("good-fact"))

			c, w := Files(context.Background(), fx.dir(), base, map[string][]byte{rel: raw}, "attest: x #1")
			if c == "" {
				t.Fatalf("Files = %q, %q", c, w)
			}
			want := append(strings.Fields(git(t, fx.dir(), "--no-replace-objects", "ls-tree", "-r", "--name-only", base)), rel)
			slices.Sort(want)
			if got := strings.Fields(git(t, fx.dir(), "--no-replace-objects", "ls-tree", "-r", "--name-only", c)); !slices.Equal(got, want) {
				t.Errorf("commit tree = %v, want HEAD's real tree plus %s: %v", got, rel, want)
			}
			if got := strings.TrimSpace(git(t, fx.dir(), "--no-replace-objects", "rev-parse", c+"^")); got != base {
				t.Errorf("parent = %s, want %s", got, base)
			}
		})
	}
}

func orphanCommit(t *testing.T, dir, tree string, parents ...string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", "x"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return strings.TrimSpace(git(t, dir, args...))
}

func TestPushBranchLandsTheCommitOnTheNamedBranch(t *testing.T) {
	fx, bare := withRemote(t)
	pushProtocol = "file"
	t.Cleanup(func() { pushProtocol = "ssh" })
	base := head(t, fx.dir())
	git(t, fx.dir(), "checkout", "-q", "-b", "side")
	git(t, fx.dir(), "config", "remote.origin.push", "refs/heads/*:refs/heads/other")
	git(t, fx.dir(), "config", "push.default", "nothing")
	next := orphanCommit(t, fx.dir(), base+"^{tree}", base)

	if err := PushBranch(context.Background(), fx.dir(), bare, next, "main"); err != nil {
		t.Fatal(err)
	}
	if got := rev(t, bare, "main"); got != next {
		t.Errorf("remote main = %s, want %s", got, next)
	}
	if out := git(t, bare, "branch", "--list", "other"); strings.TrimSpace(out) != "" {
		t.Errorf("the checkout's push refspec leaked: %q", out)
	}
}

func TestPushBranchNonFastForwardLeavesRemoteUnchanged(t *testing.T) {
	fx, bare := withRemote(t)
	pushProtocol = "file"
	t.Cleanup(func() { pushProtocol = "ssh" })
	base := head(t, fx.dir())
	diverged := orphanCommit(t, fx.dir(), base+"^{tree}")

	if err := PushBranch(context.Background(), fx.dir(), bare, diverged, "main"); err == nil {
		t.Fatal("a non-fast-forward push succeeded")
	}
	assertMain(t, bare, base)
}
