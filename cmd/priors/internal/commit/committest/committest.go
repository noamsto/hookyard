// Package committest holds the store-commit invariant check shared by the
// tests of every package that commits to a store checkout.
package committest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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

// PinPush points tools.SSHConfig and tools.KnownHosts at empty files, and
// tools.PushGitDir at an empty bare git dir made read-only as the Nix store's
// is, for the length of t, as a push refuses to run without them.
func PinPush(t testing.TB) {
	t.Helper()
	saved := [3]string{tools.SSHConfig, tools.KnownHosts, tools.PushGitDir}
	t.Cleanup(func() { tools.SSHConfig, tools.KnownHosts, tools.PushGitDir = saved[0], saved[1], saved[2] })
	dir := t.TempDir()
	tools.SSHConfig, tools.KnownHosts = filepath.Join(dir, "ssh_config"), filepath.Join(dir, "known_hosts")
	for _, path := range []string{tools.SSHConfig, tools.KnownHosts} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitDir := filepath.Join(dir, "push.git")
	if out, err := tools.Command(context.Background(), tools.Git, "init", "-q", "--bare", "--template=", "--object-format=sha1", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Cleanup(func() { chmodTree(t, gitDir, 0o200, 0) })
	chmodTree(t, gitDir, 0, 0o222)
	tools.PushGitDir = gitDir
}

// chmodTree adds set to and clears unset from the mode of everything under root.
func chmodTree(t testing.TB, root string, set, unset fs.FileMode) {
	t.Helper()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	err = fs.WalkDir(r.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return r.Chmod(path, (info.Mode().Perm()|set)&^unset)
	})
	if err != nil {
		t.Fatal(err)
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
