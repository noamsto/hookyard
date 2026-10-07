package lint

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

var pinOnce sync.Once

const garbageSig = "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"

// entryFor is a well-formed v1 entry whose signature is garbage: the remote
// check never verifies one.
func entryFor(name string) []byte {
	e := attesttest.Entry("work", name, "repo-a/"+name+".md", strings.Repeat("a", 64), 1, "attest")
	return append(e, garbageSig...)
}

func reviewedFact(name string) []byte {
	f := cleanFact(name)
	f.Metadata.Confidence = "reviewed"
	b, err := f.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func gitRepo(t *testing.T) store.Root {
	t.Helper()
	pinOnce.Do(toolstest.Pin)
	root := checkout(t, route.StoreWork)
	git(t, root.Path, "init", "-q")
	return root
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := tools.Command(context.Background(), tools.Git, append([]string{
		"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
	}, args...)...)
	cmd.Env = route.RepoEnv("LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitAll(t *testing.T, root store.Root, msg string) string {
	t.Helper()
	git(t, root.Path, "add", "-A")
	git(t, root.Path, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, root.Path, "rev-parse", "HEAD")
}

func remove(t *testing.T, root store.Root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root.Path, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func keys(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.File + ":" + f.Rule
	}
	slices.Sort(out)
	return out
}

func assertFindings(t *testing.T, got []Finding, want ...string) {
	t.Helper()
	slices.Sort(want)
	if g := keys(got); !slices.Equal(g, want) {
		t.Errorf("findings = %v, want %v\nfull: %v", g, want, got)
	}
}

func TestRemoteTree(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root store.Root)
		want  []string
	}{
		{"no attest dir", func(t *testing.T, root store.Root) {
			put(t, root, "repo-a/plain.md", cleanFact("plain"))
		}, nil},
		{"non-v1 entry", func(t *testing.T, root store.Root) {
			putRaw(t, root, ".attest/x", []byte("not an entry\n"))
		}, []string{".attest/x:attest-entry"}},
		{"entry named for another", func(t *testing.T, root store.Root) {
			putRaw(t, root, ".attest/x", entryFor("y"))
		}, []string{".attest/x:attest-entry"}},
		{"subdirectory in .attest", func(t *testing.T, root store.Root) {
			putRaw(t, root, ".attest/a/b", entryFor("b"))
		}, []string{".attest/a:attest-entry"}},
		{"symlink in .attest", func(t *testing.T, root store.Root) {
			putRaw(t, root, "elsewhere", entryFor("x"))
			if err := os.MkdirAll(filepath.Join(root.Path, ".attest"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../elsewhere", filepath.Join(root.Path, ".attest/x")); err != nil {
				t.Fatal(err)
			}
		}, []string{".attest/x:attest-entry"}},
		{"reviewed fact without entry", func(t *testing.T, root store.Root) {
			putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
		}, []string{"repo-a/r.md:unattested-review"}},
		{"reviewed fact with a malformed name", func(t *testing.T, root store.Root) {
			putRaw(t, root, "repo-a/Bad_Name.md", reviewedFact("Bad_Name"))
		}, []string{"repo-a/Bad_Name.md:unattested-review"}},
		{"reviewed fact whose entry is a directory", func(t *testing.T, root store.Root) {
			putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
			putRaw(t, root, ".attest/r/inner", entryFor("inner"))
		}, []string{".attest/r:attest-entry", "repo-a/r.md:unattested-review"}},
		{"unsigned entry next to a reviewed fact", func(t *testing.T, root store.Root) {
			putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
			putRaw(t, root, ".attest/r", entryFor("r"))
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := checkout(t, route.StoreWork)
			tt.setup(t, root)
			assertFindings(t, Remote(context.Background(), root, ""), tt.want...)
		})
	}
}

func TestRemoteMessagesDoNotQuoteFacts(t *testing.T) {
	root := checkout(t, route.StoreWork)
	f := cleanFact("r")
	f.Metadata.Confidence = "reviewed"
	f.Body = "SECRET-BODY-TEXT\n"
	put(t, root, "repo-a/r.md", f)
	for _, got := range Remote(context.Background(), root, "") {
		if strings.Contains(got.String(), "SECRET-BODY-TEXT") {
			t.Errorf("finding quotes fact text: %v", got)
		}
	}
}

func TestRemoteSinceDeletedEntry(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	remove(t, root, ".attest/x")
	commitAll(t, root, "drop entry")

	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted")
}

func TestRemoteSinceEntryDeletedThenRestored(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	remove(t, root, ".attest/x")
	commitAll(t, root, "drop")
	putRaw(t, root, ".attest/x", entryFor("x"))
	commitAll(t, root, "restore")

	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted")
}

func TestRemoteSinceMiddleCommitUnattested(t *testing.T) {
	root := gitRepo(t)
	base := commitAll(t, root, "base")
	putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
	middle := commitAll(t, root, "review without entry")
	putRaw(t, root, ".attest/r", entryFor("r"))
	commitAll(t, root, "add entry")

	got := Remote(context.Background(), root, base)
	assertFindings(t, got, "repo-a/r.md:unattested-review")
	if len(got) == 0 {
		return
	}
	if !strings.Contains(got[0].Msg, middle[:12]) {
		t.Errorf("msg %q does not name commit %s", got[0].Msg, middle[:12])
	}
}

func TestRemoteSinceFactAndEntryTogether(t *testing.T) {
	root := gitRepo(t)
	base := commitAll(t, root, "base")
	putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
	putRaw(t, root, ".attest/r", entryFor("r"))
	commitAll(t, root, "reviewed with entry")
	put(t, root, "repo-a/other.md", cleanFact("other"))
	commitAll(t, root, "unrelated")

	assertFindings(t, Remote(context.Background(), root, base))
}

func TestRemoteSinceEntryReplacedBySymlinkThenRestored(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/a", entryFor("a"))
	putRaw(t, root, ".attest/b", entryFor("b"))
	base := commitAll(t, root, "base")
	remove(t, root, ".attest/a")
	if err := os.Symlink("b", filepath.Join(root.Path, ".attest", "a")); err != nil {
		t.Fatal(err)
	}
	swapped := commitAll(t, root, "entry to symlink")
	remove(t, root, ".attest/a")
	putRaw(t, root, ".attest/a", entryFor("a"))
	commitAll(t, root, "restore")

	got := Remote(context.Background(), root, base)
	assertFindings(t, got, ".attest/a:attest-deleted")
	if len(got) == 0 {
		return
	}
	if !strings.Contains(got[0].Msg, swapped[:12]) {
		t.Errorf("msg %q does not name commit %s", got[0].Msg, swapped[:12])
	}
}

func TestRemoteSinceEntryAddedThenDeleted(t *testing.T) {
	root := gitRepo(t)
	base := commitAll(t, root, "base")
	putRaw(t, root, ".attest/x", entryFor("x"))
	commitAll(t, root, "add")
	remove(t, root, ".attest/x")
	gone := commitAll(t, root, "drop")

	got := Remote(context.Background(), root, base)
	assertFindings(t, got, ".attest/x:attest-deleted")
	if len(got) == 0 {
		return
	}
	if !strings.Contains(got[0].Msg, gone[:12]) {
		t.Errorf("msg %q does not name commit %s", got[0].Msg, gone[:12])
	}
}

func TestRemoteSinceEntryDeletedOnceAcrossCommits(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	remove(t, root, ".attest/x")
	commitAll(t, root, "drop")
	putRaw(t, root, ".attest/x", entryFor("x"))
	commitAll(t, root, "restore")
	remove(t, root, ".attest/x")
	commitAll(t, root, "drop again")

	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted")
}

func TestRemoteSinceMergeTakingDeletingSide(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	main := git(t, root.Path, "branch", "--show-current")
	git(t, root.Path, "switch", "-q", "-c", "side")
	remove(t, root, ".attest/x")
	commitAll(t, root, "drop on side")
	git(t, root.Path, "switch", "-q", main)
	put(t, root, "repo-a/other.md", cleanFact("other"))
	commitAll(t, root, "work on main")
	git(t, root.Path, "merge", "-q", "--no-ff", "--no-edit", "-X", "theirs", "side")
	if _, err := os.Stat(filepath.Join(root.Path, ".attest/x")); err == nil {
		t.Fatal("merge kept the entry")
	}

	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted")
}

func TestRemoteSinceMergeDroppingEntryFromBothSides(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	main := git(t, root.Path, "branch", "--show-current")
	git(t, root.Path, "switch", "-q", "-c", "side")
	put(t, root, "repo-a/other.md", cleanFact("other"))
	commitAll(t, root, "work on side")
	git(t, root.Path, "switch", "-q", main)
	put(t, root, "repo-a/more.md", cleanFact("more"))
	commitAll(t, root, "work on main")
	git(t, root.Path, "merge", "-q", "--no-ff", "--no-commit", "side")
	remove(t, root, ".attest/x")
	git(t, root.Path, "add", "-A")
	git(t, root.Path, "commit", "-q", "--no-edit")

	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted")
}

func TestRemoteSinceEntryRenamed(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, ".attest/x", entryFor("x"))
	base := commitAll(t, root, "base")
	git(t, root.Path, "mv", ".attest/x", ".attest/y")
	commitAll(t, root, "rename")

	// y still says it is x, so the tree check flags it as well.
	assertFindings(t, Remote(context.Background(), root, base), ".attest/x:attest-deleted", ".attest/y:attest-entry")
}

func TestRemoteSinceUnresolvable(t *testing.T) {
	root := gitRepo(t)
	commitAll(t, root, "base")
	for _, since := range []string{strings.Repeat("0", 40), "no-such-branch", "--output=x", "-h"} {
		got := Remote(context.Background(), root, since)
		assertFindings(t, got, ".:remote-range")
	}
	if _, err := os.Stat(filepath.Join(root.Path, "x")); err == nil {
		t.Error("an option-looking --since was run as an option")
	}
}

func TestRemoteSinceHeadChecksTreeOnly(t *testing.T) {
	root := gitRepo(t)
	putRaw(t, root, "repo-a/r.md", reviewedFact("r"))
	head := commitAll(t, root, "review without entry")

	assertFindings(t, Remote(context.Background(), root, head), "repo-a/r.md:unattested-review")
	assertFindings(t, Remote(context.Background(), root, "HEAD"), "repo-a/r.md:unattested-review")
}

func TestRemoteSinceMultiCommitRange(t *testing.T) {
	root := gitRepo(t)
	base := commitAll(t, root, "base")
	for _, n := range []string{"a", "b", "c"} {
		putRaw(t, root, "repo-a/"+n+".md", reviewedFact(n))
		putRaw(t, root, ".attest/"+n, entryFor(n))
		commitAll(t, root, "add "+n)
	}
	remove(t, root, ".attest/b")
	commitAll(t, root, "drop b's entry")

	// b's entry was added and dropped inside the range.
	assertFindings(t, Remote(context.Background(), root, base),
		".attest/b:attest-deleted", "repo-a/b.md:unattested-review", "repo-a/b.md:unattested-review")
}

func TestRemoteSinceOversizedFact(t *testing.T) {
	root := gitRepo(t)
	base := commitAll(t, root, "base")
	big := cleanFact("big")
	big.Body = strings.Repeat("x", maxHistoryFactBytes+1)
	put(t, root, "repo-a/big.md", big)
	put(t, root, "repo-a/after.md", cleanFact("after"))
	commitAll(t, root, "big")

	assertFindings(t, Remote(context.Background(), root, base), "repo-a/big.md:remote-blob")
}

func TestIsFactPath(t *testing.T) {
	for p, want := range map[string]bool{
		"repo-a/x.md":          true,
		"_archive/repo-a/x.md": true,
		"README.md":            false,
		".attest/x.md":         false,
		"repo-a/.hidden/x.md":  false,
		"repo-a/x.txt":         false,
	} {
		if got := isFactPath(p); got != want {
			t.Errorf("isFactPath(%q) = %v, want %v", p, got, want)
		}
	}
}
