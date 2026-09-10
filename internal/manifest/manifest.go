// Package manifest loads and validates the per-repo handler declarations that
// together form hookyard's single table (docs/design/hookyard.md §8).
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"

	"github.com/noamsto/hookyard/internal/vocab"
)

// MaxHandlerTimeoutMS is §4's per-handler sub-budget: the router's own
// deadline minus the consolidation margin. An override above it could not be
// honoured, so it is refused rather than silently clamped.
const MaxHandlerTimeoutMS = 4300

type Handler struct {
	ID        string   `json:"id"`
	Exec      string   `json:"exec"`
	Events    []string `json:"events"`
	Engines   []string `json:"engines"`
	Match     []string `json:"match"`
	TimeoutMS int      `json:"timeout_ms"`
}

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
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if len(m.Handlers) == 0 {
		return fmt.Errorf("%s: no handlers declared", m.Source)
	}
	seen := map[string]bool{}
	for _, h := range m.Handlers {
		if err := m.validateHandler(h); err != nil {
			return err
		}
		if seen[h.ID] {
			return fmt.Errorf("%s: handler id %q declared twice in one manifest", m.Source, h.ID)
		}
		seen[h.ID] = true
	}
	return nil
}

func (m *Manifest) validateHandler(h Handler) error {
	where := fmt.Sprintf("%s: handler %q", m.Source, h.ID)
	if !idPattern.MatchString(h.ID) {
		return fmt.Errorf("%s: id must match %s", where, idPattern)
	}
	if err := execIsRunnable(h.Exec); err != nil {
		return fmt.Errorf("%s: exec %q: %w", where, h.Exec, err)
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
	return validateCoverage(where, h, engines)
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
