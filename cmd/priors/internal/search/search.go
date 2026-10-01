// Package search finds facts by term across the store roots a session may
// read. A Backend narrows the candidates; everything that decides whether a
// fact is a hit (scope, read rule, redaction) happens here, over the parsed
// fact.
package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	defaultLimit  = 20
	archivePrefix = "_archive/"
	noRepoDir     = "_norepo"
)

// ErrPartial is a backend's report that some files could not be searched; the
// candidates it returns alongside are still good.
var ErrPartial = errors.New("some files could not be searched")

// Backend lists the files under roots that contain term. Paths are absolute.
type Backend interface {
	Candidates(ctx context.Context, roots []string, term string) ([]string, error)
}

// Rg is the ripgrep backend. An empty Bin means "rg" on PATH.
type Rg struct{ Bin string }

// Candidates runs ripgrep over the roots that exist. No match is not an error;
// a file rg could not read is ErrPartial, alongside whatever did match.
func (r Rg) Candidates(ctx context.Context, roots []string, term string) ([]string, error) {
	bin := r.Bin
	if bin == "" {
		bin = "rg"
	}
	// Looked up before the roots are checked, so a missing rg fails even a
	// root that does not exist and the search reads as unavailable.
	bin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("rg: %w", err)
	}
	var existing []string
	for _, root := range roots {
		if _, err := os.Stat(root); err == nil {
			existing = append(existing, root)
		}
	}
	if len(existing) == 0 {
		return nil, nil
	}
	args := append([]string{
		"--no-config", "--files-with-matches", "--ignore-case", "--fixed-strings", "--no-messages",
		"--glob", "*.md", "-e", term, "--",
	}, existing...)
	out, err := exec.CommandContext(ctx, bin, args...).Output() //nolint:gosec // bin is the operator's rg; term is passed after -e, never parsed as a flag
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	var exit *exec.ExitError
	var partial error
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return nil, nil
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		partial = ErrPartial
	default:
		return nil, fmt.Errorf("rg: %w", err)
	}
	var paths []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		p, err := filepath.Abs(line)
		if err != nil {
			return nil, fmt.Errorf("rg: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, partial
}

// Query is what to search for and how to narrow it. Every term must match. Repo
// overrides the session's repo for checkouts; AnyRepo drops the repo filter on
// checkouts. All includes archived and superseded facts. A Limit of zero means
// the default.
type Query struct {
	Terms        []string
	Repo         string
	AnyRepo, All bool
	Type, Scope  string
	Limit        int
}

// Hit is one matching fact.
type Hit struct{ Entry store.Entry }

// Run searches roots, which the caller has already limited to what the session
// may read and ordered checkout before local, so that a checkout copy wins
// over a local one of the same name. The reports name files and roots that
// were skipped without ever quoting their content. A root the backend could
// not search is reported and passed over; only a search in which no root could
// be searched at all is an error.
func Run(ctx context.Context, b Backend, roots []store.Root, s route.Session, q Query, rules gate.Rules) ([]Hit, []string, error) {
	if len(q.Terms) == 0 {
		return nil, nil, errors.New("search: no terms")
	}
	terms := make([]string, len(q.Terms))
	for i, t := range q.Terms {
		terms[i] = strings.ToLower(t)
	}

	var hits []Hit
	var reports []string
	var firstErr error
	failed := 0
	for _, root := range roots {
		paths, err := b.Candidates(ctx, []string{root.Path}, q.Terms[0])
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		if errors.Is(err, ErrPartial) {
			// A partial result that matched nothing and names no unreadable
			// file is a failed search, not a clean miss.
			if skipped := unsearched(root, paths); len(paths) > 0 || len(skipped) > 0 {
				reports = append(reports, skipped...)
				err = nil
			}
		}
		if err != nil {
			reports = append(reports, fmt.Sprintf("unsearched %s %s: %v", root.Store, root.Kind, err))
			if firstErr == nil {
				firstErr = fmt.Errorf("search %s: %w", root.Path, err)
			}
			failed++
			continue
		}
		slices.Sort(paths)
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			e, ok, report := load(root, path)
			if report != "" {
				reports = append(reports, report)
			}
			if !ok || !visible(e, s, q, terms) {
				continue
			}
			if ids := rules.Match(string(e.Raw)); len(ids) > 0 {
				reports = append(reports, fmt.Sprintf("excluded %s/%s: rule %s", root.Store, e.Rel, strings.Join(ids, ",")))
				continue
			}
			hits = append(hits, Hit{Entry: e})
		}
	}

	if failed > 0 && failed == len(roots) {
		return nil, nil, firstErr
	}

	hits = dedupe(hits)
	slices.SortStableFunc(hits, func(a, b Hit) int {
		an, bn := nameMatches(a.Entry.Fact.Name, terms), nameMatches(b.Entry.Fact.Name, terms)
		if an != bn {
			if an {
				return -1
			}
			return 1
		}
		if c := b.Entry.Fact.ModifiedTime().Compare(a.Entry.Fact.ModifiedTime()); c != 0 {
			return c
		}
		return strings.Compare(a.Entry.Fact.Name, b.Entry.Fact.Name)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	return hits[:min(limit, len(hits))], reports, nil
}

// unsearched reports the files of root that its walk could not read, except
// the candidates, which the backend did read and load reports on.
func unsearched(root store.Root, candidates []string) []string {
	_, errs := root.Walk()
	var reports []string
	for _, we := range errs {
		if slices.Contains(candidates, filepath.Join(root.Path, filepath.FromSlash(we.Rel))) {
			continue
		}
		reports = append(reports, fmt.Sprintf("skipped %s/%s: %v", root.Store, we.Rel, we.Err))
	}
	return reports
}

// load reads the fact at path if it is a fact file of root. ok is false for
// anything that is not one; report is set when that is worth telling the user.
func load(root store.Root, path string) (e store.Entry, ok bool, report string) {
	relPath, err := filepath.Rel(root.Path, path)
	if err != nil {
		return store.Entry{}, false, ""
	}
	rel := filepath.ToSlash(relPath)
	segments := strings.Split(rel, "/")
	if len(segments) < 2 || slices.ContainsFunc(segments, func(s string) bool { return strings.HasPrefix(s, ".") }) {
		return store.Entry{}, false, ""
	}
	confined, err := root.Confine(rel)
	if err != nil {
		return store.Entry{}, false, ""
	}
	info, err := os.Stat(confined)
	if err != nil || !info.Mode().IsRegular() {
		return store.Entry{}, false, ""
	}
	data, err := os.ReadFile(confined) //nolint:gosec // confined by Root.Confine to the store root
	if err != nil {
		return store.Entry{}, false, ""
	}
	f, err := fact.Parse(data)
	if err != nil {
		return store.Entry{}, false, fmt.Sprintf("unparsable %s/%s", root.Store, rel)
	}
	return store.Entry{
		Root:     root,
		Rel:      rel,
		Fact:     f,
		Raw:      data,
		Size:     int64(len(data)),
		Archived: strings.HasPrefix(rel, archivePrefix),
	}, true, ""
}

// visible applies every filter except the redaction rules. terms are lower-case.
func visible(e store.Entry, s route.Session, q Query, terms []string) bool {
	f := e.Fact
	text := strings.ToLower(f.Text())
	for _, t := range terms {
		if !strings.Contains(text, t) {
			return false
		}
	}
	if (e.Archived || f.Metadata.SupersededBy != "") && !q.All {
		return false
	}
	if q.Type != "" && f.Metadata.Type != q.Type {
		return false
	}
	if q.Scope != "" && f.Metadata.Scope != q.Scope {
		return false
	}
	if e.Root.Kind == store.KindLocal {
		learned, _, _ := strings.Cut(e.Rel, "/")
		want := s.Repo
		if want == "" {
			want = noRepoDir
		}
		return learned == want
	}
	if q.AnyRepo {
		return true
	}
	repo := q.Repo
	if repo == "" {
		repo = s.Repo
	}
	return f.Metadata.Scope == "global" || (repo != "" && slices.Contains(f.Metadata.Repos, repo))
}

// dedupe keeps the first hit of each name.
func dedupe(hits []Hit) []Hit {
	seen := make(map[string]bool, len(hits))
	return slices.DeleteFunc(hits, func(h Hit) bool {
		name := h.Entry.Fact.Name
		if seen[name] {
			return true
		}
		seen[name] = true
		return false
	})
}

func nameMatches(name string, terms []string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc(terms, func(t string) bool { return strings.Contains(name, t) })
}
