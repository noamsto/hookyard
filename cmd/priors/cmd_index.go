package main

import (
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

	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
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
	CanonicalEvent string          `json:"canonical_event"`
	CWD            string          `json:"cwd"`
	SessionID      string          `json:"session_id"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// toolCall is a post_tool envelope; partial means the read failed partway, so
// fields after the failure are missing.
type toolCall struct {
	env     envelope
	partial bool
}

type hookResult struct {
	text    string
	reports []string
	call    *toolCall
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
// envelope says what to do. session_start answers with the index; post_tool
// answers nothing and leaves gate 2's marker for the session.
func cmdBare(s streams) int {
	if !isPiped(s.in) {
		s.errText(usage)
		return 1
	}
	res := runHook(func(ctx context.Context) hookResult {
		input, err := readEnvelope(s.in)
		if input.CanonicalEvent == "post_tool" {
			return hookResult{call: &toolCall{env: input, partial: err != nil}}
		}
		if err != nil || input.CanonicalEvent != "session_start" {
			return hookResult{}
		}
		return assembleIndex(ctx, "", input.CWD)
	})
	if res.call != nil {
		recordToolCall(*res.call)
	}
	printHook(s, res, false)
	return 0
}

// recordToolCall marks the call's session as watched, and as having ingested
// external content when the call may have. A partial read that lost tool_input
// counts as ingestion: the command is unknowable. Failing to mark is silent; a
// session left unmarked is flagged by gate 2.
func recordToolCall(c toolCall) {
	defer func() { _ = recover() }()
	if c.env.SessionID == "" {
		return
	}
	cfg, err := loadConfig("")
	if err != nil {
		return
	}
	ingest := (c.partial && c.env.ToolInput == nil) || gate.IngestsCall(c.env.ToolName, c.env.ToolInput)
	_ = gate.MarkSession(cfg.ProvenanceDir(), c.env.SessionID, ingest)
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
// anything else that is not a JSON object is an error. On a decode error it
// returns the fields decoded before the error along with it: a capped post_tool
// payload truncates native, which hookyard marshals after tool_input.
func readEnvelope(r io.Reader) (envelope, error) {
	dec := json.NewDecoder(io.LimitReader(r, maxEnvelope))
	var input envelope
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return input, nil
	}
	if err != nil {
		return input, err
	}
	if tok != json.Delim('{') {
		return input, errors.New("not a JSON object")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return input, err
		}
		switch key {
		case "canonical_event":
			err = dec.Decode(&input.CanonicalEvent)
		case "cwd":
			err = dec.Decode(&input.CWD)
		case "session_id":
			err = dec.Decode(&input.SessionID)
		case "tool_name":
			err = dec.Decode(&input.ToolName)
		case "tool_input":
			var raw json.RawMessage
			if err = dec.Decode(&raw); err == nil {
				input.ToolInput = raw
			}
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return input, err
		}
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
	// A missing scanner leaves it zero, which fails the commit closed.
	scanner, _ := gate.FindScanner(cfg.Scanner)
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
		skipped, err := writeGroup(context.Background(), cfg, rules, scanner, g, s)
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

// writeGroup regenerates g's indexes under g's lock, commits each checkout
// whose index changed, and reports whether the walk skipped any file.
func writeGroup(ctx context.Context, cfg config.Config, rules gate.Rules, scanner gate.Scanner, g indexGroup, s streams) (skipped bool, err error) {
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
		if !changed {
			continue
		}
		s.outln("wrote", filepath.Join(root.Path, store.IndexFile))
		if root.Kind == store.KindCheckout {
			if w := commit.Checkout(ctx, cfg, root, rules, scanner, "priors: regenerate index"); w != "" {
				s.errf("%s: warning: %s\n", root.Path, w)
			}
		}
	}
	return skipped, nil
}
