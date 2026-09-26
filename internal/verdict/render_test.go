package verdict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

func checkRendered(t *testing.T, name string, got Rendered, wantStdout string, wantEnforced, wantDelivered bool) {
	t.Helper()
	if string(got.Stdout) != wantStdout {
		t.Errorf("%s: stdout = %s, want %s", name, got.Stdout, wantStdout)
	}
	if wantStdout == "" && got.Stdout != nil {
		t.Errorf("%s: stdout is non-nil where nothing is printed", name)
	}
	if got.Enforced != wantEnforced {
		t.Errorf("%s: enforced = %v, want %v", name, got.Enforced, wantEnforced)
	}
	if got.AdviceDelivered != wantDelivered {
		t.Errorf("%s: advice delivered = %v, want %v", name, got.AdviceDelivered, wantDelivered)
	}
}

func TestRenderOnDecisionCapableEvents(t *testing.T) {
	cases := []struct {
		name      string
		in        Input
		stdout    string
		enforced  bool
		delivered bool
	}{
		{
			name: "claude-code abstain, no advice",
			in:   Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Abstain},
			// Nothing printed, and still enforced: abstain asks for nothing.
			enforced: true,
		},
		{
			name:     "claude-code allow",
			in:       Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Allow, Reason: "r"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"r"}}`,
			enforced: true,
		},
		{
			name:     "claude-code ask",
			in:       Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Ask, Reason: "r"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"r"}}`,
			enforced: true,
		},
		{
			name:     "claude-code deny",
			in:       Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: "r"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r"}}`,
			enforced: true,
		},
		{
			name:      "claude-code deny carrying advice",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:      "claude-code standalone advice",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "codex abstain",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Abstain},
			enforced: true,
		},
		{
			// Codex rejects an explicit allow by name, so printing nothing
			// leaves its own permission flow to run — not what allow asked for.
			name: "codex allow prints nothing, unenforced",
			in:   Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Allow, Reason: "r"},
		},
		{
			// §7: ask degrades to deny on Codex's binary channel, and a
			// rendered deny is enforced — Codex can and does act on it.
			name:     "codex ask degrades to deny, enforced",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Ask, Reason: "r"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r — hookyard verdict was ask; Codex has no ask channel, so the call was denied"}}`,
			enforced: true,
		},
		{
			// Codex rejects a deny with an empty reason, so the degraded
			// reason must be non-empty even when the handler gave none.
			name:     "codex ask with no handler reason still gets a non-empty degraded reason",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Ask},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"hookyard verdict was ask; Codex has no ask channel, so the call was denied"}}`,
			enforced: true,
		},
		{
			name:     "codex deny",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: "r"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r"}}`,
			enforced: true,
		},
		{
			name:     "codex deny with an empty reason gets the fallback",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"denied by hookyard handler"}}`,
			enforced: true,
		},
		{
			// PreToolUse is a decision slot and an advisory slot: the advisory
			// rides additionalContext in the same deny payload, the shape the
			// 0.156.1 deny+advice probe confirmed Codex acts on while still
			// delivering the advice.
			name:      "codex deny carries advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			// An abstain on PreToolUse carries no decision, only the advisory —
			// the same additionalContext-only shape SessionStart uses.
			name:      "codex standalone advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "cursor abstain",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Abstain},
			enforced: true,
		},
		{
			name:     "cursor allow",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Allow, Reason: "r"},
			stdout:   `{"permission":"allow","user_message":"r"}`,
			enforced: true,
		},
		{
			name:     "cursor ask",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Ask, Reason: "r"},
			stdout:   `{"permission":"ask","user_message":"r"}`,
			enforced: true,
		},
		{
			name:     "cursor deny",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Deny, Reason: "r"},
			stdout:   `{"permission":"deny","user_message":"r"}`,
			enforced: true,
		},
		{
			name:     "cursor deny with neither reason nor advice",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Deny},
			stdout:   `{"permission":"deny"}`,
			enforced: true,
		},
		{
			name:      "cursor deny joins reason and advice into the one text slot",
			in:        Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"permission":"deny","user_message":"r\n\na"}`,
			enforced:  true,
			delivered: true,
		},
		{
			// No advisory-only response Cursor honours has been confirmed, so
			// standalone advice is dropped rather than claimed as delivered.
			name:     "cursor standalone advice is not delivered",
			in:       Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Abstain, Advice: "a"},
			enforced: true,
		},
		{
			name:      "cursor engine-scoped beforeShellExecution is decision-capable",
			in:        Input{Engine: vocab.Cursor, NativeEvent: "beforeShellExecution", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"permission":"deny","user_message":"r\n\na"}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "pi abstain",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Abstain},
			enforced: true,
		},
		{
			// Pi's decision channel is binary with no wire form for allow at
			// all: not blocking already is allow, so an explicit allow prints
			// nothing, same as Codex.
			name: "pi allow prints nothing, unenforced",
			in:   Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Allow, Reason: "r"},
		},
		{
			name:     "pi ask degrades to deny, enforced",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Ask, Reason: "r"},
			stdout:   `{"block":true,"reason":"r — hookyard verdict was ask; Pi has no ask channel, so the call was denied"}`,
			enforced: true,
		},
		{
			name:     "pi ask with no handler reason still gets a non-empty degraded reason",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Ask},
			stdout:   `{"block":true,"reason":"hookyard verdict was ask; Pi has no ask channel, so the call was denied"}`,
			enforced: true,
		},
		{
			name:     "pi deny",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Deny, Reason: "r"},
			stdout:   `{"block":true,"reason":"r"}`,
			enforced: true,
		},
		{
			name:     "pi deny with neither reason nor advice",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Deny},
			stdout:   `{"block":true}`,
			enforced: true,
		},
		{
			// Same shape as Cursor: one text slot, so reason and advice riding
			// together on a block are joined into it.
			name:      "pi deny joins reason and advice into the one reason field",
			in:        Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"block":true,"reason":"r\n\na"}`,
			enforced:  true,
			delivered: true,
		},
		{
			// An allow has no block to carry the advice, so it rides as a
			// standalone advisory instead, appended to the call's own tool
			// result (§11.1).
			name:      "pi allow carries the advice riding it as a standalone advisory",
			in:        Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Allow, Reason: "r", Advice: "a"},
			stdout:    `{"advisory":"a"}`,
			delivered: true,
		},
		{
			name:      "pi ask joins the degraded reason and advice",
			in:        Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Ask, Reason: "r", Advice: "a"},
			stdout:    `{"block":true,"reason":"r — hookyard verdict was ask; Pi has no ask channel, so the call was denied\n\na"}`,
			enforced:  true,
			delivered: true,
		},
		{
			// Abstain plus advice on pre_tool now has a standalone advisory
			// slot, appended to the call's own tool result the same as
			// session_start and post_tool's (TestRenderPiAdvisoryOnlyEvents),
			// even with no block.
			name:      "pi standalone advice is appended to the call's own tool result",
			in:        Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Abstain, Advice: "a"},
			stdout:    `{"advisory":"a"}`,
			enforced:  true,
			delivered: true,
		},
	}

	for _, c := range cases {
		checkRendered(t, c.name, Render(c.in), c.stdout, c.enforced, c.delivered)
	}
}

func TestRenderOffADecisionSlotPrintsNothing(t *testing.T) {
	for _, engine := range vocab.Engines {
		if engine == vocab.ClaudeCode || engine == vocab.Codex {
			// Claude Code and Codex both have an advisory slot on post_tool:
			// see TestRenderAdvisoryOnlyEvents.
			continue
		}
		native, ok := vocab.NativeEvent(engine, vocab.PostTool)
		if !ok {
			t.Fatalf("%s has no native post_tool event", engine)
		}
		// Pi has a post_tool advisory slot too, so the case that prints nothing
		// is its no-advice one; the advice case is in
		// TestRenderPiAdvisoryOnlyEvents.
		advice := "a"
		if engine == vocab.Pi {
			advice = ""
		}
		for _, v := range lattice {
			in := Input{Engine: engine, CanonicalEvent: vocab.PostTool, NativeEvent: native, Verdict: v, Reason: "r", Advice: advice}
			// Enforced is true only for abstain: any other verdict was computed
			// and cannot be acted on, which is what the record must show.
			checkRendered(t, fmt.Sprintf("%s post_tool %s", engine, v), Render(in), "", v == Abstain, false)
		}
	}
}

func TestRenderAdvisoryOnlyEvents(t *testing.T) {
	cases := []struct {
		name      string
		in        Input
		stdout    string
		enforced  bool
		delivered bool
	}{
		{
			name:     "session_start abstain, no advice",
			in:       Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Abstain},
			enforced: true,
		},
		{
			name: "post_tool abstain, no advice",
			in:   Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Abstain},
			// Abstain plus no advice asks for nothing, so it's enforced.
			enforced: true,
		},
		{
			// The wire shape the linked GitHub issue reproduces against.
			name:      "session_start abstain with advice",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:      "post_tool abstain with advice",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			// No decision channel here, so a computed deny is recorded
			// unenforced even though its advice still rides additionalContext —
			// and no permissionDecision leaks in, since there's nowhere for it.
			name:      "session_start deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"a"}}`,
			delivered: true,
		},
		{
			name:      "post_tool deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"a"}}`,
			delivered: true,
		},
		{
			// Reason has no channel here and must not leak into additionalContext
			// or anywhere else — a deliberate boundary, not an oversight.
			name: "session_start deny with reason only drops the reason silently",
			in:   Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Deny, Reason: "r"},
		},
		{
			name: "post_tool deny with reason only drops the reason silently",
			in:   Input{Engine: vocab.ClaudeCode, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Deny, Reason: "r"},
		},
		{
			name:      "codex session_start abstain with advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:      "codex prompt_submit abstain with advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PromptSubmit, NativeEvent: "UserPromptSubmit", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "codex session_start abstain, no advice",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Abstain},
			enforced: true,
		},
		{
			name:     "codex prompt_submit abstain, no advice",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PromptSubmit, NativeEvent: "UserPromptSubmit", Verdict: Abstain},
			enforced: true,
		},
		{
			name:      "codex session_start deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.SessionStart, NativeEvent: "SessionStart", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"a"}}`,
			delivered: true,
		},
		{
			name:      "codex prompt_submit deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PromptSubmit, NativeEvent: "UserPromptSubmit", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"a"}}`,
			delivered: true,
		},
		{
			name:      "codex post_tool abstain with advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "codex post_tool abstain, no advice",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Abstain},
			enforced: true,
		},
		{
			name:      "codex post_tool deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: vocab.PostTool, NativeEvent: "PostToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"a"}}`,
			delivered: true,
		},
		{
			// SubagentStart has no canonical event, so it is reached only as the
			// engine-scoped codex:SubagentStart and carries canonical "".
			name:      "codex scoped SubagentStart abstain with advice",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: "", NativeEvent: "SubagentStart", Verdict: Abstain, Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SubagentStart","additionalContext":"a"}}`,
			enforced:  true,
			delivered: true,
		},
		{
			name:     "codex scoped SubagentStart abstain, no advice",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: "", NativeEvent: "SubagentStart", Verdict: Abstain},
			enforced: true,
		},
		{
			name:      "codex scoped SubagentStart deny with advice renders advice only, unenforced",
			in:        Input{Engine: vocab.Codex, CanonicalEvent: "", NativeEvent: "SubagentStart", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:    `{"hookSpecificOutput":{"hookEventName":"SubagentStart","additionalContext":"a"}}`,
			delivered: true,
		},
	}

	for _, c := range cases {
		checkRendered(t, c.name, Render(c.in), c.stdout, c.enforced, c.delivered)
	}
}

// TestRenderPiAdvisoryOnlyEvents walks the whole (verdict × advice) matrix on
// Pi's two advisory-only events. The reply must stay a bare advisory whatever
// the verdict was: no block key — which is what piResponse.Block's omitempty
// buys — and no reason, since neither event has anywhere to act on one.
func TestRenderPiAdvisoryOnlyEvents(t *testing.T) {
	for _, event := range []string{vocab.SessionStart, vocab.PostTool} {
		native, ok := vocab.NativeEvent(vocab.Pi, event)
		if !ok {
			t.Fatalf("pi has no native %s event", event)
		}
		for _, v := range lattice {
			for _, advice := range []string{"", "a"} {
				in := Input{Engine: vocab.Pi, CanonicalEvent: event, NativeEvent: native, Verdict: v, Reason: "r", Advice: advice}
				want := ""
				if advice != "" {
					want = `{"advisory":"a"}`
				}
				// Enforced tracks abstain alone: delivering an advisory is not
				// acting on a verdict, and there is no decision slot here.
				name := fmt.Sprintf("pi %s %s advice=%q", event, v, advice)
				checkRendered(t, name, Render(in), want, v == Abstain, advice != "")
			}
		}
	}
}

// TestRenderPiPreToolNeverMixesBlockAndAdvisory walks the whole (verdict ×
// advice) matrix on Pi's pre_tool decision slot and decodes the reply as a
// bare key set: "block" appears iff the verdict denies the call (deny, or ask
// degraded to deny), "advisory" appears iff there's advice and the call was
// not blocked, and the two never appear together — a block's advice rides the
// reason field instead (renderPiDeny), never its own key.
func TestRenderPiPreToolNeverMixesBlockAndAdvisory(t *testing.T) {
	for _, v := range lattice {
		for _, advice := range []string{"", "a"} {
			in := Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: v, Reason: "r", Advice: advice}
			got := Render(in)

			keys := map[string]json.RawMessage{}
			if len(got.Stdout) > 0 {
				if err := json.Unmarshal(got.Stdout, &keys); err != nil {
					t.Fatalf("verdict=%s advice=%q: stdout not valid JSON: %s", v, advice, got.Stdout)
				}
			}

			_, hasBlock := keys["block"]
			wantBlock := v == Deny || v == Ask
			if hasBlock != wantBlock {
				t.Errorf("verdict=%s advice=%q: block key = %v, want %v", v, advice, hasBlock, wantBlock)
			}

			_, hasAdvisory := keys["advisory"]
			wantAdvisory := advice != "" && (v == Abstain || v == Allow)
			if hasAdvisory != wantAdvisory {
				t.Errorf("verdict=%s advice=%q: advisory key = %v, want %v", v, advice, hasAdvisory, wantAdvisory)
			}

			if hasBlock && hasAdvisory {
				t.Errorf("verdict=%s advice=%q: stdout has both block and advisory keys: %s", v, advice, got.Stdout)
			}
		}
	}
}

// TestRenderPiTurnEnd walks pi's settle-boundary decision slot (D3): deny
// renders the same block shape as pre_tool — falling back to
// turnEndEmptyDenyReason when a handler gives neither reason nor advice,
// rather than printing a bare block the bridge's own decision() would then
// caption "Blocked by hookyard" — ask/allow print nothing and stay unenforced
// (no degrade-to-deny, unlike pre_tool — there is no safe direction to force
// a continuation toward), abstain is enforced with no output, and a
// standalone advisory is never rendered even when the handler gave one.
// StopHookActive short-circuits every verdict to "print nothing", with
// Enforced tracking abstain alone.
func TestRenderPiTurnEnd(t *testing.T) {
	native, ok := vocab.NativeEvent(vocab.Pi, vocab.TurnEnd)
	if !ok {
		t.Fatal("pi has no native turn_end event")
	}
	cases := []struct {
		name      string
		verdict   Verdict
		reason    string
		advice    string
		stopHook  bool
		stdout    string
		enforced  bool
		delivered bool
	}{
		{name: "deny", verdict: Deny, reason: "r", stdout: `{"block":true,"reason":"r"}`, enforced: true},
		{
			name: "deny with advice joins reason and advice", verdict: Deny, reason: "r", advice: "adv",
			stdout: `{"block":true,"reason":"r\n\nadv"}`, enforced: true, delivered: true,
		},
		{
			name:    "deny with no reason and no advice falls back to the fixed continuation reason",
			verdict: Deny, reason: "", advice: "",
			stdout: `{"block":true,"reason":"` + turnEndEmptyDenyReason + `"}`, enforced: true,
		},
		{
			name:    "deny with whitespace-only reason and no advice falls back to the fixed continuation reason",
			verdict: Deny, reason: "  ", advice: "",
			stdout: `{"block":true,"reason":"` + turnEndEmptyDenyReason + `"}`, enforced: true,
		},
		{
			name: "deny with whitespace-only reason and advice uses advice alone", verdict: Deny, reason: " ", advice: "adv",
			stdout: `{"block":true,"reason":"adv"}`, enforced: true, delivered: true,
		},
		{name: "ask prints nothing, unenforced", verdict: Ask, reason: "r"},
		{name: "allow prints nothing, unenforced", verdict: Allow, reason: "r"},
		{name: "abstain prints nothing, enforced", verdict: Abstain, reason: "r", enforced: true},
		{
			name:    "abstain with advice still delivers nothing: turn_end has no advisory slot",
			verdict: Abstain, reason: "r", advice: "adv", enforced: true,
		},
		{name: "deny + stop_hook_active prints nothing, unenforced", verdict: Deny, reason: "r", stopHook: true},
		{name: "abstain + stop_hook_active stays enforced", verdict: Abstain, reason: "r", stopHook: true, enforced: true},
	}
	for _, c := range cases {
		in := Input{
			Engine: vocab.Pi, CanonicalEvent: vocab.TurnEnd, NativeEvent: native,
			Verdict: c.verdict, Reason: c.reason, Advice: c.advice, StopHookActive: c.stopHook,
		}
		checkRendered(t, "pi turn_end "+c.name, Render(in), c.stdout, c.enforced, c.delivered)
	}
}

// TestRenderStopTurnEnd walks Claude Code's and Codex's Stop decision slot
// (§11.3): deny renders the top-level {"decision":"block","reason":...}
// shape via renderStopBlock rather than hookSpecificOutput, falling back to
// turnEndEmptyDenyReason when a handler gives neither reason nor advice —
// ask/allow print nothing and stay unenforced (no degrade-to-deny, same as
// pi's turn_end), abstain is enforced with no output, and a standalone
// advisory is never rendered. StopHookActive short-circuits every verdict to
// "print nothing", with Enforced tracking abstain alone — the same shape as
// TestRenderPiTurnEnd, engine-general via renderTurnEnd.
func TestRenderStopTurnEnd(t *testing.T) {
	for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Codex} {
		native, ok := vocab.NativeEvent(engine, vocab.TurnEnd)
		if !ok {
			t.Fatalf("%s has no native turn_end event", engine)
		}
		cases := []struct {
			name      string
			verdict   Verdict
			reason    string
			advice    string
			stopHook  bool
			stdout    string
			enforced  bool
			delivered bool
		}{
			{name: "deny", verdict: Deny, reason: "r", stdout: `{"decision":"block","reason":"r"}`, enforced: true},
			{
				name: "deny with advice joins reason and advice", verdict: Deny, reason: "r", advice: "adv",
				stdout: `{"decision":"block","reason":"r\n\nadv"}`, enforced: true, delivered: true,
			},
			{
				name:    "deny with no reason and no advice falls back to the fixed continuation reason",
				verdict: Deny, reason: "", advice: "",
				stdout: `{"decision":"block","reason":"` + turnEndEmptyDenyReason + `"}`, enforced: true,
			},
			{
				name: "deny with empty reason and advice uses advice alone, no stand-in", verdict: Deny, reason: "", advice: "adv",
				stdout: `{"decision":"block","reason":"adv"}`, enforced: true, delivered: true,
			},
			{
				name:    "deny with whitespace-only reason and no advice falls back to the fixed continuation reason",
				verdict: Deny, reason: "  ", advice: "",
				stdout: `{"decision":"block","reason":"` + turnEndEmptyDenyReason + `"}`, enforced: true,
			},
			{
				name: "deny with whitespace-only reason and advice uses advice alone", verdict: Deny, reason: " ", advice: "adv",
				stdout: `{"decision":"block","reason":"adv"}`, enforced: true, delivered: true,
			},
			{name: "ask prints nothing, unenforced", verdict: Ask, reason: "r"},
			{name: "allow prints nothing, unenforced", verdict: Allow, reason: "r"},
			{name: "abstain prints nothing, enforced", verdict: Abstain, reason: "r", enforced: true},
			{
				name:    "abstain with advice still delivers nothing: turn_end has no advisory slot",
				verdict: Abstain, reason: "r", advice: "adv", enforced: true,
			},
			{name: "deny + stop_hook_active prints nothing, unenforced", verdict: Deny, reason: "r", stopHook: true},
			{name: "abstain + stop_hook_active stays enforced", verdict: Abstain, reason: "r", stopHook: true, enforced: true},
		}
		for _, c := range cases {
			in := Input{
				Engine: engine, CanonicalEvent: vocab.TurnEnd, NativeEvent: native,
				Verdict: c.verdict, Reason: c.reason, Advice: c.advice, StopHookActive: c.stopHook,
			}
			checkRendered(t, string(engine)+" turn_end "+c.name, Render(in), c.stdout, c.enforced, c.delivered)
		}
	}
}

func TestRenderDoesNotRenderPermissionRequest(t *testing.T) {
	// A confirmed decision channel hookyard deliberately leaves unrendered
	// (§4): a computed deny arrives here unenforced, and says so.
	for _, v := range lattice {
		in := Input{Engine: vocab.ClaudeCode, NativeEvent: "PermissionRequest", Verdict: v, Reason: "r", Advice: "a"}
		checkRendered(t, "claude-code:PermissionRequest "+string(v), Render(in), "", v == Abstain, false)
	}
}

// TestHasDecisionSlotTurnEnd pins §11.3/D3: pi, Claude Code and Codex all get
// a decision slot on their native turn_end, Cursor does not, and pi's
// pre_tool slot is unaffected by the split of Pi out of the shared
// Claude/Codex case in HasDecisionSlot.
func TestHasDecisionSlotTurnEnd(t *testing.T) {
	if !HasDecisionSlot(vocab.Pi, vocab.TurnEnd, "agent_before_settle") {
		t.Error("HasDecisionSlot(Pi, TurnEnd, agent_before_settle) = false, want true")
	}
	if !HasDecisionSlot(vocab.Pi, vocab.PreTool, "tool_call") {
		t.Error("HasDecisionSlot(Pi, PreTool, tool_call) = false, want true")
	}
	for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Codex} {
		native, ok := vocab.NativeEvent(engine, vocab.TurnEnd)
		if !ok {
			t.Fatalf("%s has no native turn_end event", engine)
		}
		if !HasDecisionSlot(engine, vocab.TurnEnd, native) {
			t.Errorf("HasDecisionSlot(%s, TurnEnd, %s) = false, want true", engine, native)
		}
	}
	native, ok := vocab.NativeEvent(vocab.Cursor, vocab.TurnEnd)
	if !ok {
		t.Fatal("cursor has no native turn_end event")
	}
	if HasDecisionSlot(vocab.Cursor, vocab.TurnEnd, native) {
		t.Errorf("HasDecisionSlot(Cursor, TurnEnd, %s) = true, want false", native)
	}
}

// TestHasAdvisorySlotTurnEnd pins that turn_end's decision slot has no
// matching advisory slot on any engine: a standalone advisory is never
// delivered there.
func TestHasAdvisorySlotTurnEnd(t *testing.T) {
	for _, engine := range vocab.Engines {
		native, ok := vocab.NativeEvent(engine, vocab.TurnEnd)
		if !ok {
			t.Fatalf("%s has no native turn_end event", engine)
		}
		if HasAdvisorySlot(engine, vocab.TurnEnd, native) {
			t.Errorf("HasAdvisorySlot(%s, TurnEnd, %s) = true, want false", engine, native)
		}
	}
}

// TestHasGuardSlot pins the predicate validateLane uses (D3, §11.3): it
// agrees with HasDecisionSlot everywhere except turn_end, which requests a
// continuation and guards no call.
func TestHasGuardSlot(t *testing.T) {
	if !HasGuardSlot(vocab.Pi, vocab.PreTool, "tool_call") {
		t.Error("HasGuardSlot(Pi, PreTool, tool_call) = false, want true")
	}
	if HasGuardSlot(vocab.Pi, vocab.TurnEnd, "agent_before_settle") {
		t.Error("HasGuardSlot(Pi, TurnEnd, agent_before_settle) = true, want false")
	}
	if !HasGuardSlot(vocab.Cursor, "", "beforeShellExecution") {
		t.Error("HasGuardSlot(Cursor, beforeShellExecution) = false, want true")
	}
	if !HasGuardSlot(vocab.ClaudeCode, vocab.PreTool, "PreToolUse") {
		t.Error("HasGuardSlot(ClaudeCode, PreTool, PreToolUse) = false, want true")
	}
	for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Codex} {
		native, ok := vocab.NativeEvent(engine, vocab.TurnEnd)
		if !ok {
			t.Fatalf("%s has no native turn_end event", engine)
		}
		if HasGuardSlot(engine, vocab.TurnEnd, native) {
			t.Errorf("HasGuardSlot(%s, TurnEnd, %s) = true, want false", engine, native)
		}
	}
}

func TestCapabilityTable(t *testing.T) {
	for _, engine := range vocab.Engines {
		for _, event := range vocab.CanonicalEvents {
			native, ok := vocab.NativeEvent(engine, event)
			if !ok {
				t.Fatalf("%s has no native %s event", engine, event)
			}
			want := event == vocab.PreTool
			// Pi, Claude Code and Codex also have a decision slot on turn_end
			// (D3, §11.3): a consolidated deny there asks the engine to
			// continue instead of stopping. Cursor does not.
			if event == vocab.TurnEnd && engine != vocab.Cursor {
				want = true
			}
			if got := HasDecisionSlot(engine, event, native); got != want {
				t.Errorf("HasDecisionSlot(%s, %s) = %v, want %v", engine, event, got, want)
			}
			wantAdvisory := want && engine != vocab.Codex
			// Claude Code and Pi both reach the model off the permission path.
			if engine == vocab.ClaudeCode || engine == vocab.Pi {
				wantAdvisory = event == vocab.PreTool || event == vocab.SessionStart || event == vocab.PostTool
			}
			// Codex delivers additionalContext on all five events the
			// 0.156.1 probe confirmed: SessionStart, UserPromptSubmit,
			// PreToolUse and PostToolUse here, SubagentStart via the scoped
			// map asserted below.
			if engine == vocab.Codex {
				wantAdvisory = event == vocab.SessionStart || event == vocab.PromptSubmit ||
					event == vocab.PreTool || event == vocab.PostTool
			}
			if got := HasAdvisorySlot(engine, event, native); got != wantAdvisory {
				t.Errorf("HasAdvisorySlot(%s, %s) = %v, want %v", engine, event, got, wantAdvisory)
			}
		}
	}

	for native := range cursorScopedDecisionEvents {
		if !HasDecisionSlot(vocab.Cursor, "", native) {
			t.Errorf("HasDecisionSlot(cursor, %s) = false, want true", native)
		}
		if !HasAdvisorySlot(vocab.Cursor, "", native) {
			t.Errorf("HasAdvisorySlot(cursor, %s) = false, want true", native)
		}
		// The scoped names are Cursor's alone.
		for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Codex} {
			if HasDecisionSlot(engine, "", native) {
				t.Errorf("HasDecisionSlot(%s, %s) = true, want false", engine, native)
			}
		}
	}

	for native := range codexScopedAdvisoryEvents {
		if !HasAdvisorySlot(vocab.Codex, "", native) {
			t.Errorf("HasAdvisorySlot(codex, %s) = false, want true", native)
		}
		// SubagentStart is advisory-only on Codex, and the scoped name is
		// Codex's alone.
		if HasDecisionSlot(vocab.Codex, "", native) {
			t.Errorf("HasDecisionSlot(codex, %s) = true, want false", native)
		}
		for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Pi, vocab.Cursor} {
			if HasAdvisorySlot(engine, "", native) {
				t.Errorf("HasAdvisorySlot(%s, %s) = true, want false", engine, native)
			}
		}
	}
}

const captureScript = "../../docs/design/fixtures/hook-payloads/capture-hook.sh"

var singleQuotedJSON = regexp.MustCompile(`'(\{.*\})'`)

// capturedResponses pulls the deny responses capture-hook.sh sends out of the
// committed script itself: these three engines each blocked a live tool call on
// exactly these bytes, so retyping them here would assert against a copy rather
// than against the ground truth.
func capturedResponses(t *testing.T, marker string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(captureScript)
	if err != nil {
		t.Fatal(err)
	}
	var found [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		quoted := singleQuotedJSON.FindStringSubmatch(line)
		if quoted == nil {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(quoted[1])); err != nil {
			t.Fatalf("%s: %q is not JSON: %v", captureScript, quoted[1], err)
		}
		found = append(found, compact.Bytes())
	}
	if len(found) == 0 {
		t.Fatalf("no %s deny response found in %s: this assertion has silently become a no-op, "+
			"so re-point it at the script's current deny responses rather than deleting it", marker, captureScript)
	}
	return found
}

func TestDenyRenderingsMatchTheCapturedResponses(t *testing.T) {
	const reason = "hookyard probe deny"

	for _, want := range capturedResponses(t, `"hookSpecificOutput":`) {
		for _, engine := range []vocab.Engine{vocab.ClaudeCode, vocab.Codex} {
			in := Input{Engine: engine, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: reason}
			if got := Render(in).Stdout; !bytes.Equal(got, want) {
				t.Errorf("%s deny rendering = %s, want %s", engine, got, want)
			}
		}
	}

	for _, want := range capturedResponses(t, `"permission":`) {
		in := Input{Engine: vocab.Cursor, CanonicalEvent: vocab.PreTool, NativeEvent: "preToolUse", Verdict: Deny, Reason: reason}
		if got := Render(in).Stdout; !bytes.Equal(got, want) {
			t.Errorf("cursor deny rendering = %s, want %s", got, want)
		}
	}
}
