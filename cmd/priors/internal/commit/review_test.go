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

// bareRef is ref's commit in bare, or "" when bare lacks it.
func bareRef(t *testing.T, bare, ref string) string {
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

// pointUpstream makes the remote's main and the checkout's tracking ref hold
// a commit of tree on HEAD, leaving the checkout's main behind.
func pointUpstream(t *testing.T, fx fixture, tree string) {
	t.Helper()
	commit := pointTracking(t, fx, tree)
	git(t, fx.dir(), "push", "-q", "origin", commit+":refs/heads/main")
}

// pointTracking makes only the checkout's tracking ref hold a commit of tree
// on HEAD, which the remote lacks, and returns the commit.
func pointTracking(t *testing.T, fx fixture, tree string) string {
	t.Helper()
	commit := strings.TrimSpace(git(t, fx.dir(), "commit-tree", tree, "-p", "HEAD", "-m", "base"))
	git(t, fx.dir(), "update-ref", "refs/remotes/origin/main", commit)
	return commit
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
	pushed := bareRef(t, bare, "refs/heads/"+req.Branch)
	before := checkoutState(t, fx.dir())

	if outcome, detail := fx.review(t, req); outcome != ReviewExists || detail != "" {
		t.Errorf("second Review = %d, %q; want exists", outcome, detail)
	}
	if got := bareRef(t, bare, "refs/heads/"+req.Branch); got != pushed {
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

// evilTree is tree with a local-only .github/workflows/evil.yml added, and
// that file's blob.
func evilTree(t *testing.T, dir, tree string) (evil, blob string) {
	t.Helper()
	blob = strings.TrimSpace(gitIn(t, dir, "on: push # local only\n", "hash-object", "-w", "--stdin"))
	workflows := mktree(t, dir, "100644 blob "+blob+"\tevil.yml")
	github := mktree(t, dir, "040000 tree "+workflows+"\tworkflows")
	return strings.TrimSpace(gitIn(t, dir, git(t, dir, "ls-tree", tree)+"040000 tree "+github+"\t.github\n", "mktree")), blob
}

// A replace ref on the base tree would show the gates a tree other than the
// one the push sends.
func TestReviewIgnoresReplaceRefs(t *testing.T) {
	fx, bare := withRemote(t)
	base := rev(t, bare, "main")
	baseTree := rev(t, fx.dir(), base+"^{tree}")
	evil, blob := evilTree(t, fx.dir(), baseTree)
	git(t, fx.dir(), "replace", "-f", baseTree, evil)
	req := reviewRequest(t, "flagged-fact", "plain body\n")

	outcome, detail := fx.review(t, req)
	if exec.Command("git", "-C", bare, "cat-file", "-e", blob).Run() == nil {
		t.Errorf("the remote received the local-only blob %s", blob)
	}
	if outcome != ReviewPushed || detail != "" {
		t.Fatalf("Review = %d, %q; want pushed", outcome, detail)
	}
	if got := git(t, bare, "diff-tree", "-r", "--name-status", "main", "refs/heads/"+req.Branch); got != "A\t"+req.Rel+"\n" {
		t.Errorf("review tree differs from main by %q, want one added %s", got, req.Rel)
	}
	assertMain(t, bare, base)
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

// The commit message carries the local fact's flags, so it is scanned too.
func TestReviewRefusesSecretMessage(t *testing.T) {
	for _, tc := range []struct {
		name, msg, want, leak string
	}{
		{"redaction rule", "priors: review\n\n-----BEGIN " + "RSA PRIVATE" + " KEY-----\n", "secret rule(s) in the message: private-key", "BEGIN"},
		{"scanner", "priors: review\n\ntoken " + scannerHit + "\n", "secret scan matched in the message: fake", scannerHit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, bare := withRemote(t)
			req := reviewRequest(t, "flagged-fact", "plain body\n")
			req.Msg = tc.msg
			before := checkoutState(t, fx.dir())

			outcome, detail := fx.review(t, req)
			if outcome != ReviewPermanent || detail != tc.want {
				t.Errorf("Review = %d, %q; want permanent %q", outcome, detail, tc.want)
			}
			if strings.Contains(detail, tc.leak) {
				t.Errorf("detail %q repeats the matched text", detail)
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

// A base fact of the same name elsewhere never goes away by retrying, whichever
// of the two files the lint reports.
func TestReviewDuplicateNameIsPermanent(t *testing.T) {
	for _, tc := range []struct {
		name, rel string
		repo      string
	}{
		{"reported on the review file", "_archive/_global/flagged-fact.md", ""},
		{"reported on the base file", "repo-a/flagged-fact.md", "repo-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, bare := withRemote(t)
			f := cleanFact("flagged-fact")
			if tc.repo != "" {
				f.Metadata.Scope, f.Metadata.Repos = "repo", []string{tc.repo}
			}
			fx.put(t, tc.rel, f)
			fx.index(t)
			git(t, fx.dir(), "add", "-A")
			git(t, fx.dir(), "commit", "-q", "-m", "add "+tc.rel)
			git(t, fx.dir(), "push", "-q", "origin", "main")
			req := reviewRequest(t, "flagged-fact", "plain body\n")
			before := checkoutState(t, fx.dir())

			want := "lint: " + req.Rel + ": duplicate-name"
			if outcome, detail := fx.review(t, req); outcome != ReviewPermanent || detail != want {
				t.Errorf("Review = %d, %q; want permanent %q", outcome, detail, want)
			}
			assertNoReviewRefs(t, bare)
			assertUntouched(t, fx.dir(), before)
		})
	}
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
	if bareRef(t, bare, "refs/heads/"+req.Branch) == "" {
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

// A tracking ref holding a commit the remote lacks would make the review
// branch carry that commit, and whatever it adds, to the remote.
func TestReviewRefusesTrackingRefTheRemoteLacks(t *testing.T) {
	fx, bare := withRemote(t)
	tree, blob := evilTree(t, fx.dir(), "HEAD")
	extra := pointTracking(t, fx, tree)
	base := rev(t, bare, "main")
	before := checkoutState(t, fx.dir())

	want := "refs/remotes/origin/main is not the remote's refs/heads/main; fetch first"
	if outcome, detail := fx.review(t, reviewRequest(t, "flagged-fact", "plain body\n")); outcome != ReviewTransient || detail != want {
		t.Errorf("Review = %d, %q; want transient %q", outcome, detail, want)
	}
	for _, oid := range []string{extra, blob} {
		if exec.Command("git", "-C", bare, "cat-file", "-e", oid).Run() == nil {
			t.Errorf("the remote received the local-only object %s", oid)
		}
	}
	assertNoReviewRefs(t, bare)
	assertMain(t, bare, base)
	assertUntouched(t, fx.dir(), before)
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
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
	if got := bareRef(t, bare, ref); got != main {
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
