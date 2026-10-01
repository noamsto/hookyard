package main

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
)

func (sb *sandbox) gitOut(dir string, args ...string) string {
	sb.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = sb.childEnv()
	out, err := cmd.Output()
	if err != nil {
		sb.t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func (sb *sandbox) head(dir string) string {
	sb.t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "HEAD")
	cmd.Env = sb.childEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestLintMoveFlaggedCommitsDeletions(t *testing.T) {
	sb := newSandbox(t, "personal")
	flagged := newFact("linky-fact", "demo", "project")
	flagged.Body = "see https://example.com/docs for details\n"
	sb.putFact(sb.personal, "demo/linky-fact.md", flagged)
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	sb.indexWrite()
	sb.git(sb.personal, "add", "-A")
	sb.git(sb.personal, "commit", "-q", "-m", "seed")

	res := sb.run("", "lint", "--move-flagged")
	wantExit(t, res, 0)
	wantContains(t, "lint --move-flagged", res.stdout, "moved demo/linky-fact.md")
	if subject := strings.TrimSpace(sb.gitOut(sb.personal, "log", "-1", "--format=%s")); subject != "priors: move flagged facts to the local layer" {
		t.Errorf("subject = %q, stderr: %s", subject, res.stderr)
	}
	changes := sb.gitOut(sb.personal, "log", "-1", "--name-status", "--format=")
	wantContains(t, "last commit", changes, "D\tdemo/linky-fact.md", "M\tMEMORY.md")
	if status := sb.gitOut(sb.personal, "status", "--porcelain"); status != "" {
		t.Errorf("checkout still dirty: %q", status)
	}
	committest.AssertHeadIndexInTree(t, sb.personal)
}

func TestIndexWriteCommitsHandAddedFact(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))

	res := sb.run("", "index", "--write")
	wantExit(t, res, 0)
	if subject := strings.TrimSpace(sb.gitOut(sb.personal, "log", "-1", "--format=%s")); subject != "priors: regenerate index" {
		t.Errorf("subject = %q, stderr: %s", subject, res.stderr)
	}
	files := strings.Fields(sb.gitOut(sb.personal, "ls-files"))
	if !slices.Equal(files, []string{"MEMORY.md", "demo/clean-fact.md"}) {
		t.Errorf("committed files = %v", files)
	}
	committest.AssertHeadIndexInTree(t, sb.personal)

	before := sb.head(sb.personal)
	wantExit(t, sb.run("", "index", "--write"), 0)
	if sb.head(sb.personal) != before {
		t.Error("an unchanged index produced another commit")
	}
}

func TestIndexWriteLeavesUngatedFactUncommitted(t *testing.T) {
	sb := newSandbox(t, "personal")
	flagged := newFact("linky-fact", "demo", "project")
	flagged.Body = "see https://example.com/docs for details\n"
	sb.putFact(sb.personal, "demo/linky-fact.md", flagged)

	res := sb.run("", "index", "--write")
	wantExit(t, res, 0)
	wantContains(t, "index --write stderr", res.stderr, "warning", "demo/linky-fact.md")
	if strings.Contains(res.stderr, "example.com") {
		t.Errorf("stderr repeats fact content: %s", res.stderr)
	}
	if h := sb.head(sb.personal); h != "" {
		t.Errorf("HEAD = %s, want no commit", h)
	}
	committest.AssertHeadIndexInTree(t, sb.personal)
}

func TestAddPrintsIndexReports(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	sb.writeFile(filepath.Join(sb.personal, "demo", "broken.md"), "no frontmatter\n")

	res := sb.run("", "add", "--name", "reported-fact", "--description", "a fact", "--type", "project",
		"--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	wantContains(t, "add stderr", res.stderr, "skipped demo/broken.md", "warning:")
	if h := sb.head(sb.personal); h != "" {
		t.Errorf("HEAD = %s, want no commit while broken.md is in the checkout", h)
	}
	committest.AssertHeadIndexInTree(t, sb.personal)
}

func TestLintWorkFlagsCannotNarrowTheCommitGate(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.writeConfig(`work_names = ["acme-corp"]`)
	flagged := newFact("linky-fact", "demo", "project")
	flagged.Body = "see https://example.com/docs for details\n"
	sb.putFact(sb.personal, "demo/linky-fact.md", flagged)
	sb.indexWrite()
	sb.git(sb.personal, "add", "-A")
	sb.git(sb.personal, "commit", "-q", "-m", "seed")
	before := sb.head(sb.personal)
	leak := newFact("leak-fact", "demo", "project")
	leak.Body = "acme-corp deploys on fridays\n"
	sb.putFact(sb.personal, "demo/leak-fact.md", leak)

	res := sb.run("", "lint", "--move-flagged", "--work-name", "zzz", "--work-org", "github.com/zzz")

	wantContains(t, "lint", res.stdout, "demo/leak-fact.md: work-name:")
	wantContains(t, "lint stderr", res.stderr, "nothing committed", "demo/leak-fact.md")
	if got := sb.head(sb.personal); got != before {
		t.Errorf("HEAD moved to %s, committing %q", got, sb.gitOut(sb.personal, "log", "-1", "--name-status", "--format="))
	}
}
