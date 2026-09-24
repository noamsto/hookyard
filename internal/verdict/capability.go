package verdict

import "github.com/noamsto/hookyard/internal/vocab"

// cursorScopedDecisionEvents is the engine-scoped half of Cursor's
// decision-capable set: the native events §7 records Cursor honours
// `permission` on beyond preToolUse, read off the shipped cursor-agent bundle.
var cursorScopedDecisionEvents = map[string]bool{
	"beforeShellExecution": true,
	"beforeMCPExecution":   true,
	"beforeReadFile":       true,
	"beforeTabFileRead":    true,
	"subagentStart":        true,
}

// HasDecisionSlot reports whether engine can act on a verdict for this event.
// permissionDecision is a PreToolUse contract, not a universal one, and a
// manifest may register a handler on any of the six canonical events or on an
// engine-scoped one — so route runs well outside the permission path. Emitting
// a decision there is worse than useless: the engine ignores it, or on Codex,
// whose validator rejects fields it does not know, rejects the whole response.
//
// claude-code:PermissionRequest is absent deliberately, not by oversight. §4
// confirms it is a real decision channel on both Claude Code and Codex, but it
// takes a different wire shape (decision.behavior) that no captured payload
// covers, so a verdict on it is computed and recorded unenforced rather than
// rendered half-confirmed.
//
// Pi's input event (prompt_submit) can suppress a turn, but it carries no
// reason channel back to the model — unlike pre_tool's block reason — so it
// is deliberately not a decision slot rather than an oversight left for later.
//
// turn_end (canonical, native agent_before_settle on pi, Stop on Claude Code
// and Codex) is a decision slot on all three engines: a consolidated deny
// there means "do not stop yet — continue with this reason", the same
// lattice as pre_tool's block, not a new verdict value. On pi this is D3,
// docs/design/hookyard.md §11.2; on Claude Code and Codex it is rendered on
// Stop as a top-level decision:block (§11.3) — evidence: claude-Stop.json
// plus the installed claude-code 2.1.281 bundle's hook-output schema for
// Claude Code, and Codex source at openai/codex rust-v0.156.1
// (stop.command.output.schema.json, output_parser.rs stop_output,
// core/src/session/turn.rs) for Codex. `enforced: true` on a turn_end deny
// means hookyard rendered a block the engine acted on, not that the run
// actually continued: pi may still decline it (canContinue false, or an
// abort during before_settle), the same class of gap pre_tool's `enforced`
// already accepts. The loop bound differs by engine: pi's bridge caps a
// settle deny to one continuation per bridge per run (pi_bridge.ts); Claude
// Code and Codex instead bound it with the native stop_hook_active, set by
// the engine after any Stop hook's block and shared by every Stop hook the
// engine runs that turn — Codex has no engine-side cap at all, and Claude
// Code only a warn-and-override backstop (CLAUDE_CODE_STOP_HOOK_BLOCK_CAP).
func HasDecisionSlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	switch engine {
	case vocab.ClaudeCode, vocab.Codex:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.TurnEnd
	case vocab.Pi:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.TurnEnd
	case vocab.Cursor:
		return canonicalEvent == vocab.PreTool || cursorScopedDecisionEvents[nativeEvent]
	}
	return false
}

// HasGuardSlot reports whether engine's decision slot on this event guards a
// call hookyard's router can veto, as opposed to the turn_end slot (pi D3,
// Claude Code and Codex §11.3), which only requests one more continuation and
// guards no call at all. Only a guard slot makes a fire-and-forget handler
// misleading: turn-end observers are the common case, and a fire-and-forget
// one records "dispatched", so nothing claims enforcement it never had.
func HasGuardSlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	return HasDecisionSlot(engine, canonicalEvent, nativeEvent) && canonicalEvent != vocab.TurnEnd
}

// HasAdvisorySlot reports whether engine has anywhere to put an advisory
// string on this event. Claude Code's advisory set is pre_tool, session_start,
// post_tool — confirmed by aeye's diagram-guidance.sh (session_start) and
// diagrams.sh (post_tool) running against real Claude Code in production, in
// addition to pre_tool's additionalContext. Codex's set is UNPROBED, and this
// false is a current limitation rather than a boundary: what it rests on is an
// absence of observation, while Codex's own docs specify
// hookSpecificOutput.additionalContext on SessionStart, UserPromptSubmit,
// PreToolUse, PostToolUse and SubagentStart. Until
// docs/design/fixtures/codex-advisory/ has been run (issue #87), no handler may
// read this false as advice being undeliverable to Codex by nature.
// Cursor's is the user_message field of the decision object itself, so its
// advisory set is exactly its decision set — and advice only rides there
// alongside a rendered permission, which Render is what enforces. Pi's set is
// wider than its decision set: on pre_tool, advice rides the block reason on
// a deny, and a standalone advisory is appended to that call's own tool
// result, which the model reads in the next request beside the result — the
// same place Claude Code's pre_tool additionalContext lands (§11.1). The bridge
// also delivers a standalone advisory on session_start (as an injected message
// before the agent starts) and on post_tool (appended to the tool result),
// neither of which can carry a decision. On turn_end, a deny's advice does
// reach Codex, joined into the block reason Codex uses as its continuation
// prompt (§11.3). turn_end itself has a decision slot on pi, Claude Code and
// Codex (above) but no advisory slot on any engine here — Claude Code's Stop
// has no additionalContext either, and a standalone (non-deny) verdict on
// turn_end is never delivered; a deny's advice rides its block reason instead.
func HasAdvisorySlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	switch engine {
	case vocab.ClaudeCode:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.SessionStart || canonicalEvent == vocab.PostTool
	case vocab.Cursor:
		return HasDecisionSlot(engine, canonicalEvent, nativeEvent)
	case vocab.Pi:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.SessionStart || canonicalEvent == vocab.PostTool
	}
	return false
}
