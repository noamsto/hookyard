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

type Manifest struct {
	Handlers []Handler `json:"handlers"`

	// Source is the path this manifest was read from, used to name both sides
	// of a collision.
	Source string `json:"-"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./-]*$`)

// eventPattern constrains the native half of an engine-scoped event name.
// Those names reach a TOML array-of-table header verbatim, where an encoder
// cannot protect them, so the character set is the protection (§8).
var eventPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// Load reads and validates one manifest. A manifest that cannot be parsed, or
// that describes a handler hookyard could not run, fails here rather than
// after some of three engines' configs have been rewritten.
func Load(path string) (*Manifest, error) {
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
	if err := m.validateAll(validateExec); err != nil {
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

// validateAll holds every rule Load and LoadStatic share, so the two cannot
// drift into disagreeing about which manifest files are legal — a build that
// accepts one activation then refuses fails after the store paths are already
// realised. extra runs per handler on top of the shared rules; it is nil for
// the caller that cannot afford to touch the filesystem.
func (m *Manifest) validateAll(extra func(where string, h Handler) error) error {
	if len(m.Handlers) == 0 {
		return fmt.Errorf("%s: no handlers declared", m.Source)
	}
	seen := map[string]bool{}
	for _, h := range m.Handlers {
		where := fmt.Sprintf("%s: handler %q", m.Source, h.ID)
		if err := validateStatic(where, h); err != nil {
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
func validateStatic(where string, h Handler) error {
	if !idPattern.MatchString(h.ID) {
		return fmt.Errorf("%s: id must match %s", where, idPattern)
	}
	// Same rule §9 applies to the emitted router command, applied to handlers.
	// A relative exec resolves at hook-fire time against the directory the
	// agent's tool call runs in, and a bare name against the PATH the engine
	// hands down — neither is hookyard's to choose, so either would let a file
	// that happens to sit there stand in for the guard.
	if !filepath.IsAbs(h.Exec) {
		return fmt.Errorf("%s: exec %q must be an absolute path, because it is resolved at "+
			"hook-fire time against the agent's working directory and PATH, not the installer's",
			where, h.Exec)
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
// the engine would hand it a decision slot. The router never waits for a
// fire-and-forget handler (§4), so a verdict computed
// there has nowhere to go — the handler's author would reasonably believe it
// guards a call it in fact never can.
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
			if verdict.HasDecisionSlot(engine, canonical, native) {
				return fmt.Errorf("%s: event %q on %s has a decision slot, but a fire-and-forget "+
					"handler never returns a verdict so it cannot guard this event", where, event, engine)
			}
		}
	}
	return nil
}

// resolveEvent fills both halves of a manifest event name for engine.
// HasDecisionSlot asks about a canonical name and a native one, and a
// manifest carries only ever one of them.
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

// LoadStatic reads and validates one manifest the way Load does, minus
// execIsRunnable: it runs validateStatic alone, the same rule ReadTable
// already carves out and for the same reason (its doc comment explains the
// stat is redundant on the critical path; here the stat is worse than
// redundant, it is wrong). `emit` runs inside a Nix build sandbox, where a
// manifest's `exec` may be an ordinary absolute path like `/home/you/bin/
// guard` that simply does not exist yet — it will, at activation, when
// `install` runs and re-validates through Load. So `emit` must not fail a
// build over a manifest `install` would accept minutes later in the same
// activation (R7).
//
// The narrowing is exactly one check wide, and every other rule stays shared
// through validateAll. validateStatic still refuses a relative or bare exec —
// that rule lives there, not in the stat, so nothing about §9's hook-fire-time
// argument is given up. A file declaring zero handlers is still refused, as it
// is under Load: `install --allow-empty` is about a caller passing no
// `--manifest` flag at all, not about a manifest file whose handlers array is
// empty, so accepting one here would let a Nix build succeed over a manifest
// set activation then rejects. Duplicate ids within one file are still
// refused, matching both Load and ReadTable — only Merge's cross-manifest
// check is left to the caller, who must still run it (R7): LoadStatic dedupes
// one file, not a caller's whole --manifest list.
func LoadStatic(path string) (*Manifest, error) {
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
	if err := m.validateAll(nil); err != nil {
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
	return all, nil
}

// WriteTable writes the consolidated handler table, in the same shape as a
// manifest, at a fixed 0600: the table is hookyard's own file, so a
// pre-existing one left looser must be tightened rather than honoured.
func WriteTable(path string, handlers []Handler) error {
	raw, err := json.Marshal(Manifest{Handlers: handlers})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw, 0o600)
}

// ReadTable reads the table WriteTable produces and re-runs every validation
// rule that does not touch the filesystem, because a hand-edited or stale
// table must not be trusted on the critical path just because some earlier
// install was well-behaved. Unlike Load, it does not call execIsRunnable and
// does not reject a zero-handler table: "nothing is registered" is a
// legitimate table state, distinct from "the table is gone".
func ReadTable(path string) ([]Handler, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Manifest
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t.normalizeLanes()
	seen := map[string]bool{}
	for _, h := range t.Handlers {
		where := fmt.Sprintf("%s: handler %q", path, h.ID)
		if err := validateStatic(where, h); err != nil {
			return nil, err
		}
		if seen[h.ID] {
			return nil, fmt.Errorf("%s: handler id %q declared twice in table", path, h.ID)
		}
		seen[h.ID] = true
	}
	return t.Handlers, nil
}
