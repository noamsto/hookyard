package main

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/lint"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// lintTarget is one store to lint: its checkout, and its local layer when
// there is one.
type lintTarget struct {
	id              route.StoreID
	checkout, local store.Root
	hasLocal        bool
}

func cmdLint(args []string, s streams) int {
	fs := newFlagSet("lint", s)
	cfgPath := fs.String("config", "", "config file")
	storeFlag := fs.String("store", "", "store to lint: personal, work or all (default: every configured one)")
	dir := fs.String("dir", "", "lint this store directory instead of the configured stores")
	kind := fs.String("kind", "", "with --dir: personal or work")
	var workOrgs, workNames stringList
	fs.Var(&workOrgs, "work-org", "work org (host/owner) the personal store must not name (repeatable)")
	fs.Var(&workNames, "work-name", "work name the personal store must not use (repeatable)")
	moveFlagged := fs.Bool("move-flagged", false, "move checkout facts that trip a content or size gate into the local layer")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}

	var cfg config.Config
	var targets []lintTarget
	if *dir != "" {
		id := route.StoreID(*kind)
		if id != route.StorePersonal && id != route.StoreWork {
			s.errln("--dir needs --kind personal or work")
			return 1
		}
		targets = []lintTarget{{id: id, checkout: store.Root{Store: id, Kind: store.KindCheckout, Path: *dir}}}
	} else {
		var err error
		if cfg, err = loadConfig(*cfgPath); err != nil {
			s.errln("config:", err)
			return 2
		}
		if targets, err = configuredTargets(cfg, *storeFlag); err != nil {
			s.errln(err)
			return 1
		}
		if len(workOrgs) == 0 {
			workOrgs = cfg.WorkOrgs
		}
		if len(workNames) == 0 {
			workNames = cfg.WorkNames
		}
	}

	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.outln(lint.Finding{File: ".", Rule: "rules", Msg: "redaction rule set unavailable: " + err.Error()})
		return 1
	}
	var scanner *gate.Scanner
	if sc, err := gate.FindScanner(cfg.Scanner); err == nil {
		scanner = &sc
	}

	ctx := context.Background()
	failed := false
	for _, t := range targets {
		opts := lint.Options{Store: t.id, WorkOrgs: workOrgs, WorkNames: workNames, Rules: rules, Scanner: scanner, Gates: true}
		if *moveFlagged && t.hasLocal {
			moved, err := lint.MoveFlagged(ctx, t.checkout, t.local, opts)
			for _, rel := range moved {
				s.outln("moved", rel)
			}
			if err != nil {
				s.errln("move-flagged:", err)
				return 1
			}
		}
		findings := lint.Store(ctx, t.checkout, opts)
		if t.hasLocal && dirExists(t.local.Path) {
			opts.Gates = false
			findings = append(findings, lint.Store(ctx, t.local, opts)...)
		}
		for _, f := range findings {
			s.outln(f)
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func configuredTargets(cfg config.Config, which string) ([]lintTarget, error) {
	ids := []route.StoreID{route.StorePersonal}
	if cfg.WorkPresent() {
		ids = append(ids, route.StoreWork)
	}
	switch which {
	case "", "all":
	case string(route.StorePersonal), string(route.StoreWork):
		if !slices.Contains(ids, route.StoreID(which)) {
			return nil, fmt.Errorf("the %s store is not configured on this host", which)
		}
		ids = []route.StoreID{route.StoreID(which)}
	default:
		return nil, fmt.Errorf("--store must be personal, work or all, got %q", which)
	}
	targets := make([]lintTarget, len(ids))
	for i, id := range ids {
		targets[i] = lintTarget{id: id, checkout: store.CheckoutRoot(cfg, id), local: store.LocalRoot(cfg, id), hasLocal: true}
	}
	return targets, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
