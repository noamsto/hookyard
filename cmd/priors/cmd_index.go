package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tier1"
)

const (
	hookDeadline = 800 * time.Millisecond
	maxEnvelope  = 1 << 20
)

// envelope is the part of hookyard's exec-handler input priors reads.
type envelope struct {
	CanonicalEvent string `json:"canonical_event"`
	CWD            string `json:"cwd"`
}

type hookResult struct {
	text    string
	reports []string
}

type hookOutput struct {
	HookSpecificOutput struct {
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func cmdIndex(args []string, s streams) int {
	fs := newFlagSet("index", s)
	cfgPath := fs.String("config", "", "config file")
	write := fs.Bool("write", false, "regenerate every MEMORY.md instead of printing the tier-1 block")
	text := fs.Bool("text", false, "print the fenced block instead of JSON")
	cwd := fs.String("cwd", "", "working directory (default: the envelope's, else the process's)")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *write {
		return writeIndexes(*cfgPath, s)
	}
	res := runHook(func(ctx context.Context) hookResult {
		var input envelope
		if *cwd == "" && isPiped(s.in) {
			var err error
			if input, err = readEnvelope(s.in); err != nil {
				return hookResult{}
			}
		}
		return assembleIndex(ctx, *cfgPath, cmp.Or(*cwd, input.CWD))
	})
	printHook(s, res, *text)
	return 0
}

// cmdBare is hookyard's exec handler: priors takes no arguments there, so the
// envelope says what to do.
func cmdBare(s streams) int {
	if !isPiped(s.in) {
		s.errText(usage)
		return 1
	}
	res := runHook(func(ctx context.Context) hookResult {
		input, err := readEnvelope(s.in)
		if err != nil || input.CanonicalEvent != "session_start" {
			return hookResult{}
		}
		return assembleIndex(ctx, "", input.CWD)
	})
	printHook(s, res, false)
	return 0
}

// runHook runs job under the hook deadline and returns nothing if it is late
// or panics: a hook must never stall or break session start. A late job is
// abandoned, not waited for.
func runHook(job func(ctx context.Context) hookResult) hookResult {
	ctx, cancel := context.WithTimeout(context.Background(), hookDeadline)
	defer cancel()
	done := make(chan hookResult, 1)
	go func() {
		var res hookResult
		defer func() {
			if recover() != nil {
				res = hookResult{}
			}
			done <- res
		}()
		res = job(ctx)
	}()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		return hookResult{}
	}
}

func assembleIndex(ctx context.Context, cfgPath, cwd string) hookResult {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return hookResult{}
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		return hookResult{}
	}
	id, err := resolveIdentity(cwd, "", "")
	if err != nil {
		return hookResult{}
	}
	text, reports := tier1.Assemble(ctx, sessionAt(ctx, id.cwd, cfg), cfg, rules)
	return hookResult{text: text, reports: reports}
}

func printHook(s streams, res hookResult, raw bool) {
	for _, r := range res.reports {
		s.errln(r)
	}
	if res.text == "" {
		return
	}
	if raw {
		s.outText(res.text)
		return
	}
	var out hookOutput
	out.HookSpecificOutput.AdditionalContext = res.text
	enc := json.NewEncoder(s.out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

func isPiped(in *os.File) bool {
	info, err := in.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// readEnvelope reads the envelope from r. Empty input is an empty envelope;
// anything else that is not a JSON object is an error.
func readEnvelope(r io.Reader) (envelope, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxEnvelope))
	if err != nil {
		return envelope{}, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return envelope{}, nil
	}
	if raw[0] != '{' {
		return envelope{}, errors.New("not a JSON object")
	}
	var input envelope
	if err := json.Unmarshal(raw, &input); err != nil {
		return envelope{}, err
	}
	return input, nil
}

// indexGroup is the roots one lock covers.
type indexGroup struct {
	lock  string
	roots []store.Root
}

// writeIndexes regenerates every index priors owns on this host. A local layer
// or quarantine that does not exist yet has nothing to index. A fact the walk
// had to skip fails the run, so a broken fact cannot silently drop out.
func writeIndexes(cfgPath string, s streams) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("refused: redaction rule set unavailable:", err)
		return 1
	}
	ids := []route.StoreID{route.StorePersonal}
	if cfg.WorkPresent() {
		ids = append(ids, route.StoreWork)
	}
	var groups []indexGroup
	for _, id := range ids {
		g := indexGroup{lock: string(id), roots: []store.Root{store.CheckoutRoot(cfg, id)}}
		if local := store.LocalRoot(cfg, id); dirExists(local.Path) {
			g.roots = append(g.roots, local)
		}
		groups = append(groups, g)
	}
	if q := store.QuarantineRoot(cfg); dirExists(q.Path) {
		groups = append(groups, indexGroup{lock: "quarantine", roots: []store.Root{q}})
	}
	code := 0
	for _, g := range groups {
		skipped, err := writeGroup(cfg, rules, g, s)
		if err != nil {
			s.errln(err)
			return 1
		}
		if skipped {
			code = 1
		}
	}
	return code
}

// writeGroup regenerates g's indexes under g's lock and reports whether the
// walk skipped any file.
func writeGroup(cfg config.Config, rules gate.Rules, g indexGroup, s streams) (skipped bool, err error) {
	unlock, err := store.Lock(cfg, g.lock)
	if err != nil {
		return false, err
	}
	defer unlock()
	for _, root := range g.roots {
		changed, reports, err := root.WriteIndex(rules)
		for _, r := range reports {
			s.errf("%s: %s\n", root.Path, r)
			skipped = skipped || strings.HasPrefix(r, "skipped ")
		}
		if err != nil {
			return skipped, fmt.Errorf("%s: %w", root.Path, err)
		}
		if changed {
			s.outln("wrote", filepath.Join(root.Path, store.IndexFile))
		}
	}
	return skipped, nil
}
