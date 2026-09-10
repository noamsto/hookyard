// Package envelope turns each engine's native hook payload into one
// normalized shape hookyard's handlers read regardless of which engine sent
// it (docs/design/hookyard.md §7).
package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/noamsto/hookyard/internal/vocab"
)

// Envelope is the normalized shape every handler reads. Fields keep the
// engine-native json.RawMessage shapes rather than map[string]any: a
// map[string]any round-trip reorders keys and re-marshals a large integer in
// exponent form, which would break the "nothing is reshaped" promise below
// for tool_input and native without any fixture catching it
// (internal/render/orderedjson.go makes the same call for the same reason).
type Envelope struct {
	Engine         vocab.Engine               `json:"engine"`
	CanonicalEvent string                     `json:"canonical_event"`
	NativeEvent    string                     `json:"native_event"`
	SessionID      string                     `json:"session_id"`
	Cwd            string                     `json:"cwd"`
	Protocol       string                     `json:"protocol"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input,omitempty"`
	Native         map[string]json.RawMessage `json:"native"`
}

// ErrUnknownEngine means none of Detect's three discriminators matched.
// Detect never guesses an engine: a wrong guess would point a guard's
// tool-name branch at the wrong vocabulary, and a fail-open router would
// then abstain on every call on that engine forever with nothing visible to
// say why.
var ErrUnknownEngine = errors.New("envelope: unknown engine")

// Detect identifies which engine sent a payload from field presence alone,
// checked in this stated order:
//
//  1. cursor_version present -> Cursor
//  2. prompt_id or effort present -> Claude Code
//  3. turn_id present -> Codex
//
// "Present" means the key exists and is not JSON null: a null discriminator
// is an absent value, not evidence of an engine.
//
// The order is load-bearing only if the rules ever overlap. Across the ten
// captured fixtures they are disjoint, so first-match-wins picks the same
// engine any order would; an engine that later grows another's discriminator
// would silently be claimed by whichever rule sits higher.
//
// Known residual: these discriminators are observed only on
// PreToolUse/PostToolUse/UserPromptSubmit/beforeShellExecution payloads. No
// SessionStart or Stop payload has been captured for any engine, so
// detection on those event kinds is unverified.
func Detect(native map[string]json.RawMessage) (vocab.Engine, error) {
	if present(native, "cursor_version") {
		return vocab.Cursor, nil
	}
	if present(native, "prompt_id") || present(native, "effort") {
		return vocab.ClaudeCode, nil
	}
	if present(native, "turn_id") {
		return vocab.Codex, nil
	}
	return "", ErrUnknownEngine
}

func present(native map[string]json.RawMessage, key string) bool {
	raw, ok := native[key]
	if !ok {
		return false
	}
	return string(raw) != "null"
}

// From builds the normalized envelope for one engine's raw payload.
func From(engine vocab.Engine, native map[string]json.RawMessage) *Envelope {
	nativeEvent := stringField(native, "hook_event_name")
	canonicalEvent, _ := vocab.InboundEvent(engine, nativeEvent)

	e := &Envelope{
		Engine: engine,
		// Absent hook_event_name/session_id decode to "" via stringField: no
		// branch, no error.
		CanonicalEvent: canonicalEvent,
		NativeEvent:    nativeEvent,
		// session_id, not prompt_id/turn_id/generation_id: all three engines
		// send it, and the narrower per-engine id would leave two engines
		// unhandled.
		SessionID: stringField(native, "session_id"),
		Cwd:       cwd(native),
		Protocol:  vocab.Protocol(nativeEvent),
		ToolName:  vocab.InboundTool(engine, stringField(native, "tool_name")),
		// Passed through verbatim: Cursor's own tool_input.cwd == "" survives
		// as "". The fallback in cwd() below is envelope-level only.
		ToolInput: native["tool_input"],
		Native:    native,
	}
	synthesizeShell(e, native)
	return e
}

// cwd implements §7's cwd fallback, in the order the design calls
// non-negotiable: a non-empty payload cwd always wins, on every engine.
// Cursor is the one engine that sends cwd: "" while putting the real path in
// workspace_roots[0], so only an empty payload cwd falls back to it. An
// absent, empty, or non-string-array workspace_roots leaves cwd at "" rather
// than indexing an empty slice: From has no error return, so it degrades
// instead of panicking. A panicking router exits 2, which Cursor treats as a
// BLOCK with stderr as the reason — a fail-closed crash where the design
// wants fail-open.
func cwd(native map[string]json.RawMessage) string {
	if c := stringField(native, "cwd"); c != "" {
		return c
	}
	roots, ok := native["workspace_roots"]
	if !ok {
		return ""
	}
	var list []string
	if err := json.Unmarshal(roots, &list); err != nil {
		return ""
	}
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// synthesizeShell fills tool_name/tool_input for Cursor's
// beforeShellExecution, whose command lives at the payload's top level
// rather than under tool_input. Left alone, a handler subscribed to
// cursor:beforeShellExecution — the narrower, permission-capable matcher a
// security guard would prefer (§7) — reads an empty tool name and silently
// abstains. This is translation, not invention: §7 establishes Cursor's
// shell-tool identifier is literally Shell, which maps to Bash, and the
// command string is copied verbatim; the raw payload including the
// top-level command and sandbox stays in Native regardless.
//
// Only beforeShellExecution is fixture-backed; afterShellExecution may nest
// its command differently, so the guard conditions below degrade to no
// synthesis rather than a hollow one whenever they don't hold exactly.
func synthesizeShell(e *Envelope, native map[string]json.RawMessage) {
	if e.Protocol != "shell" {
		return
	}
	if present(native, "tool_name") {
		return
	}
	if present(native, "tool_input") {
		return
	}
	command := stringField(native, "command")
	if command == "" {
		// An empty command with tool_name synthesized to "Bash" is strictly
		// worse than leaving both alone: an empty tool_name makes a guard
		// abstain, but an empty command inside a well-formed-looking call
		// can make it ALLOW.
		return
	}
	// native["command"] is already a json.RawMessage holding a valid quoted
	// JSON string, so its bytes are reused directly rather than re-marshalled,
	// copying the command verbatim.
	e.ToolName = "Bash"
	e.ToolInput = append(append([]byte(`{"command":`), native["command"]...), '}')
}

func stringField(native map[string]json.RawMessage, key string) string {
	raw, ok := native[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// maxPayload is §4's inbound cap, counted as raw bytes read including any
// trailing newline an engine appends.
const maxPayload = 1 << 20 // 1 MiB

// ErrTooLarge means the payload exceeded maxPayload. Decode refuses it
// outright rather than truncating and re-parsing, which would change the
// meaning of the JSON.
var ErrTooLarge = errors.New("envelope: payload exceeds 1 MiB cap")

// Decode reads one native hook payload from r, detects its engine, and
// normalizes it. r is an io.Reader rather than os.Stdin: cmd/hookyard/main.go's
// route stays a stub, so a cap wired to the process's stdin would have
// nothing testable.
func Decode(r io.Reader) (*Envelope, error) {
	// Read one byte past the cap: a reader that just stops at maxPayload
	// cannot tell an exactly-at-limit payload from an over-limit one.
	raw, err := io.ReadAll(io.LimitReader(r, maxPayload+1))
	if err != nil {
		return nil, fmt.Errorf("envelope: reading payload: %w", err)
	}
	if len(raw) > maxPayload {
		return nil, ErrTooLarge
	}

	var native map[string]json.RawMessage
	if err := json.Unmarshal(raw, &native); err != nil {
		return nil, fmt.Errorf("envelope: decoding payload: %w", err)
	}
	if native == nil {
		// json.Unmarshal([]byte("null"), &m) sets m to nil with no error, so
		// the top-level JSON literal null needs its own check; a JSON array
		// or string already fails to unmarshal into this map type above.
		return nil, fmt.Errorf("envelope: payload is not a JSON object")
	}

	engine, err := Detect(native)
	if err != nil {
		return nil, err
	}
	return From(engine, native), nil
}
