// Package tier1 assembles the memory index injected at session start: the
// stores a session may read, filtered to the facts that apply to it, cut to a
// fixed budget and fenced as reference data.
package tier1

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// Budget is the runes of inner text across all stores; fence and header are
// outside it.
const Budget = 4000

const flaggedPrefix = "(flagged, unreviewed) "

// Options tunes the index for the engine reading it. The zero value is the
// plain flat index every engine without a native memory index gets.
type Options struct {
	// Claude groups the index by type and leaves out the facts that Claude's
	// own memory dir, NativeMemDir, already loads into the session.
	Claude       bool
	NativeMemDir string
}

// Assemble is the tier-1 injection for session s, and what it skipped (never
// the skipped text itself), then v's attestation reports. It returns "" when
// there is nothing to inject or ctx is done.
func Assemble(ctx context.Context, s route.Session, cfg config.Config, rules gate.Rules, v *attest.Verifier, opts Options) (text string, reports []string) {
	return assemble(ctx, s, cfg, rules, v, opts, Budget)
}

func assemble(ctx context.Context, s route.Session, cfg config.Config, rules gate.Rules, v *attest.Verifier, opts Options, budget int) (text string, reports []string) {
	a := assembler{ctx: ctx, session: s, rules: rules, verifier: v}
	grouped := opts.Claude && opts.NativeMemDir != ""
	if grouped {
		a.native = nativeIndex(ctx, opts.NativeMemDir)
	}
	avail := budget
	var blocks []string
	for _, id := range route.ReadStores(s, cfg) {
		entries := a.storeEntries(id, cfg)
		if ctx.Err() != nil {
			return "", a.allReports()
		}
		var lines []string
		if grouped {
			lines = groupedLines(entries, avail)
		} else {
			lines = fit(flatLines(entries), avail)
		}
		if len(lines) == 0 {
			continue
		}
		avail -= cost(lines)
		body := strings.Join(lines, "\n") + "\n"
		blocks = append(blocks, sanitize.Fence(sanitize.Header(string(id)+" store"), body, sanitize.NewDelimiter()))
	}
	return strings.Join(blocks, "\n"), a.allReports()
}

// entry is one indexed fact: its index line and what the grouped layout sorts
// it by.
type entry struct {
	line    string
	typ     string
	flagged bool
	// native is set for a fact Claude's own memory dir already holds.
	native bool
}

type assembler struct {
	ctx     context.Context
	session route.Session
	rules   gate.Rules
	reports []string
	// verifier judges each indexed fact that claims review; the index shows no
	// verdict, so only its reports reach the output.
	verifier *attest.Verifier
	native   native
}

func (a *assembler) allReports() []string {
	return append(a.reports, a.verifier.Reports()...)
}

func (a *assembler) reportf(format string, args ...any) {
	a.reports = append(a.reports, fmt.Sprintf(format, args...))
}

// storeEntries is one store's index entries: the checkout, then the local
// layer minus any name the checkout already has.
func (a *assembler) storeEntries(id route.StoreID, cfg config.Config) []entry {
	seen := map[string]bool{}
	var entries []entry
	if checkout, err := store.CheckoutRoot(a.ctx, cfg, id); err != nil {
		a.reportf("%v", err)
	} else {
		entries = a.rootEntries(checkout, a.appliesInCheckout, false, seen)
	}
	return append(entries, a.rootEntries(store.LocalRoot(cfg, id), a.appliesInLocal, true, seen)...)
}

func (a *assembler) appliesInCheckout(_ string, f fact.Fact) bool {
	if f.Metadata.Scope == "global" {
		return true
	}
	return a.session.Repo != "" && slices.Contains(f.Metadata.Repos, a.session.Repo)
}

// appliesInLocal goes by the directory the fact was learned in; the local
// layer ignores scope.
func (a *assembler) appliesInLocal(rel string, _ fact.Fact) bool {
	want := a.session.Repo
	if want == "" {
		want = "_norepo"
	}
	first, _, _ := strings.Cut(rel, "/")
	return first == want
}

func (a *assembler) rootEntries(root store.Root, applies func(rel string, f fact.Fact) bool, flagged bool, seen map[string]bool) []entry {
	if root.Path == "" {
		return nil
	}
	index, err := root.ReadIndex()
	if err != nil {
		// Most hosts have no local layer yet.
		if !flagged || !errors.Is(err, fs.ErrNotExist) {
			a.reportf("skipped %s index: %v", root.Store, err)
		}
		return nil
	}

	var out []entry
	for _, line := range index {
		if a.ctx.Err() != nil {
			return nil
		}
		name, rel, ok := store.ParseIndexLine(line)
		if !ok || seen[name] {
			continue
		}
		f, raw, ok := a.load(root, rel)
		if !ok || f.Metadata.SupersededBy != "" || f.Name != name || !applies(rel, f) {
			continue
		}
		if ids := a.rules.Match(string(raw)); len(ids) > 0 {
			a.reportf("excluded %s/%s: rule %s", root.Store, rel, strings.Join(ids, ","))
			continue
		}
		a.verifier.Reviewed(store.Entry{Root: root, Rel: rel, Fact: f, Raw: raw})
		rendered := root.IndexLines([]store.Entry{{Root: root, Rel: rel, Fact: f}})
		if len(rendered) == 0 {
			continue
		}
		seen[name] = true
		out = append(out, entry{line: rendered[0], typ: f.Metadata.Type, flagged: flagged, native: a.native.has(f)})
	}
	return out
}

// load reads the fact an index line points at, and its raw bytes. Anything
// that is not a regular file inside the root is skipped unopened: opening a
// FIFO would hang session start.
func (a *assembler) load(root store.Root, rel string) (fact.Fact, []byte, bool) {
	abs, err := root.Confine(rel)
	if err != nil {
		a.reportf("skipped %s/%s: %v", root.Store, rel, err)
		return fact.Fact{}, nil, false
	}
	info, err := os.Stat(abs)
	if err != nil {
		a.reportf("skipped %s/%s: %v", root.Store, rel, err)
		return fact.Fact{}, nil, false
	}
	if !info.Mode().IsRegular() {
		a.reportf("skipped %s/%s: not a regular file", root.Store, rel)
		return fact.Fact{}, nil, false
	}
	b, err := os.ReadFile(abs) //nolint:gosec // abs was confined to the store root above
	if err != nil {
		a.reportf("skipped %s/%s: %v", root.Store, rel, err)
		return fact.Fact{}, nil, false
	}
	f, err := fact.Parse(b)
	if err != nil {
		a.reportf("skipped %s/%s: %v", root.Store, rel, err)
		return fact.Fact{}, nil, false
	}
	return f, b, true
}

func flag(line string, flagged bool) string {
	if !flagged {
		return line
	}
	return "- " + flaggedPrefix + strings.TrimPrefix(line, "- ")
}

func flatLines(entries []entry) []string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = flag(e.line, e.flagged)
	}
	return lines
}

func cost(lines []string) int {
	n := 0
	for _, l := range lines {
		n += utf8.RuneCountInString(l) + 1
	}
	return n
}

func marker(dropped int) string {
	return fmt.Sprintf("… %d entries omitted …", dropped)
}

// fit returns lines if they fit in avail runes; otherwise the head and tail
// that do, around a marker counting the middle. The newest entries lead the
// index and the oldest trail it, so both ends are worth keeping.
func fit(lines []string, avail int) []string {
	lo, hi := fitRange(lines, avail)
	switch {
	case lo == hi:
		return lines
	case hi-lo == len(lines):
		return nil
	}
	out := slices.Clone(lines[:lo])
	out = append(out, marker(hi-lo))
	return append(out, lines[hi:]...)
}

// fitRange is the part of lines that fits in avail runes: lines[:lo] and
// lines[hi:] are kept, with room left for a marker over the rest. lo == hi
// means everything fits.
func fitRange(lines []string, avail int) (lo, hi int) {
	if cost(lines) <= avail {
		return len(lines), len(lines)
	}
	hi = len(lines)
	used := 0
	for fromHead := true; lo < hi; fromHead = !fromHead {
		next := lines[lo]
		if !fromHead {
			next = lines[hi-1]
		}
		c := utf8.RuneCountInString(next) + 1
		m := marker(hi - lo - 1)
		if used+c+utf8.RuneCountInString(m)+1 > avail {
			break
		}
		used += c
		if fromHead {
			lo++
		} else {
			hi--
		}
	}
	return lo, hi
}
