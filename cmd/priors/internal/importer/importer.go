// Package importer copies engines' native memories into the personal store
// (memory-layer §4.9). Every item that survives dedup goes through
// write.Add, so redaction, the secret gate, routing and the gates are the
// write path's own; the importer only normalises, dedups and reports.
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
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
// write.Result.Warning words it.
type Result struct {
	Reports []Report
	Warning string
}

// Run imports items into the personal store. A dry run is the review step:
// it runs every check and writes nothing, so the owner sees what would land
// and what is only proposed before a real run commits. A real run commits
// what it published once, at the end, through the store's commit gate. The
// error is a store the run cannot read, before anything is written.
func Run(ctx context.Context, items []Item, o Options) (Result, error) {
	checkout, known, err := loadStore(ctx, o.Config)
	if err != nil {
		return Result{}, err
	}
	var out Result
	var accepted []candidate
	for _, it := range items {
		c, rep, ok := prepare(it, o, known)
		if !ok {
			out.Reports = append(out.Reports, rep)
			continue
		}
		known = append(known, existing{path: it.Source + " (this run)", fact: c.fact, sum: c.fact.Metadata.Source.SHA256})
		accepted = append(accepted, c)
	}

	names := map[string]bool{}
	for _, k := range known {
		names[k.fact.Name] = true
	}
	// A wiki-link may name a fact this run writes later, or, in a dry run,
	// one it would have written earlier; a refused fact stops counting.
	pending := map[string]bool{}
	for _, c := range accepted {
		pending[c.fact.Name] = true
	}
	published := 0
	for _, c := range accepted {
		f := c.fact
		f.Body = relink(f.Body, names)
		res, err := write.Add(ctx, o.Config, write.Request{
			Fact:             f,
			Session:          c.item.Session,
			SessionID:        o.SessionID,
			Engine:           o.Engine,
			Host:             o.Host,
			External:         o.External,
			IdentityConflict: o.IdentityConflict,
			Imported:         true,
			Pending:          slices.Sorted(maps.Keys(pending)),
			DryRun:           o.DryRun,
			NoCommit:         true,
		}, o.Deps)
		if err != nil {
			delete(pending, f.Name)
			out.Reports = append(out.Reports, Report{Outcome: Skipped, Source: c.item.Source, Detail: "refused: " + err.Error()})
			continue
		}
		if res.Outcome == "published" {
			published++
		}
		out.Reports = append(out.Reports, written(res, c.item.Source, o.DryRun))
	}
	if published > 0 && !o.DryRun {
		out.Warning = commitImport(ctx, o, checkout, published)
	}
	return out, nil
}

// commitImport commits the checkout under the store's lock, as write.Add
// commits one fact.
func commitImport(ctx context.Context, o Options, checkout store.Root, n int) (warning string) {
	unlock, err := store.Lock(o.Config, string(route.StorePersonal))
	if err != nil {
		return "nothing committed: " + err.Error()
	}
	defer unlock()
	return commit.Checkout(ctx, o.Config, checkout, o.Deps.Rules, o.Deps.Scanner, fmt.Sprintf("priors: import %d facts", n))
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
// that was not imported; raw is the sha256 of its file's bytes.
type existing struct {
	path string
	fact fact.Fact
	sum  string
	raw  string
}

// loadStore reads the personal store's checkout and local layer, the
// destinations an import can land in.
func loadStore(ctx context.Context, cfg config.Config) (store.Root, []existing, error) {
	checkout, err := store.CheckoutRoot(ctx, cfg, route.StorePersonal)
	if err != nil {
		return store.Root{}, nil, err
	}
	var out []existing
	for _, root := range []store.Root{checkout, store.LocalRoot(cfg, route.StorePersonal)} {
		entries, _ := root.Walk()
		for _, e := range entries {
			k := existing{path: filepath.Join(root.Path, filepath.FromSlash(e.Rel)), fact: e.Fact, raw: digest(e.Raw)}
			if s := e.Fact.Metadata.Source; s != nil {
				k.sum = s.SHA256
			}
			out = append(out, k)
		}
	}
	return checkout, out, nil
}

// prepare routes, names, normalises and dedups one item. ok is false when
// the item stops here, with rep saying why.
func prepare(it Item, o Options, known []existing) (c candidate, rep Report, ok bool) {
	rep = Report{Outcome: Skipped, Source: it.Source}
	sum := digest(it.Raw)
	for _, k := range known {
		if k.sum != sum && k.raw != sum {
			continue
		}
		if s := k.fact.Metadata.Source; s != nil && s.Engine == it.Engine && s.Path == it.Source {
			return c, Report{Outcome: Unchanged, Source: it.Source, Path: k.path}, false
		}
		return c, Report{Outcome: Duplicate, Source: it.Source, Path: k.path, Detail: "same source bytes"}, false
	}

	// Only the personal store is in scope: a fact §4.2 sends to the work store
	// or the quarantine is reported and written nowhere.
	switch dest := route.WriteDest(it.Session, o.Config); {
	case dest.Quarantine:
		rep.Detail = "routes to the quarantine (" + dest.Why + "); only the personal store is imported"
		return c, rep, false
	case dest.Store != route.StorePersonal:
		rep.Detail = "routes to the " + string(dest.Store) + " store; only the personal store is imported"
		return c, rep, false
	}
	repo := it.Session.Repo
	if repo == "" {
		rep.Detail = "no repo to scope the fact to"
		return c, rep, false
	}
	if !fact.NameRE.MatchString(repo) {
		rep.Detail = fmt.Sprintf("repo %q is not a valid name", repo)
		return c, rep, false
	}
	name := it.Native.Name
	if !fact.NameRE.MatchString(name) {
		name = slug(name)
	}
	if name == "" {
		rep.Detail = fmt.Sprintf("name %q cannot be made a valid name", it.Native.Name)
		return c, rep, false
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
	return candidate{item: it, fact: f}, rep, true
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
