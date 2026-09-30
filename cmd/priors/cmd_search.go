package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/search"
)

const searchDeadline = 800 * time.Millisecond

// cmdSearch is a read path: whatever goes wrong, it prints no hits and exits 0.
func cmdSearch(args []string, s streams) int {
	fs := newFlagSet("search", s)
	cfgPath := fs.String("config", "", "config file")
	repo := fs.String("repo", "", "repo to search checkout facts of (default: the session's)")
	anyRepo := fs.Bool("any-repo", false, "search checkout facts of every repo")
	typ := fs.String("type", "", "only facts of this type")
	scope := fs.String("scope", "", "only facts of this scope")
	all := fs.Bool("all", false, "include archived and superseded facts")
	limit := fs.Int("limit", 0, "most hits to print (default 20)")
	cwd := fs.String("cwd", "", "working directory (default: the process's)")
	terms, code, ok := parseFlags(fs, args)
	if !ok {
		return code
	}
	if len(terms) == 0 {
		s.errln("usage: priors search <terms...>")
		return 1
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return 0
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		return 0
	}
	id, err := resolveIdentity(*cwd, "", "")
	if err != nil {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), searchDeadline)
	defer cancel()
	sess := sessionAt(ctx, id.cwd, cfg)
	q := search.Query{Terms: terms, Repo: *repo, AnyRepo: *anyRepo, Type: *typ, Scope: *scope, All: *all, Limit: *limit}
	hits, reports, err := search.Run(ctx, search.Rg{}, readRoots(cfg, sess), sess, q, rules)
	if err != nil {
		s.errln("search unavailable:", err)
		return 0
	}
	for _, r := range reports {
		s.errln(r)
	}
	if len(hits) == 0 {
		return 0
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		e := h.Entry
		lines[i] = sanitize.Line(fmt.Sprintf("%s/%s — %s — %s", e.Root.Store, e.Rel, e.Fact.Name, e.Fact.Description), sanitize.IndexLineMax)
	}
	header := sanitize.Header("search: " + strings.Join(terms, " "))
	s.outText(sanitize.Fence(header, strings.Join(lines, "\n"), sanitize.NewDelimiter()))
	return 0
}
