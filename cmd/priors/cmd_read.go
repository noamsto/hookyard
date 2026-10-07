package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	staleAfter = 90 * 24 * time.Hour
	noRepoDir  = "_norepo"
)

// localApplies reports whether a local-layer fact was learned in the session's
// repo; the local layer ignores scope.
func localApplies(rel string, sess route.Session) bool {
	want := cmp.Or(sess.Repo, noRepoDir)
	first, _, _ := strings.Cut(rel, "/")
	return first == want
}

// checkoutApplies reports whether a checkout fact applies to repo.
func checkoutApplies(f fact.Fact, repo string) bool {
	return f.Metadata.Scope == "global" || (repo != "" && slices.Contains(f.Metadata.Repos, repo))
}

func isStale(f fact.Fact, now time.Time) bool {
	verified, err := time.Parse(time.DateOnly, f.Metadata.Verified)
	return err != nil || verified.Before(now.Add(-staleAfter))
}

func cmdList(args []string, s streams) int {
	fs := newFlagSet("list", s)
	cfgPath := fs.String("config", "", "config file")
	repo := fs.String("repo", "", "repo to list checkout facts for (default: the session's)")
	typ := fs.String("type", "", "only facts of this type")
	storeID := fs.String("store", "", "only this store (personal or work)")
	stale := fs.Bool("stale", false, "only facts never verified or not verified for 90 days")
	flagged := fs.Bool("flagged", false, "only the local layer's flagged facts")
	all := fs.Bool("all", false, "include archived and superseded facts")
	cwd := fs.String("cwd", "", "working directory (default: the process's)")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("refused: redaction rule set unavailable:", err)
		return 1
	}
	id, err := resolveIdentity(*cwd, "", "")
	if err != nil {
		s.errln(err)
		return 1
	}
	ctx := context.Background()
	sess := sessionAt(ctx, id.cwd, cfg)
	wantRepo := cmp.Or(*repo, sess.Repo)
	now := time.Now()

	v := attest.ForHost(cfg)
	var rows []string
	skipped := 0
	roots, refused := readRoots(ctx, cfg, sess, s)
	for _, root := range roots {
		if *storeID != "" && string(root.Store) != *storeID || *flagged && root.Kind != store.KindLocal {
			continue
		}
		entries, rootSkipped := visibleEntries(root, sess, wantRepo, s)
		skipped += rootSkipped
		for _, e := range entries {
			f := e.Fact
			if !*all && (e.Archived || f.Metadata.SupersededBy != "") ||
				*typ != "" && f.Metadata.Type != *typ ||
				*stale && !isStale(f, now) {
				continue
			}
			if ids := rules.Match(string(e.Raw)); len(ids) > 0 {
				s.errf("excluded %s/%s: rule %s\n", root.Store, e.Rel, strings.Join(ids, ","))
				continue
			}
			rows = append(rows, listRow(e, v.Reviewed(e)))
		}
	}
	if len(rows) > 0 {
		s.outText(sanitize.Fence(sanitize.Header("list"), strings.Join(rows, "\n"), sanitize.NewDelimiter()))
	}
	printReports(s, v)
	// A skipped file or refused checkout makes the listing incomplete, so exit
	// non-zero even when some rows printed; a redaction exclusion is reported
	// and does not.
	if skipped > 0 || refused {
		return 1
	}
	return 0
}

// readRoots is the session's read roots, with each refused checkout reported
// on stderr and flagged in refused.
func readRoots(ctx context.Context, cfg config.Config, sess route.Session, s streams) (roots []store.Root, refused bool) {
	roots, errs := store.ReadRoots(ctx, cfg, route.ReadStores(sess, cfg))
	for _, err := range errs {
		s.errln("refused:", err)
	}
	return roots, len(errs) > 0
}

// walk is root.Walk with a missing checkout and every skipped file reported
// on stderr. skipped counts the walk errors; a missing root is empty, not
// skipped.
func walk(root store.Root, s streams) (entries []store.Entry, skipped int) {
	if root.Kind == store.KindCheckout {
		if _, err := os.Stat(root.Path); errors.Is(err, fs.ErrNotExist) {
			s.errf("%s store root %s does not exist\n", root.Store, root.Path)
			return nil, 0
		}
	}
	entries, errs := root.Walk()
	for _, we := range errs {
		s.errf("skipped %s/%s: %v\n", root.Store, we.Rel, we.Err)
	}
	return entries, len(errs)
}

// visibleEntries are the root's facts the session may see: a checkout's that
// apply to repo, a local layer's learned in the session's repo.
func visibleEntries(root store.Root, sess route.Session, repo string, s streams) (entries []store.Entry, skipped int) {
	entries, skipped = walk(root, s)
	return slices.DeleteFunc(entries, func(e store.Entry) bool {
		if root.Kind == store.KindLocal {
			return !localApplies(e.Rel, sess)
		}
		return !checkoutApplies(e.Fact, repo)
	}), skipped
}

func printReports(s streams, v *attest.Verifier) {
	for _, r := range v.Reports() {
		s.errln(r)
	}
}

// reviewedMark leads a verified fact's row, before the store name, which no
// fact controls: a description can neither forge it nor truncate it away.
const reviewedMark = "reviewed · "

func listRow(e store.Entry, reviewed bool) string {
	f := e.Fact
	where := string(e.Root.Store)
	if e.Root.Kind == store.KindLocal {
		where += " local"
	}
	if reviewed {
		where = reviewedMark + where
	}
	verified := cmp.Or(f.Metadata.Verified, "never")
	row := fmt.Sprintf("%s %s — %s — verified %s — %s", where, e.Rel, f.Metadata.Type, verified, f.Description)
	if e.Root.Kind == store.KindLocal {
		row += " — flags: " + strings.Join(f.Metadata.Flags, ", ")
	}
	return sanitize.Line(row, sanitize.IndexLineMax)
}

func cmdShow(args []string, s streams) int {
	fs := newFlagSet("show", s)
	cfgPath := fs.String("config", "", "config file")
	cwd := fs.String("cwd", "", "working directory (default: the process's)")
	names, code, ok := parseFlags(fs, args)
	if !ok {
		return code
	}
	if len(names) != 1 {
		s.errln("usage: priors show <name>")
		return 1
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("refused: redaction rule set unavailable:", err)
		return 1
	}
	id, err := resolveIdentity(*cwd, "", "")
	if err != nil {
		s.errln(err)
		return 1
	}
	ctx := context.Background()
	sess := sessionAt(ctx, id.cwd, cfg)

	roots, _ := readRoots(ctx, cfg, sess, s)
	e, found := findFact(roots, sess, names[0], s)
	if !found {
		s.errf("no fact named %q\n", names[0])
		return 1
	}
	if ids := rules.Match(string(e.Raw)); len(ids) > 0 {
		s.errf("excluded %s/%s: rule %s\n", e.Root.Store, e.Rel, strings.Join(ids, ","))
		return 1
	}
	// An unverified claim of review is never printed as one.
	shown := e.Raw
	v := attest.ForHost(cfg)
	if attest.Claims(e.Fact) && !v.Reviewed(e) {
		if shown, err = fact.WithConfidence(e.Raw, "proposed"); err != nil {
			s.errln("show:", err)
			return 1
		}
	}
	printReports(s, v)
	header := sanitize.Header(string(e.Root.Store) + " store · " + e.Rel)
	s.outText(sanitize.Fence(header, sanitize.Text(string(shown)), sanitize.NewDelimiter()))
	return 0
}

// findFact is the first fact called name the session may see, checkout before
// local layer within each store, a live fact before an archived one.
func findFact(roots []store.Root, sess route.Session, name string, s streams) (store.Entry, bool) {
	for _, root := range roots {
		var archived *store.Entry
		entries, _ := walk(root, s)
		for i, e := range entries {
			if e.Fact.Name != name {
				continue
			}
			if root.Kind == store.KindLocal && !localApplies(e.Rel, sess) ||
				root.Kind == store.KindCheckout && !checkoutApplies(e.Fact, sess.Repo) {
				continue
			}
			if !e.Archived {
				return e, true
			}
			if archived == nil {
				archived = &entries[i]
			}
		}
		if archived != nil {
			return *archived, true
		}
	}
	return store.Entry{}, false
}
