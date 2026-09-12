// Command hookyard registers agent hooks once and routes them to every engine.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/internal/doctor"
	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/router"
	"github.com/noamsto/hookyard/internal/verdict"
	"github.com/noamsto/hookyard/internal/vocab"
)

const usage = `hookyard — register agent hooks once, route them to every coding agent

  hookyard install   render every manifest into every engine's native config
  hookyard emit      print Claude Code's overlay to stdout for Nix to place
  hookyard validate  check manifests without writing anything
  hookyard doctor    report whether each engine will actually run the hooks
  hookyard route     dispatch one hook event to every handler that matches it
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = install(os.Args[2:])
	case "emit":
		err = emit(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "route":
		err = route(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hookyard: %v\n", err)
		os.Exit(1)
	}
}

// manifestPaths collects a repeatable --manifest flag. hookyard is invoked
// once per activation with every manifest, never once per repo: the strip is
// keyed on a marker that does not record which manifest produced a row, so a
// second per-repo invocation would strip the first repo's rows (§8).
type manifestPaths []string

func (m *manifestPaths) String() string { return strings.Join(*m, ",") }

func (m *manifestPaths) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type targets struct {
	codex  string
	cursor string
	pi     string
}

func defaultTargets() (targets, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return targets{}, err
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	// PI_CODING_AGENT_DIR replaces ~/.pi/agent rather than ~/.pi (verified
	// against a live pi), so the settings file is a direct join onto it.
	piAgentDir := os.Getenv("PI_CODING_AGENT_DIR")
	if piAgentDir == "" {
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}
	return targets{
		codex:  filepath.Join(codexHome, "config.toml"),
		cursor: filepath.Join(home, ".cursor", "hooks.json"),
		pi:     filepath.Join(piAgentDir, "settings.json"),
	}, nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	defaults, err := defaultTargets()
	if err != nil {
		return err
	}
	defaultStateDir, err := record.DefaultStateDir()
	if err != nil {
		return err
	}
	routerPath := fs.String("router-path", "", "absolute path hookyard is invoked by (defaults to this binary)")
	stateDir := fs.String("state-dir", defaultStateDir, "directory the router reads its table from and writes records to")
	codex := fs.String("codex-config", defaults.codex, "Codex config.toml to write")
	cursor := fs.String("cursor-hooks", defaults.cursor, "Cursor hooks.json to write")
	pi := fs.String("pi-settings", defaults.pi, "Pi settings.json to write; the bridge lands in bin/ beside it")
	dryRun := fs.Bool("dry-run", false, "print what would be written and exit")
	allowEmpty := fs.Bool("allow-empty", false, "render an empty table and strip every hookyard entry when no --manifest is given")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A bare `hookyard install` with no --manifest is almost always a typo, so
	// it stays an error. The home-manager module needs the opposite: an empty
	// manifest list is a state it must be able to render, not skip.
	// ReadTable already treats zero handlers as legitimate, distinct from the
	// table being gone (manifest.go); install had no way to produce that
	// state. Skipping the invocation instead would leave the previous
	// generation's rows and table.json in place, so a handler the consumer
	// just removed would keep firing with nothing registering it.
	if len(paths) == 0 && !*allowEmpty {
		return fmt.Errorf("no --manifest given")
	}
	return runInstall(paths, *routerPath, *stateDir, *codex, *cursor, *pi, *dryRun)
}

// runInstall is install's pipeline, split out so tests can drive it with
// explicit paths instead of os.Args.
func runInstall(paths manifestPaths, routerPath, stateDir, codex, cursor, pi string, dryRun bool) error {
	handlers, err := loadAll(paths)
	if err != nil {
		return err
	}
	router := routerPath
	if router == "" {
		router, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if err := checkShellSafe("--router-path", router); err != nil {
		return err
	}
	if err := checkShellSafe("--state-dir", stateDir); err != nil {
		return err
	}
	plan, err := render.BuildPlan(handlers, router, stateDir)
	if err != nil {
		return err
	}

	if dryRun {
		printPlan(plan, pi)
		return nil
	}
	// Checked here, over every path at once, rather than inside each writer:
	// everything above this point is read-only, so this is the last moment an
	// install is still all-or-nothing. A per-writer refusal would land after
	// the table and the earlier engines were already written.
	if err := render.CheckDestinations(
		render.Destination{Flag: "--codex-config", Path: codex},
		render.Destination{Flag: "--cursor-hooks", Path: cursor},
		render.Destination{Flag: "--pi-settings", Path: pi},
		// The bridge's location follows --pi-settings, so that is still the
		// flag to name when the refusal is about the bridge.
		render.Destination{Flag: "--pi-settings", Path: render.PiBridgePath(pi)},
	); err != nil {
		return err
	}
	// Separate from the check above, which cannot see a link one segment up.
	if err := render.CheckPiBridgeDir("--pi-settings", render.PiBridgePath(pi)); err != nil {
		return err
	}
	// The table must exist before any engine config can point at it: an
	// entry pointing at a --state-dir whose table isn't there yet is a
	// fail-open with a wider window than a config write failing outright.
	if err := record.EnsureStateDir(stateDir); err != nil {
		return err
	}
	if err := manifest.WriteTable(filepath.Join(stateDir, "table.json"), handlers); err != nil {
		return err
	}
	// Each writer is independent, so a later failure does not undo an earlier
	// write. They are ordered least to most consequential: Codex last, because
	// its file also holds the trust stores.
	if err := render.WriteCursor(cursor, plan[vocab.Cursor]); err != nil {
		return err
	}
	// More consequential than the config-only writer above it: this one also
	// lands executable code.
	if err := render.WritePi(pi, plan[vocab.Pi], piVersion()); err != nil {
		return err
	}
	if err := render.WriteCodex(codex, plan[vocab.Codex]); err != nil {
		return err
	}
	for _, engine := range vocab.Engines {
		if engine == vocab.ClaudeCode {
			// install never reaches settings.json (R2): the count is real, but
			// saying only "N entries" here would read exactly like the other
			// three engines and tell an operator a file was written that was not.
			fmt.Printf("%-12s %d entries (emitted for Nix to place, not written by install)\n", engine, len(plan[engine]))
			continue
		}
		fmt.Printf("%-12s %d entries\n", engine, len(plan[engine]))
	}
	return nil
}

// piVersion is the value envelope.Detect keys on to recognise a Pi payload at
// all. Pi exposes no version to an extension, so it is resolved here, at
// install time, and written into the bridge's data constant.
//
// Every failure — pi absent, pi wedged past the timeout, pi printing nothing
// — has to land on a non-empty string: an empty one would be dropped by
// JSON.stringify, Detect would fall through every rule, and every Pi hook
// would become a silent no-op.
func piVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pi", "--version").Output()
	if err != nil {
		return "unknown"
	}
	if version := strings.TrimSpace(string(out)); version != "" {
		return version
	}
	return "unknown"
}

// shellUnsafe are characters render.command's caller must keep out of the
// paths it formats: whitespace ends the argument early, and the rest take on
// meaning when the engine's own shell re-parses the unquoted command string.
const shellUnsafe = " \t\n\r;&|$`\"'\\<>(){}[]*?!#~"

func checkShellSafe(flagName, path string) error {
	if i := strings.IndexAny(path, shellUnsafe); i >= 0 {
		return fmt.Errorf("%s %q contains %q, which a shell would treat specially; pass a path without whitespace or shell metacharacters", flagName, path, path[i])
	}
	return nil
}

// emit renders Claude Code's overlay to stdout for Nix to place at its
// --settings path, and writes nothing itself (R1, R2). It is engine-scoped
// and claude-code-only: Cursor's, Codex's and Pi's destinations are real files
// with other writers or executable state of their own (§4.2), so a store path
// would lose what they already hold — they stay on install.
func emit(args []string) error {
	fs := flag.NewFlagSet("emit", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	engine := fs.String("engine", "", "engine to emit for; only claude-code is supported")
	routerPath := fs.String("router-path", "", "absolute path hookyard is invoked by, embedded in each entry")
	stateDir := fs.String("state-dir", "", "state dir embedded in each entry as text; emit never creates it")
	base := fs.String("base", "", `existing settings file to merge hookyard's entries into (omit for a bare {"hooks":...} document)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *engine == "" {
		return fmt.Errorf("--engine is required")
	}
	if *engine != string(vocab.ClaudeCode) {
		return fmt.Errorf("--engine %q is not supported by emit: only claude-code's destination is Nix-managed; "+
			"codex, cursor and pi keep other writers or executable state a store path can't hold, so they stay on hookyard install", *engine)
	}
	if *routerPath == "" {
		return fmt.Errorf("--router-path is required")
	}
	if *stateDir == "" {
		return fmt.Errorf("--state-dir is required")
	}
	if err := checkShellSafe("--router-path", *routerPath); err != nil {
		return err
	}
	if err := checkShellSafe("--state-dir", *stateDir); err != nil {
		return err
	}

	// LoadStatic, not loadAll (which calls manifest.Load): emit runs inside a
	// Nix build sandbox, where a manifest's $HOME-rooted exec does not exist,
	// and stat-ing it would fail the build for a reason that has nothing to do
	// with the manifest being wrong (R7). Merge still runs on top of it, so a
	// duplicate id across manifests is refused here exactly as install refuses
	// it later in the same activation — build time and activation time cannot
	// disagree about what a legal manifest set is.
	manifests := make([]*manifest.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := manifest.LoadStatic(p)
		if err != nil {
			return err
		}
		manifests = append(manifests, m)
	}
	handlers, err := manifest.Merge(manifests)
	if err != nil {
		return err
	}

	plan, err := render.BuildPlan(handlers, *routerPath, *stateDir)
	if err != nil {
		return err
	}

	var baseBytes []byte
	if *base != "" {
		baseBytes, err = os.ReadFile(*base)
		if err != nil {
			return err
		}
	}
	doc, err := render.ClaudeSettings(baseBytes, plan[vocab.ClaudeCode])
	if err != nil {
		return err
	}
	// The whole document, one write: a Nix derivation redirects this with
	// `> $out`, so anything printed ahead of a later refusal would be captured
	// as if it were a valid overlay.
	_, err = os.Stdout.Write(doc)
	return err
}

func validate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no --manifest given")
	}
	handlers, err := loadAll(paths)
	if err != nil {
		return err
	}
	fmt.Printf("%d manifests, %d handlers, no problems found\n", len(paths), len(handlers))
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to report on (defaults to the working directory)")
	stateDir := fs.String("state-dir", "", "state directory to report on (defaults to the one recovered from the engines' configs, falling back to record.DefaultStateDir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		target, _ = os.Getwd()
	}
	paths, err := doctor.DefaultPaths()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		paths.StateDir = *stateDir
	}

	fmt.Printf("hookyard doctor — %s\n\n", target)
	problems := 0
	for _, f := range doctor.Run(paths, target) {
		if f.Status == doctor.Fail {
			problems++
		}
		fmt.Printf("%-12s %-20s %-8s %s\n", f.Engine, f.Check, f.Status, f.Detail)
	}
	if problems == 1 {
		return fmt.Errorf("1 problem found")
	}
	if problems > 1 {
		return fmt.Errorf("%d problems found", problems)
	}
	return nil
}

// routeOptions is route's argv, already parsed. parseErr rides along instead
// of returning to main: a flag failure is a router error to be recorded (§5),
// and the flags parsed before it are still the best identity the record can
// carry.
type routeOptions struct {
	start         time.Time
	registeredFor string
	event         string
	stateDir      string
	parseErr      error
}

// route dispatches one hook event. It returns nil on every path, because main
// turns a returned error into os.Exit(1) and Cursor reads a non-zero exit as a
// block — a router failing on its own defect would fail closed on the one
// engine the design promises to fail open on (§5).
func route(args []string) error {
	start := time.Now()
	// ContinueOnError, never ExitOnError: ExitOnError exits 2 on an unknown
	// flag, which is exactly that block shape, and its usage text would land on
	// the stderr Cursor reads as the reason.
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registeredFor := fs.String("registered-for", "", "engine this invocation was registered for")
	event := fs.String("event", "", "canonical or engine-scoped event name")
	stateDir := fs.String("state-dir", "", "directory holding the handler table and the record stream")
	parseErr := fs.Parse(args)

	runRoute(context.Background(), routeOptions{
		start:         start,
		registeredFor: *registeredFor,
		event:         *event,
		stateDir:      *stateDir,
		parseErr:      parseErr,
	}, os.Stdin, os.Stdout)
	return nil
}

// runRoute is route's pipeline (§4), split out so tests can drive §5's
// failure paths with an explicit stdin and stdout instead of the process's own.
func runRoute(ctx context.Context, opts routeOptions, in io.Reader, out io.Writer) {
	stateDir, fallbackNote := resolveStateDir(opts.stateDir)

	// appendRecord ends every path. The error is swallowed because by the time
	// this runs the verdict is already on stdout, and a failed append must
	// never change what the engine read (§6).
	appendRecord := func(e record.Event) {
		if stateDir == "" {
			return
		}
		e.Reason = joinReason(e.Reason, fallbackNote)
		e.RouterElapsed = time.Since(opts.start)
		w := &record.Writer{StateDir: stateDir}
		_ = w.Append(e)
	}
	routerError := func(reason string) {
		appendRecord(record.Event{
			// The payload that would have named the engine and the event is the
			// thing that failed, so argv is what is left to identify the call (§8).
			Engine:      vocab.Engine(opts.registeredFor),
			NativeEvent: opts.event,
			Verdict:     string(verdict.Abstain),
			Enforced:    true,
			Reason:      reason,
			Router:      record.RouterError,
		})
	}
	// An unrecovered panic exits 2, the same block shape as ExitOnError above.
	defer func() {
		if r := recover(); r != nil {
			routerError(fmt.Sprintf("hookyard panicked: %v", r))
		}
	}()

	if opts.parseErr != nil {
		routerError(fmt.Sprintf("parsing flags: %v", opts.parseErr))
		return
	}
	registered, err := vocab.ParseEngine(opts.registeredFor)
	if err != nil {
		routerError(fmt.Sprintf("--registered-for: %v", err))
		return
	}
	env, err := envelope.Decode(in)
	if err != nil {
		routerError(fmt.Sprintf("decoding the payload: %v", err))
		return
	}
	if env.Engine != registered {
		// This entry belongs to another engine's config, which imported it: no
		// handler runs and nothing is printed (§8).
		appendRecord(record.Event{
			Engine:         env.Engine,
			SessionID:      env.SessionID,
			CanonicalEvent: env.CanonicalEvent,
			NativeEvent:    env.NativeEvent,
			CWD:            env.Cwd,
			ToolName:       env.ToolName,
			Verdict:        record.OutcomeSuppressed,
			Enforced:       true,
			Router:         record.RouterOK,
		})
		return
	}
	if stateDir == "" {
		// Joining an empty directory would read ./table.json out of the agent's
		// own working directory, i.e. execute whatever the repo under review
		// happens to ship.
		routerError("no --state-dir and no default state directory")
		return
	}
	table, err := manifest.ReadTable(filepath.Join(stateDir, "table.json"))
	if err != nil {
		routerError(fmt.Sprintf("reading the handler table: %v", err))
		return
	}

	result := router.Run(ctx, router.Select(table, env), env, router.DefaultBudget())
	rendered := verdict.Render(verdict.Input{
		Engine:         env.Engine,
		CanonicalEvent: env.CanonicalEvent,
		NativeEvent:    env.NativeEvent,
		Verdict:        result.Verdict,
		Reason:         result.Reason,
		Advice:         result.Advice,
	})
	// Printed before the record is touched, and that order is load-bearing
	// (§6): the verdict is complete and flushed before any filesystem I/O that
	// could stall. Rendered.Stdout carries no trailing newline of its own.
	if len(rendered.Stdout) > 0 {
		_, _ = fmt.Fprintf(out, "%s\n", rendered.Stdout)
	}
	appendRecord(record.Event{
		Engine:         env.Engine,
		SessionID:      env.SessionID,
		CanonicalEvent: env.CanonicalEvent,
		NativeEvent:    env.NativeEvent,
		CWD:            env.Cwd,
		ToolName:       env.ToolName,
		Verdict:        string(result.Verdict),
		Enforced:       rendered.Enforced,
		Reason:         result.Reason,
		Router:         routerStatus(result),
		Handlers:       handlerOutcomes(result.Handlers, rendered.AdviceDelivered),
	})
}

// resolveStateDir handles a stale config entry emitted by an older hookyard,
// which carries no --state-dir at all. Falling back silently
// would make that failure indistinguishable from health, so the note names it
// in the record. An empty directory means even the fallback found no home.
func resolveStateDir(flagValue string) (dir, note string) {
	if flagValue != "" {
		return flagValue, ""
	}
	dir, err := record.DefaultStateDir()
	if err != nil {
		return "", ""
	}
	return dir, "no --state-dir in this hook entry; fell back to " + dir
}

// joinReason puts the stale-config note after the consolidated reason, and
// that order is the point (§8): if the record's 512 B truncation then costs
// the note, the note is the correct thing to lose.
func joinReason(reason, note string) string {
	switch {
	case note == "":
		return reason
	case reason == "":
		return note
	}
	return reason + "; " + note
}

// routerStatus is §8's router field for a run that reached fan-out. The error
// arm belongs to the paths that never got there.
func routerStatus(result router.Result) string {
	if result.DeadlineExpired {
		return record.RouterTimeout
	}
	return record.RouterOK
}

// handlerOutcomes carries the render's delivery decision onto the advisory
// entries alone: whether advice reached the model is a property of the engine
// and the event (§7), not of the handler that wrote it.
func handlerOutcomes(results []router.HandlerResult, adviceDelivered bool) []record.HandlerOutcome {
	outcomes := make([]record.HandlerOutcome, 0, len(results))
	for _, h := range results {
		o := record.HandlerOutcome{
			Name:    h.ID,
			Outcome: h.Outcome,
			Elapsed: h.Elapsed,
			Advice:  h.Advice,
			Message: h.Message,
		}
		if h.Outcome == record.OutcomeAdvise {
			delivered := adviceDelivered
			o.Delivered = &delivered
		}
		outcomes = append(outcomes, o)
	}
	return outcomes
}

func loadAll(paths []string) ([]manifest.Handler, error) {
	manifests := make([]*manifest.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := manifest.Load(p)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifest.Merge(manifests)
}

func printPlan(plan render.Plan, piSettings string) {
	for _, engine := range vocab.Engines {
		fmt.Printf("%s\n", engine)
		if engine == vocab.ClaudeCode {
			fmt.Println("  (emitted for Nix to place, not written by install)")
		}
		if len(plan[engine]) == 0 {
			fmt.Println("  (nothing)")
			continue
		}
		for _, e := range plan[engine] {
			matcher := e.Matcher
			if matcher == "" {
				matcher = "(every tool)"
			}
			fmt.Printf("  %-22s %-24s %s\n", e.Event, matcher, e.Command)
		}
	}
	// Pi is the one engine whose install writes a second file, and it is the
	// executable half.
	fmt.Printf("pi writes two files\n  %s\n  %s\n", piSettings, render.PiBridgePath(piSettings))
}
