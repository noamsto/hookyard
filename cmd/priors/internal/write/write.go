// Package write is priors' write path: a fact is completed, checked, routed,
// gated and written to a store checkout, its local layer or the quarantine,
// and a published fact is committed to the checkout.
package write

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	gitTimeout  = 10 * time.Second
	pushTimeout = 30 * time.Second
	maxGitOut   = 200
)

// Request is one fact to add and who is adding it. Session is where the
// session runs; SessionID, Engine and Host become the fact's provenance.
// External marks a session that ingested external content.
type Request struct {
	Fact                    fact.Fact
	Session                 route.Session
	SessionID, Engine, Host string
	External                bool
}

// Result says where the fact landed. Outcome is "published", "flagged" or
// "quarantined"; Path is the fact file. Reasons are the flag reasons, led by
// the quarantine's reason for a quarantined fact. Warning reports a publish
// (commit or push) that failed after the fact was written.
type Result struct {
	Outcome, Path string
	Store         route.StoreID
	Reasons       []string
	Warning       string
}

type Deps struct {
	Rules   gate.Rules
	Scanner gate.Scanner
	Now     func() time.Time
}

// Add writes req's fact. A non-nil error is a refusal (or a failed write) and
// leaves nothing written.
func Add(ctx context.Context, cfg config.Config, req Request, d Deps) (Result, error) {
	f := complete(req, d.Now())
	data, err := f.Marshal()
	if err != nil {
		return Result{}, err
	}

	if findings := lint.FactRules(store.Entry{Rel: f.Name + ".md", Fact: f}, d.Rules); len(findings) > 0 {
		return Result{}, refusal(findings)
	}
	ids, err := d.Scanner.ScanText(ctx, string(data))
	if err != nil {
		return Result{}, fmt.Errorf("secret scanner unavailable: %w", err)
	}
	if len(ids) > 0 {
		return Result{}, fmt.Errorf("refused by secret scan: %s", strings.Join(ids, ", "))
	}

	dest := route.WriteDest(req.Session, cfg)
	learned := req.Session.Repo
	lockName := string(dest.Store)
	if dest.Quarantine {
		lockName = "quarantine"
	}
	unlock, err := lock(filepath.Join(cfg.State(), "locks", lockName+".lock"))
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	checkout := store.CheckoutRoot(cfg, dest.Store)
	if err := checkDuplicate(cfg, dest, f.Name); err != nil {
		return Result{}, err
	}
	if !dest.Quarantine {
		rel, err := filepath.Rel(checkout.Path, checkout.PathFor(f, ""))
		if err != nil {
			return Result{}, err
		}
		candidate := store.Entry{Root: checkout, Rel: filepath.ToSlash(rel), Fact: f}
		opts := lint.Options{Store: dest.Store, WorkOrgs: cfg.WorkOrgs, WorkNames: cfg.WorkNames, Rules: d.Rules}
		if findings := lint.Candidate(ctx, checkout, candidate, opts); len(findings) > 0 {
			return Result{}, refusal(findings)
		}
	}

	flags := gate.Provenance(cfg.RecordDir(), req.SessionID, req.External)
	flags = append(flags, gate.Content(f.Text())...)
	flags = append(flags, gate.Size(len(data))...)

	res := Result{Store: dest.Store}
	var root store.Root
	switch {
	case dest.Quarantine:
		root = store.QuarantineRoot(cfg)
		res.Path = root.PathFor(f, learned)
		res.Outcome = "quarantined"
		res.Reasons = append([]string{dest.Why}, flags...)
		f.Metadata.Flags = flags
	case len(flags) > 0:
		root = store.LocalRoot(cfg, dest.Store)
		res.Path = root.PathFor(f, learned)
		res.Outcome = "flagged"
		res.Reasons = flags
		f.Metadata.Flags = flags
	default:
		root = checkout
		res.Path = root.PathFor(f, "")
		res.Outcome = "published"
	}
	if data, err = f.Marshal(); err != nil {
		return Result{}, err
	}
	if err := atomicfile.Write(res.Path, data, 0o644); err != nil {
		return Result{}, err
	}
	if _, err := root.WriteIndex(); err != nil {
		_ = os.Remove(res.Path)
		return Result{}, err
	}

	if res.Outcome == "published" && cfg.CommitEnabled() {
		res.Warning = publish(ctx, cfg, root.Path, res.Path, f.Name)
	}
	return res, nil
}

// complete fills the fields add owns. Flags are cleared because only the
// gates below may set them.
func complete(req Request, now time.Time) fact.Fact {
	f := req.Fact
	m := &f.Metadata
	m.NodeType = "memory"
	m.Confidence = "proposed"
	today := now.Format(time.DateOnly)
	if m.ValidFrom == "" {
		m.ValidFrom = today
	}
	if m.Verified == "" {
		m.Verified = today
	}
	m.Modified = now.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	m.Provenance = &fact.Provenance{Engine: req.Engine, Session: req.SessionID, Host: req.Host}
	m.OriginSessionID = req.SessionID
	if len(m.Engines) == 0 && req.Engine != "" && req.Engine != "unknown" {
		m.Engines = []string{req.Engine}
	}
	m.Flags = nil
	return f
}

func refusal(findings []lint.Finding) error {
	msgs := make([]string, len(findings))
	for i, f := range findings {
		msgs[i] = f.String()
	}
	return errors.New("refused: " + strings.Join(msgs, "; "))
}

// checkDuplicate refuses a name the destination already holds. A store's
// checkout and its local layer share one namespace, since a flagged fact is
// published under its name once reviewed.
func checkDuplicate(cfg config.Config, dest route.Dest, name string) error {
	roots := []store.Root{store.CheckoutRoot(cfg, dest.Store), store.LocalRoot(cfg, dest.Store)}
	if dest.Quarantine {
		roots = []store.Root{store.QuarantineRoot(cfg)}
	}
	for _, r := range roots {
		if e, ok := r.FindByName(name); ok {
			return fmt.Errorf("refused: a fact named %q already exists at %s", name, filepath.Join(r.Path, filepath.FromSlash(e.Rel)))
		}
	}
	return nil
}

// lock takes an exclusive flock on path. It serialises writers across
// processes, so a duplicate-name check, the write, the index and the commit
// see one consistent destination.
func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the state dir is the user's own; 0755 matches the rest of it
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is built from the configured state dir and a fixed name
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // a file descriptor fits in an int
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// publish commits the fact and the index to dir when dir is itself a git
// work tree's top level, and pushes when configured. It returns a warning, or
// "" on success or when dir is not a repo of its own.
func publish(ctx context.Context, cfg config.Config, dir, path, name string) string {
	top, err := runGit(ctx, dir, gitTimeout, "rev-parse", "--show-toplevel")
	if err != nil || !sameDir(strings.TrimSpace(top), dir) {
		return ""
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "commit skipped: " + err.Error()
	}
	rel = filepath.ToSlash(rel)
	if out, err := runGit(ctx, dir, gitTimeout, "add", "--", rel, store.IndexFile); err != nil {
		return gitWarning("git add", out, err)
	}
	if out, err := runGit(ctx, dir, gitTimeout, "commit", "-q", "-m", "priors: add "+name, "--", rel, store.IndexFile); err != nil {
		return gitWarning("git commit", out, err)
	}
	if cfg.Push {
		if out, err := runGit(ctx, dir, pushTimeout, "push", "-q"); err != nil {
			return gitWarning("git push", out, err)
		}
	}
	return ""
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // dir is the configured store checkout; args are fixed git subcommands and store-relative paths
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
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
