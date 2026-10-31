// Package importer copies engines' native memories into the personal store
// (memory-layer §4.9). Every item that survives dedup goes through
// write.Add, so redaction, the secret gate, routing and the gates are the
// write path's own; the importer only normalises, dedups and reports.
package importer

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/write"
)

// Item is one native memory as an engine's reader found it. Source is the
// native path as the fact's source block records it; Raw is the source's
// bytes, which dedup hashes; Native carries the fields a canonical copy keeps,
// mapped from the engine's own shape; Session is the repo the engine wrote it
// in, resolved per engine (§4.2).
type Item struct {
	Engine  string
	Source  string
	Raw     []byte
	Native  fact.Fact
	Session route.Session
}

// Outcome is what a run did with one item, or one note from a reader.
type Outcome string

const (
	Published    Outcome = "published"
	Flagged      Outcome = "flagged"
	WouldPublish Outcome = "would publish"
	WouldFlag    Outcome = "would flag"
	Unchanged    Outcome = "unchanged"
	Duplicate    Outcome = "duplicate"
	Proposed     Outcome = "propose"
	Skipped      Outcome = "skipped"
)

// Report is one line of a run's output. Path is the store file the outcome
// concerns: where the fact landed, or the fact a duplicate or proposal is
// against. Detail is the reason or the flag reasons.
type Report struct {
	Outcome Outcome
	Source  string
	Path    string
	Detail  string
}

func (r Report) String() string {
	var line string
	switch r.Outcome {
	case Published, Flagged, WouldPublish, WouldFlag:
		line = fmt.Sprintf("%s personal %s <- %s", r.Outcome, r.Path, r.Source)
	case Proposed:
		line = fmt.Sprintf("%s %s -> %s", r.Outcome, r.Source, r.Path)
	case Unchanged, Duplicate:
		line = fmt.Sprintf("%s %s: %s", r.Outcome, r.Source, r.Path)
	default:
		line = fmt.Sprintf("%s %s", r.Outcome, r.Source)
	}
	if r.Detail != "" {
		line += ": " + r.Detail
	}
	return line
}

// Options is who runs the import and how. SessionID, Engine, External and
// IdentityConflict describe the importing session, which gate 2 judges:
// native memory is not a non-owner source, so only what the importer's own
// session ingested flags. Host is this host, recorded in each copy's
// provenance, since a native store is per host.
type Options struct {
	Config           config.Config
	Deps             write.Deps
	SessionID        string
	Engine           string
	Host             string
	External         bool
	IdentityConflict bool
	DryRun           bool
}

// candidate is an item that passed normalisation and dedup.
type candidate struct {
	item Item
	fact fact.Fact
}

// Result is a run's reports, one per item, and the commit's warning, as
// write.Result.Warning words it. Failed counts the items that did not land
// because something broke (the scanner, a write), not because the
// write path refused them: a re-run may clear these, and the caller should
// not report the run as clean.
type Result struct {
	Reports []Report
	Warning string
	Failed  int
}

// Run imports items into the personal store. A dry run is the review step:
// it runs every check and writes nothing, so the owner sees what would land
// and what is only proposed before a real run commits. A real run holds the
// store's lock throughout, so no other writer commits a half-imported
// checkout, and commits what it published once, at the end, through the
// store's commit gate; it commits on every run, so a commit held back last
// time is retried. The error is a store the run cannot read, before anything
// is written.
func Run(ctx context.Context, items []Item, o Options) (Result, error) {
	if !o.DryRun {
		unlock, err := store.Lock(o.Config, string(route.StorePersonal))
		if err != nil {
			return Result{}, err
		}
		defer unlock()
	}
	checkout, known, damaged, err := loadStore(ctx, o.Config)
	if err != nil {
		return Result{}, err
	}
	var out Result
	for _, d := range damaged {
		out.Reports = append(out.Reports, Report{Outcome: Skipped, Source: d.path, Detail: "unreadable store fact: " + d.err.Error()})
	}
	pl := planRun(ctx, items, o, known, damaged)
	out.Reports = append(out.Reports, pl.notes...)
	out.Failed = pl.failed
	if o.DryRun {
		for _, p := range pl.items {
			out.Reports = append(out.Reports, written(p.res, p.item.Source, true))
		}
		return out, nil
	}
	published := writeAll(ctx, o, checkout, pl, &out)
	out.Warning = joinWarnings(out.Warning, commitImport(ctx, o, checkout, published))
	return out, nil
}

func joinWarnings(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "; " + b
}

// planned is a candidate with the outcome of its dry run: where it would
// land, and whether that is the checkout, which only a published fact's
// wiki-link target counts.
type planned struct {
	candidate
	idx       int
	res       write.Result
	publishes bool
}

// plan is the run's fate for every item. setAside holds the names of the items
// set aside for a held-back link, which a fact planned flagged may still link.
// unlanded holds every name a link may name that is not in the checkout, so a
// published fact linking one of them, and not written before it, dangles.
type plan struct {
	items    []planned
	notes    []Report
	failed   int
	setAside map[string]bool
	unlanded map[string]bool
}

// excluded is why an item left the plan. policy is true for a decision (the
// write path's refusal, a damaged file in the way), false for a failure.
// waitsFor, when set, makes it a set-aside item instead: it was refused only
// for its link to the held-back fact waitsFor, found at waitsAt. blockers are
// all the held-back facts it links.
type excluded struct {
	err      error
	policy   bool
	waitsFor string
	waitsAt  string
	blockers []string
}

func request(p candidate, o Options, pending []string, dryRun bool) write.Request {
	return write.Request{
		Fact:             p.fact,
		Session:          p.item.Session,
		SessionID:        o.SessionID,
		Engine:           o.Engine,
		Host:             o.Host,
		External:         o.External,
		IdentityConflict: o.IdentityConflict,
		Imported:         true,
		Pending:          pending,
		DryRun:           dryRun,
		NoCommit:         true,
	}
}

// planRun decides every item's fate before anything is written. A fact's
// wiki-link may name another fact of the run, but only one that lands in the
// checkout: a target that is refused, or that lands flagged in the local
// layer, leaves a published fact's link dangling and the commit gate refusing
// the checkout. A fact that lands flagged is not under that gate, so it may
// name any fact of the run still live, held or set aside. So each round
// dry-runs the items with the names that publish as the ones a published
// fact's link may name, and repeats until nothing changes; an item the write
// path refuses is dropped and the rest re-prepared, so a near duplicate no
// longer shadows against a fact that will not land.
//
// A link may name any personal-bound item of the run (runNames), whatever its
// fate, so facts that link each other resolve. A refusal that only a link to a
// held-back name explains is not final, since that item may be shadowed now
// and land once another is dropped. Within a round such an item is held: it
// leaves the names others may link, and the round dry-runs the rest again
// until nothing more is held. Held only grows, so that settles. A drop or a
// newly flagged item changes the next round, so the held items are dry-run
// afresh in it.
//
// Once planning settles with items still held, they are set aside a generation
// at a time, so they stop shadowing their near duplicates. A held item waits,
// and is kept for the next round, when a link it is held for names a fact that
// a near duplicate in the run hides (it may surface once that duplicate is set
// aside) or another waiting item; waiting is propagated to a fixpoint. Only the
// items not waiting are set aside, or all of them when every item waits. Dropped
// only grows, so the rounds stay bounded.
//
// A later item whose bytes, repo and engine an earlier item of the run has is
// reported a duplicate of it, whatever the earlier item's fate. That is decided
// once up front, so a copy set aside or refused never lets the other copy stand
// in for it. A copy of a fact in the store is reported against the store first.
func planRun(ctx context.Context, items []Item, o Options, base []existing, damaged []damagedFact) plan {
	damagedPaths := map[string]bool{}
	for _, d := range damaged {
		damagedPaths[filepath.Clean(d.path)] = true
	}
	inCheckout, localOnly := map[string]bool{}, map[string]string{}
	for _, k := range base {
		if k.inCheckout {
			inCheckout[k.fact.Name] = true
		} else {
			localOnly[k.fact.Name] = k.path
		}
	}
	type copyKey struct{ sum, repo, engine string }
	runNames, resolved := map[string]string{}, make([]string, len(items))
	sums, firstCopy, copyOf := make([]string, len(items)), map[copyKey]int{}, map[int]int{}
	for i, it := range items {
		sums[i] = digest(it.Raw)
		k := copyKey{sums[i], it.Session.Repo, it.Engine}
		if j, ok := firstCopy[k]; ok {
			copyOf[i] = j
		}
		if _, name, why := personalBound(it, o); why == "" {
			resolved[i] = name
			if _, ok := runNames[name]; !ok {
				runNames[name] = it.Source
			}
			if _, ok := firstCopy[k]; !ok {
				firstCopy[k] = i
			}
		}
	}
	unlanded := map[string]bool{}
	for _, names := range []map[string]string{runNames, localOnly} {
		for n := range names {
			unlanded[n] = !inCheckout[n]
		}
	}
	dropped := map[int]excluded{}
	unpublished := map[int]bool{}
	setAside := map[string]bool{}
	for {
		known := slices.Clone(base)
		var cands []candidate
		idxs := make([]int, 0, len(items))
		var notes []Report
		hidden := map[string]bool{}
		for i, it := range items {
			if why, ok := dropped[i]; ok {
				notes = append(notes, droppedReport(it, why))
				continue
			}
			if j, ok := copyOf[i]; ok {
				rep, ok := sameBytes(it, sums[i], base)
				if !ok {
					rep = Report{Outcome: Duplicate, Source: it.Source, Path: items[j].Source + " (this run)", Detail: "same source bytes"}
				}
				notes = append(notes, rep)
				continue
			}
			c, rep, ok := prepare(it, o, known)
			if !ok {
				notes = append(notes, rep)
				if rep.Outcome == Proposed && strings.HasSuffix(rep.Path, " (this run)") && resolved[i] != "" {
					hidden[resolved[i]] = true
				}
				continue
			}
			known = append(known, existing{path: it.Source + " (this run)", fact: c.fact, sum: c.fact.Metadata.Source.SHA256})
			cands = append(cands, c)
			idxs = append(idxs, i)
		}
		names := map[string]bool{}
		for _, k := range known {
			names[k.fact.Name] = true
		}
		for n := range runNames {
			names[n] = true
		}
		for j := range cands {
			cands[j].fact.Body = relink(cands[j].fact.Body, names)
		}

		held := map[int]excluded{}
		var out []planned
		changed, grew := false, true
		for grew && !changed {
			grew = false
			out = nil
			pending, flaggedMay := map[string]bool{}, maps.Clone(setAside)
			for j := range cands {
				flaggedMay[cands[j].fact.Name] = true
				if _, ok := held[idxs[j]]; ok || unpublished[idxs[j]] {
					continue
				}
				pending[cands[j].fact.Name] = true
			}
			publishing, flagging := slices.Sorted(maps.Keys(pending)), slices.Sorted(maps.Keys(flaggedMay))
			for j, c := range cands {
				if _, ok := held[idxs[j]]; ok {
					continue
				}
				targets := publishing
				if unpublished[idxs[j]] {
					targets = flagging
				}
				res, err := write.AddLocked(ctx, o.Config, request(c, o, targets, true), o.Deps)
				if err != nil && errors.Is(err, write.ErrRefusal) {
					if missing := heldBackTargets(ctx, o, c, targets, unlanded); len(missing) > 0 {
						held[idxs[j]] = excluded{err: err, policy: true, waitsFor: missing[0], blockers: missing}
						grew = true
						continue
					}
				}
				switch {
				case err != nil:
					dropped[idxs[j]] = excluded{err: err, policy: errors.Is(err, write.ErrRefusal)}
					changed = true
					continue
				case damagedPaths[filepath.Clean(res.Path)]:
					dropped[idxs[j]] = excluded{err: fmt.Errorf("it would overwrite the unreadable store fact %s", res.Path), policy: true}
					changed = true
					continue
				}
				publishes := res.Outcome == "published"
				if !publishes && !unpublished[idxs[j]] {
					unpublished[idxs[j]] = true
					changed = true
				}
				out = append(out, planned{candidate: c, idx: idxs[j], res: res, publishes: publishes})
			}
		}
		if changed {
			continue
		}
		if len(held) > 0 {
			at := map[string]string{}
			for _, p := range out {
				at[p.fact.Name] = p.res.Path
			}
			heldAt := map[string]int{}
			for idx := range held {
				heldAt[resolved[idx]] = idx
			}
			waiting := map[int]bool{}
			for grew := true; grew; {
				grew = false
				for idx, why := range held {
					if waiting[idx] || !slices.ContainsFunc(why.blockers, func(t string) bool {
						j, isHeld := heldAt[t]
						return hidden[t] || (isHeld && waiting[j])
					}) {
						continue
					}
					waiting[idx] = true
					grew = true
				}
			}
			aside := maps.Clone(held)
			for idx := range waiting {
				delete(aside, idx)
			}
			if len(aside) == 0 {
				aside = held
			}
			for idx, why := range aside {
				if _, ok := at[why.waitsFor]; !ok {
					at[why.waitsFor] = cmp.Or(localOnly[why.waitsFor], runNames[why.waitsFor])
				}
				why.waitsAt = at[why.waitsFor]
				dropped[idx] = why
				setAside[resolved[idx]] = true
			}
			continue
		}
		pl := plan{items: out, notes: notes, setAside: setAside, unlanded: unlanded}
		for _, why := range dropped {
			if !why.policy {
				pl.failed++
			}
		}
		return pl
	}
}

// heldBackTargets are the names c, refused with targets as the names its links
// may name, links that are held back (unlanded: in the run or the local layer,
// not in the checkout) and are not in targets, provided c would pass if it
// could name them. A link to a name nowhere cannot come to resolve, so c stays
// refused, as it does when something else also refuses it.
func heldBackTargets(ctx context.Context, o Options, c candidate, targets []string, unlanded map[string]bool) []string {
	var missing []string
	for _, t := range linkTargets(c.fact.Body) {
		if unlanded[t] && !slices.Contains(targets, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := write.AddLocked(ctx, o.Config, request(c, o, slices.Concat(targets, missing), true), o.Deps); err != nil {
		return nil
	}
	return missing
}

func droppedReport(it Item, why excluded) Report {
	if why.waitsFor != "" {
		return Report{Outcome: Proposed, Source: it.Source, Path: why.waitsAt, Detail: "waits for [[" + why.waitsFor + "]], which is held back (flagged, refused or set aside)"}
	}
	kind := "failed: "
	if why.policy {
		kind = "refused: "
	}
	return Report{Outcome: Skipped, Source: it.Source, Detail: kind + why.err.Error()}
}

// writeAll writes the plan's items, appending their reports to out in plan
// order, and returns how many it published. An item is written after the facts
// of the run it links, so a run killed half way leaves no published link
// dangling. The plan holds, but a write can still fail, and a fact planned
// flagged can publish once the session's record has caught up. A published
// fact that links an in-run fact outside the checkout is not kept; neither
// are the facts already written that link it, directly or through another,
// and later facts no longer count it as a link target.
func writeAll(ctx context.Context, o Options, checkout store.Root, pl plan, out *Result) (published int) {
	pending, live := map[string]bool{}, maps.Clone(pl.setAside)
	for _, p := range pl.items {
		live[p.fact.Name] = true
		if p.publishes {
			pending[p.fact.Name] = true
		}
	}
	type landed struct {
		p      planned
		report int
	}
	var landing []landed
	rolledBack := false
	first := len(out.Reports)
	out.Reports = append(out.Reports, make([]Report, len(pl.items))...)
	for _, i := range writeOrder(pl.items) {
		p := pl.items[i]
		report := first + i
		targets := pending
		if !p.publishes {
			targets = live
		}
		dangling := ""
		for _, t := range linkTargets(p.fact.Body) {
			if t != p.fact.Name && pl.unlanded[t] && !pending[t] {
				dangling = t
				break
			}
		}
		var res write.Result
		var err error
		if p.publishes && dangling != "" {
			err = fmt.Errorf("it links %s, which was not written", dangling)
		} else {
			res, err = write.AddLocked(ctx, o.Config, request(p.candidate, o, slices.Sorted(maps.Keys(targets)), false), o.Deps)
		}
		if err == nil && res.Outcome == "published" && dangling != "" {
			if rmErr := os.Remove(res.Path); rmErr != nil {
				out.Warning = joinWarnings(out.Warning, "rolling back "+p.item.Source+": "+rmErr.Error())
			}
			rolledBack = true
			err = fmt.Errorf("it links %s, which was not written", dangling)
		}
		if err != nil {
			delete(live, p.fact.Name)
			out.Reports[report] = droppedReport(p.item, excluded{err: err, policy: errors.Is(err, write.ErrRefusal)})
			if !errors.Is(err, write.ErrRefusal) {
				out.Failed++
			}
		} else {
			out.Reports[report] = written(res, p.item.Source, false)
			if res.Outcome == "published" {
				landing = append(landing, landed{p: planned{candidate: p.candidate, res: res}, report: report})
				pending[p.fact.Name] = true
			}
		}
		if !p.publishes || (err == nil && res.Outcome == "published") {
			continue
		}
		delete(pending, p.fact.Name)
		dead := map[string]bool{p.fact.Name: true}
		for grew := true; grew; {
			grew = false
			for _, l := range landing {
				if dead[l.p.fact.Name] || !slices.ContainsFunc(linkTargets(l.p.fact.Body), func(t string) bool { return dead[t] }) {
					continue
				}
				dead[l.p.fact.Name] = true
				grew = true
				if err := os.Remove(l.p.res.Path); err != nil {
					out.Warning = joinWarnings(out.Warning, "rolling back "+l.p.item.Source+": "+err.Error())
				}
				out.Reports[l.report] = Report{Outcome: Skipped, Source: l.p.item.Source, Detail: "failed: rolled back, it links " + p.fact.Name + " or a fact that links it, which was not written"}
				out.Failed++
				delete(pending, l.p.fact.Name)
				rolledBack = true
			}
		}
		landing = slices.DeleteFunc(landing, func(l landed) bool { return dead[l.p.fact.Name] })
	}
	if rolledBack {
		if _, _, err := checkout.WriteIndex(o.Deps.Rules); err != nil {
			out.Warning = joinWarnings(out.Warning, "regenerating the index after a rollback: "+err.Error())
		}
	}
	return len(landing)
}

// writeOrder lists the indices of items in the order to write them: each item
// after the published facts of the run it links, otherwise in plan order.
// Facts that link each other keep their plan order, since one must go first.
func writeOrder(items []planned) []int {
	publishing := map[string]int{}
	for i, p := range items {
		if p.publishes {
			publishing[p.fact.Name] = i
		}
	}
	deps := make([][]int, len(items))
	for i, p := range items {
		for _, t := range linkTargets(p.fact.Body) {
			if j, ok := publishing[t]; ok && j != i {
				deps[i] = append(deps[i], j)
			}
		}
	}
	done := make([]bool, len(items))
	order := make([]int, 0, len(items))
	for len(order) < len(items) {
		next := -1
		for i := range items {
			if done[i] {
				continue
			}
			if next < 0 {
				next = i
			}
			if !slices.ContainsFunc(deps[i], func(j int) bool { return !done[j] }) {
				next = i
				break
			}
		}
		done[next] = true
		order = append(order, next)
	}
	return order
}

// linkTargets names the facts body links to.
func linkTargets(body string) []string {
	var out []string
	for _, m := range wikilinkRE.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// commitImport commits the checkout, as write.Add commits one fact. The
// caller holds the store's lock.
func commitImport(ctx context.Context, o Options, checkout store.Root, n int) (warning string) {
	msg := "priors: import"
	if n > 0 {
		msg = fmt.Sprintf("priors: import %d facts", n)
	}
	return commit.Checkout(ctx, o.Config, checkout, o.Deps.Rules, o.Deps.Scanner, msg)
}

func written(res write.Result, source string, dryRun bool) Report {
	r := Report{Source: source, Path: res.Path, Detail: strings.Join(res.Reasons, ", ")}
	switch {
	case res.Outcome == "flagged" && dryRun:
		r.Outcome = WouldFlag
	case res.Outcome == "flagged":
		r.Outcome = Flagged
	case dryRun:
		r.Outcome = WouldPublish
	default:
		r.Outcome = Published
	}
	return r
}

// existing is a fact dedup compares against: one already in the store, or one
// accepted earlier in this run. sum is its source.sha256, or "" for a fact
// that was not imported; raw is the sha256 of its file's bytes. inCheckout is
// true for a fact in the checkout, the only place a published fact's wiki-link
// can resolve to.
type existing struct {
	path       string
	fact       fact.Fact
	sum        string
	raw        string
	inCheckout bool
}

// damagedFact is a file in the store Walk could not read, which dedup cannot
// see.
type damagedFact struct {
	path string
	err  error
}

// loadStore reads the personal store's checkout and local layer, the
// destinations an import can land in, and the files in them it could not
// read.
func loadStore(ctx context.Context, cfg config.Config) (store.Root, []existing, []damagedFact, error) {
	checkout, err := store.CheckoutRoot(ctx, cfg, route.StorePersonal)
	if err != nil {
		return store.Root{}, nil, nil, err
	}
	var out []existing
	var damaged []damagedFact
	for _, root := range []store.Root{checkout, store.LocalRoot(cfg, route.StorePersonal)} {
		entries, walkErrs := root.Walk()
		for _, e := range entries {
			k := existing{path: filepath.Join(root.Path, filepath.FromSlash(e.Rel)), fact: e.Fact, raw: digest(e.Raw), inCheckout: root.Path == checkout.Path}
			if s := e.Fact.Metadata.Source; s != nil {
				k.sum = s.SHA256
			}
			out = append(out, k)
		}
		for _, we := range walkErrs {
			damaged = append(damaged, damagedFact{path: filepath.Join(root.Path, filepath.FromSlash(we.Rel)), err: we.Err})
		}
	}
	return checkout, out, damaged, nil
}

// sameBytes is the Unchanged or Duplicate report for an item whose bytes a
// known fact already holds.
func sameBytes(it Item, sum string, known []existing) (Report, bool) {
	for _, k := range known {
		if k.sum != sum && k.raw != sum {
			continue
		}
		if s := k.fact.Metadata.Source; s != nil && s.Engine == it.Engine && s.Path == it.Source {
			return Report{Outcome: Unchanged, Source: it.Source, Path: k.path}, true
		}
		return Report{Outcome: Duplicate, Source: it.Source, Path: k.path, Detail: "same source bytes"}, true
	}
	return Report{}, false
}

// personalBound is the repo and fact name an item would land under in the
// personal store. Only the personal store is in scope: a fact §4.2 sends to
// the work store or the quarantine is reported and written nowhere, and why
// says what stopped an item that cannot land there.
func personalBound(it Item, o Options) (repo, name, why string) {
	switch dest := route.WriteDest(it.Session, o.Config); {
	case dest.Quarantine:
		return "", "", "routes to the quarantine (" + dest.Why + "); only the personal store is imported"
	case dest.Store != route.StorePersonal:
		return "", "", "routes to the " + string(dest.Store) + " store; only the personal store is imported"
	}
	repo = it.Session.Repo
	if repo == "" {
		return "", "", "no repo to scope the fact to"
	}
	if !fact.NameRE.MatchString(repo) {
		return "", "", fmt.Sprintf("repo %q is not a valid name", repo)
	}
	name = it.Native.Name
	if !fact.NameRE.MatchString(name) {
		name = slug(name)
	}
	if name == "" {
		return "", "", fmt.Sprintf("name %q cannot be made a valid name", it.Native.Name)
	}
	return repo, name, ""
}

// prepare routes, names, normalises and dedups one item. ok is false when
// the item stops here, with rep saying why.
func prepare(it Item, o Options, known []existing) (c candidate, rep Report, ok bool) {
	sum := digest(it.Raw)
	if rep, ok := sameBytes(it, sum, known); ok {
		return c, rep, false
	}
	repo, name, why := personalBound(it, o)
	if why != "" {
		return c, Report{Outcome: Skipped, Source: it.Source, Detail: why}, false
	}

	f := normalise(it, name, repo, o.Host, sum)
	for _, k := range known {
		if s := k.fact.Metadata.Source; s != nil && s.Engine == it.Engine && s.Path == it.Source {
			return c, Report{Outcome: Proposed, Source: it.Source, Path: k.path, Detail: "changed source"}, false
		}
	}
	for _, k := range known {
		if why := nearDuplicate(f, k.fact); why != "" {
			return c, Report{Outcome: Proposed, Source: it.Source, Path: k.path, Detail: why}, false
		}
	}
	return candidate{item: it, fact: f}, Report{}, true
}

// normalise builds the canonical copy: the native name (validated),
// description, type, body and modified, and every trust-bearing field set
// here, whatever the native file says.
func normalise(it Item, name, repo, host, sum string) fact.Fact {
	n := it.Native
	return fact.Fact{
		Name:        name,
		Description: n.Description,
		Body:        n.Body,
		Metadata: fact.Metadata{
			Type:            n.Metadata.Type,
			Scope:           "repo",
			Repos:           []string{repo},
			Engines:         []string{it.Engine},
			Confidence:      "proposed",
			Provenance:      &fact.Provenance{Engine: it.Engine, Session: n.Metadata.OriginSessionID, Host: host},
			Source:          &fact.Source{Engine: it.Engine, Path: it.Source, SHA256: sum},
			OriginSessionID: n.Metadata.OriginSessionID,
			Modified:        n.Metadata.Modified,
		},
	}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// slug folds s into the fact-name alphabet; "" when nothing survives.
func slug(s string) string {
	s = strings.Trim(nonName.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 81 {
		s = strings.TrimRight(s[:81], "-")
	}
	return s
}

var wikilinkRE = regexp.MustCompile(`\[\[([^\[\]\n|#]*)([^\[\]\n]*)\]\]`)

// relink points a wiki-link at the slugified name of the fact it names, so a
// fact renamed by import does not leave its referrers dangling.
func relink(body string, names map[string]bool) string {
	return wikilinkRE.ReplaceAllStringFunc(body, func(m string) string {
		sub := wikilinkRE.FindStringSubmatch(m)
		target := strings.TrimSpace(sub[1])
		if fact.NameRE.MatchString(target) || !names[slug(target)] {
			return m
		}
		return "[[" + slug(target) + sub[2] + "]]"
	})
}

// minOverlapTokens keeps a short body from reading as a near-duplicate of
// every other short body.
const (
	minOverlapTokens = 8
	overlapThreshold = 0.8
)

// nearDuplicate is why f looks like a copy of k (§4.9): the same name, the
// same description once case, whitespace and punctuation are folded, or high
// overlap between body tokens. "" means it does not.
func nearDuplicate(f, k fact.Fact) string {
	if f.Name == k.Name {
		return "same name"
	}
	if d := fold(f.Description); d != "" && d == fold(k.Description) {
		return "same description"
	}
	a, b := tokens(f.Body), tokens(k.Body)
	if len(a) < minOverlapTokens || len(b) < minOverlapTokens {
		return ""
	}
	shared := 0
	for t := range a {
		if b[t] {
			shared++
		}
	}
	if j := float64(shared) / float64(len(a)+len(b)-shared); j >= overlapThreshold {
		return fmt.Sprintf("body overlap %.0f%%", 100*j)
	}
	return ""
}

var (
	punct   = regexp.MustCompile(`[\p{P}\p{S}]+`)
	nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// fold drops case and punctuation and collapses whitespace.
func fold(s string) string {
	return strings.Join(strings.Fields(punct.ReplaceAllString(strings.ToLower(s), "")), " ")
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for t := range strings.FieldsSeq(nonWord.ReplaceAllString(strings.ToLower(s), " ")) {
		out[t] = true
	}
	return out
}
