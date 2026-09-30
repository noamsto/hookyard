// Package commit is the only code in priors that runs git add, commit or
// push. It commits a store checkout's dirty files only when every one of them
// is gated content, so a store commit never carries a file priors' gates have
// not passed, and HEAD's MEMORY.md never lists a file HEAD lacks.
package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
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
func Checkout(ctx context.Context, cfg config.Config, root store.Root, rules gate.Rules, scanner gate.Scanner, workOrgs, workNames []string, msg string) (warning string) {
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

	opts := lint.Options{Store: root.Store, WorkOrgs: workOrgs, WorkNames: workNames, Rules: rules, Scanner: &scanner, Gates: true}
	findings := map[string][]string{}
	for _, f := range lint.Store(ctx, root, opts) {
		if f.File == "." {
			return fmt.Sprintf("nothing committed in %s: %s: %s", dir, f.Rule, f.Msg)
		}
		findings[f.File] = append(findings[f.File], f.Rule)
	}
	c := classifier{root: root, rules: rules, findings: findings}
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

	pathspecs := strings.NewReader(strings.Join(accepted, "\x00") + "\x00")
	if out, err := runGit(ctx, dir, gitTimeout, pathspecs, "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return gitWarning("git add", out, err)
	}
	if _, err := runGit(ctx, dir, gitTimeout, nil, "diff", "--cached", "--quiet"); err == nil {
		return ""
	}
	if missing, err := unlisted(ctx, dir); err != nil || len(missing) > 0 {
		out, resetErr := runGit(ctx, dir, gitTimeout, nil, "reset", "-q")
		switch {
		case resetErr != nil:
			return gitWarning("git reset", out, resetErr)
		case err != nil:
			return fmt.Sprintf("nothing committed in %s: checking the staged index: %v", dir, err)
		}
		return fmt.Sprintf("nothing committed in %s: %s would list files the commit lacks: %s", dir, store.IndexFile, strings.Join(missing, ", "))
	}
	if out, err := runGit(ctx, dir, gitTimeout, nil, "commit", "-q", "-m", msg); err != nil {
		return gitWarning("git commit", out, err)
	}
	if cfg.Push {
		if out, err := runGit(ctx, dir, pushTimeout, nil, "push", "-q"); err != nil {
			return gitWarning("git push", out, err)
		}
	}
	return ""
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
		return ""
	}
	if !indexable(e.path) {
		return "not a fact file"
	}
	return c.factRefusal(e.path)
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

// unlisted returns the files the staged MEMORY.md lists that the staged tree
// lacks.
func unlisted(ctx context.Context, dir string) ([]string, error) {
	out, err := runGit(ctx, dir, gitTimeout, nil, "ls-files", "-z")
	if err != nil {
		return nil, errors.New(gitWarning("git ls-files", out, err))
	}
	files := strings.Split(out, "\x00")
	if !slices.Contains(files, store.IndexFile) {
		return nil, nil
	}
	index, err := runGit(ctx, dir, gitTimeout, nil, "show", ":"+store.IndexFile)
	if err != nil {
		return nil, errors.New(gitWarning("git show", index, err))
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // dir is the configured store checkout; args are fixed git subcommands
	// Checkout matches git's English "not a git repository"; literal
	// pathspecs keep a file named like a glob from staging its neighbours.
	cmd.Env = repoEnv("GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1")
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

// repoEnv is os.Environ plus extra, minus the variables git exports into a
// hook's environment to locate its own repository: left in place they would
// point every `git -C dir` at that repository instead of dir.
func repoEnv(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(repoLocatingEnv, k)
	})
	return append(env, extra...)
}

var repoLocatingEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE",
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
