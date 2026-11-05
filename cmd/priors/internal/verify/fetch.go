package verify

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	fetchRef = "refs/priors/fetch"
	tipsRef  = "refs/priors/tips/"
)

// preAuthPatterns mark a failure that happened before the remote
// authenticated us: nothing the remote said can be trusted to explain it.
var preAuthPatterns = []string{
	"Host key verification failed",
	"Permission denied (publickey",
	"Could not resolve hostname",
	"Connection refused",
	"Connection timed out",
	"No route to host",
	"Network is unreachable",
}

// fetchError is a failed fetch; preAuth means it failed before authentication.
type fetchError struct {
	preAuth bool
	msg     string
}

func (e *fetchError) Error() string { return e.msg }

// authenticated is the line ssh at LogLevel=VERBOSE logs once authentication
// has succeeded.
const authenticated = "Authenticated to "

// preAuth reports whether stderr shows a pre-authentication failure. Lines
// the remote sent ("remote:") are ignored: the remote controls them. Once
// ssh has authenticated, git also quotes pushed content (fsck messages)
// without that prefix, so no line counts after authentication; text an agent
// injects can then only make the run behind.
func preAuth(stderr string) bool {
	if strings.Contains(stderr, authenticated) {
		return false
	}
	for line := range strings.Lines(stderr) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "remote:") {
			continue
		}
		for _, p := range preAuthPatterns {
			if strings.Contains(line, p) {
				return true
			}
		}
	}
	return false
}

// cleanTmp removes the partial objects an interrupted fetch leaves behind.
func cleanTmp(gitDir string) error {
	for _, pattern := range []string{
		"objects/pack/tmp_pack_*",
		"objects/pack/tmp_idx_*",
		"objects/incoming-*",
		"objects/??/tmp_obj_*",
	} {
		matches, err := filepath.Glob(filepath.Join(gitDir, pattern))
		if err != nil {
			return err
		}
		for _, p := range matches {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// dirSize sums the sizes of the regular files under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// fetch brings the remote HEAD into refs/priors/fetch and returns its commit.
func (m mirror) fetch(ctx context.Context, url string) (string, error) {
	cmd := m.cmd(ctx, "-c", "fetch.fsckObjects=true", "fetch", "--no-tags", "--no-write-fetch-head",
		"--", url, "+HEAD:"+fetchRef)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &fetchError{
			preAuth: ctx.Err() == nil && preAuth(stderr.String()),
			msg:     fmt.Sprintf("git fetch: %v: %s", err, short(stderr.String())),
		}
	}
	out, err := m.run(ctx, "rev-parse", "--verify", fetchRef+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// recordTip keeps refs/priors/tips/<oid> and drops every tip ref it contains,
// so the tip refs stay an antichain that keeps the history reachable.
func (m mirror) recordTip(ctx context.Context, oid string) error {
	out, err := m.run(ctx, "for-each-ref", "--format=%(refname)", "--merged", oid, tipsRef)
	if err != nil {
		return err
	}
	merged := strings.Fields(out)
	ref := tipsRef + oid

	var tx strings.Builder
	tx.WriteString("start\n")
	if !slices.Contains(merged, ref) {
		fmt.Fprintf(&tx, "create %s %s\n", ref, oid)
	}
	for _, r := range merged {
		if r != ref {
			fmt.Fprintf(&tx, "delete %s\n", r)
		}
	}
	tx.WriteString("commit\n")

	cmd := m.cmd(ctx, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(tx.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, short(stderr.String()))
	}
	return nil
}
