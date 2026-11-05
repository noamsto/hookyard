package attestcmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/pin"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/verify"
)

const maxGitOut = 200

var (
	branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	fetchedRef = regexp.MustCompile(`^([0-9a-f]{40}) refs/heads/(.+)$`)
)

// repo runs git over a checkout's objects. Every run reads them through the
// pinned empty push git dir, never the checkout's config, refs, grafts,
// shallow file, replace refs (GIT_NO_REPLACE_OBJECTS) or commit-graph
// (core.commitGraph=false).
type repo struct {
	root, common string
	env          []string
}

func newRepo(root, common string) (repo, error) {
	env, err := pin.ObjectsEnv(common)
	if err != nil {
		return repo{}, err
	}
	return repo{root: root, common: common, env: append(env, "GIT_ALLOW_PROTOCOL=ssh")}, nil
}

func (r repo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := tools.Command(ctx, tools.Git, append(slices.Clone(verify.GitConfigArgs), args...)...)
	cmd.Env = r.env
	cmd.Dir = tools.PushGitDir
	cmd.WaitDelay = time.Second
	return cmd
}

// run returns stdout; a failure carries the trimmed stderr.
func (r repo) run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := r.cmd(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", gitErr(subcommand(args), err, stderr.String())
	}
	return stdout.String(), nil
}

// subcommand is the git subcommand in args, past any leading -c options.
func subcommand(args []string) string {
	for len(args) > 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func gitErr(sub string, err error, stderr string) error {
	msg := strings.Join(strings.Fields(stderr), " ")
	if len(msg) > maxGitOut {
		msg = strings.ToValidUTF8(msg[:maxGitOut], "") + "…"
	}
	if msg == "" {
		return fmt.Errorf("git %s: %w", sub, err)
	}
	return fmt.Errorf("git %s: %w: %s", sub, err, msg)
}

// fetchDefault fetches the remote's default branch into the checkout's object
// store without touching any ref, and returns its name and tip.
func (r repo) fetchDefault(ctx context.Context, url string) (branch, tip string, err error) {
	out, err := r.run(ctx, "ls-remote", "--symref", "--", url, "HEAD")
	if err != nil {
		return "", "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch, _ = strings.CutSuffix(ref, "\tHEAD")
			break
		}
	}
	if !safeBranch(branch) {
		return "", "", fmt.Errorf("remote default branch %q is not usable", branch)
	}
	if strings.HasPrefix(url, "-") {
		return "", "", fmt.Errorf("remote url %q is not usable", url)
	}
	// fetch-pack takes no "--" separator, so the leading-dash check stands in for it.
	out, err = r.run(ctx, "-c", "fetch.fsckObjects=true", "fetch-pack", "--no-progress", url, "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	m := fetchedRef.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil || m[2] != branch {
		return "", "", fmt.Errorf("git fetch-pack returned no tip for %q", branch)
	}
	return branch, m[1], nil
}

func safeBranch(b string) bool {
	return branchName.MatchString(b) &&
		!strings.Contains(b, "..") &&
		!strings.HasPrefix(b, "-") &&
		!strings.HasSuffix(b, "/") &&
		!strings.HasSuffix(b, ".lock")
}

// isAncestor reports whether a is an ancestor of b, or equal to it.
func (r repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	var stderr bytes.Buffer
	cmd := r.cmd(ctx, "merge-base", "--is-ancestor", a, b)
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, gitErr("merge-base", err, stderr.String())
}

func (r repo) has(ctx context.Context, oid string) bool {
	return r.cmd(ctx, "cat-file", "-e", oid+"^{commit}").Run() == nil
}

// history returns the distinct versions of .attest/name across every commit
// reachable from head, in the order first seen. A version over the entry cap
// is skipped.
func (r repo) history(ctx context.Context, head, name string) ([][]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	objs, err := verify.OpenObjects(r.cmd(ctx, "cat-file", "--batch-command"))
	if err != nil {
		return nil, err
	}
	defer objs.Close()

	var stderr bytes.Buffer
	list := r.cmd(ctx, "rev-list", head)
	list.Stderr = &stderr
	stdout, err := list.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := list.Start(); err != nil {
		return nil, err
	}

	var versions [][]byte
	seen := map[string]bool{}
	scan := bufio.NewScanner(stdout)
	var walkErr error
	for walkErr == nil && scan.Scan() {
		walkErr = collect(objs, scan.Text(), name, seen, &versions)
	}
	if walkErr == nil {
		walkErr = scan.Err()
	}
	if walkErr != nil {
		cancel()
	}
	if err := list.Wait(); walkErr == nil && err != nil {
		walkErr = gitErr("rev-list", err, stderr.String())
	}
	if walkErr != nil {
		return nil, walkErr
	}
	return versions, nil
}

func collect(objs *verify.Objects, commit, name string, seen map[string]bool, versions *[][]byte) error {
	tree, _, err := objs.Commit(commit)
	if err != nil {
		return err
	}
	blob, ok, err := objs.AttestBlob(tree, name)
	if err != nil || !ok || seen[blob] {
		return err
	}
	seen[blob] = true
	raw, err := objs.Blob(blob, attest.MaxEntryBytes)
	if errors.Is(err, verify.ErrTooBig) {
		return nil
	}
	if err != nil {
		return err
	}
	*versions = append(*versions, raw)
	return nil
}

// blobAt returns the regular file at rel in commit, or false when there is none.
func (r repo) blobAt(ctx context.Context, commit, rel string, max int64) ([]byte, bool, error) {
	objs, err := verify.OpenObjects(r.cmd(ctx, "cat-file", "--batch-command"))
	if err != nil {
		return nil, false, err
	}
	defer objs.Close()

	tree, _, err := objs.Commit(commit)
	if err != nil {
		return nil, false, err
	}
	blob, ok, err := objs.PathBlob(tree, rel)
	if err != nil || !ok {
		return nil, false, err
	}
	raw, err := objs.Blob(blob, max)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}
