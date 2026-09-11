package vocab

import "fmt"

// buildInboundTools inverts each engine's column of a native-tools table
// (normalized name -> native name) into native name -> normalized name, for
// translating an inbound payload's tool_name back to hookyard's vocabulary.
//
// It takes src as a parameter, rather than closing over the package-level
// nativeTools, so a test can feed it a synthetic non-injective column without
// touching the real table.
func buildInboundTools(src map[Engine]map[string]string) (map[Engine]map[string]string, error) {
	inverted := make(map[Engine]map[string]string, len(src))
	for engine, column := range src {
		byNative := make(map[string]string, len(column))
		for normalized, native := range column {
			if prev, collide := byNative[native]; collide {
				return nil, fmt.Errorf("vocab: %s: native tool %q is the target of both %q and %q, "+
					"so inbound payloads naming it cannot be inverted unambiguously", engine, native, prev, normalized)
			}
			byNative[native] = normalized
		}
		inverted[engine] = byNative
	}
	return inverted, nil
}

// inboundTools is nativeTools inverted once at package init. A collision here
// would mean the compiled-in nativeTools table itself is broken — not
// something any caller's input could trigger — so it panics rather than
// threading an error through every caller, the same tradeoff regexp.MustCompile
// makes. The design deliberately keeps open Edit->Write for Cursor and
// apply_patch->Edit for Codex; the first such row fails here at startup rather
// than resolving itself by map iteration order on some later call.
var inboundTools = mustInboundTools(nativeTools)

func mustInboundTools(src map[Engine]map[string]string) map[Engine]map[string]string {
	inverted, err := buildInboundTools(src)
	if err != nil {
		panic(err)
	}
	return inverted
}

// InboundTool resolves a payload's native tool_name back to hookyard's
// normalized vocabulary. A native name this engine has no mapping for is
// returned unchanged: dropping it would turn "a tool hookyard has no mapping
// for" into "no tool at all", which reads to a manifest matcher as a
// malformed payload rather than an unrecognized one.
func InboundTool(engine Engine, native string) string {
	if normalized, ok := inboundTools[engine][native]; ok {
		return normalized
	}
	return native
}

// inboundEvents maps (engine, native payload spelling) -> canonical event
// name. It is keyed by engine rather than flat because the same canonical
// concept is spelled differently by different engines in ways that collide
// case-insensitively: Cursor's preToolUse and Claude Code's PreToolUse differ
// only in case, and a flat map would canonicalize a Cursor payload spelled
// Claude Code's way (or vice versa) instead of rejecting it.
//
// This table is deliberately NOT nativeEvents inverted. Codex uses three
// different spellings for three different surfaces: [[hooks.X]] config block
// keys are CamelCase (PreToolUse, per a live ~/.codex/config.toml and §8 —
// the spelling nativeEvents' Codex column renders, consumed by
// internal/render/codex.go as a TOML [[hooks.%s]] header), [hooks.state]
// trust keys are snake_case (pre_tool_use, per §7), and this is
// the third: hook_event_name in the payload itself, which the two Codex
// fixtures show is CamelCase again. nativeEvents' Codex column is correct for
// its own job of rendering config headers; it is simply answering a different
// question than "what spelling appears in an inbound payload", and one engine
// demonstrably uses different spellings for different surfaces, so payload
// and config spellings are not guaranteed to move together for any engine.
var inboundEvents = map[Engine]map[string]string{
	ClaudeCode: {
		"SessionStart":     SessionStart, // assumed
		"UserPromptSubmit": PromptSubmit, // assumed
		"PreToolUse":       PreTool,      // observed: claude-PreToolUse.json
		"PostToolUse":      PostTool,     // observed: claude-PostToolUse.json
		"PreCompact":       PreCompact,   // assumed
		"Stop":             TurnEnd,      // assumed
	},
	Codex: {
		"SessionStart":     SessionStart, // assumed
		"UserPromptSubmit": PromptSubmit, // observed: codex-user_prompt_submit-TICK.json
		"PreToolUse":       PreTool,      // observed: codex-pre_tool_use.json
		"PostToolUse":      PostTool,     // assumed
		"PreCompact":       PreCompact,   // assumed
		"Stop":             TurnEnd,      // assumed
	},
	Cursor: {
		"sessionStart":       SessionStart, // assumed
		"beforeSubmitPrompt": PromptSubmit, // assumed
		"preToolUse":         PreTool,      // observed: cursor-preToolUse.json
		"postToolUse":        PostTool,     // observed: cursor-postToolUse.json
		"preCompact":         PreCompact,   // assumed
		"stop":               TurnEnd,      // assumed
	},
	// Pi's payload spelling is nativeEvents' Pi column verbatim, because
	// hookyard authors both — unlike Codex, there is no second surface to
	// diverge from.
	Pi: {
		"session_start":          SessionStart, // observed: pi-session_start.json
		"input":                  PromptSubmit, // observed: pi-input.json
		"tool_call":              PreTool,      // observed: pi-tool_call.json
		"tool_result":            PostTool,     // observed: pi-tool_result.json
		"session_before_compact": PreCompact,   // inferred, not captured
		"turn_end":               TurnEnd,      // observed: pi-turn_end.json
	},
}

// InboundEvent resolves a payload's hook_event_name to hookyard's canonical
// event name, scoped to the engine that sent it. It returns ("", false) for a
// spelling this engine doesn't use — the engine-scoped path, not an error: an
// unrecognized event is something a caller decides how to handle, not a
// malformed-input failure.
func InboundEvent(engine Engine, native string) (string, bool) {
	canonical, ok := inboundEvents[engine][native]
	return canonical, ok
}

// protocolSplit maps Cursor's protocol-split native event names (§7) to the
// field hookyard's envelope records the split under. Only beforeShellExecution
// is fixture-backed (cursor-beforeShellExecution.json); the rest come from
// §7's enumeration of the pattern, not a captured payload.
var protocolSplit = map[string]string{
	"beforeShellExecution": "shell", // observed
	"afterShellExecution":  "shell", // assumed
	"beforeMCPExecution":   "mcp",   // assumed
	"afterMCPExecution":    "mcp",   // assumed
	"beforeReadFile":       "file",  // assumed
	"afterFileEdit":        "file",  // assumed
	"beforeTabFileRead":    "file",  // assumed
	"afterTabFileEdit":     "file",  // assumed
}

// Protocol reports the protocol-split field a Cursor native event name
// belongs to, or "" for an event that isn't protocol-split — including
// Cursor's deployed preToolUse, which is a single tool-call event rather than
// a shell/mcp/file split.
func Protocol(native string) string {
	return protocolSplit[native]
}
