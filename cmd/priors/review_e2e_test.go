package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/review"
)

const (
	flaggedName = "nix-check-first"
	// flaggedBody tells the reader to run a command, so an import by a
	// session that ingested shell output keeps provenance:shell.
	flaggedBody = "Run `nix flake check --all-systems` before every push.\n"
	noteName    = "ci-flake-check"
	// noteBody only mentions a command, so the same import publishes it.
	noteBody = "The CI job runs `nix flake check --all-systems` nightly and reports failures to the team channel.\n"
)

// reviewFixture is a personal store pinned to a bare remote, a repo routed to
// it and a session that ingested shell output, which imports native memory.
type reviewFixture struct {
	sb      *sandbox
	pin     pinnedStore
	repo    string
	project string
}

func newReviewFixture(t *testing.T, push bool) reviewFixture {
	t.Helper()
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	sb.toolCall("pre_tool", "sess-1", "Bash", `{"command":"gh pr view 1"}`)
	pin := sb.pinPersonal()
	if push {
		sb.writeConfig("push = true")
	}
	return reviewFixture{
		sb:      sb,
		pin:     pin,
		repo:    repo,
		project: regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(repo, "-"),
	}
}

// native writes a Claude native memory topic file that author-session wrote.
func (fx reviewFixture) native(name, description, body string) {
	fx.sb.t.Helper()
	path := filepath.Join(fx.sb.dir, ".claude", "projects", fx.project, "memory", name+".md")
	fx.sb.writeFile(path, "---\nname: "+name+"\ndescription: "+description+"\nmetadata:\n  type: project\n  originSessionId: author-session\n---\n\n"+body)
}

func (fx reviewFixture) sourcePath(name string) string {
	return "~/.claude/projects/" + fx.project + "/memory/" + name + ".md"
}

func (fx reviewFixture) localPath(name string) string {
	return filepath.Join(fx.sb.state, "local", "personal", "demo", name+".md")
}

func (fx reviewFixture) pendingDir() string {
	return filepath.Join(fx.sb.state, "review", "personal", "pending")
}

// reviewRefs lists the bare remote's review branches.
func (fx reviewFixture) reviewRefs() []string {
	fx.sb.t.Helper()
	return strings.Fields(fx.sb.gitOut(fx.pin.bare, "for-each-ref", "--format=%(refname)", "refs/heads/review/"))
}

// pending lists the queued review markers; a missing queue holds none.
func (fx reviewFixture) pending() []string {
	fx.sb.t.Helper()
	ents, err := os.ReadDir(fx.pendingDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		fx.sb.t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func (fx reviewFixture) importRun() result {
	fx.sb.t.Helper()
	res := fx.sb.run("", "import", "--session", "sess-1")
	wantExit(fx.sb.t, res, 0)
	return res
}

// refNames lists the checkout's ref names, which a review never adds to.
func (fx reviewFixture) refNames() []string {
	fx.sb.t.Helper()
	return strings.Fields(fx.sb.gitOut(fx.sb.personal, "for-each-ref", "--format=%(refname)"))
}

func wantLine(t *testing.T, what, out, line string) {
	t.Helper()
	if !slices.Contains(strings.Split(out, "\n"), line) {
		t.Errorf("%s lacks the line %q:\n%s", what, line, out)
	}
}

// importFlagged imports an instruction-shaped and a note-shaped memory under
// an ingesting session with push on, and returns the review branch the
// flagged one was proposed on.
func importFlagged(t *testing.T) (reviewFixture, string) {
	t.Helper()
	fx := newReviewFixture(t, true)
	fx.native(flaggedName, "the check before pushing", flaggedBody)
	fx.native(noteName, "what the nightly CI job does", noteBody)
	res := fx.importRun()
	refs := fx.reviewRefs()
	if len(refs) != 1 {
		t.Fatalf("review refs on the remote = %v, want one\nstdout: %s\nstderr: %s", refs, res.stdout, res.stderr)
	}
	return fx, strings.TrimPrefix(refs[0], "refs/heads/")
}

func TestImportFlaggedFactOpensReviewBranch(t *testing.T) {
	fx := newReviewFixture(t, true)
	sb := fx.sb
	fx.native(flaggedName, "the check before pushing", flaggedBody)
	fx.native(noteName, "what the nightly CI job does", noteBody)
	refsBefore := fx.refNames()

	res := fx.importRun()
	wantContains(t, "import stdout", res.stdout,
		"flagged personal "+fx.localPath(flaggedName)+" <- "+fx.sourcePath(flaggedName)+": provenance:shell",
		"published personal "+filepath.Join(sb.personal, "demo", noteName+".md")+" <- "+fx.sourcePath(noteName))
	if strings.Contains(res.stdout, "https://") {
		t.Errorf("a file:// remote has no compare URL, stdout:\n%s", res.stdout)
	}

	refs := fx.reviewRefs()
	if len(refs) != 1 {
		t.Fatalf("review refs on the remote = %v, want one\nstderr: %s", refs, res.stderr)
	}
	branch := strings.TrimPrefix(refs[0], "refs/heads/")
	wantLine(t, "import stdout", res.stdout, "review personal "+branch)

	rel := "demo/" + flaggedName + ".md"
	data := sb.gitOut(fx.pin.bare, "show", branch+":"+rel)
	f, err := fact.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Metadata.Confidence != "reviewed" || len(f.Metadata.Flags) != 0 || strings.Contains(data, "flags:") {
		t.Errorf("review file is not reviewed and unflagged:\n%s", data)
	}
	if f.Metadata.Provenance == nil {
		t.Fatalf("review file has no provenance:\n%s", data)
	}
	if want := review.Branch(f.Metadata.Provenance.Host, flaggedName, []byte(data)); branch != want {
		t.Errorf("branch %s, want %s (host and file hash)", branch, want)
	}

	remoteMain := sb.ref(fx.pin.bare, "main")
	if parents := strings.Fields(sb.gitOut(fx.pin.bare, "rev-list", "--parents", "-n", "1", branch)); len(parents) != 2 || parents[1] != remoteMain {
		t.Errorf("review commit and parents = %v, want one parent, the remote main %s", parents, remoteMain)
	}
	if got, want := sb.gitOut(fx.pin.bare, "diff-tree", "-r", "--no-commit-id", "--name-status", branch), "A\t"+rel+"\n"; got != want {
		t.Errorf("review commit changes %q, want %q", got, want)
	}
	if sb.ref(fx.pin.bare, branch+":MEMORY.md") != sb.ref(fx.pin.bare, "main:MEMORY.md") {
		t.Error("the review branch changed MEMORY.md")
	}
	wantContains(t, "review commit message", sb.gitOut(fx.pin.bare, "log", "-1", "--format=%B", branch),
		"priors: review "+flaggedName, "flagged by: provenance:shell", "path "+fx.sourcePath(flaggedName),
		"session author-session", "priors lint ran locally")
	if !strings.Contains(sb.gitOut(fx.pin.bare, "ls-tree", "-r", "--name-only", "main"), "demo/"+noteName+".md") {
		t.Error("the note-shaped memory did not publish to the remote main")
	}

	if got := sb.head(sb.personal); got != remoteMain {
		t.Errorf("checkout HEAD %s, want the published main %s", got, remoteMain)
	}
	if st := sb.gitOut(sb.personal, "status", "--porcelain"); st != "" {
		t.Errorf("checkout status after the review:\n%s", st)
	}
	if after := fx.refNames(); !slices.Equal(after, refsBefore) {
		t.Errorf("checkout refs %v, were %v", after, refsBefore)
	}
	if _, err := os.Stat(filepath.Join(sb.personal, rel)); err == nil {
		t.Error("the flagged fact landed in the checkout")
	}
	if p := fx.pending(); len(p) != 0 {
		t.Errorf("pending markers %v after a pushed review", p)
	}

	again := fx.importRun()
	wantContains(t, "re-import stdout", again.stdout, "unchanged "+fx.sourcePath(flaggedName), "unchanged "+fx.sourcePath(noteName))
	if strings.Contains(again.stdout, "review personal") {
		t.Errorf("re-import proposed again:\n%s", again.stdout)
	}
	if got := fx.reviewRefs(); !slices.Equal(got, refs) {
		t.Errorf("review refs after a re-import %v, want %v", got, refs)
	}
}

func TestMergedReviewReadsReviewedOnce(t *testing.T) {
	fx, branch := importFlagged(t)
	sb := fx.sb

	scratch := filepath.Join(sb.dir, "scratch")
	sb.git(sb.dir, "clone", "-q", fx.pin.url, scratch)
	sb.git(scratch, "merge", "-q", "--no-ff", "-m", "Merge "+branch, "origin/"+branch)
	sb.git(scratch, "push", "-q", "origin", "main")
	sb.git(sb.personal, "pull", "-q", "--ff-only")

	lint := sb.run("", "lint")
	wantExit(t, lint, 0)
	if lint.stdout+lint.stderr != "" {
		t.Errorf("lint after the merge is not clean:\nstdout: %s\nstderr: %s", lint.stdout, lint.stderr)
	}

	idx := sb.run("", "index", "--write")
	wantExit(t, idx, 0)
	if _, err := os.Stat(fx.localPath(flaggedName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the local copy outlived the merge: %v", err)
	}
	wantContains(t, "MEMORY.md", sb.readFile(filepath.Join(sb.personal, "MEMORY.md")), "["+flaggedName+"](demo/"+flaggedName+".md)")
	if strings.Contains(idx.stdout, "review personal") {
		t.Errorf("index --write proposed the merged fact again:\n%s", idx.stdout)
	}

	start := sb.run(sessionStartEnvelope(fx.repo))
	wantExit(t, start, 0)
	if n := strings.Count(additionalContext(t, start.stdout), "["+flaggedName+"]("); n != 1 {
		t.Errorf("session_start lists %s %d times, want once:\n%s", flaggedName, n, start.stdout)
	}

	show := sb.run("", "show", flaggedName, "--cwd", fx.repo)
	wantExit(t, show, 0)
	wantContains(t, "show", show.stdout, "confidence: reviewed")

	flagged := sb.run("", "list", "--flagged", "--cwd", fx.repo)
	wantExit(t, flagged, 0)
	if strings.Contains(flagged.stdout, flaggedName) {
		t.Errorf("list --flagged still lists the merged fact:\n%s", flagged.stdout)
	}
}

// A review PR leaves MEMORY.md alone, so the first write after the pull heals
// the checkout's index before it prunes the local copy.
func TestMergedReviewThenUnchangedImportHealsIndex(t *testing.T) {
	fx, branch := importFlagged(t)
	sb := fx.sb

	scratch := filepath.Join(sb.dir, "scratch")
	sb.git(sb.dir, "clone", "-q", fx.pin.url, scratch)
	sb.git(scratch, "merge", "-q", "--no-ff", "-m", "Merge "+branch, "origin/"+branch)
	sb.git(scratch, "push", "-q", "origin", "main")
	sb.git(sb.personal, "pull", "-q", "--ff-only")

	res := fx.importRun()
	wantContains(t, "import stdout", res.stdout, "unchanged "+fx.sourcePath(flaggedName), "unchanged "+fx.sourcePath(noteName))
	if _, err := os.Stat(fx.localPath(flaggedName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the local copy outlived the merge: %v", err)
	}
	entry := "[" + flaggedName + "](demo/" + flaggedName + ".md)"
	wantContains(t, "checkout MEMORY.md", sb.readFile(filepath.Join(sb.personal, "MEMORY.md")), entry)
	wantContains(t, "committed MEMORY.md", sb.gitOut(sb.personal, "show", "HEAD:MEMORY.md"), entry)
	if st := sb.gitOut(sb.personal, "status", "--porcelain"); st != "" {
		t.Errorf("checkout status after the import:\n%s", st)
	}

	start := sb.run(sessionStartEnvelope(fx.repo))
	wantExit(t, start, 0)
	if n := strings.Count(additionalContext(t, start.stdout), "["+flaggedName+"]("); n != 1 {
		t.Errorf("session_start lists %s %d times, want once:\n%s", flaggedName, n, start.stdout)
	}
}

func TestClosedReviewIsNotReproposed(t *testing.T) {
	fx, branch := importFlagged(t)
	sb := fx.sb

	again := fx.importRun()
	wantContains(t, "re-import stdout", again.stdout, "unchanged "+fx.sourcePath(flaggedName))

	sb.git(fx.pin.bare, "update-ref", "-d", "refs/heads/"+branch)
	reimport := fx.importRun()
	wantContains(t, "import after close stdout", reimport.stdout, "unchanged "+fx.sourcePath(flaggedName))
	idx := sb.run("", "index", "--write")
	wantExit(t, idx, 0)
	for _, out := range []string{reimport.stdout, idx.stdout} {
		if strings.Contains(out, "review personal") {
			t.Errorf("a closed review was proposed again:\n%s", out)
		}
	}
	if refs := fx.reviewRefs(); len(refs) != 0 {
		t.Errorf("review refs after closing %v, want none", refs)
	}
	if p := fx.pending(); len(p) != 0 {
		t.Errorf("pending markers %v after closing", p)
	}
	if _, err := os.Stat(fx.localPath(flaggedName)); err != nil {
		t.Errorf("the local copy is gone after closing: %v", err)
	}
}

func TestSecretFactNeverReachesReviewBranch(t *testing.T) {
	fx := newReviewFixture(t, true)
	sb := fx.sb

	fx.native("leaky", "a key", "the key is "+"AK"+"IA"+"IOSFODNN7EXAMPLE\n")
	res := fx.importRun()
	wantContains(t, "import stdout", res.stdout, "skipped "+fx.sourcePath("leaky")+": refused: ")
	if refs := fx.reviewRefs(); len(refs) != 0 {
		t.Errorf("review refs %v after a refused import", refs)
	}
	if p := fx.pending(); len(p) != 0 {
		t.Errorf("pending markers %v after a refused import", p)
	}

	// A local copy tampered after it was queued meets the same gates before
	// its push.
	const name = "tampered-key"
	f := newFact(name, "demo", "project")
	f.Metadata.Flags = []string{"provenance:shell"}
	f.Body = "-----BEGIN " + "RSA PRIVATE" + " KEY-----\n"
	sb.putFact(filepath.Join(sb.state, "local", "personal"), "demo/"+name+".md", f)
	sb.writeFile(filepath.Join(fx.pendingDir(), name), "")

	idx := sb.run("", "index", "--write")
	wantExit(t, idx, 0)
	wantContains(t, "index --write stderr", idx.stderr, "review "+name+": not proposed: secret rule(s): private-key")
	if strings.Contains(idx.stdout+idx.stderr, "BEGIN") {
		t.Errorf("index --write echoed the secret:\nstdout: %s\nstderr: %s", idx.stdout, idx.stderr)
	}
	if refs := fx.reviewRefs(); len(refs) != 0 {
		t.Errorf("review refs %v after a secret proposal", refs)
	}
	if p := fx.pending(); len(p) != 0 {
		t.Errorf("pending markers %v after a permanent refusal", p)
	}
}

func TestPushOffQueuesUntilPushIsOn(t *testing.T) {
	fx := newReviewFixture(t, false)
	sb := fx.sb

	res := sb.run("", "add", "--name", "shell-tip", "--description", "from a shell session", "--type", "project",
		"--cwd", fx.repo, "--session", "sess-1")
	wantExit(t, res, 0)
	wantContains(t, "add stdout", res.stdout, "flagged personal "+fx.localPath("shell-tip"), "provenance:shell")
	const waiting = "warning: review: 1 flagged fact(s) wait for push = true to be proposed"
	if n := strings.Count(res.stderr, "review"); n != 1 || !strings.Contains(res.stderr, waiting) {
		t.Errorf("add stderr mentions review %d times, want the one line %q:\n%s", n, waiting, res.stderr)
	}
	if refs := fx.reviewRefs(); len(refs) != 0 {
		t.Errorf("review refs %v with push off", refs)
	}
	if p := fx.pending(); !slices.Equal(p, []string{"shell-tip"}) {
		t.Errorf("pending markers %v, want the flagged fact", p)
	}

	sb.writeConfig("push = true")
	idx := sb.run("", "index", "--write")
	wantExit(t, idx, 0)
	refs := fx.reviewRefs()
	if len(refs) != 1 {
		t.Fatalf("review refs %v after push = true, want one\nstderr: %s", refs, idx.stderr)
	}
	wantLine(t, "index --write stdout", idx.stdout, "review personal "+strings.TrimPrefix(refs[0], "refs/heads/"))
	if p := fx.pending(); len(p) != 0 {
		t.Errorf("pending markers %v after the push", p)
	}
}
