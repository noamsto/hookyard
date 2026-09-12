// Package render turns the merged handler table into each engine's native hook
// config (docs/design/hookyard.md §8).
//
// Every writer here is one more independent writer into a file it does not
// own: it strips only entries carrying hookyard's own marker, leaves every
// other writer's entries untouched, refuses a file it cannot parse rather than
// clobbering it, and lands its result through a single rename.
//
// Claude Code is the exception, and by construction: ClaudeSettings is a pure
// function over bytes, because the file its result belongs in is a Nix store
// link that hookyard must never be able to name for writing (#29).
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

// Marker identifies hookyard's own entries in a shared config file. It must
// not appear in any other writer's command string; §8 records the one time
// two writers' markers collided on this machine and what it cost.
const Marker = "/bin/hookyard"

// EmittedTimeoutSeconds bounds how long one stuck handler can hold a tool call
// (§4). Emitting it is not optional: Codex applies no default at all when an
// entry declares none, so an omitted timeout stalls the turn indefinitely.
const EmittedTimeoutSeconds = 5

// Entry is one hook registration in one engine's config.
type Entry struct {
	// Event is the engine's own native key.
	Event string
	// Matcher is the engine's native matcher; empty means every tool.
	Matcher string
	Command string
}

// Plan is what each engine's config should contain after rendering.
type Plan map[vocab.Engine][]Entry

type planKey struct {
	engine vocab.Engine
	event  string
}

// BuildPlan renders the table into one entry per engine per event. There is
// one entry rather than one per handler because the router is a single
// per-event exec that fans out internally (§4) — so the emitted matcher is the
// union of that event's handlers' matchers, and the router decides which
// handlers a given call actually reaches.
func BuildPlan(handlers []manifest.Handler, routerPath, stateDir string) (Plan, error) {
	if !strings.Contains(routerPath, Marker) {
		return nil, fmt.Errorf("router path %q does not contain the marker %q, so emitted entries "+
			"could not be found again to strip", routerPath, Marker)
	}

	matchers := map[planKey]map[string]bool{}
	everyTool := map[planKey]bool{}

	for _, h := range handlers {
		for _, name := range h.Engines {
			engine, err := vocab.ParseEngine(name)
			if err != nil {
				return nil, err
			}
			for _, event := range h.Events {
				if _, ok := vocab.NativeEvent(engine, event); !ok {
					continue // scoped to another engine; validation already allowed this
				}
				k := planKey{engine, event}
				if len(h.Match) == 0 {
					everyTool[k] = true
					continue
				}
				if matchers[k] == nil {
					matchers[k] = map[string]bool{}
				}
				for _, native := range vocab.NativeMatcher(engine, h.Match) {
					matchers[k][native] = true
				}
			}
		}
	}

	plan := Plan{}
	for _, k := range planKeys(matchers, everyTool) {
		native, _ := vocab.NativeEvent(k.engine, k.event)
		entry := Entry{
			Event:   native,
			Command: command(routerPath, k.engine, k.event, stateDir),
		}
		if !everyTool[k] {
			entry.Matcher = strings.Join(sorted(matchers[k]), "|")
		}
		plan[k.engine] = append(plan[k.engine], entry)
	}
	for engine := range plan {
		sort.Slice(plan[engine], func(i, j int) bool {
			if plan[engine][i].Event != plan[engine][j].Event {
				return plan[engine][i].Event < plan[engine][j].Event
			}
			return plan[engine][i].Matcher < plan[engine][j].Matcher
		})
	}
	return plan, nil
}

// command tags each rendering with the engine it was registered for. That tag
// is the router's only way to spot a cross-registration delivery: Cursor reads
// Claude Code's settings as a hook source, and no payload says which config
// asked for the call (§8, sink 4).
func command(routerPath string, engine vocab.Engine, event, stateDir string) string {
	return fmt.Sprintf("%s route --registered-for %s --event %s --state-dir %s", routerPath, engine, event, stateDir)
}

func planKeys(matchers map[planKey]map[string]bool, everyTool map[planKey]bool) []planKey {
	seen := map[planKey]bool{}
	for k := range matchers {
		seen[k] = true
	}
	for k := range everyTool {
		seen[k] = true
	}
	out := make([]planKey, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
