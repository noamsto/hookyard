// Package lint checks a store against the memory layer's invariants and moves
// facts that trip the content and size gates out of a checkout. A finding
// names a file and a rule, never the text that tripped it, so a report cannot
// leak a secret.
package lint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// Finding is one rule a file breaks. File is relative to the store root with
// '/' separators, or MEMORY.md for the index, or "." for the store itself.
type Finding struct{ File, Rule, Msg string }

func (f Finding) String() string { return f.File + ": " + f.Rule + ": " + f.Msg }

// Options configures Store. Store is the store being checked; WorkOrgs and
// WorkNames feed the personal store's work-name tripwire. Gates adds the
// gate-2/3/4 checks that only apply to a checkout.
type Options struct {
	Store     route.StoreID
	WorkOrgs  []string
	WorkNames []string
	Rules     gate.Rules
	Scanner   *gate.Scanner
	Gates     bool
}

var wikilinkRE = regexp.MustCompile(`\[\[([^\[\]\n]*)\]\]`)

// FactRules are the checks that need only one fact.
func FactRules(e store.Entry, rules gate.Rules) []Finding {
	f := e.Fact
	m := f.Metadata
	var out []Finding
	add := func(rule, msg string) { out = append(out, Finding{File: e.Rel, Rule: rule, Msg: msg}) }

	if !slices.Contains(fact.Types, m.Type) {
		add("type", "metadata.type must be one of "+strings.Join(fact.Types, ", "))
	}
	if f.Name == "" {
		add("name-missing", "name is required")
	} else {
		if !fact.NameRE.MatchString(f.Name) {
			add("name-format", "name must match "+fact.NameRE.String())
		}
		if filepath.Base(e.Rel) != f.Name+".md" {
			add("filename", "file name must be <name>.md")
		}
	}
	if f.Description == "" {
		add("description-missing", "description is required")
	}
	if p := m.Provenance; p == nil || p.Engine == "" || p.Host == "" {
		add("provenance", "metadata.provenance needs an engine and a host")
	}
	for i, repo := range m.Repos {
		if !fact.NameRE.MatchString(repo) {
			add("repos-format", fmt.Sprintf("metadata.repos[%d] must match %s", i, fact.NameRE))
		}
	}
	if m.Scope != "repo" && m.Scope != "global" || m.Scope == "repo" && len(m.Repos) == 0 {
		add("scope", "metadata.scope must be global, or repo with at least one repos entry")
	}
	text, err := fileText(e)
	switch {
	case err != nil:
		add("secret", "fact cannot be serialised for the secret check")
	default:
		if ids := rules.Match(text); len(ids) > 0 {
			add("secret", "matches secret rule(s): "+strings.Join(ids, ", "))
		}
	}
	return out
}

// fileText is what the text rules read: the whole file, frontmatter
// included, since an unknown key reaches every reader of the file too. An
// entry not yet written is read as it would be written.
func fileText(e store.Entry) (string, error) {
	if len(e.Raw) > 0 {
		return string(e.Raw), nil
	}
	b, err := e.Fact.Marshal()
	return string(b), err
}

// Store checks every fact under root and the store as a whole.
func Store(ctx context.Context, root store.Root, opts Options) []Finding {
	entries, walkErrs := root.Walk()
	var out []Finding
	links := symlinks(root.Path)
	for _, rel := range links {
		out = append(out, Finding{File: rel, Rule: "outside-tree", Msg: "symlink in the store tree"})
	}
	for _, we := range walkErrs {
		if slices.Contains(links, we.Rel) {
			continue
		}
		if _, err := root.Confine(we.Rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			out = append(out, Finding{File: we.Rel, Rule: "outside-tree", Msg: "file resolves outside the store tree"})
			continue
		}
		out = append(out, Finding{File: we.Rel, Rule: "parse", Msg: "not a readable fact file"})
	}

	names := map[string]string{}
	for _, e := range entries {
		addName(names, e)
	}
	c := checker{root: root, opts: opts, names: names, workTerms: workTerms(opts)}
	for _, e := range entries {
		out = append(out, FactRules(e, opts.Rules)...)
		out = append(out, c.entry(e)...)
	}
	out = append(out, c.index(entries)...)
	if opts.Gates {
		out = append(out, c.gates(ctx, entries)...)
	}
	sortFindings(out)
	return out
}

// Candidate checks one not-yet-written fact against root's existing facts:
// the store-level rules that concern it. A repo-scoped fact bound for a
// checkout creates its first repos entry's directory, so that entry is not
// reported unknown.
func Candidate(_ context.Context, root store.Root, e store.Entry, opts Options) []Finding {
	entries, _ := root.Walk()
	names := map[string]string{}
	for _, existing := range entries {
		addName(names, existing)
	}
	var out []Finding
	if first, ok := names[e.Fact.Name]; ok {
		out = append(out, Finding{File: e.Rel, Rule: "duplicate-name", Msg: "name is also used by " + first})
	}
	addName(names, e)

	c := checker{root: root, opts: opts, names: names, workTerms: workTerms(opts)}
	if m := e.Fact.Metadata; e.Root.Kind == store.KindCheckout && m.Scope == "repo" && len(m.Repos) > 0 {
		c.creates = m.Repos[0]
	}
	out = append(out, c.refs(e)...)
	sortFindings(out)
	return out
}

// symlinks lists every symlink under dir but inside .git. Walk follows none
// of them, so a symlinked directory would otherwise be invisible to lint while
// a write through it lands outside the store.
func symlinks(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // Walk reports unreadable paths; this pass only looks for links
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 && path != dir {
			if rel, err := filepath.Rel(dir, path); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return out
}

func addName(names map[string]string, e store.Entry) {
	if e.Fact.Name == "" {
		return
	}
	if _, seen := names[e.Fact.Name]; !seen {
		names[e.Fact.Name] = e.Rel
	}
}

func sortFindings(out []Finding) {
	slices.SortStableFunc(out, func(a, b Finding) int {
		return cmpStrings(a.File, b.File, a.Rule, b.Rule, a.Msg, b.Msg)
	})
}

func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

type workTerm struct {
	label string
	re    *regexp.Regexp
}

// workTerms matches a work org's owner part ("github.com/factify-inc" →
// "factify-inc") or a work name as a whole word, case-insensitively.
func workTerms(opts Options) []workTerm {
	if opts.Store != route.StorePersonal {
		return nil
	}
	var terms []workTerm
	add := func(label, word string) {
		if word == "" {
			return
		}
		terms = append(terms, workTerm{label, regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)})
	}
	for _, org := range opts.WorkOrgs {
		_, owner, found := strings.Cut(org, "/")
		if !found {
			owner = org
		}
		add(org, owner)
	}
	for _, name := range opts.WorkNames {
		add(name, name)
	}
	return terms
}

type checker struct {
	root      store.Root
	opts      Options
	names     map[string]string
	workTerms []workTerm
	// creates is a repos entry whose directory the caller is about to create.
	creates string
}

// quote names a value in a message unless it matches a secret rule: a value
// read from a fact is arbitrary text.
func (c checker) quote(s string) string {
	if len(c.opts.Rules.Match(s)) > 0 {
		return "(redacted)"
	}
	return fmt.Sprintf("%q", s)
}

func (c checker) entry(e store.Entry) []Finding {
	var out []Finding
	if first, ok := c.names[e.Fact.Name]; ok && e.Fact.Name != "" && first != e.Rel {
		out = append(out, Finding{File: e.Rel, Rule: "duplicate-name", Msg: "name is also used by " + first})
	}
	return append(out, c.refs(e)...)
}

// refs checks what e's text and metadata point at: other facts, repo
// directories and work names.
func (c checker) refs(e store.Entry) []Finding {
	f := e.Fact
	var out []Finding
	add := func(rule, msg string) { out = append(out, Finding{File: e.Rel, Rule: rule, Msg: msg}) }

	if by := f.Metadata.SupersededBy; by != "" {
		if _, ok := c.names[by]; !ok {
			add("superseded-dangling", "superseded_by names no fact: "+c.quote(by))
		}
	}
	for _, repo := range f.Metadata.Repos {
		// The local layer and the quarantine file by learned repo, so their
		// directories say nothing about a repos entry.
		if c.root.Kind != store.KindCheckout || !fact.NameRE.MatchString(repo) || repo == c.creates {
			continue
		}
		if info, err := os.Stat(filepath.Join(c.root.Path, repo)); err != nil || !info.IsDir() {
			add("repos-unknown", "repos entry has no directory in the store: "+c.quote(repo))
		}
	}
	for _, m := range wikilinkRE.FindAllStringSubmatch(f.Body, -1) {
		target, _, _ := strings.Cut(m[1], "|")
		target, _, _ = strings.Cut(target, "#")
		target = strings.TrimSpace(target)
		switch {
		case strings.ContainsAny(target, `/\`) || strings.Contains(target, ".."):
			add("outside-tree", "wiki-link target leaves the store: "+c.quote(target))
		default:
			if _, ok := c.names[target]; !ok {
				add("wikilink-dangling", "wiki-link names no fact: "+c.quote(target))
			}
		}
	}
	if len(c.workTerms) > 0 {
		text, err := fileText(e)
		if err != nil {
			add("work-name", "fact cannot be serialised for the work-name check")
		}
		for _, t := range c.workTerms {
			if err == nil && t.re.MatchString(text) {
				add("work-name", "names configured work entry "+fmt.Sprintf("%q", t.label))
			}
		}
	}
	return out
}

func (c checker) index(entries []store.Entry) []Finding {
	kept, _ := store.Unredacted(entries, c.opts.Rules)
	want := c.root.IndexLines(kept)
	have, err := c.root.ReadIndex()
	switch {
	case err != nil && len(want) > 0:
		return []Finding{{File: store.IndexFile, Rule: "index-sync", Msg: "index is missing or unreadable"}}
	case err == nil && !slices.Equal(have, want):
		return []Finding{{File: store.IndexFile, Rule: "index-sync", Msg: "index lines differ from the facts on disk"}}
	}
	if err != nil {
		return nil
	}
	raw, rerr := os.ReadFile(filepath.Join(c.root.Path, store.IndexFile))
	if rerr != nil || !c.root.IndexIntact(string(raw)) {
		return []Finding{{File: store.IndexFile, Rule: "index-sync", Msg: "index is not as generated"}}
	}
	return nil
}

func (c checker) gates(ctx context.Context, entries []store.Entry) []Finding {
	var out []Finding
	byFile, err := c.scan(ctx)
	if err != nil {
		out = append(out, Finding{File: ".", Rule: "gate:secret-unavailable", Msg: err.Error()})
	}
	for file, ids := range byFile {
		rel := filepath.ToSlash(file)
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			continue
		}
		out = append(out, Finding{File: rel, Rule: "gate:secret", Msg: "scanner matched rule(s): " + strings.Join(ids, ", ")})
	}
	for _, e := range entries {
		if e.Archived {
			continue
		}
		for _, class := range flagReasons(e) {
			out = append(out, Finding{File: e.Rel, Rule: "gate:" + class, Msg: "fact needs review before it reaches the store"})
		}
	}
	return out
}

func (c checker) scan(ctx context.Context) (map[string][]string, error) {
	if c.opts.Scanner == nil {
		return nil, errors.New("no secret scanner configured; failing closed")
	}
	byFile, err := c.opts.Scanner.ScanDir(ctx, c.root.Path)
	if err != nil {
		return nil, fmt.Errorf("secret scanner failed, failing closed: %w", err)
	}
	return byFile, nil
}

// flagReasons are the gate-3 and gate-4 reasons e trips, in the form stored in
// metadata.flags.
func flagReasons(e store.Entry) []string {
	return append(gate.Content(string(e.Raw)), gate.Size(int(e.Size))...)
}

// MoveFlagged moves the checkout's facts that trip a content or size gate into
// the local layer, flagged and proposed, and regenerates both indexes. It
// returns the moved facts' paths in the checkout and the index regeneration's
// reports, each led by its root's path. A name already present in local
// refuses the whole move so that nothing is half-moved.
func MoveFlagged(ctx context.Context, root, local store.Root, opts Options) (moved, reports []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	entries, _ := root.Walk()
	type move struct {
		rel  string
		dest string
		data []byte
	}
	var moves []move
	seen := map[string]bool{}
	for _, e := range entries {
		reasons := flagReasons(e)
		if e.Archived || len(reasons) == 0 {
			continue
		}
		f := e.Fact
		if !fact.NameRE.MatchString(f.Name) {
			return nil, nil, fmt.Errorf("%s: cannot move a fact whose name is not a valid name", e.Rel)
		}
		if _, exists := local.FindByName(f.Name); exists || seen[f.Name] {
			return nil, nil, fmt.Errorf("%s: a fact named %q already exists in the local layer", e.Rel, f.Name)
		}
		seen[f.Name] = true

		f.Metadata.Flags = reasons
		f.Metadata.Confidence = "proposed"
		data, err := f.Marshal()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", e.Rel, err)
		}
		moves = append(moves, move{e.Rel, local.PathFor(f, learnedRepo(f)), data})
	}
	if len(moves) == 0 {
		return nil, nil, nil
	}

	moved = make([]string, 0, len(moves))
	for _, m := range moves {
		if err := atomicfile.Write(m.dest, m.data, 0o644); err != nil {
			return moved, nil, err
		}
		if err := os.Remove(filepath.Join(root.Path, filepath.FromSlash(m.rel))); err != nil {
			return moved, nil, err
		}
		moved = append(moved, m.rel)
	}
	for _, r := range []store.Root{root, local} {
		_, rootReports, err := r.WriteIndex(opts.Rules)
		for _, rep := range rootReports {
			reports = append(reports, r.Path+": "+rep)
		}
		if err != nil {
			return moved, reports, err
		}
	}
	return moved, reports, nil
}

// learnedRepo is the directory a moved fact files under in the local layer; a
// repos entry that is not a plain name could climb out of it.
func learnedRepo(f fact.Fact) string {
	if len(f.Metadata.Repos) > 0 && fact.NameRE.MatchString(f.Metadata.Repos[0]) {
		return f.Metadata.Repos[0]
	}
	return ""
}
