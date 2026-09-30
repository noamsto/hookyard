// Package committest holds the store-commit invariant check shared by the
// tests of every package that commits to a store checkout.
package committest

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// UnsetRepoEnv drops the variables git exports into a hook's environment to
// locate its own repository, which a TestMain calls so the test's `git -C dir`
// helpers act on dir, not on the repo running `go test` from a hook.
func UnsetRepoEnv() {
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE",
	} {
		_ = os.Unsetenv(k)
	}
}

// AssertHeadIndexInTree fails t when HEAD's MEMORY.md lists a file HEAD does
// not hold. An unborn HEAD, or one without a MEMORY.md, holds trivially.
func AssertHeadIndexInTree(t testing.TB, dir string) {
	t.Helper()
	if err := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "HEAD").Run(); err != nil { //nolint:gosec // dir is the calling test's own store
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
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // dir is the calling test's own store; args are fixed read-only git subcommands
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
