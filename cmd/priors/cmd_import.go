package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/write"
)

func cmdImport(args []string, s streams) int {
	fs := newFlagSet("import", s)
	cfgPath := fs.String("config", "", "config file")
	claudeDir := fs.String("claude-dir", "", "Claude Code's config dir (default ~/.claude)")
	dryRun := fs.Bool("dry-run", false, "report what would be written and proposed, writing nothing")
	external := fs.Bool("external", false, "the importing session ingested external content")
	engine := fs.String("engine", "", "engine running the import")
	session := fs.String("session", "", "session id running the import")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	id, err := resolveIdentity("", *engine, *session)
	if err != nil {
		s.errln(err)
		return 1
	}
	if *claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			s.errln(err)
			return 1
		}
		*claudeDir = filepath.Join(home, ".claude")
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("refused: redaction rule set unavailable:", err)
		return 1
	}
	scanner, err := gate.PinnedScanner()
	if err != nil {
		s.errln("refused: secret scanner unavailable:", err)
		return 1
	}

	ctx := context.Background()
	items, notes, err := importer.Claude(ctx, *claudeDir, cfg, route.Resolver{})
	if err != nil {
		s.errln("reading Claude memory:", err)
		return 1
	}
	res, err := importer.Run(ctx, items, importer.Options{
		Config:           cfg,
		Deps:             write.Deps{Rules: rules, Scanner: scanner, Now: time.Now},
		SessionID:        id.session,
		Engine:           id.engine,
		Host:             id.host,
		External:         *external,
		IdentityConflict: id.conflict,
		DryRun:           *dryRun,
	})
	if err != nil {
		s.errln("refused:", err)
		return 1
	}
	for _, r := range append(notes, res.Reports...) {
		s.outln(r)
	}
	if res.Warning != "" {
		s.errln("warning:", res.Warning)
	}
	if res.Failed > 0 {
		s.errln(fmt.Sprintf("%d items failed for reasons other than a refusal; running again may clear them", res.Failed))
		return 1
	}
	return 0
}
