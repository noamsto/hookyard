// Package committest holds the store-commit invariant check shared by the
// tests of every package that commits to a store checkout.
package committest

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// UnsetRepoEnv drops route.RepoLocatingEnv, which a TestMain calls so the
// test's `git -C dir` helpers act on dir, not on the repo running `go test`
// from a hook.
func UnsetRepoEnv() {
	for _, k := range route.RepoLocatingEnv {
		_ = os.Unsetenv(k)
	}
}

// AssertHeadIndexInTree fails t when HEAD's MEMORY.md lists a file HEAD does
// not hold. An unborn HEAD, or one without a MEMORY.md, holds trivially.
func AssertHeadIndexInTree(t testing.TB, dir string) {
	t.Helper()
	if err := tools.Command(context.Background(), tools.Git, "-C", dir, "rev-parse", "-q", "--verify", "HEAD").Run(); err != nil {
		return
	}
	tree := strings.Split(gitOut(t, dir, "ls-tree", "-r", "-z", "--name-only", "HEAD"), "\x00")
	if !slices.Contains(tree, store.IndexFile) {
		return
	}
	lines, _ := sanitize.Unfence(gitOut(t, dir, "show", "HEAD:"+store.IndexFile))
	for _, line := range lines {
		if _, rel, ok := store.ParseIndexLine(line); ok && !slices.Contains(tree, rel) {
			t.Errorf("HEAD's %s lists %s, which HEAD does not hold", store.IndexFile, rel)
		}
	}
}

func gitOut(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := tools.Command(context.Background(), tools.Git, append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
