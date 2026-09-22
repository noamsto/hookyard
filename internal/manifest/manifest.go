// Package manifest loads and validates the per-repo handler declarations that
// together form hookyard's single table (docs/design/hookyard.md §8).
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/noamsto/hookyard/internal/atomicfile"
	"github.com/noamsto/hookyard/internal/verdict"
	"github.com/noamsto/hookyard/internal/vocab"
)

// MaxHandlerTimeoutMS is §4's per-handler sub-budget: the router's own
// deadline minus the consolidation margin. An override above it could not be
// honoured, so it is refused rather than silently clamped.
const MaxHandlerTimeoutMS = 4300

// Lane vocabulary: which of the two ways the router runs a handler. No
// omitempty on the field below — the table states the lane explicitly rather
// than leaving a reader to know that absent means verdict.
const (
	LaneVerdict       = "verdict"
	LaneFireAndForget = "fire_and_forget"
)

type Handler struct {
	ID        string   `json:"id"`
	Exec      string   `json:"exec"`
	Events    []string `json:"events"`
	Engines   []string `json:"engines"`
	Match     []string `json:"match"`
	TimeoutMS int      `json:"timeout_ms"`
	Lane      string   `json:"lane"`
}

// FireAndForget reports whether the router starts h and never waits for it.
func (h Handler) FireAndForget() bool { return h.Lane == LaneFireAndForget }

// Command is a pi-only, build-mode-only command surface: build
// --engine pi renders each entry into pi.registerCommand, so a handler's
// exec can also be invoked as a slash command rather than only fired from a
// hook. Yard mode and every other engine have no equivalent — Load,
// LoadBuildTime and ReadTable reject a non-empty Commands outright rather
// than accepting a shape they could never serve.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Exec        string `json:"exec"`
}

type Manifest struct {
	Handlers []Handler `json:"handlers"`
	Commands []Command `json:"commands,omitempty"`

	// Source is the path this manifest was read from, used to name both sides
	// of a collision.
	Source string `json:"-"`
}

// ExecForm is the shape a handler's exec must take, and so which mode a
// manifest serves: an absolute exec cannot ship inside a plugin, and a
// relative one has nothing trustworthy to resolve against in the yard.
type ExecForm int

const (
	ExecAbsolute       ExecForm = iota // yard mode
	ExecPluginRelative                 // build mode
)

// PluginTablePath is where a built plugin's baked handler table lives,
// relative to the plugin root.
const PluginTablePath = "hookyard/table.json"

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./-]*$`)

// commandNamePattern constrains a manifest command's name. The name is
// passed to pi.registerCommand verbatim and becomes the literal string a user
// types after "/", so unlike idPattern it excludes "/" and "." — either would
// read as a nested command path pi's command palette does not support.
var commandNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// eventPattern constrains the native half of an engine-scoped event name.
// Those names reach a TOML array-of-table header verbatim, where an encoder
// cannot protect them, so the character set is the protection (§8).
var eventPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// Load reads and validates one manifest. A manifest that cannot be parsed, or
// that describes a handler hookyard could not run, fails here rather than
// after some of three engines' configs have been rewritten.
func Load(path string) (*Manifest, error) {
	return loadAbsolute(path, validateExec)
}

// loadAbsolute is Load's and LoadBuildTime's shared body: both read
// yard-mode manifests with absolute execs, and differ only in what extra can
// afford to check — Load stats every exec because it runs at activation
// time; LoadBuildTime cannot, because a build sandbox does not see every
// exec a real activation would (see validateExecBuildTime).
func loadAbsolute(path string, extra func(where string, h Handler) error) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Source = path
	m.normalizeLanes()
	if err := m.validateAll(ExecAbsolute, extra); err != nil {
		return nil, err
	}
	return &m, nil
}

// normalizeLanes fills in the lane every reader defaults to, so validateStatic
// can treat "" as invalid rather than normalizing a second time. Every entry
// point that parses handlers calls it, because a manifest one of them accepts
// and another rejects is exactly the build-time/activation-time split
// validateAll exists to prevent.
func (m *Manifest) normalizeLanes() {
	for i := range m.Handlers {
		if m.Handlers[i].Lane == "" {
			m.Handlers[i].Lane = LaneVerdict
		}
	}
}

// validateAll holds every rule Load and loadStatic share, so build mode and
// yard mode cannot diverge on a rule that has nothing to do with exec form.
// extra runs per handler on top of the shared rules; it is nil for the
// caller that cannot afford to touch the filesystem.
func (m *Manifest) validateAll(form ExecForm, extra func(where string, h Handler) error) error {
	if len(m.Handlers) == 0 {
		return fmt.Errorf("%s: no handlers declared", m.Source)
	}
	if err := validateCommandsMode(m.Source, m.Commands, form); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, h := range m.Handlers {
		where := fmt.Sprintf("%s: handler %q", m.Source, h.ID)
		if err := validateStatic(where, h, form); err != nil {
			return err
		}
		if err := validateCatalog(where, h); err != nil {
			return err
		}
		if extra != nil {
			if err := extra(where, h); err != nil {
				return err
			}
		}
		if seen[h.ID] {
			return fmt.Errorf("%s: handler id %q declared twice in one manifest", m.Source, h.ID)
		}
		seen[h.ID] = true
	}
	seenCommand := map[string]bool{}
	for _, c := range m.Commands {
		where := fmt.Sprintf("%s: command %q", m.Source, c.Name)
		if err := validateCommand(where, c); err != nil {
			return err
		}
		if seenCommand[c.Name] {
			return fmt.Errorf("%s: command name %q declared twice in one manifest", m.Source, c.Name)
		}
		seenCommand[c.Name] = true
	}
	return nil
}

// validateCommandsMode refuses a non-empty Commands outside build mode
// (ExecPluginRelative). Yard mode's handlers run standalone with nothing
// resembling pi's command registry to register into, so a manifest naming a
// command there could never be served.
func validateCommandsMode(source string, commands []Command, form ExecForm) error {
	if form == ExecAbsolute && len(commands) > 0 {
		return fmt.Errorf("%s: yard mode has no command surface; commands are accepted only in "+
			"build mode (hookyard build)", source)
	}
	return nil
}

func validateExec(where string, h Handler) error {
	if err := execIsRunnable(h.Exec); err != nil {
		return fmt.Errorf("%s: exec %q: %w", where, h.Exec, err)
	}
	return nil
}

// validateStatic runs every rule that does not touch the filesystem: id
// pattern, exec path form, event and engine vocabulary, match vocabulary, the
// timeout_ms bound, lane, and coverage. ReadTable calls this alone, on the
// critical path of every guarded tool call, because re-statting an exec there
// buys nothing the exec attempt does not already report as a handler error.
// Callers normalize an empty Lane to LaneVerdict before reaching here, so this
// treats "" as invalid rather than re-normalizing it a second place.
func validateStatic(where string, h Handler, form ExecForm) error {
	if !idPattern.MatchString(h.ID) {
		return fmt.Errorf("%s: id must match %s", where, idPattern)
	}
	switch form {
	case ExecAbsolute:
		// Same rule §9 applies to the emitted router command, applied to handlers.
		// A relative exec resolves at hook-fire time against the directory the
		// agent's tool call runs in, and a bare name against the PATH the engine
		// hands down — neither is hookyard's to choose, so either would let a file
		// that happens to sit there stand in for the guard.
		if !filepath.IsAbs(h.Exec) {
			return fmt.Errorf("%s: exec %q must be an absolute path, because it is resolved at "+
				"hook-fire time against the agent's working directory and PATH, not the installer's; "+
				"a plugin-root-relative exec is build mode's form (hookyard build)",
				where, h.Exec)
		}
	case ExecPluginRelative:
		if err := validatePluginRelativeExec(where, h.Exec); err != nil {
			return err
		}
	}
	if h.Lane != LaneVerdict && h.Lane != LaneFireAndForget {
		return fmt.Errorf("%s: lane %q must be %q or %q", where, h.Lane, LaneVerdict, LaneFireAndForget)
	}
	// Non-zero, not "present": TimeoutMS is a plain int with no omitempty, so
	// WriteTable emits "timeout_ms":0 on every entry. A presence check would
	// make hookyard's own table fail ReadTable on the critical path of every
	// hook fire.
	if h.FireAndForget() && h.TimeoutMS != 0 {
		return fmt.Errorf("%s: timeout_ms %d set on a fire-and-forget handler, but nothing waits "+
			"for it so a timeout could not be enforced", where, h.TimeoutMS)
	}
	if h.TimeoutMS < 0 || h.TimeoutMS > MaxHandlerTimeoutMS {
		return fmt.Errorf("%s: timeout_ms %d outside 0..%d", where, h.TimeoutMS, MaxHandlerTimeoutMS)
	}
	if len(h.Events) == 0 {
		return fmt.Errorf("%s: no events declared", where)
	}
	if len(h.Engines) == 0 {
		return fmt.Errorf("%s: no engines declared", where)
	}
	for _, event := range h.Events {
		native := event
		if _, scopedNative, ok := vocab.SplitEngineScoped(event); ok {
			native = scopedNative
		} else if !vocab.IsCanonicalEvent(event) {
			return fmt.Errorf("%s: event %q is neither a canonical event (%v) nor engine-scoped "+
				"(\"engine:NativeName\")", where, event, vocab.CanonicalEvents)
		}
		if !eventPattern.MatchString(native) {
			return fmt.Errorf("%s: event %q must match %s", where, native, eventPattern)
		}
	}
	for _, t := range h.Match {
		if !vocab.IsNormalizedTool(t) {
			return fmt.Errorf("%s: match %q is not a normalized tool name (want one of %v); "+
				"engine-native names such as Shell belong in an engine-scoped event, not here",
				where, t, vocab.NormalizedTools)
		}
	}
	engines, err := parseEngines(where, h.Engines)
	if err != nil {
		return err
	}
	if err := validateCoverage(where, h, engines); err != nil {
		return err
	}
	return validateLane(where, h, engines)
}

// validatePluginRelativeExec is the ExecPluginRelative form's shared rule:
// the router joins the exec onto the plugin root rather than the agent's
// cwd, so relative is safe here; ResolvePluginExec still enforces
// containment after symlinks, which a lexical check cannot see. Besides
// validateStatic's handler case, commands take this branch unconditionally
// (validateCommand) — a command is build-mode-only regardless of the
// manifest's own exec form, so there is no ExecAbsolute case for it to switch
// on.
func validatePluginRelativeExec(where, exec string) error {
	if !filepath.IsLocal(exec) {
		return fmt.Errorf("%s: exec %q must be relative to the plugin root with no .. segments, "+
			"because an absolute path names the author's machine rather than the end user's; "+
			"an absolute exec is yard mode's form (hookyard install)",
			where, exec)
	}
	return nil
}

// validateCatalog refuses an engine-scoped event whose native half isn't one
// of that engine's routed catalog: "claude-code:X" against the eight events
// vocab.ClaudeCodeCatalog documents evidence for, and "pi:X" against
// vocab.PiCatalog. This is how the pi:before_agent_start rejection lands —
// before_agent_start is absent from PiCatalog, so it fails
// the same catalog rule every other out-of-catalog event does, rather than a
// bespoke check with its own message. Claude Code and pi are the only
// engines with a catalog here: Codex and Cursor have none, so
// vocab.NativeEvent resolves codex:X or cursor:X for any eventPattern-shaped
// X against that engine's own scope, and validateCoverage's nil return for
// it is not coverage of a catalog that does not exist.
//
// It runs from validateAll, not validateStatic, so ReadTable — which calls
// validateStatic alone — stays exempt: a table an older hookyard wrote before
// some catalog row existed would otherwise turn every event for every engine
// into a router error until the next successful install rewrites it (#59).
func validateCatalog(where string, h Handler) error {
	for _, event := range h.Events {
		engine, native, scoped := vocab.SplitEngineScoped(event)
		if !scoped {
			continue
		}
		switch engine {
		case vocab.ClaudeCode:
			if !vocab.IsClaudeCodeEvent(native) {
				natives := make([]string, len(vocab.ClaudeCodeCatalog))
				for i, e := range vocab.ClaudeCodeCatalog {
					natives[i] = e.Native
				}
				return fmt.Errorf("%s: event %q is not a Claude Code event hookyard routes (want one of %s)",
					where, event, strings.Join(natives, ", "))
			}
		case vocab.Pi:
			if !vocab.IsPiEvent(native) {
				return fmt.Errorf("%s: event %q is not a Pi event hookyard routes (want one of %s)",
					where, event, strings.Join(vocab.PiCatalog, ", "))
			}
		}
	}
	return nil
}

// validateCommand checks one manifest command: name shape, a non-empty
// description, and exec under the ExecPluginRelative rule — commands are
// build-mode-only regardless of the manifest's own exec form.
func validateCommand(where string, c Command) error {
	if !commandNamePattern.MatchString(c.Name) {
		return fmt.Errorf("%s: name must match %s", where, commandNamePattern)
	}
	if c.Description == "" {
		return fmt.Errorf("%s: no description", where)
	}
	return validatePluginRelativeExec(where, c.Exec)
}

func parseEngines(where string, names []string) ([]vocab.Engine, error) {
	engines := make([]vocab.Engine, 0, len(names))
	for _, name := range names {
		engine, err := vocab.ParseEngine(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		engines = append(engines, engine)
	}
	return engines, nil
}

// validateCoverage refuses a handler that would render to nothing on an engine
// it claims. Both failures are silent in the engine itself — Cursor drops an
// untranslatable matcher with a warning nobody reads, and an unmapped event
// simply never fires — and a handler that was never registered cannot even
// abstain, so it leaves no trace in the record either (§5).
func validateCoverage(where string, h Handler, engines []vocab.Engine) error {
	for _, engine := range engines {
		for _, event := range h.Events {
			native, ok := vocab.NativeEvent(engine, event)
			if !ok {
				if scoped, _, isScoped := vocab.SplitEngineScoped(event); isScoped && scoped != engine {
					continue // scoped to another engine, not this one's problem
				}
				return fmt.Errorf("%s: event %q has no %s equivalent", where, event, engine)
			}
			if len(h.Match) == 0 {
				continue
			}
			if got := vocab.NativeMatcher(engine, h.Match); len(got) == 0 {
				return fmt.Errorf("%s: match %v renders empty for %s on event %s, so the handler "+
					"would be registered but never fire", where, h.Match, engine, native)
			}
		}
	}
	return nil
}

// validateLane refuses a fire-and-forget handler registered on an event where
// the engine would hand it a guard slot: a decision slot that actually gates
// a call, as opposed to pi's turn_end slot (D3), which only asks the bridge
// for one more continuation and guards nothing. The router never waits for a
// fire-and-forget handler (§4), so a verdict computed there has nowhere to
// go on a guard event — the handler's author would reasonably believe it
// guards a call it in fact never can. HasGuardSlot, not HasDecisionSlot, is
// the right predicate here: rejecting pi turn_end would reject every
// fire-and-forget turn_end observer that claims pi, forcing the common case —
// an observer, not a guard — onto pi's synchronous verdict lane for no
// guarding benefit.
func validateLane(where string, h Handler, engines []vocab.Engine) error {
	if !h.FireAndForget() {
		return nil
	}
	for _, engine := range engines {
		for _, event := range h.Events {
			canonical, native := resolveEvent(engine, event)
			if canonical == "" && native == "" {
				continue
			}
			if verdict.HasGuardSlot(engine, canonical, native) {
				return fmt.Errorf("%s: event %q on %s has a guard slot, but a fire-and-forget "+
					"handler never returns a verdict so it cannot guard this event", where, event, engine)
			}
		}
	}
	return nil
}

// resolveEvent fills both halves of a manifest event name for engine.
// HasGuardSlot (via HasDecisionSlot) asks about a canonical name and a native
// one, and a manifest carries only ever one of them.
func resolveEvent(engine vocab.Engine, event string) (canonical, native string) {
	if scoped, n, ok := vocab.SplitEngineScoped(event); ok {
		if scoped != engine {
			return "", "" // another engine's event, skipped as validateCoverage skips it
		}
		// ok is deliberately ignored: Cursor's scoped decision events have no
		// inbound mapping, and returning early on !ok would drop exactly the
		// entries HasDecisionSlot's native arm catches.
		canonical, _ = vocab.InboundEvent(engine, n)
		return canonical, n
	}
	native, ok := vocab.NativeEvent(engine, event)
	if !ok {
		return "", ""
	}
	return event, native
}

func execIsRunnable(path string) error {
	if path == "" {
		return errors.New("not set")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable (mode %s)", fs.FileMode(info.Mode().Perm()))
	}
	return nil
}

// LoadPlugin reads and validates one manifest for build mode: exec must be
// plugin-root-relative. It does not stat execs, because only the caller knows
// the plugin root; CheckPluginExecs is that check.
func LoadPlugin(path string) (*Manifest, error) {
	return loadStatic(path, ExecPluginRelative)
}

// LoadBuildTime reads and validates one yard-mode manifest for a Nix build
// sandbox, which is a third caller with its own carve-out alongside Load
// (stats every exec) and LoadPlugin (stats none): it stats an exec only when
// the sandbox could plausibly have that exec in its closure at all. See
// validateExecBuildTime for why that is not the same as "exec looks like a
// store path".
func LoadBuildTime(path string) (*Manifest, error) {
	return loadAbsolute(path, validateExecBuildTime)
}

// storeDir is the Nix store root a build sandbox exposes. The fallback to the
// conventional path only matters for callers that never set NIX_STORE — real
// builds always export it — but keeping the fallback is what lets the tests
// below point it at a tempdir instead of asserting against the real store.
func storeDir() string {
	if dir := os.Getenv("NIX_STORE"); dir != "" {
		return dir
	}
	return "/nix/store"
}

// storeRoot reports the top-level store entry exec lives under — the
// directory whose store path would show up as one of a derivation's
// scanned references — and whether exec is under the store at all.
// filepath.Rel compares on path-separator boundaries, so a lookalike
// directory next to the store (/nix/storefoo/...) does not read as "under"
// it the way a raw string-prefix check would.
func storeRoot(exec string) (string, bool) {
	dir := storeDir()
	rel, err := filepath.Rel(dir, exec)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(dir, first), true
}

// validateExecBuildTime is the decidability boundary a build-time check must
// respect. Whether a manifest's execs are visible in the sandbox depends on
// how the manifest itself entered the store: one written by a derivation
// (writeText and friends) has its store-path references scanned, so its
// execs are pulled into the sandbox closure — measured, nix-store --query
// --references on a live consumer manifest returns its 6 exec store paths.
// One added as a bare source path does not get scanned — measured, running a
// file naming an exec through nix-store --add yields a store path with zero
// references. So a present store root means the sandbox was actually handed
// this exec, the same as install would be at activation time, and a missing
// exec there is a real typo worth failing the build over. A missing root
// means the opposite: this manifest took the source-path route, the exec was
// never handed to the sandbox, and stat-ing it would fail every such build
// regardless of whether the exec is fine on the target machine.
func validateExecBuildTime(where string, h Handler) error {
	root, ok := storeRoot(h.Exec)
	if !ok {
		return nil
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := execIsRunnable(h.Exec); err != nil {
		return fmt.Errorf("%s: exec %q: %w", where, h.Exec, err)
	}
	return nil
}

// loadStatic reads and validates one manifest the way Load does, minus
// execIsRunnable — the same carve-out ReadTable makes. Every other rule stays
// shared through validateAll, including the exec-form rule, which lives in
// validateStatic rather than in the stat. Merge's cross-manifest duplicate-id
// check is left to the caller: loadStatic dedupes one file, not a caller's
// whole --manifest list.
func loadStatic(path string, form ExecForm) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Source = path
	m.normalizeLanes()
	if err := m.validateAll(form, nil); err != nil {
		return nil, err
	}
	return &m, nil
}

// Merge combines validated manifests into one table. A duplicate id across
// repos is an error rather than a last-writer-wins override, because that is
// the case where one repo's change silently replaces another repo's guard.
func Merge(manifests []*Manifest) ([]Handler, error) {
	owner := map[string]string{}
	var all []Handler
	for _, m := range manifests {
		for _, h := range m.Handlers {
			if prev, clash := owner[h.ID]; clash {
				return nil, fmt.Errorf("handler id %q declared by both %s and %s", h.ID, prev, m.Source)
			}
			owner[h.ID] = m.Source
			all = append(all, h)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	// Commands share handlers' cross-manifest uniqueness rule even though
	// they are not part of the returned table: a duplicate name is one
	// repo's command silently shadowing another's, the same collision a
	// duplicate handler id catches above.
	commandOwner := map[string]string{}
	for _, m := range manifests {
		for _, c := range m.Commands {
			if prev, clash := commandOwner[c.Name]; clash {
				return nil, fmt.Errorf("command name %q declared by both %s and %s", c.Name, prev, m.Source)
			}
			commandOwner[c.Name] = m.Source
		}
	}
	return all, nil
}

// WriteTable writes the consolidated handler table, in the same shape as a
// manifest, at a fixed 0600: the table is hookyard's own file, so a
// pre-existing one left looser must be tightened rather than honoured.
func WriteTable(path string, handlers []Handler) error {
	return writeTable(path, handlers, 0o600)
}

// WritePluginTable writes a built plugin's baked table. Unlike WriteTable's it
// is 0644, because it ships to end users inside git and zip artifacts.
func WritePluginTable(path string, handlers []Handler) error {
	return writeTable(path, handlers, 0o644)
}

func writeTable(path string, handlers []Handler, perm fs.FileMode) error {
	raw, err := json.Marshal(Manifest{Handlers: handlers})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw, perm)
}

// ReadTable reads the table WriteTable produces and re-runs every validation
// rule that does not touch the filesystem, because a hand-edited or stale
// table must not be trusted on the critical path just because some earlier
// install was well-behaved. Unlike Load, it does not call execIsRunnable and
// does not reject a zero-handler table: "nothing is registered" is a
// legitimate table state, distinct from "the table is gone".
func ReadTable(path string) ([]Handler, error) {
	return readTable(path, ExecAbsolute)
}

// ReadPluginTable is ReadTable for a built plugin's baked table, whose execs
// are plugin-root-relative.
func ReadPluginTable(path string) ([]Handler, error) {
	return readTable(path, ExecPluginRelative)
}

func readTable(path string, form ExecForm) ([]Handler, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Manifest
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateCommandsMode(path, t.Commands, form); err != nil {
		return nil, err
	}
	t.normalizeLanes()
	seen := map[string]bool{}
	for _, h := range t.Handlers {
		where := fmt.Sprintf("%s: handler %q", path, h.ID)
		if err := validateStatic(where, h, form); err != nil {
			return nil, err
		}
		if seen[h.ID] {
			return nil, fmt.Errorf("%s: handler id %q declared twice in table", path, h.ID)
		}
		seen[h.ID] = true
	}
	return t.Handlers, nil
}

// ResolvePluginExec returns the real path of exec under root, refusing one that
// resolves outside it.
//
// The containment check runs on symlink-resolved paths on both sides: a
// lexically local exec can still be a link out of the plugin, and a root that
// is itself reached through a link (a store path, a symlinked install dir)
// would otherwise make every target look outside it.
func ResolvePluginExec(root, exec string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("plugin root %q must be an absolute path", root)
	}
	if !filepath.IsLocal(exec) {
		return "", fmt.Errorf("exec %q must be relative to the plugin root with no .. segments", exec)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("plugin root %q: %w", root, err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, exec))
	if err != nil {
		return "", fmt.Errorf("exec %q: %w", exec, err)
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil {
		return "", fmt.Errorf("exec %q resolves to %s, outside the plugin root %s: %w", exec, target, realRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("exec %q resolves to %s, outside the plugin root %s", exec, target, realRoot)
	}
	return target, nil
}

// CheckPluginExecs is Load's exec check for build mode: every handler's exec
// must resolve inside root and be runnable there.
func CheckPluginExecs(root string, handlers []Handler) error {
	for _, h := range handlers {
		target, err := ResolvePluginExec(root, h.Exec)
		if err == nil {
			err = execIsRunnable(target)
		}
		if err != nil {
			return fmt.Errorf("handler %q: exec %q: %w", h.ID, h.Exec, err)
		}
	}
	return nil
}

// CheckCommandExecs is CheckPluginExecs' sibling for a manifest command's
// exec: same containment and runnable rules, so the two surfaces cannot
// drift apart the way a private reimplementation would let them.
func CheckCommandExecs(root string, commands []Command) error {
	for _, c := range commands {
		target, err := ResolvePluginExec(root, c.Exec)
		if err == nil {
			err = execIsRunnable(target)
		}
		if err != nil {
			return fmt.Errorf("command %q: exec %q: %w", c.Name, c.Exec, err)
		}
	}
	return nil
}
