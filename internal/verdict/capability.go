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
func HasDecisionSlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	switch engine {
	case vocab.ClaudeCode, vocab.Codex, vocab.Pi:
		return canonicalEvent == vocab.PreTool
	case vocab.Cursor:
		return canonicalEvent == vocab.PreTool || cursorScopedDecisionEvents[nativeEvent]
	}
	return false
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
// alongside a rendered permission, which Render is what enforces. Pi's is its
// single reason field, riding the same way: the block reason was observed
// reaching the model as the tool outcome, but only alongside a block, so Pi's
// advisory set is its decision set too.
func HasAdvisorySlot(engine vocab.Engine, canonicalEvent, nativeEvent string) bool {
	switch engine {
	case vocab.ClaudeCode:
		return canonicalEvent == vocab.PreTool || canonicalEvent == vocab.SessionStart || canonicalEvent == vocab.PostTool
	case vocab.Cursor, vocab.Pi:
		return HasDecisionSlot(engine, canonicalEvent, nativeEvent)
	}
	return false
}
