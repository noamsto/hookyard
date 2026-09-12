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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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
	// StateDir is the operator's explicit --state-dir. Empty means none was
	// given: Run recovers it from the --state-dir the four engines' emitted
	// configs already name, rather than treating empty as shorthand for
	// record.DefaultStateDir().
	StateDir string
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
	return Paths{
		ClaudeConfigDir:     claude,
		CodexHome:           codex,
		CursorHome:          filepath.Join(home, ".cursor"),
		PiAgentDir:          piAgentDir,
		ClaudeSettingsFlags: claudeLauncherSettings(),
	}, nil
}

// Run reports on each engine for one working directory.
func Run(p Paths, dir string) []Finding {
	claude := resolveClaudeSources(p)

	var findings []Finding
	findings = append(findings, claudeFindings(p, dir, claude)...)
	findings = append(findings, codexFindings(p, dir)...)
	findings = append(findings, cursorFindings(p, dir)...)
	findings = append(findings, piFindings(p)...)

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
	findings = append(findings, streamFindings(stateDir, disagreement, time.Now())...)
	return findings
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
	// unresolved names each source doctor knows exists but could not read, so
	// a Pass on the disableAllHooks gate can say what it did not see (§4.4).
	unresolved []string
}

func resolveClaudeSources(p Paths) claudeSources {
	c := claudeSources{settingsPath: filepath.Join(p.ClaudeConfigDir, "settings.json")}
	// A missing settings.json is the expected state now that hookyard does not
	// write it; only a file that is there and unreadable is worth naming.
	if raw, err := os.ReadFile(c.settingsPath); err == nil {
		c.settings = raw
	} else if !os.IsNotExist(err) {
		c.unresolved = append(c.unresolved, fmt.Sprintf("%s (%v)", c.settingsPath, err))
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
// settings.json: §8's gate fires on the flag being set in user *or* flag
// settings, so checking one of the two and calling the result a Pass is a
// fail-open now that doctor holds the overlay's bytes.
func claudeHooksEnabled(c claudeSources) Finding {
	f := Finding{Engine: vocab.ClaudeCode, Check: "hooks enabled", Detail: c.settingsPath}

	var checked []string
	blind := c.unresolved
	for _, s := range c.all() {
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
		f.Detail += " (not read: " + strings.Join(blind, ", ") + ")"
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
		f.Detail += " (not read: " + strings.Join(c.unresolved, ", ") + ")"
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

	return []Finding{trust, hookTrust, registration(vocab.Codex, config), routerPath(vocab.Codex, config)}
}

func cursorFindings(p Paths, dir string) []Finding {
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

	return []Finding{trust, registration(vocab.Cursor, hooks), routerPath(vocab.Cursor, hooks)}
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
func piFindings(p Paths) []Finding {
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
	}
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
func claudeLauncherSettings() []string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(resolved)
	if err != nil || looksBinary(raw) {
		return nil
	}

	var values []string
	for _, m := range claudeSettingsPattern.FindAllStringSubmatch(string(raw), -1) {
		values = append(values, strings.Trim(m[1], `'"`))
	}
	return distinctStrings(values)
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
// same character class and for the same reason.
var stateDirPattern = regexp.MustCompile(`--state-dir\s+([^\s"']+)`)

// routerPath reports whether the absolute path this
// engine's emitted command names is actually there to exec. §9's closing
// argument is why doctor, not the event record, has to catch this: a broken
// profile symlink fails at exec, before any hookyard code runs, so there is
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
	var bad []string
	for _, p := range paths {
		if reason := notExecutable(p); reason != "" {
			bad = append(bad, fmt.Sprintf("%s (%s)", p, reason))
		}
	}
	if len(bad) > 0 {
		f.Status = Fail
		f.Detail = "not runnable: " + strings.Join(bad, "; ")
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
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "missing"
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
		// Pi's settings.json only names the bridge file; the router command,
		// and the --state-dir stateDirPattern is after, is data spliced into
		// the bridge itself (render.WritePi). Scanning settings.json here
		// would recover nothing and reproduce the exact silent-fallback bug
		// this function exists to close.
		render.PiBridgePath(filepath.Join(p.PiAgentDir, "settings.json")),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scan(raw)
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
