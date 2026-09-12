package render

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/noamsto/hookyard/internal/atomicfile"
)

// cursorEntry shapes the rows hookyard itself writes into
// ~/.cursor/hooks.json. matcher is omitted when the entry matches every tool,
// which is how Cursor's own writers spell it; timeout is omitted too so this
// type stays safe if it is ever reused to encode a row with none.
//
// It is never used to decode an inherited row: Cursor's native converter, and
// presumably other writers, fill in fields this struct does not declare
// (loop_limit, failClosed), and decoding through it would silently drop them.
// Foreign rows are kept as raw JSON instead — see hooks below.
type cursorEntry struct {
	Command string `json:"command"`
	Matcher string `json:"matcher,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

// cursorRowCommand reads just the command field out of one hooks.json row,
// leaving every other field of the row untouched in its caller's raw bytes.
func cursorRowCommand(row json.RawMessage) (string, error) {
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(row, &probe); err != nil {
		return "", err
	}
	return probe.Command, nil
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

	hooks := map[string][]json.RawMessage{}
	if existing, ok := root.get("hooks"); ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			return fmt.Errorf("%s has a hooks key hookyard cannot read, refusing to overwrite it: %w", path, err)
		}
	}
	for event, rows := range hooks {
		kept := rows[:0]
		for _, row := range rows {
			command, err := cursorRowCommand(row)
			if err != nil {
				return fmt.Errorf("%s has a hooks row hookyard cannot read, refusing to overwrite it: %w", path, err)
			}
			if !strings.Contains(command, Marker) {
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
		row, err := json.Marshal(cursorEntry{
			Command: e.Command,
			Matcher: e.Matcher,
			Timeout: EmittedTimeoutSeconds,
		})
		if err != nil {
			return err
		}
		hooks[e.Event] = append(hooks[e.Event], row)
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
	// This is a file render does not own, so a pre-existing one keeps its
	// perm bits; one hookyard creates starts at 0600, matching how Cursor
	// ships its own.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return atomicfile.Write(path, out, mode)
}
