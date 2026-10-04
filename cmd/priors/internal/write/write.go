// Package write is priors' write path: a fact is completed, checked, routed,
// gated and written to a store checkout, its local layer or the quarantine,
// and a published fact's checkout is committed.
package write

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// Request is one fact to add and who is adding it. Session is where the
// session runs; SessionID, Engine and Host become the fact's provenance.
// External marks a session that ingested external content; IdentityConflict
// marks an asserted session identity the caller could not corroborate.
type Request struct {
	Fact                    fact.Fact
	Session                 route.Session
	SessionID, Engine, Host string
	External                bool
	IdentityConflict        bool
}

// Result says where the fact landed. Outcome is "published", "flagged" or
// "quarantined"; Path is the fact file. Reasons are the flag reasons, led by
// the quarantine's reason for a quarantined fact. Warning is the
// commit.Checkout report: "" on a clean commit+push (or nothing to do), or a
// note describing a commit/push failure or paths outside the fact layout that
// were left uncommitted. Reports are the index regeneration's, naming each
// file it skipped or kept out.
type Result struct {
	Outcome, Path string
	Store         route.StoreID
	Reasons       []string
	Warning       string
	Reports       []string
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

	if findings := lint.FactRules(store.Entry{Rel: f.Name + ".md", Fact: f, Raw: data}, d.Rules); len(findings) > 0 {
		return Result{}, refusal(findings)
	}
	ids, err := d.Scanner.ScanText(ctx, string(data))
	if err != nil {
		return Result{}, fmt.Errorf("secret scanner unavailable: %w", err)
	}
	if len(ids) > 0 {
		return Result{}, fmt.Errorf("secret scan matched: %s", strings.Join(ids, ", "))
	}

	dest := route.WriteDest(req.Session, cfg)
	learned := req.Session.Repo
	lockName := string(dest.Store)
	if dest.Quarantine {
		lockName = "quarantine"
	}
	unlock, err := store.Lock(cfg, lockName)
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
		candidate := store.Entry{Root: checkout, Rel: filepath.ToSlash(rel), Fact: f, Raw: data}
		opts := lint.Options{Store: dest.Store, WorkOrgs: cfg.WorkOrgs, WorkNames: cfg.WorkNames, Rules: d.Rules}
		if findings := lint.Candidate(ctx, checkout, candidate, opts); len(findings) > 0 {
			return Result{}, refusal(findings)
		}
	}

	flags := gate.Provenance(cfg.RecordDir(), req.SessionID, req.External)
	if req.IdentityConflict {
		flags = append(flags, "provenance:asserted-identity")
	}
	flags = append(flags, gate.Content(string(data))...)
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
	if err := confineDir(root.Path, filepath.Dir(res.Path)); err != nil {
		return Result{}, err
	}
	if data, err = f.Marshal(); err != nil {
		return Result{}, err
	}
	if err := atomicfile.Write(res.Path, data, 0o644); err != nil {
		return Result{}, err
	}
	_, reports, err := root.WriteIndex(d.Rules)
	if err != nil {
		_ = os.Remove(res.Path)
		return Result{}, err
	}
	res.Reports = reports

	if res.Outcome == "published" {
		res.Warning = commit.Checkout(ctx, cfg, root, d.Rules, d.Scanner, "priors: add "+f.Name)
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
	return errors.New(strings.Join(msgs, "; "))
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
			return fmt.Errorf("a fact named %q already exists at %s", name, filepath.Join(r.Path, filepath.FromSlash(e.Rel)))
		}
	}
	return nil
}

// confineDir refuses a destination directory that a symlink could steer out
// of root: a store commit can plant "repo -> ../elsewhere", and MkdirAll and
// the write would follow it.
func confineDir(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s is not inside the store %s", dir, root)
	}
	existing := root
	for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
		p := filepath.Join(existing, seg)
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through the symlink %s", p)
		}
		existing = p
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return err
	}
	if inside, err := filepath.Rel(resolvedRoot, resolved); err != nil || !filepath.IsLocal(inside) {
		return fmt.Errorf("%s resolves outside the store %s", dir, root)
	}
	return nil
}
