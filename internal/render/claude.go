package render

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Claude Code nests one level deeper than the other two: a matcher group owns
// an array of hooks, so a matcher is shared by the hooks under it. claudeEntry
// and claudeHook shape only the group hookyard itself writes.
//
// Neither is ever used to decode an inherited entry. Timeout carries no
// omitempty because hookyard's own rows must always declare one (§4), and that
// same field re-encoded onto an inherited row that declared none is measured,
// twice against the live claude binary, to stop the hook firing at all — the
// consumer's overlay declares no timeout anywhere, so every one of its guards
// would have gone dead with no error. Inherited groups and hooks stay raw JSON
// instead, the discipline WriteCursor already uses for the same root cause.
type claudeEntry struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []claudeHook `json:"hooks"`
}

type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// claudeGroupHooks reads just the hooks array out of one matcher group,
// leaving every other field of the group untouched in its caller's raw bytes.
func claudeGroupHooks(group json.RawMessage) ([]json.RawMessage, error) {
	var probe struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(group, &probe); err != nil {
		return nil, err
	}
	return probe.Hooks, nil
}

// claudeHookCommand reads just the command field out of one hook entry, which
// is the whole of what the strip needs to know about it.
func claudeHookCommand(hook json.RawMessage) (string, error) {
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(hook, &probe); err != nil {
		return "", err
	}
	return probe.Command, nil
}

// claudeGroupKeeping re-encodes one matcher group around the hooks that
// survived the strip. It goes through object rather than a struct so the
// group's own key order, and any field beyond matcher and hooks, survive.
func claudeGroupKeeping(group json.RawMessage, hooks []json.RawMessage) (json.RawMessage, error) {
	obj, err := parseObject(group)
	if err != nil {
		return nil, err
	}
	if err := obj.set("hooks", hooks); err != nil {
		return nil, err
	}
	return obj.marshalCompact()
}

// ClaudeSettings merges entries into base and returns the document Nix places
// as Claude Code's --settings overlay. It is a pure function and not a writer:
// hookyard has no code path that can name ~/.claude/settings.json for writing.
//
// Claude Code unions hooks across its user, local and flag settings sources
// rather than letting one override another (§8), so this merge neither needs
// nor gets exclusive ownership of the event keys it writes — the base's own
// entries keep firing beside hookyard's. What that makes load-bearing is the
// strip below: registered twice means run twice.
func ClaudeSettings(base []byte, entries []Entry) ([]byte, error) {
	root, err := parseObject(base)
	if err != nil {
		return nil, fmt.Errorf("the base is not valid JSON, refusing to render over it: %w", err)
	}

	// The event keys live in an object rather than a Go map so the base's own
	// order among them survives; a map re-sorts them on every render.
	existing, _ := root.get("hooks")
	hooks, err := parseObject(existing)
	if err != nil {
		return nil, fmt.Errorf("the base has a hooks key hookyard cannot read, refusing to render over it: %w", err)
	}

	// delete rewrites hooks' key slice, so the walk is over a copy of it.
	for _, event := range append([]string(nil), hooks.keys...) {
		raw, _ := hooks.get(event)
		var groups []json.RawMessage
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, fmt.Errorf("the base has a %s hook list hookyard cannot read, refusing to render over it: %w", event, err)
		}
		keptGroups := groups[:0]
		for _, group := range groups {
			inherited, err := claudeGroupHooks(group)
			if err != nil {
				return nil, fmt.Errorf("the base has a %s hook group hookyard cannot read, refusing to render over it: %w", event, err)
			}
			keptHooks := inherited[:0]
			for _, hook := range inherited {
				command, err := claudeHookCommand(hook)
				if err != nil {
					return nil, fmt.Errorf("the base has a %s hook hookyard cannot read, refusing to render over it: %w", event, err)
				}
				if !strings.Contains(command, Marker) {
					keptHooks = append(keptHooks, hook)
				}
			}
			if len(keptHooks) == 0 {
				continue
			}
			kept, err := claudeGroupKeeping(group, keptHooks)
			if err != nil {
				return nil, fmt.Errorf("the base has a %s hook group hookyard cannot read, refusing to render over it: %w", event, err)
			}
			keptGroups = append(keptGroups, kept)
		}
		if len(keptGroups) == 0 {
			hooks.delete(event)
			continue
		}
		if err := hooks.set(event, keptGroups); err != nil {
			return nil, err
		}
	}
	for _, e := range entries {
		group, err := json.Marshal(claudeEntry{
			Matcher: e.Matcher,
			Hooks: []claudeHook{{
				Type:    "command",
				Command: e.Command,
				Timeout: EmittedTimeoutSeconds,
			}},
		})
		if err != nil {
			return nil, err
		}
		var groups []json.RawMessage
		if raw, ok := hooks.get(e.Event); ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return nil, err
			}
		}
		if err := hooks.set(e.Event, append(groups, group)); err != nil {
			return nil, err
		}
	}

	if len(hooks.keys) == 0 {
		root.delete("hooks")
	} else {
		nested, err := hooks.marshalCompact()
		if err != nil {
			return nil, err
		}
		if err := root.set("hooks", json.RawMessage(nested)); err != nil {
			return nil, err
		}
	}
	return root.marshalIndent()
}
