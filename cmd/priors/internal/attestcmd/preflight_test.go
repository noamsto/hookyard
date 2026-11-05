package attestcmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

func TestMain(m *testing.M) {
	committest.UnsetRepoEnv()
	toolstest.Pin()
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := tools.Command(context.Background(), tools.Git, append([]string{
		"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
	}, args...)...)
	cmd.Env = route.RepoEnv("LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	if tools.Git == "" {
		t.Skip("git not installed")
	}
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	return root
}

func addWorktree(t *testing.T, root string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", wt, "-b", "linked")
	return wt
}

func TestLoaderVars(t *testing.T) {
	got := LoaderVars([]string{"PATH=/x", "LD_PRELOAD=a", "DYLD_INSERT_LIBRARIES=b", "XLD_X=c", "LD_=d"})
	want := []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "LD_"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoaderVars = %v, want %v", got, want)
	}
}

func TestGitDirs(t *testing.T) {
	root := newTestRepo(t)
	mainGit, err := filepath.Abs(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatal(err)
	}

	gitDir, common, err := gitDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if gitDir != mainGit || common != mainGit {
		t.Fatalf("plain clone: gitDir=%q common=%q, want both %q", gitDir, common, mainGit)
	}

	wt := addWorktree(t, root)
	gitDir, common, err = gitDirs(wt)
	if err != nil {
		t.Fatal(err)
	}
	wantGitDir := filepath.Join(mainGit, "worktrees", "wt")
	if gitDir != wantGitDir || common != mainGit {
		t.Fatalf("worktree: gitDir=%q common=%q, want %q and %q", gitDir, common, wantGitDir, mainGit)
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, ".git"), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitDirs(bad); err == nil {
		t.Fatal("a .git file without gitdir: must err")
	}
}

func TestRefuseGrafts(t *testing.T) {
	touch := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(t *testing.T, gitDir, common, file string) {
		t.Helper()
		err := refuseGrafts(gitDir, common)
		if err == nil || !strings.Contains(err.Error(), file) {
			t.Fatalf("refuseGrafts err = %v, want one naming %s", err, file)
		}
	}

	t.Run("clean", func(t *testing.T) {
		gitDir, common, err := gitDirs(newTestRepo(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseGrafts(gitDir, common); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("common grafts", func(t *testing.T) {
		gitDir, common, _ := gitDirs(newTestRepo(t))
		file := filepath.Join(common, "info", "grafts")
		touch(t, file)
		refused(t, gitDir, common, file)
	})
	t.Run("common shallow", func(t *testing.T) {
		gitDir, common, _ := gitDirs(newTestRepo(t))
		file := filepath.Join(common, "shallow")
		touch(t, file)
		refused(t, gitDir, common, file)
	})
	t.Run("linked worktree shallow", func(t *testing.T) {
		gitDir, common, err := gitDirs(addWorktree(t, newTestRepo(t)))
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(gitDir, "shallow")
		touch(t, file)
		refused(t, gitDir, common, file)
	})
}
