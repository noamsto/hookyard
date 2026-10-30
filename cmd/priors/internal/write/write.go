// Package write is priors' write path: a fact is completed, checked, routed,
// gated and written to a store checkout, its local layer or the quarantine,
// and a published fact's checkout is committed.
package write

import (
	"context"
	"errors"
	"fmt"
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
//
// Imported marks a fact copied from an engine's native memory (§4.9): its
// provenance, source, engines and modified are the importer's, and verified
// stays null, since a migrated fact is not freshly verified. Pending names
// facts the same batch is about to write, which a wiki-link may name before
// they land. DryRun runs every check and reports where the fact would land,
// writing nothing. NoCommit leaves a published fact uncommitted, for a caller
// that commits a whole batch once.
type Request struct {
	Fact                    fact.Fact
	Session                 route.Session
	SessionID, Engine, Host string
	External                bool
	IdentityConflict        bool
	Imported                bool
	Pending                 []string
	DryRun                  bool
	NoCommit                bool
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

	var checkout store.Root
	roots := []store.Root{store.QuarantineRoot(cfg)}
	if !dest.Quarantine {
		if checkout, err = store.CheckoutRoot(ctx, cfg, dest.Store); err != nil {
			return Result{}, err
		}
		roots = []store.Root{checkout, store.LocalRoot(cfg, dest.Store)}
	}
	if err := checkDuplicate(roots, f.Name); err != nil {
		return Result{}, err
	}
	if !dest.Quarantine {
		rel, err := filepath.Rel(checkout.Path, checkout.PathFor(f, ""))
		if err != nil {
			return Result{}, err
		}
		candidate := store.Entry{Root: checkout, Rel: filepath.ToSlash(rel), Fact: f, Raw: data}
		opts := lint.Options{Store: dest.Store, WorkOrgs: cfg.WorkOrgs, WorkNames: cfg.WorkNames(), Rules: d.Rules, Pending: req.Pending}
		if findings := lint.Candidate(ctx, checkout, candidate, opts); len(findings) > 0 {
			return Result{}, refusal(findings)
		}
	}

	flags := gate.Provenance(cfg.RecordDir(), cfg.ProvenanceDir(), req.SessionID, req.External)
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
	if err := root.CheckWrite(filepath.Dir(res.Path)); err != nil {
		return Result{}, err
	}
	if req.DryRun {
		return res, nil
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

	if res.Outcome == "published" && !req.NoCommit {
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
	m.Flags = nil
	today := now.Format(time.DateOnly)
	if m.ValidFrom == "" {
		m.ValidFrom = today
	}
	if req.Imported {
		return f
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
func checkDuplicate(roots []store.Root, name string) error {
	for _, r := range roots {
		if e, ok := r.FindByName(name); ok {
			return fmt.Errorf("a fact named %q already exists at %s", name, filepath.Join(r.Path, filepath.FromSlash(e.Rel)))
		}
	}
	return nil
}
