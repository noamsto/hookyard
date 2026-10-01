// Package commit is the only code in priors that stages, commits or pushes
// to a store. It commits a store checkout's dirty files only when every one
// of them is gated content, staging the very bytes the gates read, so a store
// commit never carries a file priors' gates have not passed, and HEAD's
// MEMORY.md never lists a file HEAD lacks.
package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	gitTimeout  = 10 * time.Second
	pushTimeout = 30 * time.Second
	maxGitOut   = 200
)

// Checkout commits root's dirty files with msg, and pushes when configured,
// if root is a git work tree's top level and every dirty path passes the
// gates; otherwise it stages nothing. The caller holds the store's lock. It
// returns a warning naming what stopped the commit, or "" on success or when
// there is nothing to do.
func Checkout(ctx context.Context, cfg config.Config, root store.Root, rules gate.Rules, scanner gate.Scanner, msg string) (warning string) {
	if !cfg.CommitEnabled() {
		return ""
	}
	dir := root.Path
	top, err := runGit(ctx, dir, gitTimeout, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(top, "not a git repository") {
			return ""
		}
		return gitWarning("git rev-parse", top, err)
	}
	if !sameDir(strings.TrimSpace(top), dir) {
		return ""
	}

	out, err := runGit(ctx, dir, gitTimeout, nil, "status", "-z", "--porcelain=v1", "--no-renames", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return gitWarning("git status", out, err)
	}
	dirty := parseStatus(out)
	if len(dirty) == 0 {
		return ""
	}

	// The gates and the staged blobs read one private copy, so nothing that
	// writes the checkout outside priors' lock can swap a file after its check.
	snapDir, special, err := snapshot(dir)
	defer func() { _ = os.RemoveAll(snapDir) }()
	if err != nil {
		return fmt.Sprintf("nothing committed in %s: snapshotting the checkout: %v", dir, err)
	}
	snap := store.Root{Store: root.Store, Kind: root.Kind, Path: snapDir}

	opts := lint.Options{Store: root.Store, WorkOrgs: cfg.WorkOrgs, WorkNames: cfg.WorkNames, Rules: rules, Scanner: &scanner, Gates: true}
	findings := map[string][]string{}
	for _, f := range lint.Store(ctx, snap, opts) {
		if f.File == "." {
			return fmt.Sprintf("nothing committed in %s: %s: %s", dir, f.Rule, f.Msg)
		}
		findings[f.File] = append(findings[f.File], f.Rule)
	}
	c := classifier{root: snap, rules: rules, findings: findings, special: special}
	var accepted, refused []string
	for _, e := range dirty {
		switch why := c.refusal(e); {
		case why == skip:
		case why != "":
			refused = append(refused, e.path+" ("+why+")")
		default:
			accepted = append(accepted, e.path)
		}
	}
	if len(refused) > 0 {
		return fmt.Sprintf("nothing committed in %s: not gated for the store: %s", dir, strings.Join(refused, ", "))
	}
	if len(accepted) == 0 {
		return ""
	}

	beforeStage()
	parent, err := runGit(ctx, dir, gitTimeout, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if parent = strings.TrimSpace(parent); err != nil {
		parent = ""
	}
	tree, warning := gatedTree(ctx, dir, snapDir, parent, accepted)
	if warning != "" || tree == "" {
		return warning
	}
	missing, err := unlisted(ctx, dir, tree)
	switch {
	case err != nil:
		return fmt.Sprintf("nothing committed in %s: checking the gated tree: %v", dir, err)
	case len(missing) > 0:
		return fmt.Sprintf("nothing committed in %s: %s would list files the commit lacks: %s", dir, store.IndexFile, strings.Join(missing, ", "))
	}
	args := []string{"commit-tree", "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := runGit(ctx, dir, gitTimeout, nil, append(args, tree)...)
	if err != nil {
		return gitWarning("git commit-tree", commit, err)
	}
	// parent as the old value makes this a compare-and-swap: a HEAD that
	// moved since the tree was built, or appeared on an unborn branch, fails it.
	if out, err := runGit(ctx, dir, gitTimeout, nil, "update-ref", "-m", msg, "HEAD", strings.TrimSpace(commit), parent); err != nil {
		return gitWarning("git update-ref", out, err)
	}
	// Every dirty path was gated, so resetting the shared index to the new
	// HEAD discards nothing a user staged that the commit does not hold.
	if out, err := runGit(ctx, dir, gitTimeout, nil, "reset", "-q"); err != nil {
		return gitWarning("git reset", out, err)
	}
	if cfg.Push {
		if out, err := runGit(ctx, dir, pushTimeout, nil, "push", "-q"); err != nil {
			return gitWarning("git push", out, err)
		}
	}
	return ""
}

// beforeStage is a test seam that runs once the gates have passed, just
// before the accepted paths are staged.
var beforeStage = func() {}

// snapshot copies the checkout at src, less its own .git, into a new
// temporary dir, reading each regular file once. It recreates symlinks as
// symlinks and returns the paths of everything else that is not a directory,
// which it does not copy. The caller removes dst, even on error.
func snapshot(src string) (dst string, special []string, err error) {
	tmp, err := os.MkdirTemp("", "priors-commit-")
	if err != nil {
		return "", nil, err
	}
	// Resolved so Root.Confine agrees with Walk behind a symlinked TMPDIR.
	if dst, err = filepath.EvalSymlinks(tmp); err != nil {
		return tmp, nil, err
	}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		// A path gone since its directory was listed is absent, as git
		// status will see it next time.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case rel == ".git" && d.IsDir():
			return filepath.SkipDir
		case rel == ".git":
			return nil
		case d.IsDir():
			return os.Mkdir(out, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return os.Symlink(target, out) //nolint:gosec // out is under the private snapshot dir, which nothing else writes
		case !d.Type().IsRegular():
			special = append(special, filepath.ToSlash(rel))
			return nil
		}
		regular, err := copyRegular(path, out)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err == nil && !regular {
			special = append(special, filepath.ToSlash(rel))
		}
		return err
	})
	return dst, special, err
}

// copyRegular copies path to out, reporting false without copying when path
// is no longer a regular file. O_NOFOLLOW and O_NONBLOCK keep a file swapped
// for a symlink or FIFO since the walk from being followed or hanging.
func copyRegular(path, out string) (regular bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // path comes from walking the checkout
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	perm := fs.FileMode(0o600)
	if info.Mode()&0o100 != 0 {
		perm = 0o700
	}
	return true, os.WriteFile(out, raw, perm)
}

// gatedTree builds, in a private index seeded from parent, the tree of
// parent with the snapshot's accepted paths applied, and checks it holds
// nothing else. It returns "" for the tree when that is parent's own tree.
func gatedTree(ctx context.Context, dir, snapDir, parent string, accepted []string) (tree, warning string) {
	tmp, err := os.MkdirTemp("", "priors-index-")
	if err != nil {
		return "", fmt.Sprintf("nothing committed in %s: making a private index: %v", dir, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	seed := []string{"read-tree", "--empty"}
	if parent != "" {
		seed = []string{"read-tree", parent}
	}
	if out, err := runGitEnv(ctx, dir, env, gitTimeout, nil, seed...); err != nil {
		return "", gitWarning("git read-tree", out, err)
	}
	gated, warning := stage(ctx, dir, env, snapDir, accepted)
	if warning != "" {
		return "", warning
	}
	tree, err = runGitEnv(ctx, dir, env, gitTimeout, nil, "write-tree")
	if err != nil {
		return "", gitWarning("git write-tree", tree, err)
	}
	tree = strings.TrimSpace(tree)
	stray, unchanged, err := checkTree(ctx, dir, parent, tree, gated)
	switch {
	case err != nil:
		return "", fmt.Sprintf("nothing committed in %s: checking the gated tree: %v", dir, err)
	case len(stray) > 0:
		return "", fmt.Sprintf("nothing committed in %s: the gated tree holds entries the gates did not pass: %s", dir, strings.Join(stray, ", "))
	case unchanged:
		return "", ""
	}
	return tree, ""
}

// stage puts the snapshot's bytes for accepted into the index env names,
// never the worktree's, and removes the accepted paths the snapshot lacks. It
// returns each path's staged "<mode> <oid>", or "" for a removal.
func stage(ctx context.Context, dir string, env []string, snapDir string, accepted []string) (gated map[string]string, warning string) {
	gated = map[string]string{}
	var info, removed strings.Builder
	for _, rel := range accepted {
		path := filepath.Join(snapDir, filepath.FromSlash(rel))
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			gated[rel] = ""
			removed.WriteString(rel + "\x00")
			continue
		}
		if err != nil {
			return nil, fmt.Sprintf("nothing committed in %s: reading the snapshot: %v", dir, err)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // path is under priors' private snapshot
		if err != nil {
			return nil, fmt.Sprintf("nothing committed in %s: reading the snapshot: %v", dir, err)
		}
		// Hashed from stdin without --path, so no clean filter rewrites the
		// gated bytes on their way into the object store.
		oid, err := runGit(ctx, dir, gitTimeout, bytes.NewReader(raw), "hash-object", "-w", "--stdin")
		if err != nil {
			return nil, gitWarning("git hash-object", oid, err)
		}
		mode := "100644"
		if fi.Mode()&0o100 != 0 {
			mode = "100755"
		}
		gated[rel] = mode + " " + strings.TrimSpace(oid)
		info.WriteString(gated[rel] + "\t" + rel + "\x00")
	}
	if info.Len() > 0 {
		if out, err := runGitEnv(ctx, dir, env, gitTimeout, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
			return nil, gitWarning("git update-index", out, err)
		}
	}
	if removed.Len() > 0 {
		if out, err := runGitEnv(ctx, dir, env, gitTimeout, strings.NewReader(removed.String()), "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return nil, gitWarning("git update-index", out, err)
		}
	}
	return gated, ""
}

// checkTree lists the paths where tree differs from parent's tree with gated
// applied, and reports whether tree is parent's tree unchanged.
func checkTree(ctx context.Context, dir, parent, tree string, gated map[string]string) (stray []string, unchanged bool, err error) {
	base := map[string]string{}
	if parent != "" {
		if base, err = treeEntries(ctx, dir, parent); err != nil {
			return nil, false, err
		}
	}
	have, err := treeEntries(ctx, dir, tree)
	if err != nil {
		return nil, false, err
	}
	want := maps.Clone(base)
	for rel, entry := range gated {
		if entry == "" {
			delete(want, rel)
		} else {
			want[rel] = entry
		}
	}
	for path, entry := range have {
		if want[path] != entry {
			stray = append(stray, path)
		}
	}
	for path := range want {
		if _, ok := have[path]; !ok {
			stray = append(stray, path)
		}
	}
	slices.Sort(stray)
	return stray, maps.Equal(have, base), nil
}

// treeEntries maps each path in treeish to its "<mode> <oid>".
func treeEntries(ctx context.Context, dir, treeish string) (map[string]string, error) {
	out, err := runGit(ctx, dir, gitTimeout, nil, "ls-tree", "-r", "-z", "--full-tree", treeish)
	if err != nil {
		return nil, errors.New(gitWarning("git ls-tree", out, err))
	}
	entries := map[string]string{}
	for meta, path := range records(out) {
		if f := strings.Fields(meta); len(f) == 3 {
			entries[path] = f[0] + " " + f[2]
		}
	}
	return entries, nil
}

// records yields the "<meta>\t<path>" records of git's -z listings.
func records(out string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for rec := range strings.SplitSeq(out, "\x00") {
			meta, path, ok := strings.Cut(rec, "\t")
			if ok && !yield(meta, path) {
				return
			}
		}
	}
}

// statusEntry is one path git status reports. Dir entries end in '/': an
// ignored directory, or an untracked nested repository.
type statusEntry struct {
	path    string
	ignored bool
}

func parseStatus(out string) []statusEntry {
	var entries []statusEntry
	for rec := range strings.SplitSeq(out, "\x00") {
		if len(rec) < 4 {
			continue
		}
		entries = append(entries, statusEntry{path: rec[3:], ignored: rec[:2] == "!!"})
	}
	return entries
}

// skip is the refusal of a path that neither blocks the commit nor is staged.
const skip = "-"

type classifier struct {
	root     store.Root
	rules    gate.Rules
	findings map[string][]string
	special  []string
}

// refusal says why e must not reach a commit: "" to stage it, skip to leave
// it out without blocking, or a content-free reason.
func (c classifier) refusal(e statusEntry) string {
	if strings.HasSuffix(e.path, "/") {
		if c.holdsIndexable(e.path) {
			return "holds facts git would not commit"
		}
		return skip
	}
	if e.ignored {
		if indexable(e.path) {
			return "ignored by git but indexed"
		}
		return skip
	}
	if slices.Contains(c.special, e.path) {
		return "not a regular file"
	}

	info, err := os.Lstat(filepath.Join(c.root.Path, filepath.FromSlash(e.path)))
	if errors.Is(err, fs.ErrNotExist) {
		if strings.HasSuffix(e.path, ".md") {
			return ""
		}
		return "not a fact file"
	}
	if err != nil {
		return "unreadable"
	}
	if !info.Mode().IsRegular() {
		return "not a regular file"
	}
	if e.path == store.IndexFile {
		return c.indexRefusal()
	}
	if !indexable(e.path) {
		return "not a fact file"
	}
	return c.factRefusal(e.path)
}

// indexRefusal gates MEMORY.md's whole bytes: its lines must match the facts
// (lint's index-sync), and nothing may sit outside or between them.
func (c classifier) indexRefusal() string {
	raw, err := os.ReadFile(filepath.Join(c.root.Path, store.IndexFile)) //nolint:gosec // the snapshot's own MEMORY.md, lstat-checked as a regular file
	if err != nil {
		return "unreadable"
	}
	rules := slices.Clone(c.findings[store.IndexFile])
	if !c.root.IndexIntact(string(raw)) {
		rules = append(rules, "not as generated")
	}
	slices.Sort(rules)
	return strings.Join(slices.Compact(rules), ", ")
}

func (c classifier) factRefusal(rel string) string {
	raw, err := os.ReadFile(filepath.Join(c.root.Path, filepath.FromSlash(rel))) //nolint:gosec // rel is a path git status reported inside the checkout, lstat-checked as a regular file
	if err != nil {
		return "unreadable"
	}
	f, err := fact.Parse(raw)
	if err != nil {
		return "parse"
	}
	var rules []string
	for _, finding := range lint.FactRules(store.Entry{Root: c.root, Rel: rel, Fact: f, Raw: raw}, c.rules) {
		rules = append(rules, finding.Rule)
	}
	rules = append(rules, c.findings[rel]...)
	for _, reason := range append(gate.Content(string(raw)), gate.Size(len(raw))...) {
		rules = append(rules, "gate:"+reason)
	}
	if len(f.Metadata.Flags) > 0 {
		rules = append(rules, "flagged")
	}
	if f.Metadata.Confidence == "reviewed" {
		rules = append(rules, "unattested-review")
	}
	slices.Sort(rules)
	return strings.Join(slices.Compact(rules), ", ")
}

// indexable reports whether store.Root.Walk would read rel as a fact: a .md
// file below the root with no dot segment.
func indexable(rel string) bool {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 || !strings.HasSuffix(rel, ".md") {
		return false
	}
	return !slices.ContainsFunc(segs, func(s string) bool { return strings.HasPrefix(s, ".") })
}

func (c classifier) holdsIndexable(dirRel string) bool {
	if slices.ContainsFunc(c.special, func(rel string) bool { return strings.HasPrefix(rel, dirRel) && indexable(rel) }) {
		return true
	}
	found := false
	base := filepath.Join(c.root.Path, filepath.FromSlash(dirRel))
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is one Walk cannot index either
		}
		rel, relErr := filepath.Rel(c.root.Path, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path != base && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if indexable(rel) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// unlisted returns the files tree's MEMORY.md lists that tree lacks.
func unlisted(ctx context.Context, dir, tree string) ([]string, error) {
	out, err := runGit(ctx, dir, gitTimeout, nil, "ls-tree", "-r", "-z", "--full-tree", "--name-only", tree)
	if err != nil {
		return nil, errors.New(gitWarning("git ls-tree", out, err))
	}
	files := strings.Split(out, "\x00")
	if !slices.Contains(files, store.IndexFile) {
		return nil, nil
	}
	index, err := runGit(ctx, dir, gitTimeout, nil, "cat-file", "blob", tree+":"+store.IndexFile)
	if err != nil {
		return nil, errors.New(gitWarning("git cat-file", index, err))
	}
	lines, _ := sanitize.Unfence(index)
	var missing []string
	for _, line := range lines {
		if _, rel, ok := store.ParseIndexLine(line); ok && !slices.Contains(files, rel) {
			missing = append(missing, rel)
		}
	}
	return missing, nil
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// runGit returns stdout, or stdout and stderr combined on failure so that a
// warning can quote git's complaint.
func runGit(ctx context.Context, dir string, timeout time.Duration, stdin io.Reader, args ...string) (string, error) {
	return runGitEnv(ctx, dir, nil, timeout, stdin, args...)
}

// runGitEnv is runGit with env set after the repo-locating variables are
// stripped. No hook runs: one could stage, amend or move a ref after the
// gates passed.
func runGitEnv(ctx context.Context, dir string, env []string, timeout time.Duration, stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...) //nolint:gosec // dir is the configured store checkout; args are fixed git subcommands
	// Checkout matches git's English "not a git repository"; literal
	// pathspecs keep a file named like a glob from staging its neighbours.
	cmd.Env = route.RepoEnv(append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1"}, env...)...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func gitWarning(what, out string, err error) string {
	msg := strings.Join(strings.Fields(out), " ")
	if len(msg) > maxGitOut {
		msg = strings.ToValidUTF8(msg[:maxGitOut], "") + "…"
	}
	if msg == "" {
		return fmt.Sprintf("%s failed: %v", what, err)
	}
	return fmt.Sprintf("%s failed: %v: %s", what, err, msg)
}
