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

// PushEnv is the whole environment for a push, built from nothing: whatever
// the caller exports (BASH_ENV for the shell git runs receive-pack and ssh
// through, a loader or TLS variable) never reaches git or ssh. No HOME or
// PATH: ssh finds the user's identities through getpwuid, and git puts its own
// exec path first on PATH.
func PushEnv() ([]string, error) {
	if !filepath.IsAbs(tools.SSHConfig) || !filepath.IsAbs(tools.KnownHosts) || !filepath.IsAbs(tools.PushGitDir) {
		return nil, fmt.Errorf("ssh config, known_hosts or push git dir: %w", tools.ErrUnpinned)
	}
	env := []string{
		"LC_ALL=C",                    // English messages, which warnings quote
		"GIT_TERMINAL_PROMPT=0",       // no credential prompt to hang on
		"GIT_CONFIG_GLOBAL=/dev/null", // no user config rewriting the URL
		"GIT_CONFIG_NOSYSTEM=1",       // nor system config
		"GIT_SSH_COMMAND=" + quote(tools.SSH) + " -F " + quote(tools.SSHConfig) + " -o " + quote("UserKnownHostsFile="+tools.KnownHosts),
		"GIT_SSH_VARIANT=ssh", // the pinned ssh's option syntax, without probing it
	}
	// The ssh-agent may hold the key; the pinned known_hosts still vouches for the host.
	if sock, ok := os.LookupEnv("SSH_AUTH_SOCK"); ok {
		env = append(env, "SSH_AUTH_SOCK="+sock)
	}
	return env, nil
}

// quote single-quotes s for the POSIX shell git runs GIT_SSH_COMMAND through.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Check refuses dir unless it matches its pin: with remote set, dir must be the
// top level of a checkout whose only remote is origin, fetching and pushing
// exactly remote, with every branch's upstream and push remote on origin; with
// none, dir must not lie in a checkout with any remote. A missing dir passes, as
// there is nothing to read. Git runs under the user's config, which any sync of
// the checkout also follows, so a rewrite from there fails closed.
func Check(ctx context.Context, dir, remote string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || !strings.Contains(top, "not a git repository")) {
		return fmt.Errorf("%s: %w", dir, gitFailed("rev-parse", top, err))
	}
	inRepo := err == nil

	if remote == "" {
		if !inRepo {
			return nil
		}
		names, err := remotes(ctx, dir)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return fmt.Errorf("%s has remote(s) %s but the trust file pins none", dir, short(strings.Join(names, ", ")))
		}
		return nil
	}

	if !inRepo || !sameDir(strings.TrimSpace(top), dir) {
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
	names, err := remotes(ctx, dir)
	if err != nil {
		return err
	}
	if !slices.Equal(names, []string{"origin"}) {
		return fmt.Errorf("%s has remote(s) %s; the trust file pins origin alone", dir, short(strings.Join(names, ", ")))
	}
	// A branch's remote may name another remote or be a URL itself, and a
	// pull or push follows it rather than origin.
	out, err := runGit(ctx, dir, "config", "-z", "--get-regexp", `^(branch\..*\.(remote|pushremote)|remote\.pushdefault)$`)
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return fmt.Errorf("%s: %w", dir, gitFailed("config --get-regexp", out, err))
	}
	for entry := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		if key != "" && value != "origin" {
			return fmt.Errorf("%s: %s is %s, not origin", dir, short(key), short(value))
		}
	}
	return nil
}

// remotes lists the remotes git knows for the repo holding dir.
func remotes(ctx context.Context, dir string) ([]string, error) {
	out, err := runGit(ctx, dir, "remote")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, gitFailed("remote", out, err))
	}
	return strings.Fields(out), nil
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
