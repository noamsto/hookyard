// Package pin judges a store checkout against the path and remote the trust file pins.
package pin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const (
	gitTimeout = 10 * time.Second
	waitDelay  = time.Second
	maxGitOut  = 200
)

// PushEnv is the full environment for a push: user and system git config
// ignored, and ssh run with the pinned config and known_hosts.
func PushEnv() ([]string, error) {
	if !filepath.IsAbs(tools.SSHConfig) || !filepath.IsAbs(tools.KnownHosts) {
		return nil, fmt.Errorf("ssh config or known_hosts: %w", tools.ErrUnpinned)
	}
	return append(cleanEnv(),
		"GIT_SSH_COMMAND="+quote(tools.SSH)+" -F "+quote(tools.SSHConfig)+" -o "+quote("UserKnownHostsFile="+tools.KnownHosts),
		"GIT_SSH_VARIANT=ssh",
	), nil
}

func cleanEnv() []string {
	return slices.DeleteFunc(
		route.RepoEnv("LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"),
		func(kv string) bool {
			k, _, _ := strings.Cut(kv, "=")
			return k == "GIT_EXEC_PATH" || k == "GIT_SSH" || k == "GIT_SSH_COMMAND" || k == "GIT_SSH_VARIANT"
		},
	)
}

// quote single-quotes s for the POSIX shell git runs GIT_SSH_COMMAND through.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Check refuses dir unless it matches its pin: with remote set, dir must be the
// top level of a checkout whose origin fetches and pushes exactly remote; with
// none, dir must not be a checkout with any remote. A missing dir passes, as
// there is nothing to read. Git runs under the user's config, which any sync of
// the checkout also follows, so a rewrite from there fails closed.
func Check(ctx context.Context, dir, remote string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	checkout := err == nil && sameDir(strings.TrimSpace(top), dir)
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || !strings.Contains(top, "not a git repository")) {
		return fmt.Errorf("%s: %w", dir, gitFailed("rev-parse", top, err))
	}

	if remote == "" {
		if !checkout {
			return nil
		}
		names, err := runGit(ctx, dir, "remote")
		if err != nil {
			return fmt.Errorf("%s: %w", dir, gitFailed("remote", names, err))
		}
		if names := strings.Fields(names); len(names) > 0 {
			return fmt.Errorf("%s has remote(s) %s but the trust file pins none", dir, short(strings.Join(names, ", ")))
		}
		return nil
	}

	if !checkout {
		return fmt.Errorf("%s is not the top level of a git checkout, but the trust file pins remote %s", dir, remote)
	}
	for _, q := range []struct {
		what string
		args []string
	}{
		{"url", []string{"config", "--get-all", "remote.origin.url"}},
		{"fetch url", []string{"remote", "get-url", "--all", "origin"}},
		{"push url", []string{"remote", "get-url", "--push", "--all", "origin"}},
	} {
		out, err := runGit(ctx, dir, q.args...)
		if err != nil {
			return fmt.Errorf("%s: origin %s: %w", dir, q.what, gitFailed(strings.Join(q.args, " "), out, err))
		}
		if out != remote+"\n" {
			return fmt.Errorf("%s: origin %s is %s, not the pinned %s", dir, q.what, short(out), remote)
		}
	}
	return nil
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// runGit returns stdout, or stdout and stderr combined on failure so that an
// error can quote git's complaint.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := tools.Command(ctx, tools.Git, append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	// Check matches git's English "not a git repository". GIT_SSH_COMMAND goes
	// last, as exec keeps the final duplicate; GIT_EXEC_PATH would swap git's helpers.
	cmd.Env = append(slices.DeleteFunc(
		route.RepoEnv("LC_ALL=C", "GIT_TERMINAL_PROMPT=0"),
		func(kv string) bool { return strings.HasPrefix(kv, "GIT_EXEC_PATH=") },
	), "GIT_SSH_COMMAND="+tools.SSH)
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func gitFailed(what, out string, err error) error {
	if msg := short(out); msg != "" {
		return fmt.Errorf("git %s failed: %w: %s", what, err, msg)
	}
	return fmt.Errorf("git %s failed: %w", what, err)
}

// short collapses whitespace and caps what git printed.
func short(s string) string {
	msg := strings.Join(strings.Fields(s), " ")
	if len(msg) > maxGitOut {
		msg = strings.ToValidUTF8(msg[:maxGitOut], "") + "…"
	}
	return msg
}
