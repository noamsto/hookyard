// Package vocab holds the translation tables between hookyard's normalized
// vocabulary and each engine's native one.
//
// Every mapping here was read off a shipped engine build or a captured hook
// payload rather than documentation; docs/design/hookyard.md §7 records which.
package vocab

import (
	"fmt"
	"strings"
)

type Engine string

const (
	ClaudeCode Engine = "claude-code"
	Codex      Engine = "codex"
	Cursor     Engine = "cursor"
)

var Engines = []Engine{ClaudeCode, Codex, Cursor}

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

// The six canonical events, one per concept that all three engines share.
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
