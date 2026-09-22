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
// Pi's turn_end (canonical TurnEnd, native agent_before_settle) is also a
// decision slot, but a pi-only one (D3, docs/design/hookyard.md §11.2): a
// consolidated deny there means "do not settle yet — continue with this
// reason", the same protocol as pre_tool's block, not a new verdict value.
// Claude Code and Codex's Stop have the same shape available (native
// stop_hook_active bounds the loop identically) but are deliberately not
// wired up here — filed as follow-up issue #77, cross-engine work out of
// #76's scope. `enforced: true` on a settle deny means hookyard rendered a
// block the bridge acted on, not that pi's run actually continued: pi may
// still decline it (canContinue false, or an abort during before_settle),
// the same class of gap pre_tool's `enforced` already accepts. The bridge
// bounds a settle deny to one continuation per bridge per run (pi_bridge.ts);
// a router failure or a second settle in the same run fails open.
func HasDecisionSlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	switch engine {
	case vocab.ClaudeCode, vocab.Codex:
		return canonicalEvent == vocab.PreTool
	case vocab.Pi:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.TurnEnd
	case vocab.Cursor:
		return canonicalEvent == vocab.PreTool || cursorScopedDecisionEvents[nativeEvent]
	}
	return false
}

// HasGuardSlot reports whether engine's decision slot on this event guards a
// call hookyard's router can veto, as opposed to pi's turn_end slot (D3),
// which only requests one more continuation and guards no call at all.
// validateLane calls this instead of HasDecisionSlot: giving pi turn_end a
// decision slot would otherwise reject every fire-and-forget turn_end
// observer that claims pi, forcing the common case — an observer, not a
// guard — onto pi's synchronous verdict-lane critical path. A fire-and-forget
// turn_end handler still records "dispatched", so nothing claims enforcement
// it never had.
func HasGuardSlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	return HasDecisionSlot(engine, canonicalEvent, nativeEvent) && canonicalEvent != vocab.TurnEnd
}

// HasAdvisorySlot reports whether engine has anywhere to put an advisory
// string on this event. Claude Code's advisory set is pre_tool, session_start,
// post_tool — confirmed by aeye's diagram-guidance.sh (session_start) and
// diagrams.sh (post_tool) running against real Claude Code in production, in
// addition to pre_tool's additionalContext. Codex has no advisory channel on
// any event, full stop: this is a settled boundary, not a gap to fill later —
// no handler can get advice to Codex by any means this package offers.
// Cursor's is the user_message field of the decision object itself, so its
// advisory set is exactly its decision set — and advice only rides there
// alongside a rendered permission, which Render is what enforces. Pi's set is
// wider than its decision set: on pre_tool, advice rides the block reason on
// a deny, and a standalone advisory is appended to that call's own tool
// result, which the model reads in the next request beside the result — the
// same place Claude Code's pre_tool additionalContext lands (§11.1). The bridge
// also delivers a standalone advisory on session_start (as an injected message
// before the agent starts) and on post_tool (appended to the tool result),
// neither of which can carry a decision. turn_end is the mirror case: it has
// a decision slot (above) but no advisory slot here — Claude Code's Stop has
// no additionalContext either, and a standalone (non-deny) verdict on pi
// turn_end is never delivered.
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
