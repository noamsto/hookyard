package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const nativeGitTimeout = 2 * time.Second

// nativeMemDir is the memory dir Claude loads for a session in cwd. Claude
// keys it by the main worktree's root, so a linked worktree shares its main
// repo's dir. Outside a repo, or when git fails, cwd itself is the root.
func nativeMemDir(ctx context.Context, cwd, home string) string {
	root := mainWorktreeRoot(ctx, cwd)
	return filepath.Join(home, ".claude", "projects", importer.EncodeProjectDir(root), "memory")
}

func mainWorktreeRoot(ctx context.Context, cwd string) string {
	ctx, cancel := context.WithTimeout(ctx, nativeGitTimeout)
	defer cancel()
	cmd := tools.Command(ctx, tools.Git, "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	cmd.Env = route.RepoEnv("LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	common, top, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !ok || common == "" || top == "" {
		return cwd
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return top
}
