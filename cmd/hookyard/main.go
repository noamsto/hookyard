// Command hookyard registers agent hooks once and routes them to every engine.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/internal/build"
	"github.com/noamsto/hookyard/internal/doctor"
	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/installstate"
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
  hookyard build     generate a native plugin that bundles hookyard (Claude Code only, for now)
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
	case "build":
		err = runBuild(os.Args[2:])
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
	routerPath := fs.String("router-path", "", "absolute path hookyard is invoked by (defaults to <state-dir>/bin/hookyard, a symlink install points at this binary; a different explicit path is used as-is and no link is managed)")
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
	return runInstall(os.Stdout, paths, *routerPath, *stateDir, *codex, *cursor, *pi, *dryRun)
}

// runInstall is install's pipeline, split out so tests can drive it with
// explicit paths instead of os.Args.
func runInstall(out io.Writer, paths manifestPaths, routerPath, stateDir, codex, cursor, pi string, dryRun bool) error {
	router := routerPath
	if router == "" {
		router = filepath.Join(stateDir, "bin", "hookyard")
	}
	if err := checkShellSafe("--router-path", router); err != nil {
		return err
	}
	if err := checkShellSafe("--state-dir", stateDir); err != nil {
		return err
	}
	// True whenever router names hookyard's own state-dir link, whether that
	// came from the empty-flag default above or from a caller (e.g. the Nix
	// home-manager module) passing --router-path explicitly equal to it. A
	// different explicit path (a plain store path, say) is used as-is and no
	// link is managed for it.
	manageRouterLink := filepath.Clean(router) == filepath.Join(stateDir, "bin", "hookyard")

	// Canonicalised the same way Nix writes ${cfg.package}/bin/hookyard, so
	// the identity nix/checks/hm-module.nix compares against matches without
	// a symlink-depth false positive. A failed EvalSymlinks (e.g. the binary
	// isn't a symlink at all) falls back to the raw path rather than aborting
	// the install over a canonicalisation nicety.
	hookyardBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("hookyard: install receipt: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(hookyardBin); err == nil {
		hookyardBin = resolved
	}
	id := installstate.Identity{
		Manifests:   []string(paths),
		RouterPath:  router,
		StateDir:    stateDir,
		CodexConfig: codex,
		CursorHooks: cursor,
		PiSettings:  pi,
		Hookyard:    hookyardBin,
	}
	// Written before anything else touches disk, and again at the very end:
	// if install aborts anywhere in between, doctor finds Complete=false
	// here rather than a stale receipt from the previous generation's run,
	// or (worse) no receipt at all implying nothing has run since.
	if !dryRun {
		if err := record.EnsureStateDir(stateDir); err != nil {
			return err
		}
		if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: false, Identity: id}); err != nil {
			return err
		}
	}

	// loadAll runs after the receipt write, not before: a manifest whose exec
	// fails its stat is one of the two abort paths this feature exists to
	// catch, and loading it earlier would let the install abort with no
	// receipt at all — which doctor could only read as a stale *finalised*
	// receipt from the last successful run, not as "an install started here
	// and did not finish".
	handlers, err := loadAll(paths)
	if err != nil {
		return err
	}
	plan, err := render.BuildPlan(handlers, router, stateDir)
	if err != nil {
		return err
	}
	// install never writes Claude Code's config (R-B): what the Nix overlay
	// routes is the fixed catalog, whatever the handlers above name.
	claudeEntries, err := render.ClaudeCatalogPlan(router, stateDir)
	if err != nil {
		return err
	}
	plan[vocab.ClaudeCode] = claudeEntries

	if dryRun {
		printPlan(out, plan, pi)
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
	// (stateDir itself already exists — the incomplete receipt write above
	// created it.)
	if err := manifest.WriteTable(filepath.Join(stateDir, "table.json"), handlers); err != nil {
		return err
	}
	if manageRouterLink {
		if err := linkRouter(router); err != nil {
			return err
		}
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
	if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: true, Identity: id}); err != nil {
		return err
	}
	for _, engine := range vocab.Engines {
		if engine == vocab.ClaudeCode {
			// install never reaches settings.json, and the catalog registers
			// every event whatever the table holds; a bare "N entries" would
			// read like the other three engines and imply a file was written.
			_, _ = fmt.Fprintf(out, "%-12s %d catalog events routed by the Nix overlay; table holds %d claude-code handlers (not written by install)\n",
				engine, len(plan[engine]), claudeCodeHandlerCount(handlers))
			continue
		}
		_, _ = fmt.Fprintf(out, "%-12s %d entries\n", engine, len(plan[engine]))
	}
	return nil
}

// linkRouter atomically (re)points router at the running binary so every
// engine's emitted command names a link that survives an activation-mode
// flip untouched (issue #61) — nix's os<->home switch, or a plain hookyard
// version bump, both leave this path resolving without any config rewrite.
func linkRouter(router string) error {
	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("hookyard: router symlink: %w", err)
	}
	dir := filepath.Dir(router)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("hookyard: router symlink: %w", err)
	}
	// PID-suffixed so two concurrent installs (e.g. overlapping
	// home-manager switch invocations) never race on the same temp path.
	// Best-effort sweep of any stale sibling first: a PID-suffixed name means
	// a run that crashed between Symlink and Rename leaves a file no later
	// run's own PID will ever match, so nothing else would ever remove it.
	if stale, err := filepath.Glob(router + ".tmp-*"); err == nil {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	tmp := fmt.Sprintf("%s.tmp-%d", router, os.Getpid())
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("hookyard: router symlink: %w", err)
	}
	if err := os.Rename(tmp, router); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hookyard: router symlink: %w", err)
	}
	return nil
}

// claudeCodeHandlerCount counts merged handlers whose Engines names
// claude-code, for install's report line (R-E). It is not plan[vocab.ClaudeCode]'s
// length: that is always the fixed 8-event catalog, not the table's contents.
func claudeCodeHandlerCount(handlers []manifest.Handler) int {
	n := 0
	for _, h := range handlers {
		if slices.Contains(h.Engines, string(vocab.ClaudeCode)) {
			n++
		}
	}
	return n
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
// --settings path, and writes nothing itself. It is engine-scoped
// and claude-code-only: Cursor's, Codex's and Pi's destinations are real files
// with other writers or executable state of their own (§4.2), so a store path
// would lose what they already hold — they stay on install.
func emit(args []string) error {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
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

	doc, err := renderClaudeOverlay(*routerPath, *stateDir, *base)
	if err != nil {
		return err
	}
	// The whole document, one write: a Nix derivation redirects this with
	// `> $out`, so anything printed ahead of a later refusal would be captured
	// as if it were a valid overlay.
	_, err = os.Stdout.Write(doc)
	return err
}

// renderClaudeOverlay renders the fixed Claude Code catalog over base. It is
// split out from emit so a test can compare its bytes directly against
// render.ClaudeSettings without shelling out or capturing stdout.
func renderClaudeOverlay(routerPath, stateDir, base string) ([]byte, error) {
	entries, err := render.ClaudeCatalogPlan(routerPath, stateDir)
	if err != nil {
		return nil, err
	}
	var baseBytes []byte
	if base != "" {
		baseBytes, err = os.ReadFile(base)
		if err != nil {
			return nil, err
		}
	}
	return render.ClaudeSettings(baseBytes, entries)
}

// runBuild is named unlike every other command (not "build"): a package-level
// build would collide with the internal/build import it calls into.
func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	engineFlag := fs.String("engine", "", "engine to build a plugin for; only claude-code is supported")
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	out := fs.String("out", "", "plugin root to write into (must already exist)")
	name := fs.String("name", "", "plugin name, required only when .claude-plugin/plugin.json does not already exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *engineFlag == "" {
		return fmt.Errorf("--engine is required")
	}
	engine, err := vocab.ParseEngine(*engineFlag)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no --manifest given")
	}
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if err := build.Build(engine, build.Options{
		Manifests: paths,
		Out:       *out,
		Name:      *name,
		Binary:    binary,
	}); err != nil {
		return err
	}
	fmt.Printf("built %s plugin at %s\n", engine, *out)
	return nil
}

func validate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	pluginRoot := fs.String("plugin-root", "", "built plugin's root directory; validates manifests in build form against it")
	buildTime := fs.Bool("build-time", false, "validate as a Nix build sandbox would: every static rule, plus the exec runnable rules for execs whose store root is present in the sandbox; an exec the build cannot see is skipped rather than failed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no --manifest given")
	}
	if *buildTime && *pluginRoot != "" {
		return fmt.Errorf("--build-time and --plugin-root are mutually exclusive: plugin form does not stat execs at all, so the two modes have nothing to combine")
	}
	if *pluginRoot != "" {
		return validatePlugin(paths, *pluginRoot)
	}
	load := manifest.Load
	if *buildTime {
		load = manifest.LoadBuildTime
	}
	handlers, err := loadAllWith(paths, load)
	if err != nil {
		return err
	}
	fmt.Printf("%d manifests, %d handlers, no problems found\n", len(paths), len(handlers))
	return nil
}

// validatePlugin is validate's build-mode path: manifests in plugin form,
// checked against root exactly as build would check them at bake time.
func validatePlugin(paths []string, root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	manifests := make([]*manifest.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := manifest.LoadPlugin(p)
		if err != nil {
			return err
		}
		manifests = append(manifests, m)
	}
	handlers, err := manifest.Merge(manifests)
	if err != nil {
		return err
	}
	if err := manifest.CheckPluginExecs(absRoot, handlers); err != nil {
		return err
	}
	fmt.Printf("%d manifests, %d handlers, no problems found\n", len(paths), len(handlers))
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to report on (defaults to the working directory)")
	stateDir := fs.String("state-dir", "", "state directory to report on (defaults to the one recovered from the engines' configs, falling back to record.DefaultStateDir)")
	jsonOutput := fs.Bool("json", false, "print the report as JSON")
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

	findings := doctor.Run(paths, target)
	problems, err := renderDoctor(os.Stdout, target, findings, stdoutIsTerminal(), *jsonOutput)
	if err != nil {
		return err
	}
	if problems == 1 {
		return fmt.Errorf("1 problem found")
	}
	if problems > 1 {
		return fmt.Errorf("%d problems found", problems)
	}
	return nil
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type doctorGroup struct {
	Engine   string        `json:"engine"`
	Verdict  string        `json:"verdict"`
	Problems int           `json:"problems"`
	Checks   []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctorReport struct {
	Engines []doctorGroup `json:"engines"`
}

func renderDoctor(out io.Writer, target string, findings []doctor.Finding, terminal, jsonOutput bool) (int, error) {
	problems := 0
	for _, finding := range findings {
		if finding.Status == doctor.Fail {
			problems++
		}
	}
	if jsonOutput {
		groups := doctorGroups(findings)
		if err := json.NewEncoder(out).Encode(doctorReport{Engines: groups}); err != nil {
			return problems, err
		}
		return problems, nil
	}
	if !terminal {
		if _, err := fmt.Fprintf(out, "hookyard doctor — %s\n\n", target); err != nil {
			return problems, err
		}
		for _, finding := range findings {
			if _, err := fmt.Fprintf(out, "%-12s %-20s %-8s %s\n", finding.Engine, finding.Check, finding.Status, finding.Detail); err != nil {
				return problems, err
			}
		}
		return problems, nil
	}

	groups := doctorGroups(findings)
	for _, group := range groups {
		color, glyph := doctorVerdictStyle(group.Verdict)
		if _, err := fmt.Fprintf(out, "%s%s %s — %s\x1b[0m\n", color, glyph, group.Engine, doctorGroupVerdict(group)); err != nil {
			return problems, err
		}
		for _, check := range group.Checks {
			checkColor, checkGlyph := doctorStatusStyle(check.Status)
			if _, err := fmt.Fprintf(out, "  %s%s %-20s %s\x1b[0m\n", checkColor, checkGlyph, check.Check, doctorDetail(check.Detail)); err != nil {
				return problems, err
			}
			if check.Status == "problem" && check.Fix != "" {
				if _, err := fmt.Fprintf(out, "    fix: %s\n", doctorDetail(check.Fix)); err != nil {
					return problems, err
				}
			}
		}
	}
	var affected []string
	for _, group := range groups {
		if group.Problems > 0 {
			affected = append(affected, group.Engine)
		}
	}
	if len(affected) > 0 {
		if _, err := fmt.Fprintf(out, "\nAffected: %s\n", strings.Join(affected, ", ")); err != nil {
			return problems, err
		}
	}
	return problems, nil
}

func doctorGroupVerdict(group doctorGroup) string {
	if group.Problems > 0 {
		if group.Problems == 1 {
			return "1 problem"
		}
		return fmt.Sprintf("%d problems", group.Problems)
	}
	if group.Verdict == "unknown" {
		return "unknown"
	}
	return "all ok"
}

func doctorGroups(findings []doctor.Finding) []doctorGroup {
	groups := make([]doctorGroup, 0)
	indices := make(map[string]int)
	for _, finding := range findings {
		engine := string(finding.Engine)
		if engine == "" {
			engine = "hookyard"
		}
		index, ok := indices[engine]
		if !ok {
			indices[engine] = len(groups)
			groups = append(groups, doctorGroup{Engine: engine})
			index = len(groups) - 1
		}
		status := doctorStatus(finding.Status)
		check := doctorCheck{Check: finding.Check, Status: status, Detail: finding.Detail, Fix: finding.Fix}
		groups[index].Checks = append(groups[index].Checks, check)
		if status == "problem" {
			groups[index].Problems++
		}
	}
	for i := range groups {
		verdict := "ok"
		if groups[i].Problems > 0 {
			verdict = "problem"
		} else {
			for _, check := range groups[i].Checks {
				if check.Status == "unknown" {
					verdict = "unknown"
					break
				}
			}
		}
		groups[i].Verdict = verdict
	}
	return groups
}

func doctorStatus(status doctor.Status) string {
	switch status {
	case doctor.Pass:
		return "ok"
	case doctor.Fail:
		return "problem"
	default:
		return "unknown"
	}
}

func doctorVerdictStyle(verdict string) (string, string) {
	switch verdict {
	case "problem":
		return "\x1b[31m", "✗"
	case "unknown":
		return "\x1b[33m", "?"
	default:
		return "\x1b[32m", "✓"
	}
}

func doctorStatusStyle(status string) (string, string) {
	return doctorVerdictStyle(status)
}

func doctorDetail(detail string) string {
	const width = 88
	const continuationIndent = 17
	const segmentWidth = width - continuationIndent
	if len(detail) <= width {
		return detail
	}
	parts := strings.Split(detail, ",")
	if len(parts) == 1 {
		return detail[:width-1] + "…"
	}
	var lines []string
	line := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= segmentWidth {
			part = part[:segmentWidth-1] + "…"
		}
		candidate := part
		if line != "" {
			candidate = line + ", " + part
		}
		if len(candidate) > width && line != "" {
			lines = append(lines, line+",")
			line = part
		} else {
			line = candidate
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n                 ")
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
	pluginRoot    string
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
	pluginRoot := fs.String("plugin-root", "", "built plugin's root directory (build mode; mutually exclusive with --state-dir)")
	parseErr := fs.Parse(args)

	runRoute(context.Background(), routeOptions{
		start:         start,
		registeredFor: *registeredFor,
		event:         *event,
		stateDir:      *stateDir,
		pluginRoot:    *pluginRoot,
		parseErr:      parseErr,
	}, os.Stdin, os.Stdout)
	return nil
}

// runRoute is route's pipeline (§4), split out so tests can drive §5's
// failure paths with an explicit stdin and stdout instead of the process's own.
func runRoute(ctx context.Context, opts routeOptions, in io.Reader, out io.Writer) {
	var stateDir, fallbackNote, registeredForNote string
	if opts.pluginRoot != "" {
		stateDir = existingDefaultStateDir()
	} else {
		stateDir, fallbackNote = resolveStateDir(opts.stateDir)
	}

	// appendRecord ends every path. The error is swallowed because by the time
	// this runs the verdict is already on stdout, and a failed append must
	// never change what the engine read (§6).
	appendRecord := func(e record.Event) {
		if stateDir == "" {
			return
		}
		e.Reason = joinReason(joinReason(e.Reason, registeredForNote), fallbackNote)
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
	if opts.pluginRoot != "" && opts.stateDir != "" {
		routerError("--plugin-root and --state-dir are mutually exclusive")
		return
	}
	if opts.pluginRoot != "" && !filepath.IsAbs(opts.pluginRoot) {
		routerError(fmt.Sprintf("--plugin-root %q must be an absolute path", opts.pluginRoot))
		return
	}
	registered, err := vocab.ParseEngine(opts.registeredFor)
	if err != nil {
		routerError(fmt.Sprintf("--registered-for: %v", err))
		return
	}
	raw, err := envelope.ReadPayload(in)
	if err != nil {
		routerError(fmt.Sprintf("decoding the payload: %v", err))
		return
	}
	env, err := envelope.Decode(bytes.NewReader(raw))
	// Claude Code omits prompt_id and effort on some events (SessionStart), so
	// Detect cannot place them (§12). Trusting --registered-for is safe only in
	// yard mode: the Claude overlay is the only yard-mode surface registered for
	// claude-code and only Claude Code reads it, whereas a build-mode plugin can
	// be loaded by another engine. The exact catalog name keeps a look-alike
	// payload from riding the fallback.
	if errors.Is(err, envelope.ErrUnknownEngine) && opts.pluginRoot == "" && registered == vocab.ClaudeCode {
		if fallback, fallbackErr := envelope.DecodeAs(raw, vocab.ClaudeCode); fallbackErr == nil && vocab.IsClaudeCodeEvent(fallback.NativeEvent) {
			env, err = fallback, nil
			registeredForNote = "engine taken from --registered-for claude-code: payload carried no engine discriminator"
		}
	}
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
	var table []manifest.Handler
	if opts.pluginRoot != "" {
		table, err = manifest.ReadPluginTable(filepath.Join(opts.pluginRoot, manifest.PluginTablePath))
	} else if stateDir == "" {
		// Joining an empty directory would read ./table.json out of the agent's
		// own working directory, i.e. execute whatever the repo under review
		// happens to ship.
		routerError("no --state-dir and no default state directory")
		return
	} else {
		table, err = manifest.ReadTable(filepath.Join(stateDir, "table.json"))
	}
	if err != nil {
		routerError(fmt.Sprintf("reading the handler table: %v", err))
		return
	}

	selected := router.Select(table, env)
	var result router.Result
	if opts.pluginRoot != "" {
		runnable, failed := resolvePluginHandlers(opts.pluginRoot, selected)
		result = router.Run(ctx, runnable, env, router.DefaultBudget())
		result.Handlers = mergeInOrder(selected, result.Handlers, failed)
	} else {
		result = router.Run(ctx, selected, env, router.DefaultBudget())
	}
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

// existingDefaultStateDir is build mode's record rule (§3.1): a plugin appends only to a
// state directory that already exists, and never creates one on an end user's machine.
func existingDefaultStateDir() string {
	dir, err := record.DefaultStateDir()
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// resolvePluginHandlers rewrites each selected handler's plugin-relative exec to its real
// path. A handler that does not resolve inside the root is never exec'd; it becomes its
// own error outcome so the rest of the plugin's handlers still run (§3.1, §5).
func resolvePluginHandlers(root string, selected []manifest.Handler) (runnable []manifest.Handler, failed map[string]router.HandlerResult) {
	failed = map[string]router.HandlerResult{}
	for _, h := range selected {
		target, err := manifest.ResolvePluginExec(root, h.Exec)
		if err != nil {
			failed[h.ID] = router.HandlerResult{
				ID:      h.ID,
				Outcome: record.OutcomeError,
				Verdict: verdict.Abstain,
				Message: err.Error(),
			}
			continue
		}
		h.Exec = target
		runnable = append(runnable, h)
	}
	return runnable, failed
}

// mergeInOrder restores the table order Select produced over Run's results, which cover
// only the handlers that were runnable; failed supplies the rest by ID. Run preserves the
// input order of the handlers it was given, so ran is walked in lockstep.
func mergeInOrder(selected []manifest.Handler, ran []router.HandlerResult, failed map[string]router.HandlerResult) []router.HandlerResult {
	merged := make([]router.HandlerResult, 0, len(selected))
	next := 0
	for _, h := range selected {
		if f, ok := failed[h.ID]; ok {
			merged = append(merged, f)
			continue
		}
		merged = append(merged, ran[next])
		next++
	}
	return merged
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
	return loadAllWith(paths, manifest.Load)
}

func loadAllWith(paths []string, load func(string) (*manifest.Manifest, error)) ([]manifest.Handler, error) {
	manifests := make([]*manifest.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := load(p)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifest.Merge(manifests)
}

func printPlan(out io.Writer, plan render.Plan, piSettings string) {
	for _, engine := range vocab.Engines {
		_, _ = fmt.Fprintf(out, "%s\n", engine)
		if engine == vocab.ClaudeCode {
			_, _ = fmt.Fprintln(out, "  (emitted for Nix to place, not written by install)")
		}
		if len(plan[engine]) == 0 {
			_, _ = fmt.Fprintln(out, "  (nothing)")
			continue
		}
		for _, e := range plan[engine] {
			matcher := e.Matcher
			if matcher == "" {
				matcher = "(every tool)"
			}
			_, _ = fmt.Fprintf(out, "  %-22s %-24s %s\n", e.Event, matcher, e.Command)
		}
	}
	// Pi is the one engine whose install writes a second file, and it is the
	// executable half.
	_, _ = fmt.Fprintf(out, "pi writes two files\n  %s\n  %s\n", piSettings, render.PiBridgePath(piSettings))
}
