package render

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// cursorEntry is one row in ~/.cursor/hooks.json. matcher is omitted when the
// entry matches every tool, which is how Cursor's own writers spell it.
type cursorEntry struct {
	Command string `json:"command"`
	Matcher string `json:"matcher,omitempty"`
	Timeout int    `json:"timeout"`
}

// WriteCursor renders entries into ~/.cursor/hooks.json as one more
// independent writer beside the ones already there (§8).
//
// The strip is whole-file rather than scoped to the event keys hookyard is
// about to write: the table can legitimately move a handler from one event to
// another between versions, and a key-scoped strip would leave the old entry
// behind under the old key, firing forever and owned by nobody.
func WriteCursor(path string, entries []Entry) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	root, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON, refusing to overwrite it: %w", path, err)
	}

	hooks := map[string][]cursorEntry{}
	if existing, ok := root.get("hooks"); ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			return fmt.Errorf("%s has a hooks key hookyard cannot read, refusing to overwrite it: %w", path, err)
		}
	}
	for event, rows := range hooks {
		kept := rows[:0]
		for _, row := range rows {
			if !strings.Contains(row.Command, Marker) {
				kept = append(kept, row)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}
	for _, e := range entries {
		hooks[e.Event] = append(hooks[e.Event], cursorEntry{
			Command: e.Command,
			Matcher: e.Matcher,
			Timeout: EmittedTimeoutSeconds,
		})
	}

	if err := root.set("version", 1); err != nil {
		return err
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
