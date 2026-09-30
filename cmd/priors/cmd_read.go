package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
	id, err := resolveIdentity(*cwd, "", "")
	if err != nil {
		s.errln(err)
		return 1
	}
	sess := sessionAt(context.Background(), id.cwd, cfg)
	wantRepo := cmp.Or(*repo, sess.Repo)
	now := time.Now()

	var rows []string
	for _, root := range readRoots(cfg, sess) {
		if *storeID != "" && string(root.Store) != *storeID || *flagged && root.Kind != store.KindLocal {
			continue
		}
		for _, e := range visibleEntries(root, sess, wantRepo) {
			f := e.Fact
			if !*all && (e.Archived || f.Metadata.SupersededBy != "") ||
				*typ != "" && f.Metadata.Type != *typ ||
				*stale && !isStale(f, now) {
				continue
			}
			rows = append(rows, listRow(e))
		}
	}
	if len(rows) == 0 {
		return 0
	}
	s.outText(sanitize.Fence(sanitize.Header("list"), strings.Join(rows, "\n"), sanitize.NewDelimiter()))
	return 0
}

func readRoots(cfg config.Config, sess route.Session) []store.Root {
	return store.ReadRoots(cfg, route.ReadStores(sess, cfg))
}

// visibleEntries are the root's facts the session may see: a checkout's that
// apply to repo, a local layer's learned in the session's repo.
func visibleEntries(root store.Root, sess route.Session, repo string) []store.Entry {
	entries, _ := root.Walk()
	return slices.DeleteFunc(entries, func(e store.Entry) bool {
		if root.Kind == store.KindLocal {
			return !localApplies(e.Rel, sess)
		}
		return !checkoutApplies(e.Fact, repo)
	})
}

func listRow(e store.Entry) string {
	f := e.Fact
	where := string(e.Root.Store)
	if e.Root.Kind == store.KindLocal {
		where += " local"
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
	sess := sessionAt(context.Background(), id.cwd, cfg)

	e, found := findFact(readRoots(cfg, sess), sess, names[0])
	if !found {
		s.errf("no fact named %q\n", names[0])
		return 1
	}
	label := string(e.Root.Store) + "/" + e.Rel
	if ids := rules.Match(e.Fact.Text()); len(ids) > 0 {
		s.errf("excluded %s: rule %s\n", label, strings.Join(ids, ","))
		return 1
	}
	path, err := e.Root.Confine(e.Rel)
	if err != nil {
		s.errln(err)
		return 1
	}
	raw, err := os.ReadFile(path) //nolint:gosec // confined to the store root above
	if err != nil {
		s.errln(err)
		return 1
	}
	header := sanitize.Header(string(e.Root.Store) + " store · " + e.Rel)
	s.outText(sanitize.Fence(header, sanitize.Text(string(raw)), sanitize.NewDelimiter()))
	return 0
}

// findFact is the first fact called name, checkout before local layer within
// each store, a live fact before an archived one.
func findFact(roots []store.Root, sess route.Session, name string) (store.Entry, bool) {
	for _, root := range roots {
		var archived *store.Entry
		entries, _ := root.Walk()
		for i, e := range entries {
			if e.Fact.Name != name || root.Kind == store.KindLocal && !localApplies(e.Rel, sess) {
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
