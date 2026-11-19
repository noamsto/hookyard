package commit

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's object ids, not a password hash
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// ReviewRequest is one fact proposed for review. Rel is the fact's
// checkout-relative path with '/' separators, Data its reviewed bytes
// (confidence: reviewed, flags cleared), Branch the review branch's name
// below refs/heads/, and Msg the whole commit message.
type ReviewRequest struct {
	Rel    string
	Data   []byte
	Branch string
	Msg    string
}

// ReviewOutcome is what Review did with a request.
type ReviewOutcome int

const (
	// ReviewPushed: the review branch was created on the remote.
	ReviewPushed ReviewOutcome = iota + 1
	// ReviewExists: the remote already has the review branch, so the fact
	// was proposed before.
	ReviewExists
	// ReviewPermanent: the fact can never be proposed as it stands.
	ReviewPermanent
	// ReviewTransient: the fact was not proposed this time; a later run may.
	ReviewTransient
)

// Review pushes a new branch req.Branch to root's pinned remote holding one
// commit on the checkout branch's fetched upstream that adds req.Data at
// req.Rel, once the remote's branch still holds that upstream, the bytes and
// message pass the fact rules and the secret scan, and the branch's whole tree
// passes the store lint. It builds the commit with
// plumbing in a private index and moves no ref of the checkout, nor touches
// its index or worktree. The caller holds the store's lock. The detail names
// what stopped it by rule id and path, never by content.
func Review(ctx context.Context, cfg config.Config, root store.Root, rules gate.Rules, scanner gate.Scanner, req ReviewRequest) (ReviewOutcome, string) {
	if !cfg.Push || !cfg.CommitEnabled() {
		return ReviewTransient, "push is off"
	}
	if root.Remote == "" {
		return ReviewTransient, "the trust file pins no remote for this store"
	}
	dir := root.Path
	top, err := runGit(ctx, dir, gitTimeout, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return ReviewTransient, gitWarning("git rev-parse", top, err)
	}
	if !sameDir(strings.TrimSpace(top), dir) {
		return ReviewTransient, dir + " is not a git work tree's top level"
	}
	if outcome, detail := gateBytes(ctx, root, rules, scanner, req); detail != "" {
		return outcome, detail
	}

	if warning := preflight(ctx, dir, root.Remote); warning != "" {
		return ReviewTransient, warning
	}
	// Checked before anything about base: a branch pushed before a crash
	// that left the request pending is sent, whatever main holds now.
	ref := "refs/heads/" + req.Branch
	switch exists, warning := remoteHas(ctx, dir, root.Remote, ref); {
	case warning != "":
		return ReviewTransient, warning
	case exists:
		return ReviewExists, ""
	}

	up, warning := upstream(ctx, dir)
	if warning != "" {
		return ReviewTransient, warning
	}
	// The push sends every object of the review commit the remote lacks, so a
	// base the remote's branch does not hold would carry local commits along.
	switch tip, _, warning := remoteRef(ctx, dir, root.Remote, up.mergeRef); {
	case warning != "":
		return ReviewTransient, warning
	case tip != up.commit:
		return ReviewTransient, fmt.Sprintf("%s is not the remote's %s; fetch first", up.tracking, up.mergeRef)
	}
	git := inCheckout(ctx, dir)
	if out, err := git(gitTimeout, nil, nil, "ls-tree", up.commit, "--", req.Rel); err != nil {
		return ReviewTransient, gitWarning("git ls-tree", out, err)
	} else if out != "" {
		return ReviewPermanent, fmt.Sprintf("%s already exists on %s", req.Rel, strings.TrimPrefix(up.tracking, "refs/remotes/"))
	}
	if outcome, detail := lintBranch(ctx, cfg, root, rules, scanner, git, up.commit, req); detail != "" {
		return outcome, detail
	}

	tree, _, warning := gatedTree(git, dir, up.commit, func(env []string) (map[string]string, string) {
		return stageBytes(git, env, req)
	})
	switch {
	case warning != "":
		return ReviewTransient, warning
	case tree == "":
		return ReviewTransient, "the review tree is the upstream's own"
	}
	sign, err := signs(ctx, dir)
	if err != nil {
		return ReviewTransient, err.Error()
	}
	commit, warning := newCommit(git, dir, up.commit, tree, req.Msg, sign)
	if warning != "" {
		return ReviewTransient, warning
	}

	gated := map[string]string{req.Rel: "100644 " + blobID(req.Data)}
	if warning := pinnedPush(ctx, dir, root.Remote, commit, ref, "", up.commit, gated); warning != "" {
		// The empty lease refuses a branch that appeared since the check above.
		if exists, _ := remoteHas(ctx, dir, root.Remote, ref); exists {
			return ReviewExists, ""
		}
		return ReviewTransient, warning
	}
	return ReviewPushed, ""
}

// gateBytes checks req's bytes alone, before any of them is hashed into the
// checkout's object store: a fact path, a parsable fact passing the fact
// rules, and a clean secret scan of the fact and of the message, which quotes
// the local fact's flags. It returns "" for the detail when they pass.
func gateBytes(ctx context.Context, root store.Root, rules gate.Rules, scanner gate.Scanner, req ReviewRequest) (ReviewOutcome, string) {
	if !indexable(req.Rel) || !filepath.IsLocal(filepath.FromSlash(req.Rel)) {
		return ReviewPermanent, req.Rel + " is not a fact path"
	}
	f, err := fact.Parse(req.Data)
	if err != nil {
		return ReviewPermanent, req.Rel + " does not parse as a fact"
	}
	var failed []string
	for _, finding := range lint.FactRules(store.Entry{Root: root, Rel: req.Rel, Fact: f, Raw: req.Data}, rules) {
		failed = append(failed, finding.Rule)
	}
	if slices.Contains(failed, "secret") {
		return ReviewPermanent, "secret rule(s): " + strings.Join(rules.Match(string(req.Data)), ", ")
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		return ReviewPermanent, "fact rule(s): " + strings.Join(slices.Compact(failed), ", ")
	}
	ids, err := scanner.ScanText(ctx, string(req.Data))
	if err != nil {
		return ReviewTransient, fmt.Sprintf("secret scan failed: %v", err)
	}
	if len(ids) > 0 {
		return ReviewPermanent, "secret scan matched: " + strings.Join(ids, ", ")
	}
	if ids := rules.Match(req.Msg); len(ids) > 0 {
		return ReviewPermanent, "secret rule(s) in the message: " + strings.Join(ids, ", ")
	}
	ids, err = scanner.ScanText(ctx, req.Msg)
	if err != nil {
		return ReviewTransient, fmt.Sprintf("secret scan of the message failed: %v", err)
	}
	if len(ids) > 0 {
		return ReviewPermanent, "secret scan matched in the message: " + strings.Join(ids, ", ")
	}
	return 0, ""
}

// lintBranch runs the store lint, gates included, over base's tree with req's
// bytes added, as `priors lint --dir` would over the branch. It returns ""
// for the detail when the lint is clean, else the findings' files and rules.
// A base fact sharing req's name is permanent: the lint reports whichever of
// the two files it walks second, so both are matched.
func lintBranch(ctx context.Context, cfg config.Config, root store.Root, rules gate.Rules, scanner gate.Scanner, git gitFunc, base string, req ReviewRequest) (ReviewOutcome, string) {
	tmp, detail := materialise(git, base)
	defer func() { _ = os.RemoveAll(tmp) }()
	if detail != "" {
		return ReviewTransient, detail
	}
	if err := writeBlob(tmp, req.Rel, req.Data); err != nil {
		return ReviewTransient, fmt.Sprintf("materialising the review tree: %v", err)
	}
	name := nameAt(tmp, req.Rel)
	snap := store.Root{Store: root.Store, Kind: store.KindCheckout, Path: tmp}
	opts := lint.Options{Store: root.Store, WorkOrgs: cfg.WorkOrgs, WorkNames: cfg.WorkNames(), Rules: rules, Scanner: &scanner, Gates: true}
	var found []string
	for _, f := range lint.Store(ctx, snap, opts) {
		if f.Rule == "duplicate-name" && (f.File == req.Rel || nameAt(tmp, f.File) == name) {
			return ReviewPermanent, "lint: " + req.Rel + ": duplicate-name"
		}
		found = append(found, f.File+": "+f.Rule)
	}
	if len(found) == 0 {
		return 0, ""
	}
	return ReviewTransient, "lint: " + strings.Join(slices.Compact(found), ", ")
}

// nameAt is the name of the fact at rel, a local '/'-separated path under
// dir, or "" when it does not parse.
func nameAt(dir, rel string) string {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // rel is a lint finding's file inside the private materialised tree
	if err != nil {
		return ""
	}
	f, err := fact.Parse(raw)
	if err != nil {
		return ""
	}
	return f.Name
}

// materialise writes base's tree into a new private temp dir from its raw
// blobs: neither git archive, which applies export-ignore and export-subst,
// nor checkout-index, which runs smudge filters, would give lint the bytes
// the branch holds. It refuses a tree holding anything but regular files at
// local paths. The caller removes dst, even when detail is set.
func materialise(git gitFunc, base string) (dst, detail string) {
	tmp, err := os.MkdirTemp("", "priors-review-")
	if err != nil {
		return "", fmt.Sprintf("materialising %s: %v", base, err)
	}
	// Resolved so Root.Confine agrees with Walk behind a symlinked TMPDIR.
	if dst, err = filepath.EvalSymlinks(tmp); err != nil {
		return tmp, fmt.Sprintf("materialising %s: %v", base, err)
	}
	out, err := git(gitTimeout, nil, nil, "ls-tree", "-r", "-z", "--full-tree", base)
	if err != nil {
		return dst, gitWarning("git ls-tree", out, err)
	}
	var paths, oids []string
	for meta, path := range records(out) {
		f := strings.Fields(meta)
		var kind string
		switch {
		case len(f) != 3:
			kind = "an unreadable entry"
		case f[0] == "120000":
			kind = "a symlink"
		case f[0] == "160000":
			kind = "a submodule"
		case f[1] != "blob":
			kind = "a " + f[1]
		case !filepath.IsLocal(filepath.FromSlash(path)):
			kind = "a non-local path"
		}
		if kind != "" {
			return dst, fmt.Sprintf("base tree holds %s at %s", kind, path)
		}
		paths, oids = append(paths, path), append(oids, f[2])
	}
	if len(oids) == 0 {
		return dst, ""
	}
	// The output is blob content, so a failure is reported without it.
	out, err = git(gitTimeout, nil, strings.NewReader(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return dst, fmt.Sprintf("git cat-file failed: %v", err)
	}
	for i, path := range paths {
		header, rest, ok := strings.Cut(out, "\n")
		f := strings.Fields(header)
		if !ok || len(f) != 3 || f[0] != oids[i] || f[1] != "blob" {
			return dst, "git cat-file gave an unexpected record for " + path
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 || len(rest) <= size || rest[size] != '\n' {
			return dst, "git cat-file gave an unexpected record for " + path
		}
		if err := writeBlob(dst, path, []byte(rest[:size])); err != nil {
			return dst, fmt.Sprintf("materialising %s: %v", base, err)
		}
		out = rest[size+1:]
	}
	return dst, ""
}

// writeBlob writes b at rel, a local '/'-separated path, under dir.
func writeBlob(dir, rel string, b []byte) error {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// blobID is git's sha1 object id of a blob holding data, computed without
// asking git.
func blobID(data []byte) string {
	sum := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(data)), data...)) //nolint:gosec // sha1 is the object format pinnedPush requires
	return hex.EncodeToString(sum[:])
}

// stageBytes hashes req's bytes into the checkout's objects and adds them at
// req.Rel to the private index env names. Hashed from stdin without --path,
// so no clean filter rewrites them.
func stageBytes(git gitFunc, env []string, req ReviewRequest) (gated map[string]string, warning string) {
	oid, err := git(gitTimeout, nil, bytes.NewReader(req.Data), "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, gitWarning("git hash-object", oid, err)
	}
	entry := "100644 " + strings.TrimSpace(oid)
	if out, err := git(gitTimeout, env, strings.NewReader(entry+"\t"+req.Rel+"\x00"), "update-index", "-z", "--index-info"); err != nil {
		return nil, gitWarning("git update-index", out, err)
	}
	return map[string]string{req.Rel: entry}, ""
}
