// Package review proposes flagged local-layer facts to their store's checkout
// as review branches, and prunes the local copy once the merged fact is back.
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	hostSlugMax = 40
	// valueMax caps each interpolated value of the body, in runes.
	valueMax    = 300
	unknownHost = "unknown"
)

// propose is commit.Review; tests replace it.
var propose = commit.Review

var (
	nonSlug   = regexp.MustCompile(`[^a-z0-9]+`)
	githubScp = regexp.MustCompile(`^git@github\.com:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?$`)
	githubSSH = regexp.MustCompile(`^ssh://git@(?:ssh\.)?github\.com(?::443)?/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?$`)
	gateID    = regexp.MustCompile(`^[a-z0-9:-]+$`)
)

// Branch is the review branch for one fact: review/<host>/<name>-<h8>, where
// h8 is the first 8 hex digits of the sha256 of the review file's bytes, so
// the same bytes always name the same branch.
func Branch(host, name string, data []byte) string {
	sum := sha256.Sum256(data)
	return "review/" + hostSlug(host) + "/" + name + "-" + hex.EncodeToString(sum[:])[:8]
}

// hostSlug reduces host to [a-z0-9-], cut to hostSlugMax runes, with no
// leading or trailing '-' so it is a valid ref segment.
func hostSlug(host string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(host), "-"), "-")
	if r := []rune(s); len(r) > hostSlugMax {
		s = strings.Trim(string(r[:hostSlugMax]), "-")
	}
	if s == "" {
		return unknownHost
	}
	return s
}

// Body is the review commit's body and the PR's. "priors lint ran locally on
// this branch's tree and was clean." is true only because commit.Review lints
// the branch's tree before it pushes. Only gate reason ids and metadata
// appear, each through sanitize.Line.
func Body(f fact.Fact, storeID route.StoreID, rel string, reasons []string) string {
	line := func(s string) string { return sanitize.Line(s, valueMax) }
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return line(s)
	}
	var prov fact.Provenance
	if f.Metadata.Provenance != nil {
		prov = *f.Metadata.Provenance
	}
	var src fact.Source
	if f.Metadata.Source != nil {
		src = *f.Metadata.Source
	}
	flagged := make([]string, len(reasons))
	for i, r := range reasons {
		flagged[i] = line(r)
	}
	return strings.Join([]string{
		"priors flagged this fact for review. Merge to publish it as reviewed; close to keep it out of the store.",
		"priors lint ran locally on this branch's tree and was clean.",
		"",
		fmt.Sprintf("- fact: %s (%s store, %s)", line(f.Name), line(string(storeID)), line(rel)),
		"- description: " + line(f.Description),
		"- flagged by: " + strings.Join(flagged, ", "),
		fmt.Sprintf("- source: engine %s, path %s, session %s, host %s",
			orDash(src.Engine), orDash(src.Path), orDash(prov.Session), orDash(prov.Host)),
		"",
	}, "\n")
}

// Message is the review commit's whole message: subject, blank line, Body.
func Message(f fact.Fact, storeID route.StoreID, rel string, reasons []string) string {
	return "priors: review " + sanitize.Line(f.Name, valueMax) + "\n\n" + Body(f, storeID, rel, reasons)
}

// Reviewed is f as the review branch stores it: confidence reviewed, flags
// cleared.
func Reviewed(f fact.Fact) ([]byte, error) {
	f.Metadata.Confidence = "reviewed"
	f.Metadata.Flags = nil
	return f.Marshal()
}

// MarkPending records that the local fact name awaits a review proposal.
func MarkPending(cfg config.Config, storeID route.StoreID, name string) error {
	if !fact.NameRE.MatchString(name) {
		return fmt.Errorf("review: %q is not a fact name", name)
	}
	dir, err := pendingDir(cfg, storeID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, name), nil, 0o600)
}

// pendingDir is the marker directory, confined like the local layer.
func pendingDir(cfg config.Config, storeID route.StoreID) (string, error) {
	dir := cfg.ReviewDir(string(storeID))
	root := store.Root{Store: storeID, Kind: store.KindLocal, State: cfg.State(), Path: dir}
	return dir, root.CheckWrite(dir)
}

// CompareURL is GitHub's compare page for branch when remote is a GitHub scp
// or ssh URL, port-443 form included, and "" for any other remote.
func CompareURL(remote, branch string) string {
	m := githubScp.FindStringSubmatch(remote)
	if m == nil {
		m = githubSSH.FindStringSubmatch(remote)
	}
	if m == nil {
		return ""
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "https://github.com/" + m[1] + "/" + m[2] + "/compare/" + strings.Join(segs, "/") + "?expand=1"
}

// Sync takes the store's lock and runs SyncLocked.
func Sync(ctx context.Context, cfg config.Config, storeID route.StoreID, rules gate.Rules, scanner gate.Scanner) (lines, warnings []string) {
	unlock, err := store.Lock(cfg, string(storeID))
	if err != nil {
		return nil, []string{fmt.Sprintf("review: locking the %s store: %v", storeID, err)}
	}
	defer unlock()
	return SyncLocked(ctx, cfg, storeID, rules, scanner)
}

// SyncLocked regenerates the checkout's index, prunes the local copies it now
// lists, then proposes every pending fact. The caller holds the store's lock.
// A fact that cannot be proposed yet stays pending and warns; the local copy
// is safe either way.
func SyncLocked(ctx context.Context, cfg config.Config, storeID route.StoreID, rules gate.Rules, scanner gate.Scanner) (lines, warnings []string) {
	checkout, err := store.CheckoutRoot(ctx, cfg, storeID)
	if err != nil {
		return nil, []string{"review: " + err.Error()}
	}
	local := store.LocalRoot(cfg, storeID)
	dir, err := pendingDir(cfg, storeID)
	if err != nil {
		return nil, []string{"review: " + err.Error()}
	}

	warnings = healIndex(ctx, cfg, checkout, rules, scanner)
	entries, walkErrs := local.Walk()
	pruned, pruneWarnings := prune(local, checkout, entries, dir)
	warnings = append(warnings, pruneWarnings...)
	if len(pruned) > 0 {
		_, reports, err := local.WriteIndex(rules)
		warnings = append(warnings, reportWarnings(local, reports)...)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("review: regenerating the %s local index: %v", storeID, err))
		}
	}

	names, err := markers(dir)
	if err != nil {
		return lines, append(warnings, fmt.Sprintf("review: listing pending facts: %v", err))
	}
	// Push defaults to off, so one summary instead of a warning per marker on
	// every write; the markers wait for push = true.
	if !cfg.Push || !cfg.CommitEnabled() {
		if len(names) > 0 {
			warnings = append(warnings, fmt.Sprintf("review: %d flagged fact(s) wait for push = true to be proposed", len(names)))
		}
		return lines, warnings
	}
	kept := slices.DeleteFunc(entries, func(e store.Entry) bool { return pruned[e.Rel] })
	localByName := byName(kept)
	for _, name := range names {
		entry, ok := localByName[name]
		if !ok {
			if unreadable(walkErrs, name) {
				warnings = append(warnings, fmt.Sprintf("review %s: local copy unreadable", name))
				continue
			}
			dropMarker(dir, name, &warnings)
			continue
		}
		f := entry.Fact
		data, err := Reviewed(f)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("review %s: %v", name, err))
			continue
		}
		rel, err := filepath.Rel(checkout.Path, checkout.PathFor(f, ""))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("review %s: %v", name, err))
			continue
		}
		rel = filepath.ToSlash(rel)
		var host string
		if f.Metadata.Provenance != nil {
			host = f.Metadata.Provenance.Host
		}
		branch := Branch(host, name, data)
		outcome, detail := propose(ctx, cfg, checkout, rules, scanner, commit.ReviewRequest{
			Rel:    rel,
			Data:   data,
			Branch: branch,
			Msg:    Message(f, storeID, rel, gateIDs(f.Metadata.Flags)),
		})
		switch outcome {
		case commit.ReviewPushed, commit.ReviewExists:
			dropMarker(dir, name, &warnings)
			line := fmt.Sprintf("review %s %s", storeID, branch)
			if u := CompareURL(checkout.Remote, branch); u != "" {
				line += " " + u
			}
			lines = append(lines, line)
		case commit.ReviewPermanent:
			dropMarker(dir, name, &warnings)
			warnings = append(warnings, fmt.Sprintf("review %s: not proposed: %s", name, detail))
		default:
			warnings = append(warnings, fmt.Sprintf("review %s: %s", name, detail))
		}
	}
	return lines, warnings
}

// healIndex regenerates the checkout's index and commits it when it changed.
// A merged review PR leaves MEMORY.md without its fact, and session start
// reads only MEMORY.md, so the first host to write after a pull heals it for
// every host. A missing checkout stays missing, for a later clone.
func healIndex(ctx context.Context, cfg config.Config, checkout store.Root, rules gate.Rules, scanner gate.Scanner) []string {
	if _, err := os.Stat(checkout.Path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	changed, reports, err := checkout.WriteIndex(rules)
	warnings := reportWarnings(checkout, reports)
	if err != nil {
		return append(warnings, fmt.Sprintf("review: regenerating the %s checkout index: %v", checkout.Store, err))
	}
	if !changed {
		return warnings
	}
	if w := commit.Checkout(ctx, cfg, checkout, rules, scanner, "priors: regenerate index"); w != "" {
		warnings = append(warnings, "review: "+w)
	}
	return warnings
}

// reportWarnings turns WriteIndex's reports into warnings, so a fact the
// regenerated index leaves out is never dropped silently.
func reportWarnings(root store.Root, reports []string) []string {
	warnings := make([]string, 0, len(reports))
	for _, report := range reports {
		warnings = append(warnings, fmt.Sprintf("review: %s: %s", root.Path, report))
	}
	return warnings
}

// prune deletes each local fact the checkout's index lists and the checkout
// holds with the same provenance and source, which is the merged copy coming
// back, and its marker, and returns the Rels it deleted. A same-named fact
// from another writer stays, and so does one the index does not list yet, or
// session start would read it nowhere. The local layer is confined from the
// state dir like every other local write, and nothing resolving into the
// checkout is deleted, since there the "local copy" is the checkout's own.
func prune(local, checkout store.Root, entries []store.Entry, markerDir string) (removed map[string]bool, warnings []string) {
	if len(entries) == 0 {
		return nil, nil
	}
	if err := local.CheckWrite(local.Path); err != nil {
		return nil, []string{fmt.Sprintf("review: not pruning the %s local layer: %v", local.Store, err)}
	}
	listed := indexedNames(checkout)
	if len(listed) == 0 {
		return nil, nil
	}
	theirEntries, _ := checkout.Walk()
	theirs := byName(theirEntries)
	resolvedCheckout, err := filepath.EvalSymlinks(checkout.Path)
	if err != nil {
		return nil, []string{fmt.Sprintf("review: not pruning the %s local layer: %v", local.Store, err)}
	}
	removed = map[string]bool{}
	for _, e := range entries {
		t, ok := theirs[e.Fact.Name]
		if !ok || !listed[e.Fact.Name] || !sameWriter(e.Fact, t.Fact) {
			continue
		}
		path := filepath.Join(local.Path, filepath.FromSlash(e.Rel))
		if err := local.CheckWrite(filepath.Dir(path)); err != nil {
			warnings = append(warnings, fmt.Sprintf("review: not pruning %s: %v", path, err))
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("review: not pruning %s: %v", path, err))
			continue
		}
		if inside, err := filepath.Rel(resolvedCheckout, resolved); err == nil && filepath.IsLocal(inside) {
			warnings = append(warnings, fmt.Sprintf("review: not pruning %s: it lies inside the checkout", path))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warnings = append(warnings, fmt.Sprintf("review: pruning %s: %v", path, err))
			continue
		}
		removed[e.Rel] = true
		dropMarker(markerDir, e.Fact.Name, &warnings)
	}
	return removed, warnings
}

// indexedNames is the set of fact names the checkout's MEMORY.md lists; an
// unreadable index lists none.
func indexedNames(checkout store.Root) map[string]bool {
	lines, err := checkout.ReadIndex()
	if err != nil {
		return nil
	}
	names := make(map[string]bool, len(lines))
	for _, line := range lines {
		if name, _, ok := store.ParseIndexLine(line); ok {
			names[name] = true
		}
	}
	return names
}

// byName maps each fact name to its entry, a live one winning over an
// archived one as in store.Root.FindByName.
func byName(entries []store.Entry) map[string]store.Entry {
	m := make(map[string]store.Entry, len(entries))
	for _, e := range entries {
		if have, ok := m[e.Fact.Name]; ok && (e.Archived || !have.Archived) {
			continue
		}
		m[e.Fact.Name] = e
	}
	return m
}

// unreadable reports whether the local walk failed on a file named name.md,
// which is most likely the pending fact itself, or on a directory or the root,
// which may hold it.
func unreadable(errs []store.WalkErr, name string) bool {
	for _, e := range errs {
		base := filepath.Base(filepath.FromSlash(e.Rel))
		if base == name+".md" || filepath.Ext(base) != ".md" {
			return true
		}
	}
	return false
}

// gateIDs keeps the reasons shaped like gate ids. Only priors writes flags,
// but a hand edit can change them, and they land in a commit message.
func gateIDs(reasons []string) []string {
	var ids []string
	for _, r := range reasons {
		if gateID.MatchString(r) {
			ids = append(ids, r)
		}
	}
	return ids
}

func sameWriter(a, b fact.Fact) bool {
	return equalProvenance(a.Metadata.Provenance, b.Metadata.Provenance) &&
		equalSource(a.Metadata.Source, b.Metadata.Source)
}

func equalProvenance(a, b *fact.Provenance) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Engine == b.Engine && a.Session == b.Session && a.Host == b.Host
}

func equalSource(a, b *fact.Source) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Engine == b.Engine && a.Path == b.Path && a.SHA256 == b.SHA256
}

// markers lists the pending fact names, sorted; a missing directory is none.
func markers(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && fact.NameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func dropMarker(dir, name string, warnings *[]string) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		*warnings = append(*warnings, fmt.Sprintf("review %s: removing its pending marker: %v", name, err))
	}
}
