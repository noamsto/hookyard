// Package doctor answers the question hookyard's own event record cannot: is
// this machine actually going to run the handlers it looks like it registered?
//
// Every engine skips hooks entirely in a directory the user has not trusted,
// and a handler that was never invoked cannot abstain, error or time out — so a
// silently disabled guard is indistinguishable, from inside the record, from a
// session where nothing dangerous was attempted (§8).
package doctor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/noamsto/hookyard/internal/installstate"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

// Status is deliberately three-valued: hookyard can read some of what each
// engine decides but not all of it, and reporting an unknown as a pass would
// be worse than reporting it as unknown.
type Status int

const (
	Unknown Status = iota
	Pass
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "ok"
	case Fail:
		return "PROBLEM"
	default:
		return "unknown"
	}
}

type Finding struct {
	Engine vocab.Engine
	Check  string
	Status Status
	Detail string
}

// Paths locates each engine's configuration. Tests set it; the CLI derives it
// from the environment.
type Paths struct {
	ClaudeConfigDir string
	CodexHome       string
	CursorHome      string
	PiAgentDir      string
	// ClaudeSettingsFlags are the --settings values the claude launcher on
	// PATH passes. Under Nix that overlay, not settings.json, is where
	// hookyard's block lives, and nothing else on the machine names it (§4.4).
	// Empty means doctor cannot see which settings file the engine starts
	// with. Tests set it directly.
	ClaudeSettingsFlags []string
	// ClaudeLauncherUnread says why ClaudeSettingsFlags came back empty, and
	// is itself empty when it did not. Without it every launcher failure — no
	// claude on PATH, an unresolvable or unreadable script, a compiled binary,
	// a wrapper passing no --settings — renders as the same Unknown.
	ClaudeLauncherUnread string
	// StateDir is the operator's explicit --state-dir. Empty means none was
	// given: Run recovers it from the --state-dir the four engines' emitted
	// configs already name, rather than treating empty as shorthand for
	// record.DefaultStateDir().
	StateDir string
	// GenerationWitness is the Nix-placed file describing what the current
	// home-manager generation expects `hookyard install` to have produced.
	// Tests set it directly; DefaultPaths derives it from the environment.
	GenerationWitness string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	// PI_CODING_AGENT_DIR replaces ~/.pi/agent rather than ~/.pi (verified
	// against a live pi, and mirrored in cmd/hookyard's defaultTargets), so
	// doctor and install agree on where Pi's config lives.
	piAgentDir := os.Getenv("PI_CODING_AGENT_DIR")
	if piAgentDir == "" {
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}
	settingsFlags, launcherUnread := claudeLauncherSettings()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	return Paths{
		ClaudeConfigDir:      claude,
		CodexHome:            codex,
		CursorHome:           filepath.Join(home, ".cursor"),
		PiAgentDir:           piAgentDir,
		ClaudeSettingsFlags:  settingsFlags,
		ClaudeLauncherUnread: launcherUnread,
		GenerationWitness:    filepath.Join(configHome, "hookyard", "generation.json"),
	}, nil
}

// Run reports on each engine for one working directory.
func Run(p Paths, dir string) []Finding {
	claude := resolveClaudeSources(p, dir)

	// stateDir is resolved before the per-engine findings rather than after,
	// so the competing-writer check can read the same handler table the router
	// would: finding one consumers' leaked native rows turns on knowing which
	// scripts hookyard owns, and that lives in stateDir/table.json.
	stateDir := p.StateDir
	var disagreement []string
	if stateDir == "" {
		switch recovered := recoverStateDir(p, claude); len(recovered) {
		case 1:
			stateDir = recovered[0]
		case 0:
			// Run cannot propagate DefaultStateDir's error; falling through to
			// an empty directory leaves streamFindings to report Unknown,
			// which is already its answer for a missing stream.
			stateDir, _ = record.DefaultStateDir()
		default:
			disagreement = recovered
		}
	}
	var findings []Finding
	findings = append(findings, claudeFindings(p, dir, claude)...)
	findings = append(findings, codexFindings(p, dir)...)
	findings = append(findings, cursorFindings(p, dir, stateDir)...)
	findings = append(findings, generationFindings(p, stateDir)...)
	findings = append(findings, piFindings(p, stateDir)...)
	findings = append(findings, streamFindings(stateDir, disagreement, time.Now())...)
	return findings
}

// tableHandlers reads the handler table the router answers each event from,
// returning the error rather than collapsing it: a missing or corrupt table is
// a different answer from a genuinely empty one, and competingWriter reports
// them differently. An empty stateDir must not join "table.json" onto it — that
// resolves to ./table.json in the directory doctor was run from, and a file a
// repo under review happens to ship would silently stand in for the real
// table.
func tableHandlers(stateDir string) ([]manifest.Handler, error) {
	if stateDir == "" {
		return nil, nil
	}
	return manifest.ReadTable(filepath.Join(stateDir, "table.json"))
}

// claudeSource is one settings source doctor can read for Claude Code, with
// the bytes it was read from: the raw text is what the marker, router-path and
// --state-dir scans all run against.
type claudeSource struct {
	name string
	raw  []byte
}

// claudeSources is every settings source doctor can reach for Claude Code.
// settings.json is kept apart from the launcher's overlays because which of
// the two carries the marker is the whole of §4.4's registration finding: in
// the overlay it is the live Nix-placed registration, in settings.json it is a
// stale one from a hookyard that still wrote that file.
type claudeSources struct {
	settingsPath string
	settings     []byte // nil when settings.json could not be read
	overlays     []claudeSource
	// checkout holds the working directory's own hook-bearing settings files:
	// `.claude/settings.json` (projectSettings) and `.claude/settings.local.json`
	// (localSettings). Claude Code's disableAllHooksInCheckout gate ORs both
	// together (§12), so claudeHooksEnabled scans these too. They are kept out
	// of all(), which registration and the router-path scan use: those are
	// about where hookyard itself could have written a marker, and hookyard
	// never writes to a checkout's own settings files.
	checkout []claudeSource
	// unresolved says what doctor could not see, so a Pass on the
	// disableAllHooks gate names what it did not read rather than implying it
	// read everything (§4.4). Three different shapes land here and the label
	// below has to hold all of them: a source that exists and would not open,
	// the launcher reasons, which say there is no source to open at all, and a
	// checkout source that exists and would not open. Rendering any of these
	// under "not read" would send an operator hunting for a permissions
	// problem on a file that was never named.
	unresolved []string
}

func resolveClaudeSources(p Paths, dir string) claudeSources {
	c := claudeSources{settingsPath: filepath.Join(p.ClaudeConfigDir, "settings.json")}
	// A missing settings.json is the expected state now that hookyard does not
	// write it; only a file that is there and unreadable is worth naming.
	if raw, err := os.ReadFile(c.settingsPath); err == nil {
		c.settings = raw
	} else if !os.IsNotExist(err) {
		c.unresolved = append(c.unresolved, fmt.Sprintf("%s (%v)", c.settingsPath, err))
	}
	if p.ClaudeLauncherUnread != "" {
		c.unresolved = append(c.unresolved, p.ClaudeLauncherUnread)
	}

	for _, v := range p.ClaudeSettingsFlags {
		if raw, err := os.ReadFile(v); err == nil {
			c.overlays = append(c.overlays, claudeSource{name: v, raw: raw})
			continue
		}
		// claude --help declares --settings <file-or-json>, so a value that is
		// not a readable file may be the settings document itself.
		if json.Valid([]byte(v)) {
			c.overlays = append(c.overlays, claudeSource{name: "inline --settings JSON", raw: []byte(v)})
			continue
		}
		c.unresolved = append(c.unresolved, v)
	}

	// A missing project or local settings.json is the common case and not
	// worth naming; only a file that is there and unreadable is.
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(dir, ".claude", name)
		if raw, err := os.ReadFile(path); err == nil {
			c.checkout = append(c.checkout, claudeSource{name: path, raw: raw})
		} else if !os.IsNotExist(err) {
			c.unresolved = append(c.unresolved, fmt.Sprintf("%s (%v)", path, err))
		}
	}
	return c
}

func (c claudeSources) all() []claudeSource {
	var out []claudeSource
	if c.settings != nil {
		out = append(out, claudeSource{name: c.settingsPath, raw: c.settings})
	}
	return append(out, c.overlays...)
}

// markerSource returns the source whose paths a session will actually run.
// The overlay outranks settings.json here, the reverse of claudeRegistration's
// arms: a marker in settings.json is the stale copy, and reporting the stale
// row's router path and --state-dir would describe a registration that is no
// longer the live one.
func (c claudeSources) markerSource() (claudeSource, bool) {
	for _, o := range c.overlays {
		if bytes.Contains(o.raw, []byte(render.Marker)) {
			return o, true
		}
	}
	if bytes.Contains(c.settings, []byte(render.Marker)) {
		return claudeSource{name: c.settingsPath, raw: c.settings}, true
	}
	return claudeSource{}, false
}

func claudeFindings(p Paths, dir string, c claudeSources) []Finding {
	state := claudeStatePath(p.ClaudeConfigDir)

	trust := Finding{Engine: vocab.ClaudeCode, Check: "workspace trust", Detail: state}
	var claudeState struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := readJSON(state, &claudeState); os.IsNotExist(err) {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("no trust record at %s, so hooks are skipped entirely", state)
	} else if err != nil {
		trust.Detail = fmt.Sprintf("cannot read %s: %v", state, err)
	} else if claudeState.Projects[dir].HasTrustDialogAccepted {
		trust.Status = Pass
		trust.Detail = "trusted in " + state
	} else {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("%s is not trusted in %s, so hooks are skipped entirely", dir, state)
	}

	return []Finding{trust, claudeHooksEnabled(c), claudeRegistration(c), claudeRouterPath(c)}
}

// claudeHooksEnabled reads disableAllHooks in every source, not just
// settings.json: §8's gate fires on the flag being set in user or flag
// settings, and §12's disableAllHooksInCheckout gate fires on the same flag
// in the checkout's own project or local settings, so checking only
// settings.json and the overlay and calling the result a Pass is a
// fail-open now that doctor holds all four sources' bytes.
func claudeHooksEnabled(c claudeSources) Finding {
	f := Finding{Engine: vocab.ClaudeCode, Check: "hooks enabled", Detail: c.settingsPath}

	var checked []string
	blind := c.unresolved
	for _, s := range append(c.all(), c.checkout...) {
		var settings struct {
			DisableAllHooks bool `json:"disableAllHooks"`
		}
		if err := json.Unmarshal(s.raw, &settings); err != nil {
			blind = append(blind, fmt.Sprintf("%s (%v)", s.name, err))
			continue
		}
		if settings.DisableAllHooks {
			f.Status = Fail
			f.Detail = "disableAllHooks is set in " + s.name
			return f
		}
		checked = append(checked, s.name)
	}

	if len(checked) == 0 {
		f.Status = Unknown
		f.Detail = "no Claude settings source hookyard can read"
	} else {
		f.Status = Pass
		f.Detail = "not disabled in " + strings.Join(checked, ", ")
	}
	if len(blind) > 0 {
		f.Detail += " (" + strings.Join(blind, ", ") + ")"
	}
	return f
}

// claudeRegistration is Claude's own arm rather than registration()'s: the
// shared one greps a single config file and tells the reader to run hookyard
// install, and under emit both halves are wrong — the block is not in
// settings.json, and no install can put it there (§4.4).
func claudeRegistration(c claudeSources) Finding {
	f := Finding{Engine: vocab.ClaudeCode, Check: "hookyard registered", Detail: c.settingsPath}

	// A marker in settings.json outranks one in the overlay instead of being
	// masked by it: §8 unions hooks across sources, so registered in both
	// means every handler runs twice.
	if bytes.Contains(c.settings, []byte(render.Marker)) {
		f.Status = Fail
		f.Detail = "stale hookyard entry in " + c.settingsPath +
			"; remove it — hookyard no longer writes that file, and an entry in both it and the --settings overlay runs every handler twice"
		return f
	}

	var names []string
	for _, o := range c.overlays {
		if bytes.Contains(o.raw, []byte(render.Marker)) {
			f.Status = Pass
			// The narrow claim §4.3b requires: --settings is last-wins, so a
			// caller passing its own displaces this one, and doctor reads the
			// launcher rather than any given invocation.
			f.Detail = "the claude launcher on PATH passes --settings " + o.name +
				", the Nix-placed overlay, and it carries hookyard's block; a caller passing its own --settings displaces it, which doctor cannot see"
			return f
		}
		names = append(names, o.name)
	}
	if len(names) > 0 {
		f.Status = Fail
		f.Detail = "no hookyard entry in the --settings overlay the claude launcher passes (" + strings.Join(names, ", ") +
			"); wire programs.hookyard.claudeOverlay.merged into it and rebuild"
		return f
	}

	f.Status = Unknown
	f.Detail = "hookyard cannot see which settings file claude is started with"
	if len(c.unresolved) > 0 {
		f.Detail += " (" + strings.Join(c.unresolved, ", ") + ")"
	}
	return f
}

func claudeRouterPath(c claudeSources) Finding {
	src, ok := c.markerSource()
	if !ok {
		// claudeRegistration above already reports a missing hookyard entry;
		// a second Fail here for the same cause would be duplicate noise.
		return Finding{
			Engine: vocab.ClaudeCode,
			Check:  "router path",
			Status: Unknown,
			Detail: "no hookyard entry in any Claude settings source to check",
		}
	}
	return routerPathIn(vocab.ClaudeCode, src.name, src.raw)
}

// Claude Code keeps per-directory trust in .claude.json, one entry per
// project. It sits inside the config dir when CLAUDE_CONFIG_DIR is set and
// beside it otherwise.
func claudeStatePath(configDir string) string {
	inside := filepath.Join(configDir, ".claude.json")
	if _, err := os.Stat(inside); err == nil {
		return inside
	}
	return filepath.Join(filepath.Dir(configDir), ".claude.json")
}

func codexFindings(p Paths, dir string) []Finding {
	config := filepath.Join(p.CodexHome, "config.toml")

	var codexConfig struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
		Hooks map[string]any `toml:"hooks"`
	}
	trust := Finding{Engine: vocab.Codex, Check: "workspace trust", Detail: config}
	hookTrust := Finding{Engine: vocab.Codex, Check: "hook trust", Detail: config}

	if _, err := toml.DecodeFile(config, &codexConfig); err != nil {
		trust.Detail = fmt.Sprintf("%s: %v", config, err)
		hookTrust.Detail = trust.Detail
	} else {
		if codexConfig.Projects[dir].TrustLevel == "trusted" {
			trust.Status = Pass
			trust.Detail = "trusted in " + config
		} else {
			trust.Status = Fail
			trust.Detail = fmt.Sprintf("%s is not trusted in %s, so hooks are not loaded", dir, config)
		}
		// Codex reviews each hook entry separately from trusting the directory,
		// and an untrusted entry is simply not run.
		if state, ok := codexConfig.Hooks["state"].(map[string]any); ok && len(state) > 0 {
			hookTrust.Status = Pass
			hookTrust.Detail = fmt.Sprintf("%d reviewed hook entries in %s", len(state), config)
		} else {
			hookTrust.Status = Fail
			hookTrust.Detail = "no reviewed hook entries in " + config + "; run codex and accept /hooks"
		}
	}

	return []Finding{trust, hookTrust, codexRegistration(config), routerPath(vocab.Codex, config)}
}

// codexEventPattern recovers the --event argument from a hookyard router
// command. Duplication is the *same* event registered more than once: BuildPlan
// emits one entry per (engine, event), so a healthy install spanning several
// Codex events legitimately carries several commands and must not read as
// duplication.
var codexEventPattern = regexp.MustCompile(`route --registered-for codex --event ([^\s"']+)`)

func codexRegistration(path string) Finding {
	f := Finding{Engine: vocab.Codex, Check: "hookyard registered", Detail: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		f.Status = Fail
		f.Detail = fmt.Sprintf("%s: %v", path, err)
		return f
	}
	body := string(raw)
	total := strings.Count(body, "route --registered-for codex")
	if total == 0 {
		f.Status = Fail
		f.Detail = "no hookyard entry in " + path + "; run hookyard install"
		return f
	}
	perEvent := map[string]int{}
	matches := codexEventPattern.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		perEvent[m[1]]++
	}
	// A command whose event cannot be recovered still counts, grouped under one
	// key, so a repeated unparsable command reads as duplication rather than
	// passing on a technicality.
	if missing := total - len(matches); missing > 0 {
		perEvent[""] += missing
	}
	for _, n := range perEvent {
		if n > 1 {
			f.Status = Fail
			f.Detail = fmt.Sprintf("%d hookyard entries in %s; run hookyard install", total, path)
			return f
		}
	}
	f.Status = Pass
	noun := "entry"
	if total != 1 {
		noun = "entries"
	}
	f.Detail = fmt.Sprintf("%d hookyard %s in %s", total, noun, path)
	return f
}

func cursorFindings(p Paths, dir, stateDir string) []Finding {
	hooks := filepath.Join(p.CursorHome, "hooks.json")
	marker := filepath.Join(p.CursorHome, "projects", cursorProjectSlug(dir), ".workspace-trusted")

	trust := Finding{Engine: vocab.Cursor, Check: "workspace trust", Detail: marker}
	if _, err := os.Stat(marker); err == nil {
		trust.Status = Pass
		trust.Detail = "trusted, per " + marker
	} else {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("no trust marker at %s, so hooks are skipped entirely", marker)
	}

	return []Finding{trust, registration(vocab.Cursor, hooks), routerPath(vocab.Cursor, hooks), competingWriter(stateDir, hooks)}
}

// competingWriter reports a handler hookyard owns that a foreign writer also
// registered in Cursor's hooks.json — the double-fire shape a consumer's
// migration reaches if its old native rows do not leave in the same commit its
// manifest path enters (§8's same-commit swap). It is the one shared file aeye
// writes that hookyard also writes, which is why the check is Cursor-only; the
// Codex plugin-cache tree and the Claude --plugin-dir tree are loaded from
// directories hookyard does not write, so a swap that forgets the plugin half
// has no file to catch it here — that leak is bounded by the swap discipline,
// not by detection.
//
// Match is by script basename, not full path, and that is deliberately a
// two-sided heuristic: two writers may spell the same script under different
// store or profile paths (a full-path match would miss the double this check
// exists to catch), while an unrelated foreign script sharing a handler's
// basename reads as a double when none exists (a spurious Fail). Both costs are
// named rather than hidden — a wrapper indirection (dispatcher's
// dispatcher-cursor-notify around dispatch-notify.sh) escapes the check on the
// false-negative side, and a name collision with another vendor's script
// over-reports on the false-positive side — so neither a Pass nor a Fail is
// read as exhaustive or exact.
func competingWriter(stateDir, hooksPath string) Finding {
	f := Finding{Engine: vocab.Cursor, Check: "competing writer", Detail: hooksPath}
	if stateDir == "" {
		f.Status = Unknown
		f.Detail = "no --state-dir recoverable to read the handler table from"
		return f
	}
	handlers, err := tableHandlers(stateDir)
	if err != nil {
		// A missing or corrupt table is not the same as an empty one: hookyard
		// may own handlers the check cannot see, so reporting Pass here would
		// turn a broken install into a green "nothing is double-registered".
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read the handler table at %s: %v", filepath.Join(stateDir, "table.json"), err)
		return f
	}
	if len(handlers) == 0 {
		f.Status = Pass
		f.Detail = "no handlers in the table, so no foreign entry can double-register one"
		return f
	}
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", hooksPath, err)
		return f
	}
	if !strings.Contains(string(raw), render.Marker) {
		// Hookyard is not registered here at all; registration() already
		// reports that as Fail, so adding a second finding would be noise, and
		// a foreign row naming one of hookyard's scripts is not a double until
		// hookyard is also registered beside it.
		f.Status = Unknown
		f.Detail = "no hookyard entry in " + hooksPath + " to compete against"
		return f
	}

	var doc struct {
		Hooks map[string][]struct {
			Command json.RawMessage `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot parse %s: %v", hooksPath, err)
		return f
	}

	// Read every foreign row once, keyed by the script basename it references,
	// so the per-handler loop below is a single lookup rather than a repeated
	// scan. Map iteration is harmless here because the result is accumulated
	// in handler order, which the table's sort fixes.
	foreign := map[string][]string{}
	for _, rows := range doc.Hooks {
		for _, row := range rows {
			var command string
			if err := json.Unmarshal(row.Command, &command); err != nil {
				continue // a foreign row whose command is not a string
			}
			if strings.Contains(command, render.Marker) {
				continue // hookyard's own row, never a double against itself
			}
			name := filepath.Base(commandPath(command))
			if name == "" || name == "." || name == "/" {
				continue
			}
			foreign[name] = append(foreign[name], command)
		}
	}

	var clashes []string
	for _, h := range handlers {
		name := filepath.Base(h.Exec)
		commands, ok := foreign[name]
		if !ok {
			continue
		}
		sort.Strings(commands)
		clashes = append(clashes, fmt.Sprintf("%s (%s) is also registered by a foreign entry: %s", h.ID, h.Exec, strings.Join(commands, "; ")))
	}
	if len(clashes) > 0 {
		f.Status = Fail
		f.Detail = strings.Join(clashes, " | ")
		return f
	}
	f.Status = Pass
	f.Detail = "no foreign entry names a handler hookyard owns"
	return f
}

// commandPath returns the leading token of a hook command as a candidate path,
// which is the part of the command string that can carry a script basename.
// A command like `hookyard route --registered-for …` is hookyard's own and has
// already been excluded; a foreign command is a bare path (`…/scripts/images.sh`)
// or a path followed by args, so the first whitespace-delimited token is the
// script. Env-var or substitution prefixes ($ADAPTER_DIR/…) are not resolved
// here — the basename is taken from the token as-is, which is the heuristic the
// function's comment names.
func commandPath(command string) string {
	if fields := strings.Fields(command); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// cursorProjectSlug mirrors how Cursor names a project directory under
// ~/.cursor/projects: path separators become dashes, runs collapse, and the
// edges are trimmed.
var slugSeparators = regexp.MustCompile(`[^A-Za-z0-9]+`)

func cursorProjectSlug(dir string) string {
	return strings.Trim(slugSeparators.ReplaceAllString(dir, "-"), "-")
}

// piFindings has no per-directory trust check, unlike the other three: Pi's
// gate (~/.pi/agent/trust.json + defaultProjectTrust) covers only
// project-local .pi/ resources, and hookyard registers a global extension,
// which that gate does not touch at all. Reporting that is the finding.
func piFindings(p Paths, stateDir string) []Finding {
	settings := filepath.Join(p.PiAgentDir, "settings.json")
	bridge := render.PiBridgePath(settings)
	trustFile := filepath.Join(p.PiAgentDir, "trust.json")

	trust := Finding{
		Engine: vocab.Pi,
		Check:  "workspace trust",
		Status: Pass,
		Detail: fmt.Sprintf(
			"%s is a global extension; Pi's project trust gate (%s) does not gate global extensions, so it runs regardless of trust state",
			bridge, trustFile,
		),
	}

	return []Finding{
		trust,
		danglingExtensions(settings),
		registration(vocab.Pi, settings),
		// The router path lives in the bridge, not settings.json: settings.json
		// only names the bridge file, and routerPathPattern run against it would
		// match the bridge path itself truncated at "hookyard" — reporting a
		// router that does not exist (routerpath_test.go documents the trap).
		routerPath(vocab.Pi, bridge),
		piLauncherFindings(),
		piDoubleFire(stateDir, settings),
		piBridgeDrift(stateDir, bridge),
	}
}

// piBuildPackageRoot resolves a settings.json extensions[] entry to the
// build-mode package root piDoubleFire compares against the yard table, per
// the layout build-pi writes ("Pi build package on disk" in the
// decomposition): pi loads a directory entry by package rules, so the entry
// is the root itself; a hookyard-built package's extension instead sits two
// levels under its root, at <root>/extensions/hookyard.ts, so a file entry's
// grandparent is the root. An entry doctor cannot stat — missing or
// otherwise — yields "": danglingExtensions already reports a missing one,
// and geometry cannot be derived for a path that is not there.
func piBuildPackageRoot(entry string) string {
	info, err := os.Stat(entry)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return entry
	}
	return filepath.Dir(filepath.Dir(entry))
}

// piDoubleFire detects a double registration: pi tolerates `hookyard install`
// (yard mode) and `pi install <built package>` naming the same handler id,
// running it twice per event with no warning of its own — the shape a
// consumer reaches when a migration to a build-mode package leaves the yard
// registration in place instead of retiring it in the same commit. It walks
// settings.json's extensions[] for a build-mode package root
// (piBuildPackageRoot) and compares that package's baked table against the
// yard table this install's stateDir names. piLauncherFindings detects a
// different hazard — an injection hookyard cannot strip — and is not
// replaced by this check.
func piDoubleFire(stateDir, settingsPath string) Finding {
	f := Finding{Engine: vocab.Pi, Check: "double-registered handlers", Detail: settingsPath}

	if stateDir == "" {
		f.Status = Unknown
		f.Detail = "no --state-dir recoverable to read the handler table from"
		return f
	}
	yardHandlers, err := tableHandlers(stateDir)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read the handler table at %s: %v", filepath.Join(stateDir, "table.json"), err)
		return f
	}
	yardPath := filepath.Join(stateDir, "table.json")
	yardIDs := make(map[string]bool, len(yardHandlers))
	for _, h := range yardHandlers {
		yardIDs[h.ID] = true
	}

	var settings struct {
		Extensions []string `json:"extensions"`
	}
	if err := readJSON(settingsPath, &settings); err != nil {
		if os.IsNotExist(err) {
			// registration() above already reports a missing settings.json as
			// Fail; a second Fail here for the same cause would be duplicate
			// noise.
			f.Status = Unknown
			f.Detail = "no " + settingsPath + " to check"
			return f
		}
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", settingsPath, err)
		return f
	}

	// found distinguishes "no build-mode package alongside settings.json"
	// from "one or more exist and none collide" — both Pass, but only the
	// latter means a package was actually read and compared.
	var found bool
	var collisions, unreadable []string
	for _, e := range settings.Extensions {
		root := piBuildPackageRoot(e)
		if root == "" {
			continue
		}
		tablePath := filepath.Join(root, manifest.PluginTablePath)
		handlers, err := manifest.ReadPluginTable(tablePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue // root does not qualify as a build package; not an error
			}
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", tablePath, err))
			continue
		}
		found = true
		for _, h := range handlers {
			if yardIDs[h.ID] {
				collisions = append(collisions, fmt.Sprintf("%s is registered in both %s and %s", h.ID, yardPath, tablePath))
			}
		}
	}

	// Fail outranks Unknown, mirroring danglingExtensions' own precedence: a
	// confirmed double-fire is the more actionable answer, and reporting
	// Unknown instead just because some other package's table also happened
	// to be unreadable would bury a real collision behind a shrug.
	if len(collisions) > 0 {
		sort.Strings(collisions)
		f.Status = Fail
		f.Detail = strings.Join(collisions, " | ")
		return f
	}
	if len(unreadable) > 0 {
		f.Status = Unknown
		f.Detail = "cannot tell whether every build-mode package is disjoint from the yard table: " + strings.Join(unreadable, ", ")
		return f
	}
	if !found {
		f.Status = Pass
		f.Detail = "no build-mode pi package found alongside " + settingsPath
		return f
	}
	f.Status = Pass
	f.Detail = "no handler id is registered in both the yard table and a build-mode pi package"
	return f
}

// piBridgeDrift catches a stale bridge: the installed bridge is a rendering
// of the handler table taken at install time, and nothing re-renders it when the table
// changes underneath it, so a handler can be added or retired in the table
// while the bridge keeps firing (or stops firing) the stale set — a
// registered-but-can-never-fire gap that looks, from inside the record,
// exactly like a guard that was never invoked (§8's opening claim). Comparing
// on (Event, Matcher) only, rather than the full entry, is deliberate: an
// entry's Bin/Args encode this install's own router path and state dir,
// which would make the comparison fail on every machine whose state dir
// isn't the one BuildPlan happens to be re-rendered with here.
//
// A duplicate (Event, Matcher) pair in the installed bridge is reported the
// same way as an orphaned one: it is exactly the session_start +
// pi:session_start aliasing hazard (two manifest event names resolving to one
// native pi.on registration) becoming visible as drift, and doctor has no
// other place that would ever say so.
func piBridgeDrift(stateDir, bridgePath string) Finding {
	f := Finding{Engine: vocab.Pi, Check: "bridge matches handler table", Detail: bridgePath}

	raw, err := os.ReadFile(bridgePath)
	if err != nil {
		if os.IsNotExist(err) {
			// danglingExtensions/registration() above already report a
			// missing bridge as Fail; a second Fail here for the same cause
			// would be duplicate noise.
			f.Status = Unknown
			f.Detail = "no " + bridgePath + " to check"
			return f
		}
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", bridgePath, err)
		return f
	}
	data, ok := recoverPiBridgeData(raw)
	if !ok {
		f.Status = Unknown
		f.Detail = "cannot recover DATA from " + bridgePath
		return f
	}

	if stateDir == "" {
		f.Status = Unknown
		f.Detail = "no --state-dir recoverable to read the handler table from"
		return f
	}
	tablePath := filepath.Join(stateDir, "table.json")
	handlers, err := tableHandlers(stateDir)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read the handler table at %s: %v", tablePath, err)
		return f
	}

	routerPaths := routerPathPattern.FindAllString(string(raw), -1)
	if len(routerPaths) == 0 {
		f.Status = Unknown
		f.Detail = "no router path recoverable from " + bridgePath
		return f
	}
	plan, err := render.BuildPlan(handlers, routerPaths[0], stateDir)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot render %s's pi plan: %v", tablePath, err)
		return f
	}

	type key struct{ event, matcher string }
	want := make(map[key]bool, len(plan[vocab.Pi]))
	for _, e := range plan[vocab.Pi] {
		want[key{e.Event, e.Matcher}] = true
	}
	got := make(map[key]int, len(data.Entries))
	for _, e := range data.Entries {
		got[key{e.Event, e.Matcher}]++
	}

	var missing, drifted []string
	for k := range want {
		if got[k] == 0 {
			missing = append(missing, k.event+"/"+k.matcher)
		}
	}
	for k, n := range got {
		switch {
		case !want[k]:
			drifted = append(drifted, k.event+"/"+k.matcher)
		case n > 1:
			drifted = append(drifted, fmt.Sprintf("%s/%s (registered %d times)", k.event, k.matcher, n))
		}
	}

	if len(missing) == 0 && len(drifted) == 0 {
		f.Status = Pass
		f.Detail = bridgePath + "'s entries match what " + tablePath + " renders for pi"
		return f
	}
	sort.Strings(missing)
	sort.Strings(drifted)
	f.Status = Fail
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, tablePath+" registers a handler with no matching entry in "+bridgePath+": "+strings.Join(missing, ", "))
	}
	if len(drifted) > 0 {
		parts = append(parts, bridgePath+" has an entry "+tablePath+" would not render this way: "+strings.Join(drifted, ", "))
	}
	f.Detail = strings.Join(parts, "; ")
	return f
}

// danglingExtensions is Pi's own gap: pi tolerates an extensions[] entry
// whose file is missing by silently skipping it, starting, running and
// exiting 0 with no warning of its own. doctor is the only thing that can
// surface it.
func danglingExtensions(settingsPath string) Finding {
	f := Finding{Engine: vocab.Pi, Check: "extensions targets exist", Detail: settingsPath}
	var settings struct {
		Extensions []string `json:"extensions"`
	}
	if err := readJSON(settingsPath, &settings); err != nil {
		if os.IsNotExist(err) {
			// registration() above already reports a missing settings.json as
			// Fail; a second Fail here for the same cause would be duplicate
			// noise.
			f.Status = Unknown
			f.Detail = "no " + settingsPath + " to check"
			return f
		}
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", settingsPath, err)
		return f
	}

	// Absence is the only error that means what this check reports. A stat can
	// also fail on an unreadable parent directory or a dead mount, and calling
	// that "pi cannot find it" would send the reader off to recreate a file
	// that is already there — a wrong answer from the tool whose whole job is
	// to be believed about silent failures.
	var missing, unreadable []string
	for _, e := range settings.Extensions {
		_, err := os.Stat(e)
		switch {
		case err == nil:
		case os.IsNotExist(err):
			missing = append(missing, e)
		default:
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", e, err))
		}
	}
	if len(missing) > 0 {
		f.Status = Fail
		f.Detail = "extensions[] names a file pi cannot find, and pi loads silently without it: " + strings.Join(missing, ", ")
		return f
	}
	if len(unreadable) > 0 {
		f.Status = Unknown
		f.Detail = "cannot tell whether every extensions[] entry resolves: " + strings.Join(unreadable, ", ")
		return f
	}
	f.Status = Pass
	f.Detail = "every extensions[] entry in " + settingsPath + " resolves to a file"
	return f
}

// piLauncherHooksPattern and piLauncherExtensionPattern recover what a
// wrapper script injects into a pi invocation. os.Getenv("PI_AGENT_HOOKS")
// cannot do this: the variable is exported inside the launcher, not in the
// ambient environment doctor runs in, so a finding wired to it would never
// fire on the one machine the hazard was found on. Scanning the launcher's
// own source is what actually found it.
var (
	piLauncherHooksPattern     = regexp.MustCompile(`PI_AGENT_HOOKS=(\S+)`)
	piLauncherExtensionPattern = regexp.MustCompile(`-e\s+(\S+)`)
)

// piLauncherFindings resolves pi on PATH, follows symlinks to the real
// target, and — only if that target is a text script rather than a compiled
// binary — scans it for injected guard paths and extensions. This machine's
// own wrapper injects a bridge plus four guard paths that already enforce on
// Pi, so the same guard can fire twice per tool_call; hookyard cannot rewrite
// a Nix store script, so naming the injection is the whole of its honest
// response.
func piLauncherFindings() Finding {
	f := Finding{Engine: vocab.Pi, Check: "launcher wrapper", Detail: "pi (PATH)"}

	path, err := exec.LookPath("pi")
	if err != nil {
		f.Status = Unknown
		f.Detail = "no pi on PATH to check for an injected launcher"
		return f
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot resolve %s: %v", path, err)
		return f
	}
	f.Detail = resolved

	raw, err := os.ReadFile(resolved)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", resolved, err)
		return f
	}
	if looksBinary(raw) {
		f.Status = Pass
		f.Detail = resolved + " is a compiled binary, not a wrapper script"
		return f
	}

	var injected []string
	for _, m := range piLauncherHooksPattern.FindAllStringSubmatch(string(raw), -1) {
		injected = append(injected, strings.Split(m[1], ":")...)
	}
	for _, m := range piLauncherExtensionPattern.FindAllStringSubmatch(string(raw), -1) {
		injected = append(injected, m[1])
	}
	injected = distinctStrings(injected)

	if len(injected) == 0 {
		f.Status = Pass
		f.Detail = resolved + " does not inject PI_AGENT_HOOKS or -e extensions"
		return f
	}
	f.Status = Fail
	f.Detail = fmt.Sprintf("%s injects registrations hookyard did not write and cannot strip: %s", resolved, strings.Join(injected, ", "))
	return f
}

// looksBinary reports whether a launcher is a compiled binary rather than a
// text script. A compiled binary can contain byte sequences that
// coincidentally match the launcher patterns; a NUL byte in the first slice is
// the standard signal that a file isn't text, the same heuristic git and
// file(1) use.
func looksBinary(raw []byte) bool {
	probe := raw
	if len(probe) > 512 {
		probe = probe[:512]
	}
	return bytes.IndexByte(probe, 0) != -1
}

// claudeSettingsPattern recovers what the claude launcher passes as
// --settings. The quoted alternatives are not decoration: --settings takes a
// file *or* an inline JSON document, and an inline one is quoted and full of
// spaces, so a bare \S+ would recover a fragment of it.
var claudeSettingsPattern = regexp.MustCompile(`--settings[=\s]+('[^']*'|"[^"]*"|\S+)`)

// claudeLauncherSettings recovers those values the way piLauncherFindings
// recovers Pi's injections: resolve on PATH, follow symlinks, scan only a text
// script. It is the only way to read them — hookyard no longer writes
// ~/.claude/settings.json, and the overlay Nix passes is named nowhere else on
// the machine (§4.4).
// The second return is why nothing was recovered, empty when something was.
// Collapsing the cases loses the distinction the operator needs: a compiled
// binary means this delivery path does not apply on that machine, while a
// wrapper that passes no --settings means the overlay is simply not wired.
func claudeLauncherSettings() ([]string, string) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, "no claude on PATH to read --settings from"
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Sprintf("cannot resolve %s: %v", path, err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Sprintf("cannot read %s: %v", resolved, err)
	}
	if looksBinary(raw) {
		return nil, resolved + " is a compiled binary, not a wrapper script"
	}

	var values []string
	for _, m := range claudeSettingsPattern.FindAllStringSubmatch(string(raw), -1) {
		values = append(values, strings.Trim(m[1], `'"`))
	}
	if len(values) == 0 {
		return nil, resolved + " passes no --settings"
	}
	return distinctStrings(values), ""
}

func registration(engine vocab.Engine, path string) Finding {
	f := Finding{Engine: engine, Check: "hookyard registered", Detail: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		f.Status = Fail
		f.Detail = fmt.Sprintf("%s: %v", path, err)
		return f
	}
	if strings.Contains(string(raw), render.Marker) {
		f.Status = Pass
		f.Detail = "present in " + path
		return f
	}
	f.Status = Fail
	f.Detail = "no hookyard entry in " + path + "; run hookyard install"
	return f
}

// routerPathPattern recovers the absolute path hookyard's own emitted
// command names, by scanning the raw config bytes rather than parsing three
// different config formats: render.command puts the identical string in all
// three verbatim (§9). The character class excludes both quote characters
// as well as whitespace: every encoder wraps the whole command in "…", so a
// quote is the delimiter, never part of the path, and checkShellSafe
// (cmd/hookyard/main.go) already guarantees a legitimate router path
// contains neither — the exclusion is exact, not heuristic. A whitespace-only
// boundary would recover the opening quote along with the path and fail the
// check on every engine, always (§9).
var routerPathPattern = regexp.MustCompile(`/[^\s"']*` + regexp.QuoteMeta(render.Marker))

// stateDirPattern recovers --state-dir from the same command line, in the
// same character class and for the same reason. It no longer covers Pi:
// Pi's invocation renders as DATA.entries[].args, a JSON array rather than a
// shell command line, so a whitespace-delimited match finds nothing there —
// recoverPiBridgeData reads that source instead.
var stateDirPattern = regexp.MustCompile(`--state-dir\s+([^\s"']+)`)

// piBridgeDataAnchor is the line writePiBridge produces by exactly one
// strings.Replace of one json.Marshal output (pi.go:169-173): the spliced
// value has no embedded raw newline — json.Marshal
// escapes control characters rather than emitting them literally — so
// everything between this anchor and the next newline, less its trailing
// semicolon, is one complete JSON object. That is what makes recovering it a
// slice-and-Unmarshal rather than a brace-counting parser.
const piBridgeDataAnchor = "const DATA = "

// piBridgeEntryShape is doctor's own copy of piBridgeEntry's wire shape
// (internal/render/pi.go): that type is unexported, and this package only
// ever reads a bridge another package wrote, never constructs one.
type piBridgeEntryShape struct {
	Event   string   `json:"event"`
	Matcher string   `json:"matcher"`
	Args    []string `json:"args"`
}

type piBridgeShape struct {
	Entries []piBridgeEntryShape `json:"entries"`
}

// recoverPiBridgeData locates and parses the bridge's spliced DATA object.
// The second return is false both when the anchor is absent and when what
// follows it does not parse — a bridge whose DATA cannot be recovered is
// Unknown, not Fail, the same absence-vs-unreadability split danglingExtensions
// already draws for a missing extensions[] target.
func recoverPiBridgeData(raw []byte) (piBridgeShape, bool) {
	idx := bytes.Index(raw, []byte(piBridgeDataAnchor))
	if idx == -1 {
		return piBridgeShape{}, false
	}
	line := raw[idx+len(piBridgeDataAnchor):]
	if end := bytes.IndexByte(line, '\n'); end != -1 {
		line = line[:end]
	}
	line = bytes.TrimSuffix(bytes.TrimSpace(line), []byte(";"))
	var data piBridgeShape
	if err := json.Unmarshal(line, &data); err != nil {
		return piBridgeShape{}, false
	}
	return data, true
}

// routerPath reports whether the absolute path this
// engine's emitted command names is actually there to exec. §9's closing
// argument is why doctor, not the event record, has to catch this: a broken
// state-dir symlink fails at exec, before any hookyard code runs, so there is
// nothing running to write a record for streamFindings to read.
func routerPath(engine vocab.Engine, configPath string) Finding {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return Finding{
			Engine: engine,
			Check:  "router path",
			Status: Unknown,
			Detail: fmt.Sprintf("cannot read %s: %v", configPath, err),
		}
	}
	return routerPathIn(engine, configPath, raw)
}

// routerPathIn takes the bytes rather than the path because Claude's source
// may be an inline --settings document, which has no file to re-read.
func routerPathIn(engine vocab.Engine, source string, raw []byte) Finding {
	f := Finding{Engine: engine, Check: "router path", Detail: source}
	paths := distinctStrings(routerPathPattern.FindAllString(string(raw), -1))
	if len(paths) == 0 {
		// registration() above already reports a missing hookyard entry as
		// Fail; a second Fail here for the same cause would be duplicate
		// noise.
		f.Status = Unknown
		f.Detail = "no hookyard entry in " + source + " to check"
		return f
	}

	// Every writer strips each row carrying the marker regardless of its
	// path, so hookyard's own re-render always leaves one form; a second
	// path can only come from a hand-edit or a foreign writer, and is
	// reported rather than collapsed.
	var listed []string
	bad := false
	for _, p := range paths {
		if reason := notExecutable(p); reason != "" {
			bad = true
			listed = append(listed, fmt.Sprintf("%s (%s)", p, reason))
			continue
		}
		listed = append(listed, fmt.Sprintf("%s (executable)", p))
	}
	if bad {
		f.Status = Fail
		f.Detail = strings.Join(listed, "; ")
		return f
	}
	f.Status = Pass
	f.Detail = strings.Join(paths, ", ") + ": executable"
	return f
}

// notExecutable reports why path cannot be run, or "" when it can. "Can be
// run" means path stats, following symlinks, to a regular file with any
// execute bit set — not identical to test -x, which asks about the calling
// user's effective access, but the two agree for the 0555 store targets §9
// enumerates, and the difference is not worth a unix.Access call that would
// cost portability.
func notExecutable(path string) string {
	lstatInfo, lstatErr := os.Lstat(path)
	info, err := os.Stat(path) // follows symlinks
	switch {
	case errors.Is(lstatErr, fs.ErrNotExist):
		return "missing (run hookyard install or home-manager switch)"
	case lstatErr != nil:
		return fmt.Sprintf("not accessible: %v", lstatErr)
	case errors.Is(err, fs.ErrNotExist) && lstatInfo.Mode()&os.ModeSymlink != 0:
		return "dangling symlink (run hookyard install or home-manager switch)"
	case err != nil:
		return fmt.Sprintf("not accessible: %v", err)
	case info.IsDir():
		return "a directory"
	case !info.Mode().IsRegular() || info.Mode()&0o111 == 0:
		return "not executable"
	default:
		return ""
	}
}

// distinctStrings deduplicates a slice while preserving first-seen order.
func distinctStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// recoverStateDir scans all four engines' config files for --state-dir and
// returns the distinct values found, in first-seen order across Claude, then
// Codex, then Cursor, then Pi.
//
// doctor's own state directory otherwise resolves from
// HOOKYARD_STATE_DIR/XDG_STATE_HOME/$HOME — the same untrusted chain the module
// refuses for install — so an overridden state directory makes the
// enforcement finding read a directory nothing writes to and report "no
// events recorded yet today" forever on a healthy machine: a silent false
// negative in the tool nominated to catch silent failures. The emitted
// config is ground truth for what the router actually uses, and it is
// already open in front of the router-path scan above.
func recoverStateDir(p Paths, claude claudeSources) []string {
	var found []string
	scan := func(raw []byte) {
		for _, m := range stateDirPattern.FindAllStringSubmatch(string(raw), -1) {
			found = append(found, m[1])
		}
	}

	// Claude's --state-dir lives in whichever source carries the registration,
	// which under Nix is the launcher's --settings overlay; settings.json
	// holds nothing to recover and scanning it would reproduce the exact
	// silent fallback this function exists to close.
	if src, ok := claude.markerSource(); ok {
		scan(src.raw)
	}

	for _, path := range []string{
		filepath.Join(p.CodexHome, "config.toml"),
		filepath.Join(p.CursorHome, "hooks.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scan(raw)
	}

	// Pi's settings.json only names the bridge file; the router command and
	// its --state-dir travel inside the bridge as DATA.entries[].args, a JSON
	// array rather than a shell command line, so stateDirPattern would
	// recover nothing there and reproduce the exact silent-fallback bug this
	// function exists to close. Each entry carries its own args,
	// so every one is scanned rather than just the first.
	if raw, err := os.ReadFile(render.PiBridgePath(filepath.Join(p.PiAgentDir, "settings.json"))); err == nil {
		if data, ok := recoverPiBridgeData(raw); ok {
			for _, e := range data.Entries {
				for i, a := range e.Args {
					if a == "--state-dir" && i+1 < len(e.Args) {
						found = append(found, e.Args[i+1])
					}
				}
			}
		}
	}
	return distinctStrings(found)
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// streamFindings counts, in today's event record, how many verdicts hookyard
// computed but the engine had no way to enforce (§12) — an ask rendered to an
// engine that only accepts deny is expected, not a misconfiguration, so this
// is Pass whenever stateDir could be resolved unambiguously; only the count
// is informational. It is Fail when disagreement names more than one
// --state-dir recovered from the four engines' registrations: runInstall
// writes Cursor, then Claude, then Pi, then Codex, and a later failure does
// not undo an earlier write, so an activation aborted partway through a
// --state-dir change genuinely leaves two of them naming different tables.
// Pi's is recovered from its bridge rather than its config, because its
// settings.json names only the bridge file — the --state-dir text lives
// inside the bridge's spliced data.
// Silently picking one would reproduce exactly the false negative that
// recovery exists to remove. Engine is left at its zero value since the
// finding isn't per-engine (it renders as a blank column in hookyard
// doctor's %-12s output).
func streamFindings(stateDir string, disagreement []string, now time.Time) []Finding {
	f := Finding{Check: "enforcement"}

	if len(disagreement) > 0 {
		f.Status = Fail
		f.Detail = "engines disagree on --state-dir: " + strings.Join(disagreement, ", ")
		return []Finding{f}
	}

	path := record.StreamPath(stateDir, now)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		f.Status = Unknown
		f.Detail = "no events recorded yet today"
		return []Finding{f}
	}
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", path, err)
		return []Finding{f}
	}
	defer func() { _ = file.Close() }()

	var total, enforcedFalse int
	scanner := bufio.NewScanner(file)
	// The default MaxScanTokenSize (64KiB) equals record's own maxRecordBytes
	// cap, so a max-size record would fail Scan with ErrTooLong; raise it well
	// above that cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var line struct {
			Enforced bool `json:"enforced"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		total++
		if !line.Enforced {
			enforcedFalse++
		}
	}
	if err := scanner.Err(); err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("%s: %v (counted %d/%d before the error)", path, err, enforcedFalse, total)
		return []Finding{f}
	}

	f.Status = Pass
	f.Detail = fmt.Sprintf("%d/%d events today had a computed verdict the engine could not enforce", enforcedFalse, total)
	return []Finding{f}
}

// generationFindings compares what the current home-manager generation
// expects (the witness nix/hm-module.nix wrote at linkGeneration) against
// what `hookyard install` actually did (the receipt it wrote into stateDir).
// Engine is left at its zero value for the same reason streamFindings'
// "enforcement" check leaves it unset: this isn't a per-engine finding.
//
// Every disagreement below is worded "the current generation and the
// installed state disagree", never "the table is stale". They are not the
// same claim: activation runs `linkGeneration`, then ~forty other entries,
// then `installPackages`, then `hookyard install`, all under `set -e`. An
// abort anywhere before `hookyard install` reaches its own work leaves the
// witness naming the new generation while the live `claude` wrapper (and
// everything else install would have touched) is still whatever the
// previous, fully-installed generation left behind — a self-consistent
// machine that this check nonetheless must fail, because the *next*
// generation's assumptions no longer hold for it. "Stale" implies something
// on disk fell behind its own past state; what actually happened is that two
// declarations of intent — the generation's and install's — stopped
// agreeing.
func generationFindings(p Paths, stateDir string) []Finding {
	f := Finding{Check: "generation"}

	if p.GenerationWitness == "" {
		f.Status = Unknown
		f.Detail = "no Nix-placed generation witness; hookyard is not managed by the home-manager module here"
		return []Finding{f}
	}

	w, witnessErr := installstate.ReadWitness(p.GenerationWitness)

	// Read the receipt from the generation the witness names, not the
	// stateDir Run resolved for the other checks: comparing the witness
	// against a receipt from an unrelated install would report drift between
	// two installs that were never meant to agree with each other. The
	// stateDir argument is only a fallback for when there is no witness
	// identity to read one from.
	receiptDir := stateDir
	if witnessErr == nil {
		receiptDir = w.StateDir
	}

	if errors.Is(witnessErr, fs.ErrNotExist) {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("no Nix-placed generation witness at %s; hookyard is not managed by the home-manager module here", p.GenerationWitness)
		return []Finding{f}
	}
	if witnessErr != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("%s: %v", p.GenerationWitness, witnessErr)
		return []Finding{f}
	}

	r, err := installstate.ReadReceipt(receiptDir)
	if errors.Is(err, fs.ErrNotExist) {
		f.Status = Fail
		f.Detail = fmt.Sprintf("hookyard install has never completed for %s; re-run home-manager switch", receiptDir)
		return []Finding{f}
	}
	if err != nil {
		f.Status = Fail
		f.Detail = fmt.Sprintf("%s: %v; re-run home-manager switch", installstate.ReceiptPath(receiptDir), err)
		return []Finding{f}
	}

	if !r.Complete {
		f.Status = Fail
		f.Detail = "the last hookyard install did not finish, so the current generation and the installed state disagree; re-run home-manager switch"
		return []Finding{f}
	}

	if diffs := installstate.Diff(w, r); len(diffs) > 0 {
		f.Status = Fail
		f.Detail = fmt.Sprintf("the current generation and the installed state disagree: %s; re-run home-manager switch", strings.Join(diffs, ", "))
		return []Finding{f}
	}

	f.Status = Pass
	f.Detail = "the installed state matches the current generation"
	return []Finding{f}
}
