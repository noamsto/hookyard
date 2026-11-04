package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(tools.Git, append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func wantNativeDir(home, root string) string {
	return filepath.Join(home, ".claude", "projects", importer.EncodeProjectDir(root), "memory")
}

func TestNativeMemDir(t *testing.T) {
	home := "/h"
	repo := filepath.Join(resolvedTemp(t), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(resolvedTemp(t), "linked")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "other", linked)
	linkedSub := filepath.Join(linked, "x")
	if err := os.Mkdir(linkedSub, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := resolvedTemp(t)

	for name, tc := range map[string]struct{ cwd, root string }{
		"plain repo":                {repo, repo},
		"subdirectory":              {sub, repo},
		"linked worktree":           {linked, repo},
		"linked worktree subdir":    {linkedSub, repo},
		"not a repo":                {plain, plain},
		"missing dir (git failure)": {filepath.Join(plain, "absent"), filepath.Join(plain, "absent")},
	} {
		t.Run(name, func(t *testing.T) {
			if got, want := nativeMemDir(context.Background(), tc.cwd, home), wantNativeDir(home, tc.root); got != want {
				t.Errorf("nativeMemDir(%q) = %q, want %q", tc.cwd, got, want)
			}
		})
	}
}

func TestNativeMemDirFallsBackToCWDWhenGitFails(t *testing.T) {
	cwd := resolvedTemp(t)
	saved := tools.Git
	t.Cleanup(func() { tools.Git = saved })
	tools.Git = filepath.Join(cwd, "no-such-git")
	if got, want := nativeMemDir(context.Background(), cwd, "/h"), wantNativeDir("/h", cwd); got != want {
		t.Errorf("nativeMemDir = %q, want %q", got, want)
	}
}
