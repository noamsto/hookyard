package verify

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

// outcome is what the work phases leave for the reserve: the status, the
// walked prefix to checkpoint, and the verdict lines (ok and stale-remote only).
type outcome struct {
	res    Result
	w      walkResult
	states map[string]attest.State
	seqs   map[string]uint64
}

func behind(soft context.Context, reason string, w walkResult) outcome {
	if soft.Err() != nil {
		reason = "deadline"
	}
	return outcome{res: Result{Status: StatusBehind, Reason: reason}, w: w}
}

// Run fetches the store, walks what is new and writes the verdict file (spec
// R3–R9). Once gating has passed, every failure ends the run behind with a
// header-only file, so content an agent pushed can never keep an old reviewed
// by making the job error out; only a failure to write the state dir itself
// is returned.
func (j *Job) Run(ctx context.Context) (Result, error) {
	hard, cancel := context.WithTimeout(ctx, j.Budget.Wall)
	defer cancel()
	soft, cancelSoft := context.WithTimeout(hard, j.Budget.Wall-j.Budget.Reserve)
	defer cancelSoft()

	private := filepath.Join(j.Dir, "private")
	m := mirror{dir: filepath.Join(private, "mirror.git"), private: private, credential: j.Credential}

	r, exists, err := loadRecord(filepath.Join(private, "record.json"))
	if err != nil {
		// The record is left for a human; with no tip to write a header from,
		// the old verdict file goes rather than standing for up to 24 h.
		if err := j.removeVerdicts(); err != nil {
			return Result{}, err
		}
		return Result{Status: StatusBehind, Reason: "record unreadable"}, nil
	}

	if err := m.init(soft); err != nil {
		return j.finish(hard, m, r, behind(soft, "mirror init failed", walkResult{}))
	}
	if !exists {
		// A fresh record saved beside old walked refs would skip their
		// commits for good, so nothing is saved until they are gone.
		if err := m.dropWalked(soft); err != nil {
			if err := j.removeVerdicts(); err != nil {
				return Result{}, err
			}
			return behind(soft, "mirror init failed", walkResult{}).res, nil
		}
	}
	return j.finish(hard, m, r, j.observe(soft, m, r))
}

// observe runs the work phases under soft: fetch, walk, tip read and lines.
// Every error becomes a behind reason, and the walked prefix is kept whatever
// the status so the next run resumes from it.
func (j *Job) observe(soft context.Context, m mirror, r *Record) outcome {
	if err := cleanTmp(m.dir); err != nil {
		return behind(soft, "mirror cleanup failed", walkResult{})
	}
	size, err := dirSize(m.dir)
	if err != nil {
		return behind(soft, "mirror unreadable", walkResult{})
	}

	oversize := size >= j.Budget.MirrorBytes
	var fetched, refused bool
	if !oversize {
		oid, err := m.fetch(soft, j.Store.Remote)
		var fe *fetchError
		switch {
		case err == nil:
			if m.recordTip(soft, oid) == nil {
				r.Tip, r.Fetched = oid, j.Now().Unix()
				fetched = true
			}
		case errors.As(err, &fe) && fe.preAuth:
			refused = true
		}
	}

	cf, err := newCatFile(soft, m)
	if err != nil {
		return behind(soft, "walk failed", walkResult{})
	}
	defer cf.close()
	w, err := m.walk(soft, cf, r, j.Store.ID, j.Budget.Commits)
	if err != nil {
		return behind(soft, "walk failed", walkResult{})
	}

	switch {
	case soft.Err() != nil:
		return behind(soft, "", w)
	case oversize:
		return outcome{res: Result{Status: StatusOversize, Reason: "mirror at its cap"}, w: w}
	case !fetched && !refused:
		return behind(soft, "fetch failed", w)
	case fetched && !w.Complete:
		return behind(soft, "walk budget", w)
	case refused && !w.Complete:
		return behind(soft, "refused before authentication with unwalked commits", w)
	case refused && (r.Tip == "" || !m.tipReachable(soft, r.Tip)):
		return behind(soft, "refused before authentication with no walked tip", w)
	}

	res := Result{Status: StatusOK}
	if refused {
		res = Result{Status: StatusStaleRemote, Reason: "refused before authentication"}
	}
	tip, err := tipEntries(cf, r.Tip)
	if err != nil {
		return behind(soft, "tip unreadable", w)
	}
	states, seqs := Lines(j.Store.ID, tip, r, j.Keys)
	if soft.Err() != nil {
		return behind(soft, "", w)
	}
	return outcome{res: res, w: w, states: states, seqs: seqs}
}

// removeVerdicts deletes the verdict file, so none stands without a header
// this run can vouch for.
func (j *Job) removeVerdicts() error {
	if err := os.Remove(filepath.Join(j.Dir, "verdicts")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// tipReachable reports whether a tip ref contains tip; after a mirror reset
// none does, and an error counts as no.
func (m mirror) tipReachable(ctx context.Context, tip string) bool {
	out, err := m.run(ctx, "for-each-ref", "--format=%(refname)", "--contains", tip, tipsRef)
	return err == nil && strings.TrimSpace(out) != ""
}

// tipEntries reads the tip commit's .attest entries; one over the entry cap
// can never verify, so it is skipped.
func tipEntries(cf *catFile, tip string) ([]tipEntry, error) {
	tree, _, err := cf.commit(tip)
	if err != nil {
		return nil, err
	}
	names, err := cf.attestEntries(tree)
	if err != nil {
		return nil, err
	}
	var es []tipEntry
	for name, blob := range names {
		raw, err := cf.blob(blob, attest.MaxEntryBytes)
		if errors.Is(err, errTooBig) {
			continue
		}
		if err != nil {
			return nil, err
		}
		es = append(es, tipEntry{Name: name, Raw: raw})
	}
	return es, nil
}

// finish runs inside the reserve: the record, then the checkpoint, then the
// verdict file, which has no header to write until a fetch has succeeded; a
// file from an earlier state dir does not stand in its place.
func (j *Job) finish(hard context.Context, m mirror, r *Record, o outcome) (Result, error) {
	if err := saveRecord(filepath.Join(m.private, "record.json"), r); err != nil {
		return Result{}, err
	}
	if err := m.checkpoint(hard, o.w); err != nil {
		o = outcome{res: Result{Status: StatusBehind, Reason: "checkpoint failed"}}
	}
	if hard.Err() != nil {
		o = outcome{res: Result{Status: StatusBehind, Reason: "deadline"}}
	}
	if r.Tip == "" {
		if err := j.removeVerdicts(); err != nil {
			return Result{}, err
		}
		return o.res, nil
	}
	b, err := attest.FormatVerdicts(attest.Verdicts{
		Store:      j.Store.ID,
		Trust:      j.TrustDigest,
		Tip:        r.Tip,
		Status:     string(o.res.Status),
		Fetched:    time.Unix(r.Fetched, 0),
		States:     o.states,
		Superseded: o.seqs,
	})
	if err != nil {
		return Result{}, err
	}
	if err := atomicfile.Write(filepath.Join(j.Dir, "verdicts"), b, 0o644); err != nil {
		return Result{}, err
	}
	o.res.Wrote = true
	return o.res, nil
}
