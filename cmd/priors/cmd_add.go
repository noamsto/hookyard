package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/write"
)

func cmdAdd(args []string, s streams) int {
	fs := newFlagSet("add", s)
	cfgPath := fs.String("config", "", "config file")
	name := fs.String("name", "", "fact name")
	description := fs.String("description", "", "one-line description")
	typ := fs.String("type", "", "project, reference, feedback or user")
	scope := fs.String("scope", "", "repo or global (default repo)")
	var repos stringList
	fs.Var(&repos, "repo", "repo the fact applies to (repeatable)")
	engines := fs.String("engines", "", "comma-separated engines the fact applies to")
	useStdin := fs.Bool("stdin", false, "read the body, or a whole fact with frontmatter, from stdin")
	external := fs.Bool("external", false, "the session ingested external content")
	cwd := fs.String("cwd", "", "working directory (default: the process's)")
	engine := fs.String("engine", "", "engine writing the fact")
	session := fs.String("session", "", "session id writing the fact")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	id, err := resolveIdentity(*cwd, *engine, *session)
	if err != nil {
		s.errln(err)
		return 1
	}
	ctx := context.Background()
	sess := sessionAt(ctx, id.cwd, cfg)

	var f fact.Fact
	if *useStdin {
		raw, err := io.ReadAll(s.in)
		if err != nil {
			s.errln("reading stdin:", err)
			return 1
		}
		if bytes.HasPrefix(raw, []byte("---\n")) {
			if f, err = fact.Parse(raw); err != nil {
				s.errln("invalid fact on stdin:", err)
				return 1
			}
		} else {
			f.Body = string(raw)
		}
	}
	if *name != "" {
		f.Name = *name
	}
	if *description != "" {
		f.Description = *description
	}
	if *typ != "" {
		f.Metadata.Type = *typ
	}
	if *scope != "" {
		f.Metadata.Scope = *scope
	}
	if len(repos) > 0 {
		f.Metadata.Repos = repos
	}
	if *engines != "" {
		f.Metadata.Engines = strings.Split(*engines, ",")
	}
	if f.Metadata.Scope == "" {
		f.Metadata.Scope = "repo"
	}
	if f.Metadata.Scope == "repo" && len(f.Metadata.Repos) == 0 {
		if sess.Repo == "" {
			s.errln("no repo: pass --repo or --scope global")
			return 1
		}
		f.Metadata.Repos = []string{sess.Repo}
	}

	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("refused: redaction rule set unavailable:", err)
		return 1
	}
	scanner, err := gate.FindScanner(cfg.Scanner)
	if err != nil {
		s.errln("refused: secret scanner unavailable:", err)
		return 1
	}

	res, err := write.Add(ctx, cfg, write.Request{
		Fact:      f,
		Session:   sess,
		SessionID: id.session,
		Engine:    id.engine,
		Host:      id.host,
		External:  *external,
	}, write.Deps{Rules: rules, Scanner: scanner, Now: time.Now})
	if err != nil {
		s.errln("refused:", err)
		return 1
	}
	if res.Warning != "" {
		s.errln("warning:", res.Warning)
	}
	s.outln(addLine(res))
	return 0
}

func addLine(res write.Result) string {
	reasons := strings.Join(res.Reasons, ", ")
	switch res.Outcome {
	case "flagged":
		return fmt.Sprintf("flagged %s %s: %s", res.Store, res.Path, reasons)
	case "quarantined":
		return fmt.Sprintf("quarantined %s: %s", res.Path, reasons)
	default:
		return fmt.Sprintf("published %s %s", res.Store, res.Path)
	}
}
