package commit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func reviewRequest(t *testing.T, name, body string) ReviewRequest {
	t.Helper()
	f := cleanFact(name)
	f.Metadata.Confidence = "reviewed"
	f.Body = body
	return ReviewRequest{
		Rel:    "_global/" + name + ".md",
		Data:   marshal(t, f),
		Branch: "review/laptop/" + name + "-0123abcd",
		Msg:    "priors: review " + name + "\n\npriors flagged this fact for review.\n",
	}
}

func (fx fixture) review(t *testing.T, req ReviewRequest) (ReviewOutcome, string) {
	t.Helper()
	return Review(context.Background(), fx.cfg, fx.root, fx.rules, fx.scanner, req)
}

// checkoutState is everything Review must leave alone in the checkout: HEAD,
// the index, the worktree and every ref.
func checkoutState(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join([]string{
		head(t, dir),
		git(t, dir, "ls-files", "-s"),
		git(t, dir, "status", "--porcelain", "--untracked-files=all", "--ignored"),
		git(t, dir, "for-each-ref"),
	}, "\n--\n")
}

func assertUntouched(t *testing.T, dir, before string) {
	t.Helper()
	if after := checkoutState(t, dir); after != before {
		t.Errorf("Review changed the checkout:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// remoteRef is ref's commit in bare, or "" when bare lacks it.
func remoteRef(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", "-q", "--verify", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func assertNoReviewRefs(t *testing.T, bare string) {
	t.Helper()
	if refs := git(t, bare, "for-each-ref", "refs/heads/review/"); refs != "" {
		t.Errorf("remote holds review refs:\n%s", refs)
	}
}

// commitToMain commits path=content on the checkout's main and pushes it, so
// the upstream Review builds on holds it.
func commitToMain(t *testing.T, fx fixture, rel string, content []byte) {
	t.Helper()
	writeFile(t, filepath.Join(fx.dir(), filepath.FromSlash(rel)), content)
	git(t, fx.dir(), "add", "--", rel)
	git(t, fx.dir(), "commit", "-q", "-m", "add "+rel)
	git(t, fx.dir(), "push", "-q", "origin", "main")
}

// pointUpstream makes the checkout's tracking ref hold a commit of tree on
// main, without the remote: Review's base is the tracking ref as fetched.
func pointUpstream(t *testing.T, fx fixture, tree string) {
	t.Helper()
	commit := strings.TrimSpace(git(t, fx.dir(), "commit-tree", tree, "-p", "HEAD", "-m", "base"))
	git(t, fx.dir(), "update-ref", "refs/remotes/origin/main", commit)
}

func TestReviewPushesBasePlusOneFile(t *testing.T) {
	fx, bare := withRemote(t)
	req := reviewRequest(t, "flagged-fact", "plain body\n")
	base := rev(t, bare, "main")
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, req); outcome != ReviewPushed || detail != "" {
		t.Fatalf("Review = %d, %q; want pushed", outcome, detail)
	}
	ref := "refs/heads/" + req.Branch
	if got := rev(t, bare, ref+"^"); got != base {
		t.Errorf("review commit's parent = %s, want main %s", got, base)
	}
	if got := git(t, bare, "diff-tree", "-r", "--name-status", "main", ref); got != "A\t"+req.Rel+"\n" {
		t.Errorf("review tree differs from main by %q, want one added %s", got, req.Rel)
	}
	if got := git(t, bare, "show", ref+":"+req.Rel); got != string(req.Data) {
		t.Errorf("review file =\n%s\nwant\n%s", got, req.Data)
	}
	if got := git(t, bare, "log", "-1", "--format=%B", ref); strings.TrimRight(got, "\n") != strings.TrimRight(req.Msg, "\n") {
		t.Errorf("review message = %q, want %q", got, req.Msg)
	}
	assertMain(t, bare, base)
	assertUntouched(t, fx.dir(), before)
}

func TestReviewSecondCallIsExists(t *testing.T) {
	fx, bare := withRemote(t)
	req := reviewRequest(t, "flagged-fact", "plain body\n")
	if outcome, detail := fx.review(t, req); outcome != ReviewPushed {
		t.Fatalf("first Review = %d, %q; want pushed", outcome, detail)
	}
	pushed := remoteRef(t, bare, "refs/heads/"+req.Branch)
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, req); outcome != ReviewExists || detail != "" {
		t.Errorf("second Review = %d, %q; want exists", outcome, detail)
	}
	if got := remoteRef(t, bare, "refs/heads/"+req.Branch); got != pushed {
		t.Errorf("review branch moved: %s -> %s", pushed, got)
	}
	assertUntouched(t, fx.dir(), before)
}

// A branch pushed before a crash left its request pending is sent, whatever
// the base holds by the retry.
func TestReviewExistingBranchWinsOverBaseChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, fx fixture, req ReviewRequest)
	}{
		{"base holds the path", func(t *testing.T, fx fixture, req ReviewRequest) {
			commitToMain(t, fx, req.Rel, req.Data)
		}},
		{"base would not lint", func(t *testing.T, fx fixture, _ ReviewRequest) {
			commitToMain(t, fx, "_global/broken.md", []byte("not a fact\n"))
		}},
		{"base holds a symlink", func(t *testing.T, fx fixture, _ ReviewRequest) {
			if err := os.Symlink("base-fact.md", filepath.Join(fx.dir(), "_global", "link.md")); err != nil {
				t.Fatal(err)
			}
			git(t, fx.dir(), "add", "--", "_global/link.md")
			git(t, fx.dir(), "commit", "-q", "-m", "link")
			git(t, fx.dir(), "push", "-q", "origin", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, _ := withRemote(t)
			req := reviewRequest(t, "flagged-fact", "plain body\n")
			if outcome, detail := fx.review(t, req); outcome != ReviewPushed {
				t.Fatalf("first Review = %d, %q; want pushed", outcome, detail)
			}
			tc.change(t, fx, req)
			before := checkoutState(t, fx.dir())

			if outcome, detail := fx.review(t, req); outcome != ReviewExists || detail != "" {
				t.Errorf("Review = %d, %q; want exists", outcome, detail)
			}
			assertUntouched(t, fx.dir(), before)
		})
	}
}

func TestReviewRefusesBaseTreeWithoutPlainFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		tree func(t *testing.T, dir string) string
		want string
	}{
		{"symlink", func(t *testing.T, dir string) string {
			oid := strings.TrimSpace(git(t, dir, "hash-object", "-w", "--stdin"))
			return mktree(t, dir, "120000 blob "+oid+"\tlink.md")
		}, "base tree holds a symlink at link.md"},
		{"submodule", func(t *testing.T, dir string) string {
			return mktree(t, dir, "160000 commit "+head(t, dir)+"\tsub")
		}, "base tree holds a submodule at sub"},
		{"non-local path", func(t *testing.T, dir string) string {
			oid := strings.TrimSpace(git(t, dir, "hash-object", "-w", "--stdin"))
			return mktree(t, dir, "100644 blob "+oid+"\t..")
		}, "base tree holds a non-local path at .."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, bare := withRemote(t)
			pointUpstream(t, fx, tc.tree(t, fx.dir()))
			before := checkoutState(t, fx.dir())

			if outcome, detail := fx.review(t, reviewRequest(t, "flagged-fact", "plain body\n")); outcome != ReviewTransient || detail != tc.want {
				t.Errorf("Review = %d, %q; want transient %q", outcome, detail, tc.want)
			}
			assertNoReviewRefs(t, bare)
			assertUntouched(t, fx.dir(), before)
		})
	}
}

func mktree(t *testing.T, dir, entry string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "mktree")
	cmd.Stdin = strings.NewReader(entry + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git mktree: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReviewRefusesSecretBytesBeforeHashing(t *testing.T) {
	for _, tc := range []struct {
		name, body, want, leak string
	}{
		{"scanner", "token " + scannerHit + "\n", "secret scan matched: fake", scannerHit},
		{"redaction rule", "-----BEGIN " + "RSA PRIVATE" + " KEY-----\n", "secret rule(s): private-key", "BEGIN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, bare := withRemote(t)
			req := reviewRequest(t, "flagged-fact", tc.body)
			before := checkoutState(t, fx.dir())

			outcome, detail := fx.review(t, req)
			if outcome != ReviewPermanent || detail != tc.want {
				t.Errorf("Review = %d, %q; want permanent %q", outcome, detail, tc.want)
			}
			if strings.Contains(detail, tc.leak) {
				t.Errorf("detail %q repeats the matched text", detail)
			}
			cmd := exec.Command("git", "-C", fx.dir(), "hash-object", "--stdin")
			cmd.Stdin = strings.NewReader(string(req.Data))
			oid, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if exec.Command("git", "-C", fx.dir(), "cat-file", "-e", strings.TrimSpace(string(oid))).Run() == nil {
				t.Error("the refused bytes were hashed into the checkout's objects")
			}
			assertNoReviewRefs(t, bare)
			assertUntouched(t, fx.dir(), before)
		})
	}
}

func TestReviewRefusesPathOnBase(t *testing.T) {
	fx, bare := withRemote(t)
	req := reviewRequest(t, "flagged-fact", "plain body\n")
	commitToMain(t, fx, req.Rel, marshal(t, cleanFact("flagged-fact")))
	before := checkoutState(t, fx.dir())

	want := req.Rel + " already exists on origin/main"
	if outcome, detail := fx.review(t, req); outcome != ReviewPermanent || detail != want {
		t.Errorf("Review = %d, %q; want permanent %q", outcome, detail, want)
	}
	assertNoReviewRefs(t, bare)
	assertUntouched(t, fx.dir(), before)
}

func TestReviewLintFindingIsTransient(t *testing.T) {
	fx, bare := withRemote(t)
	req := reviewRequest(t, "flagged-fact", "see [[no-such-fact]]\n")
	before := checkoutState(t, fx.dir())

	outcome, detail := fx.review(t, req)
	if want := "lint: " + req.Rel + ": wikilink-dangling"; outcome != ReviewTransient || detail != want {
		t.Errorf("Review = %d, %q; want transient %q", outcome, detail, want)
	}
	if strings.Contains(detail, "no-such-fact") {
		t.Errorf("detail %q quotes the fact's text", detail)
	}
	assertNoReviewRefs(t, bare)
	assertUntouched(t, fx.dir(), before)
}

// The lint reads base's blobs as stored: git archive would drop the
// export-ignore'd fact the review file links to, and checkout-index would run
// the smudge filter.
func TestReviewLintReadsRawBlobs(t *testing.T) {
	fx, bare := withRemote(t)
	commitToMain(t, fx, ".gitattributes", []byte("_global/base-fact.md export-ignore\n* filter=probe\n"))
	marker := filepath.Join(t.TempDir(), "smudged")
	git(t, fx.dir(), "config", "filter.probe.smudge", "touch '"+marker+"'; cat")
	req := reviewRequest(t, "flagged-fact", "see [[base-fact]]\n")
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, req); outcome != ReviewPushed || detail != "" {
		t.Fatalf("Review = %d, %q; want pushed", outcome, detail)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the smudge filter ran")
	}
	if remoteRef(t, bare, "refs/heads/"+req.Branch) == "" {
		t.Error("no review branch on the remote")
	}
	assertUntouched(t, fx.dir(), before)
}

func TestReviewNeedsPushOn(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		off  func(fx *fixture)
	}{
		{"push off", func(fx *fixture) { fx.cfg.Push = false }},
		{"commit off", func(fx *fixture) { fx.cfg.Commit = &off }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, bare := withRemote(t)
			tc.off(&fx)
			before := checkoutState(t, fx.dir())

			if outcome, detail := fx.review(t, reviewRequest(t, "flagged-fact", "plain body\n")); outcome != ReviewTransient || detail != "push is off" {
				t.Errorf("Review = %d, %q; want transient \"push is off\"", outcome, detail)
			}
			assertNoReviewRefs(t, bare)
			assertUntouched(t, fx.dir(), before)
		})
	}
}

func TestReviewNeedsUpstream(t *testing.T) {
	fx, bare := withRemote(t)
	git(t, fx.dir(), "branch", "--unset-upstream")
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, reviewRequest(t, "flagged-fact", "plain body\n")); outcome != ReviewTransient || !strings.Contains(detail, "main has no upstream") {
		t.Errorf("Review = %d, %q; want transient, no upstream", outcome, detail)
	}
	assertNoReviewRefs(t, bare)
	assertUntouched(t, fx.dir(), before)
}

// The empty lease refuses a branch another run created between the remote
// check and the push, which is then as good as sent.
func TestReviewBranchCreatedDuringPushIsExists(t *testing.T) {
	fx, bare := withRemote(t)
	req := reviewRequest(t, "flagged-fact", "plain body\n")
	ref := "refs/heads/" + req.Branch
	main := rev(t, bare, "main")
	beforePush = func() { git(t, bare, "update-ref", ref, main) }
	t.Cleanup(func() { beforePush = func() {} })
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, req); outcome != ReviewExists || detail != "" {
		t.Errorf("Review = %d, %q; want exists", outcome, detail)
	}
	if got := remoteRef(t, bare, ref); got != main {
		t.Errorf("remote %s = %s, want the other run's %s", ref, got, main)
	}
	assertUntouched(t, fx.dir(), before)
}

func TestReviewPushFailureIsTransient(t *testing.T) {
	fx, bare := withRemote(t)
	failPushes(t, bare, "rejected")
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, reviewRequest(t, "flagged-fact", "plain body\n")); outcome != ReviewTransient || !strings.Contains(detail, "git push failed") {
		t.Errorf("Review = %d, %q; want transient, failed push", outcome, detail)
	}
	assertNoReviewRefs(t, bare)
	assertUntouched(t, fx.dir(), before)
}
