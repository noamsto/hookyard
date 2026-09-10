package render

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Claude Code nests one level deeper than the other two: a matcher group owns
// an array of hooks, so a matcher is shared by the hooks under it.
type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type claudeGroup struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []claudeHook `json:"hooks"`
}

// WriteClaude renders entries into ~/.claude/settings.json.
//
// Claude Code unions hooks across its user, local and flag settings sources
// rather than letting one override another (§8), so this writer neither needs
// nor gets exclusive ownership of the event keys it writes — the Nix
// --settings overlay keeps its own entries, and both fire. What that makes
// load-bearing is the strip below: registered twice means run twice.
func WriteClaude(path string, entries []Entry) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	root, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON, refusing to overwrite it: %w", path, err)
	}

	hooks := map[string][]claudeGroup{}
	if existing, ok := root.get("hooks"); ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			return fmt.Errorf("%s has a hooks key hookyard cannot read, refusing to overwrite it: %w", path, err)
		}
	}
	for event, groups := range hooks {
		keptGroups := groups[:0]
		for _, group := range groups {
			keptHooks := group.Hooks[:0]
			for _, h := range group.Hooks {
				if !strings.Contains(h.Command, Marker) {
					keptHooks = append(keptHooks, h)
				}
			}
			if len(keptHooks) == 0 {
				continue
			}
			group.Hooks = keptHooks
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = keptGroups
	}
	for _, e := range entries {
		hooks[e.Event] = append(hooks[e.Event], claudeGroup{
			Matcher: e.Matcher,
			Hooks: []claudeHook{{
				Type:    "command",
				Command: e.Command,
				Timeout: EmittedTimeoutSeconds,
			}},
		})
	}

	if len(hooks) == 0 {
		root.delete("hooks")
	} else if err := root.set("hooks", hooks); err != nil {
		return err
	}
	out, err := root.marshalIndent()
	if err != nil {
		return err
	}
	return writeAtomic(path, out)
}
