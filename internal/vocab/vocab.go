// Package vocab holds the translation tables between hookyard's normalized
// vocabulary and each engine's native one.
//
// Every mapping here was read off a shipped engine build or a captured hook
// payload rather than documentation; docs/design/hookyard.md §7 records which.
package vocab

import (
	"fmt"
	"slices"
	"strings"
)

type Engine string

const (
	ClaudeCode Engine = "claude-code"
	Codex      Engine = "codex"
	Cursor     Engine = "cursor"
	Pi         Engine = "pi"
)

var Engines = []Engine{ClaudeCode, Codex, Cursor, Pi}

func ParseEngine(s string) (Engine, error) {
	for _, e := range Engines {
		if Engine(s) == e {
			return e, nil
		}
	}
	return "", fmt.Errorf("unknown engine %q (want one of %s)", s, strings.Join(engineNames(), ", "))
}

func engineNames() []string {
	names := make([]string, len(Engines))
	for i, e := range Engines {
		names[i] = string(e)
	}
	return names
}

// The six canonical events, one per concept that all four engines share.
const (
	SessionStart = "session_start"
	PromptSubmit = "prompt_submit"
	PreTool      = "pre_tool"
	PostTool     = "post_tool"
	PreCompact   = "pre_compact"
	TurnEnd      = "turn_end"
)

var CanonicalEvents = []string{SessionStart, PromptSubmit, PreTool, PostTool, PreCompact, TurnEnd}

// Cursor's pre_tool renders to preToolUse alone, never alongside
// beforeShellExecution: both fire for one shell call on the allow path, and a
// deny at preToolUse short-circuits before the protocol-split event runs, so
// registering both double-fires every allowed call (§8).
var nativeEvents = map[Engine]map[string]string{
	ClaudeCode: {
		SessionStart: "SessionStart",
		PromptSubmit: "UserPromptSubmit",
		PreTool:      "PreToolUse",
		PostTool:     "PostToolUse",
		PreCompact:   "PreCompact",
		TurnEnd:      "Stop",
	},
	Codex: {
		SessionStart: "SessionStart",
		PromptSubmit: "UserPromptSubmit",
		PreTool:      "PreToolUse",
		PostTool:     "PostToolUse",
		PreCompact:   "PreCompact",
		TurnEnd:      "Stop",
	},
	Cursor: {
		SessionStart: "sessionStart",
		PromptSubmit: "beforeSubmitPrompt",
		PreTool:      "preToolUse",
		PostTool:     "postToolUse",
		PreCompact:   "preCompact",
		TurnEnd:      "stop",
	},
	// hookyard authors both the config spelling and the payload shape for Pi:
	// extensions subscribe with pi.on(event, handler), so there is no separate
	// config-key vocabulary to diverge from the wire spelling the way Codex's
	// does (§8). PreCompact is inferred from Pi's documented lifecycle rather
	// than captured: no session_before_compact fixture exists, and Pi also
	// exposes session_compact/session_compact_failed, so which of the three
	// actually fires — and whether a handler there can act — is unverified.
	Pi: {
		SessionStart: "session_start",
		PromptSubmit: "input",
		PreTool:      "tool_call",
		PostTool:     "tool_result",
		PreCompact:   "session_before_compact", // inferred, not captured
		TurnEnd:      "turn_end",
	},
}

// No canonical session_end exists: the six canonical concepts above are the
// only ones all engines share, and "the agent finished and is idle" is not one
// of them. The lifecycle signals that fill that gap are therefore routed under
// explicit engine-scoped names, each read off a shipped build:
//
//   - pi:agent_settled  - pi 0.87.0 docs/extensions.md ("final and
//     notification-only"); the shipped binary emits it from the finally of the
//     agent run, so an aborted (Esc) run settles too.
//   - codex:SessionEnd  - codex 0.154.0 binary HookEventsToml enum plus its
//     embedded session-end.command.input schema (fields: cwd, hook_event_name,
//     reason, session_id, transcript_path). The schema carries no turn_id, so
//     envelope.Detect cannot place the payload; yard mode's --registered-for
//     fallback does.
//   - cursor:sessionEnd - cursor-agent 2026.09.18's shipped hook-step enum and
//     executeHookForStep(_E.sessionEnd, ...).
//
// Codex and Cursor have no catalog: SplitEngineScoped resolves any
// eventPattern-shaped engine-scoped name for them, so they need only this
// documentation, while Pi's catalog gates what a manifest may name.

// ClaudeCodeEvent pairs a Claude Code native hook key with the --event value
// hookyard's router command uses to route it.
type ClaudeCodeEvent struct {
	Native string // Claude Code hook key, e.g. "Notification"
	Routed string // route --event value: canonical name, or "claude-code:<Native>"
}

// ClaudeCodeCatalog is the fixed set of Claude Code hook events yard mode
// registers (R-A/R-B) and the only ones a manifest may name as
// "claude-code:<Native>" (R-C). The shipped claude-code-2.1.272 executable
// (.../claude-code-2.1.272/bin/.claude-wrapped) declares its hook-event enum
// as a literal 33-name array — found with
// `rg -a -o '\["PreToolUse","PostToolUse"[^\]]{0,800}\]'`, which is a superset
// containing all eight rows below — and a `hook_event_name:"<Name>"` payload
// literal for each of the eight, found with
// `rg -a -o 'hook_event_name:"[A-Za-z]+"'`. The catalog is deliberately not
// the full 33: every row costs a router spawn per occurrence (§4), and
// nothing consumes the rest. Notification and SessionEnd are the two houston
// already relies on (nix-config home/ai/houston/default.nix:57-58); a new row
// is added only when a consumer needs it, and R-C makes that need loud rather
// than silently registering nothing.
var ClaudeCodeCatalog = buildClaudeCodeCatalog()

func buildClaudeCodeCatalog() []ClaudeCodeEvent {
	catalog := make([]ClaudeCodeEvent, 0, len(CanonicalEvents)+2)
	for _, canonical := range CanonicalEvents {
		catalog = append(catalog, ClaudeCodeEvent{Native: nativeEvents[ClaudeCode][canonical], Routed: canonical})
	}
	return append(catalog,
		ClaudeCodeEvent{Native: "Notification", Routed: "claude-code:Notification"},
		ClaudeCodeEvent{Native: "SessionEnd", Routed: "claude-code:SessionEnd"},
	)
}

// IsClaudeCodeEvent reports whether native is one of ClaudeCodeCatalog's
// eight Claude Code hook keys.
func IsClaudeCodeEvent(native string) bool {
	for _, e := range ClaudeCodeCatalog {
		if e.Native == native {
			return true
		}
	}
	return false
}

// PiCatalog is the fixed set of pi native events hookyard routes: the six
// canonical natives, in CanonicalEvents order, plus session_shutdown and
// agent_settled, which pi fires with no canonical counterpart. agent_settled
// is pi's terminal "the run is settled and will not continue automatically"
// lifecycle event (pi 0.87.0 docs/extensions.md, docs/rpc.md); the shipped
// binary emits it from the finally of the agent run, so an aborted run (Esc)
// settles too — which is what lets a dashboard tell the final turn from an
// intermediate tool turn, since turn_end fires after every LLM response.
// before_agent_start is
// deliberately absent — the bridge registers it itself, outside any manifest
// event, solely to flush a queued session_start advisory. A
// manifest naming pi:before_agent_start would ask hookyard to register a
// second handler for that one event, one of which would answer a protocol
// the router does not speak.
//
// Unlike ClaudeCodeCatalog, PiCatalog is a []string of native names rather
// than a []ClaudeCodeEvent pair: every consumer here (IsPiEvent, the
// envelope arm, manifest validation) asks about the native name only, and
// ClaudeCodeEvent.Routed exists to drive render.ClaudeCatalogPlan, which has
// no pi counterpart — a second field nothing reads would just be dead weight.
var PiCatalog = buildPiCatalog()

func buildPiCatalog() []string {
	catalog := make([]string, 0, len(CanonicalEvents)+2)
	for _, canonical := range CanonicalEvents {
		catalog = append(catalog, nativeEvents[Pi][canonical])
	}
	return append(catalog, "session_shutdown", "agent_settled")
}

// IsPiEvent reports whether native is one of PiCatalog's members.
func IsPiEvent(native string) bool {
	return slices.Contains(PiCatalog, native)
}

// NormalizedTools is Claude Code's own tool vocabulary, which §7 normalizes
// onto. A manifest matcher outside this set is rejected on entry.
var NormalizedTools = []string{"Read", "Write", "Bash", "Grep", "Glob"}

// An absent key means the engine has no equivalent: Cursor drops a Glob
// matcher silently, and Codex exposes no grep or glob tool to match on. Both
// are why a match that renders empty for a claimed engine fails validation
// instead of shipping a handler that never fires.
var nativeTools = map[Engine]map[string]string{
	ClaudeCode: {
		"Read": "Read", "Write": "Write", "Bash": "Bash", "Grep": "Grep", "Glob": "Glob",
	},
	Codex: {
		"Read": "view_image", "Write": "apply_patch", "Bash": "Bash",
	},
	Cursor: {
		"Read": "Read", "Write": "Write", "Bash": "Shell", "Grep": "Grep",
	},
	// Pi is the first engine with no gaps: read live off pi.getAllTools(),
	// whose own description for find is "Search for files by glob pattern."
	Pi: {
		"Read": "read", "Write": "write", "Bash": "bash", "Grep": "grep", "Glob": "find",
	},
}

func IsCanonicalEvent(name string) bool {
	_, ok := nativeEvents[ClaudeCode][name]
	return ok
}

func IsNormalizedTool(name string) bool {
	_, ok := nativeTools[ClaudeCode][name]
	return ok
}

// SplitEngineScoped reports whether name is an engine-scoped event
// ("cursor:beforeShellExecution") and returns its parts.
func SplitEngineScoped(name string) (Engine, string, bool) {
	prefix, native, found := strings.Cut(name, ":")
	if !found {
		return "", "", false
	}
	engine, err := ParseEngine(prefix)
	if err != nil {
		return "", "", false
	}
	return engine, native, true
}

// NativeEvent resolves an event name to the key engine uses for it. An
// engine-scoped name resolves only for its own engine.
func NativeEvent(engine Engine, event string) (string, bool) {
	if scoped, native, ok := SplitEngineScoped(event); ok {
		if scoped != engine {
			return "", false
		}
		return native, true
	}
	native, ok := nativeEvents[engine][event]
	return native, ok
}

// NativeMatcher renders normalized tool names into engine's matcher syntax,
// dropping tools the engine has no equivalent for. It returns the surviving
// native names so a caller can tell a partial translation from a total one.
func NativeMatcher(engine Engine, tools []string) []string {
	var native []string
	seen := map[string]bool{}
	for _, t := range tools {
		n, ok := nativeTools[engine][t]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		native = append(native, n)
	}
	return native
}
