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

// Assemble is the tier-1 injection for session s, and what it skipped (never
// the skipped text itself). It returns "" when there is nothing to inject or
// ctx is done.
func Assemble(ctx context.Context, s route.Session, cfg config.Config, rules gate.Rules) (text string, reports []string) {
	a := assembler{ctx: ctx, session: s, rules: rules}
	avail := Budget
	var blocks []string
	for _, id := range route.ReadStores(s, cfg) {
		lines := a.storeLines(id, cfg)
		if ctx.Err() != nil {
			return "", a.reports
		}
		if len(lines) == 0 {
			continue
		}
		lines = fit(lines, avail)
		if len(lines) == 0 {
			continue
		}
		avail -= cost(lines)
		body := strings.Join(lines, "\n") + "\n"
		blocks = append(blocks, sanitize.Fence(sanitize.Header(string(id)+" store"), body, sanitize.NewDelimiter()))
	}
	return strings.Join(blocks, "\n"), a.reports
}

type assembler struct {
	ctx     context.Context
	session route.Session
	rules   gate.Rules
	reports []string
}

func (a *assembler) reportf(format string, args ...any) {
	a.reports = append(a.reports, fmt.Sprintf(format, args...))
}

// storeLines is one store's index lines: the checkout, then the local layer
// minus any name the checkout already has.
func (a *assembler) storeLines(id route.StoreID, cfg config.Config) []string {
	seen := map[string]bool{}
	lines := a.rootLines(store.CheckoutRoot(cfg, id), a.appliesInCheckout, false, seen)
	return append(lines, a.rootLines(store.LocalRoot(cfg, id), a.appliesInLocal, true, seen)...)
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

func (a *assembler) rootLines(root store.Root, applies func(rel string, f fact.Fact) bool, flagged bool, seen map[string]bool) []string {
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

	var out []string
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
		rendered := root.IndexLines([]store.Entry{{Root: root, Rel: rel, Fact: f}})
		if len(rendered) == 0 {
			continue
		}
		seen[name] = true
		out = append(out, flag(rendered[0], flagged))
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
	if cost(lines) <= avail {
		return lines
	}
	lo, hi := 0, len(lines) // lines[:lo] and lines[hi:] are kept
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
	if lo == 0 && hi == len(lines) {
		return nil
	}
	out := slices.Clone(lines[:lo])
	out = append(out, marker(hi-lo))
	return append(out, lines[hi:]...)
}
