package verdict

import (
	"encoding/json"
	"strings"

	"github.com/noamsto/hookyard/internal/vocab"
)

// Input is one consolidated decision, addressed to one engine on one event.
// The native event is carried alongside the canonical one because it is what
// the response echoes back as hookEventName.
type Input struct {
	Engine         vocab.Engine
	CanonicalEvent string
	NativeEvent    string
	Verdict        Verdict
	Reason         string
	Advice         string
	// StopHookActive is set for turn_end on pi, Claude Code and Codex, from
	// the native payload's stop_hook_active. True means a block rendered now
	// would not be acted on: on pi (D3, docs/design/hookyard.md §11.2) the
	// bridge already spent this run's one continuation; on Claude Code and
	// Codex (§11.3) this Stop is already a continuation a Stop hook forced.
	// Render must not print a block it knows the engine will ignore — doing
	// so would leave the record claiming an enforcement that never happened.
	StopHookActive bool
}

// Rendered is what the router prints and what it records. Enforced is false
// exactly when a verdict was computed that the engine cannot act on — an
// allow rendered to Codex, or any verdict on an event whose engine has no
// decision slot — which is the distinction the record would otherwise lie
// about.
type Rendered struct {
	Stdout          []byte
	Enforced        bool
	AdviceDelivered bool
}

// codexEmptyDenyReason stands in for an absent reason on Codex, which rejects
// a deny that carries none — and a rejected deny is an allowed tool call.
const codexEmptyDenyReason = "denied by hookyard handler"

// turnEndEmptyDenyReason stands in for a turn_end deny with a blank reason
// and no advice. Without it, Codex's stop parser trims the reason and
// rejects a block whose reason is blank (whitespace-only counts) — so the
// deny would not block at all — and on pi, renderPiDeny would print bare
// "{"block":true}", leaving the bridge's own decision() to fall back to
// "Blocked by hookyard" for the continuation message, the opposite of what a
// handler that denies a settle is trying to say (keep going, don't stop).
const turnEndEmptyDenyReason = "a hookyard handler asked you to keep working before finishing"

// hookSpecificOutput is the wrapper Claude Code and Codex share. Codex gets
// the same nested shape rather than a top-level decision because capture-hook.sh
// sent both engines this byte-identical response and Codex blocked the call;
// rendering the deny top-level instead would be an unenforced deny on the
// engine with the narrowest channel.
type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

type hookResponse struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type cursorResponse struct {
	Permission  string `json:"permission"`
	UserMessage string `json:"user_message,omitempty"`
}

// piResponse is the bridge's wire shape on both of Pi's reply paths: a block
// with its reason, which the bridge answers by refusing the call (returning
// nothing is allow), or a standalone advisory it delivers as a message — on
// session_start and post_tool as an injected or appended message, and on
// pre_tool as text appended to that call's own tool result (§11.1). Block
// carries omitempty because the two paths are disjoint on the wire — an
// advisory reply spelling "block":false would read as a decision no handler
// made — and renderPiDeny, the only producer of a block, always sets it true.
type piResponse struct {
	Block    bool   `json:"block,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Advisory string `json:"advisory,omitempty"`
}

// stopResponse is Claude Code's and Codex's Stop block, top-level rather than
// nested inside hookSpecificOutput: permissionDecision is a PreToolUse
// contract (§4), and Stop's own contract on both engines puts decision/reason
// at the top of the object instead — confirmed for Codex by
// stop.command.output.schema.json (rust-v0.156.1), whose additionalProperties:false
// means no other key, hookSpecificOutput included, may ride along. No
// omitempty: renderStopBlock is the only producer and always sets both.
type stopResponse struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// Render turns a consolidated decision into the bytes one engine expects on
// one event. It is total: an (engine, event) pair with no decision slot and no
// advisory slot prints nothing rather than failing, so no caller needs a
// fallback on this path. Stdout is nil whenever nothing is printed.
func Render(in Input) Rendered {
	hasDecision := HasDecisionSlot(in.Engine, in.CanonicalEvent, in.NativeEvent)
	if hasDecision {
		if in.CanonicalEvent == vocab.TurnEnd {
			switch in.Engine {
			case vocab.Pi:
				return renderTurnEnd(in, renderPiDeny)
			case vocab.ClaudeCode, vocab.Codex:
				return renderTurnEnd(in, renderStopBlock)
			}
			return Rendered{Enforced: in.Verdict == Abstain}
		}
		switch in.Engine {
		case vocab.ClaudeCode:
			return renderClaudeCode(in)
		case vocab.Codex:
			return renderCodex(in)
		case vocab.Cursor:
			return renderCursor(in)
		case vocab.Pi:
			return renderPi(in)
		}
		return Rendered{Enforced: in.Verdict == Abstain}
	}
	// Cursor's advisory set is identical to its decision set (already false
	// here). Claude Code, Pi, and Codex reach this branch on events that have
	// an advisory slot and no decision slot.
	if HasAdvisorySlot(in.Engine, in.CanonicalEvent, in.NativeEvent) {
		switch in.Engine {
		case vocab.ClaudeCode:
			return renderClaudeCodeAdvisoryOnly(in)
		case vocab.Codex:
			return renderCodexAdvisoryOnly(in)
		case vocab.Pi:
			return renderPiAdvisoryOnly(in)
		}
	}
	return Rendered{Enforced: in.Verdict == Abstain}
}

// renderClaudeCode also carries standalone advice, in the additionalContext-only
// shape §7 confirms reaches the model today.
func renderClaudeCode(in Input) Rendered {
	if in.Verdict == Abstain && in.Advice == "" {
		return Rendered{Enforced: true}
	}
	out := hookSpecificOutput{HookEventName: in.NativeEvent, AdditionalContext: in.Advice}
	if in.Verdict != Abstain {
		out.PermissionDecision = string(in.Verdict)
		out.PermissionDecisionReason = in.Reason
	}
	return Rendered{Stdout: marshal(hookResponse{out}), Enforced: true, AdviceDelivered: in.Advice != ""}
}

// renderClaudeCodeAdvisoryOnly renders additionalContext on an event where
// Claude Code has an advisory slot but no decision slot (session_start,
// post_tool). There is nowhere to put permissionDecision on these events —
// rendering it would be inventing a channel that doesn't exist — so a
// non-Abstain verdict here is recorded unenforced, the same as any other
// verdict computed off a decision slot.
func renderClaudeCodeAdvisoryOnly(in Input) Rendered {
	if in.Advice == "" {
		return Rendered{Enforced: in.Verdict == Abstain}
	}
	out := hookSpecificOutput{HookEventName: in.NativeEvent, AdditionalContext: in.Advice}
	return Rendered{Stdout: marshal(hookResponse{out}), Enforced: in.Verdict == Abstain, AdviceDelivered: true}
}

// renderCodexAdvisoryOnly renders additionalContext on session_start and
// prompt_submit. Those output schemas are deny_unknown_fields and have no
// permissionDecision, so a non-Abstain verdict is recorded unenforced and
// only the advice is printed.
func renderCodexAdvisoryOnly(in Input) Rendered {
	if in.Advice == "" {
		return Rendered{Enforced: in.Verdict == Abstain}
	}
	out := hookSpecificOutput{HookEventName: in.NativeEvent, AdditionalContext: in.Advice}
	return Rendered{Stdout: marshal(hookResponse{out}), Enforced: in.Verdict == Abstain, AdviceDelivered: true}
}

// renderCodex renders the deny arm, plus ask degraded to deny per §7's rule
// for engines whose decision shape is binary, and never an advisory field. An
// allow records Enforced false rather than true: Codex rejects an explicit
// allow by name, so printing nothing leaves Codex's own permission flow to
// run, which is not what an allow asked for — on Claude Code the same verdict
// bypasses the prompt, and the record must not call both outcomes enforced.
func renderCodex(in Input) Rendered {
	switch in.Verdict {
	case Deny:
		reason := in.Reason
		if reason == "" {
			reason = codexEmptyDenyReason
		}
		return renderCodexDeny(in.NativeEvent, reason)
	case Ask:
		return renderCodexDeny(in.NativeEvent, codexAskDegradedReason(in.Reason))
	default:
		return Rendered{Enforced: in.Verdict == Abstain}
	}
}

// codexAskDegradedReason is the permissionDecisionReason Codex sees for a
// degraded ask: Codex's decision channel has no ask arm (§4), so §7's
// binary-engine rule denies instead. A handler's own reason for asking rides
// along when it gave one.
func codexAskDegradedReason(handlerReason string) string {
	const degraded = "hookyard verdict was ask; Codex has no ask channel, so the call was denied"
	if handlerReason == "" {
		return degraded
	}
	return handlerReason + " — " + degraded
}

func renderCodexDeny(nativeEvent, reason string) Rendered {
	out := hookSpecificOutput{
		HookEventName:            nativeEvent,
		PermissionDecision:       string(Deny),
		PermissionDecisionReason: reason,
	}
	return Rendered{Stdout: marshal(hookResponse{out}), Enforced: true}
}

// renderCursor drops standalone advice: §7 identified no advisory-only
// response Cursor honours, and the one captured shape pairs user_message with
// a permission, i.e. as a deny reason. Recording delivery for a slot that may
// be ignored would convert a stated loss back into a silent one.
func renderCursor(in Input) Rendered {
	if in.Verdict == Abstain {
		return Rendered{Enforced: true}
	}
	// One text slot, so a reason and advice riding together are joined into it.
	var message []string
	if in.Reason != "" {
		message = append(message, in.Reason)
	}
	if in.Advice != "" {
		message = append(message, in.Advice)
	}
	out := cursorResponse{Permission: string(in.Verdict), UserMessage: strings.Join(message, "\n\n")}
	return Rendered{Stdout: marshal(out), Enforced: true, AdviceDelivered: in.Advice != ""}
}

// renderPi renders the deny arm, plus ask degraded to deny per §7's rule for
// binary-channel engines — Pi has no ask arm and no wire form for allow at
// all, so an explicit allow falls through to the default case below exactly
// as Codex's does. That case also carries standalone advice, if any, as an
// advisory, which the bridge appends to that call's own tool result. With no
// advice there is nothing to print, and Enforced stays false for an explicit
// allow: not blocking already is allow, so there is nothing this render step
// could add.
func renderPi(in Input) Rendered {
	switch in.Verdict {
	case Deny:
		return renderPiDeny(in.Reason, in.Advice)
	case Ask:
		return renderPiDeny(piAskDegradedReason(in.Reason), in.Advice)
	default:
		if in.Advice != "" {
			return Rendered{Stdout: marshal(piResponse{Advisory: in.Advice}), Enforced: in.Verdict == Abstain, AdviceDelivered: true}
		}
		return Rendered{Enforced: in.Verdict == Abstain}
	}
}

// piAskDegradedReason is Pi's own text for a degraded ask. It must not reuse
// codexAskDegradedReason verbatim: the string names the engine that has no
// ask channel, and printing "Codex" into a Pi deny would mislead about which
// engine's response this is.
func piAskDegradedReason(handlerReason string) string {
	const degraded = "hookyard verdict was ask; Pi has no ask channel, so the call was denied"
	if handlerReason == "" {
		return degraded
	}
	return handlerReason + " — " + degraded
}

// renderTurnEnd renders the turn_end decision slot (pi D3, Claude Code and
// Codex §11.3). Deny means "do not stop yet — continue with this reason",
// rendered through the engine's own block wire shape (renderPiDeny for pi,
// renderStopBlock for Claude Code/Codex). Unlike pre_tool's
// renderPi/renderCodex, Ask is not degraded to deny, because there is no safe
// direction to degrade to here — a forced continuation is not "safer" than
// stopping. Allow and Abstain print nothing; Abstain alone is
// enforced, since it is the only verdict here that matches what the engine
// would have done anyway. A standalone (non-deny) advisory is never
// rendered: HasAdvisorySlot excludes TurnEnd on every engine — Claude Code's
// Stop has no additionalContext, and pi's turn_end has no advisory slot
// either.
//
// StopHookActive short-circuits everything above: it means a block printed
// here would be silently ignored — pi's bridge already spent this run's one
// continuation (pi_bridge.ts's per-run cap), or Claude Code/Codex's engine
// already forced one. Render reports that honestly — nothing printed,
// Enforced true only for Abstain — rather than claiming an enforcement the
// engine cannot act on.
func renderTurnEnd(in Input, block func(reason, advice string) Rendered) Rendered {
	if in.StopHookActive {
		return Rendered{Enforced: in.Verdict == Abstain}
	}
	switch in.Verdict {
	case Deny:
		reason := in.Reason
		if strings.TrimSpace(reason) == "" {
			// A blank reason is dropped rather than joined in front of the advice.
			reason = ""
			if strings.TrimSpace(in.Advice) == "" {
				reason = turnEndEmptyDenyReason
			}
		}
		return block(reason, in.Advice)
	case Abstain:
		return Rendered{Enforced: true}
	default: // Ask, Allow
		return Rendered{Enforced: false}
	}
}

// renderStopBlock renders Claude Code's and Codex's Stop block: reason and
// advice join into stopResponse's one reason field, the same way
// renderPiDeny joins them into pi's.
func renderStopBlock(reason, advice string) Rendered {
	joined := joinReasonAdvice(reason, advice)
	out := stopResponse{Decision: "block", Reason: joined}
	return Rendered{Stdout: marshal(out), Enforced: true, AdviceDelivered: advice != ""}
}

// renderPiDeny joins reason and advice into Pi's one reason field, the same
// way renderCursor joins them into user_message. Advice does not get its own
// key here even though renderPi's default arm has one for a standalone
// advisory: a deny's advice rides the block reason only, so the bridge never
// also appends it to a tool result, and it is delivered exactly once.
func renderPiDeny(reason, advice string) Rendered {
	out := piResponse{Block: true, Reason: joinReasonAdvice(reason, advice)}
	return Rendered{Stdout: marshal(out), Enforced: true, AdviceDelivered: advice != ""}
}

// joinReasonAdvice joins a non-empty reason and advice into a block's one
// reason field (pi's block, Claude Code/Codex's Stop block).
func joinReasonAdvice(reason, advice string) string {
	var message []string
	if reason != "" {
		message = append(message, reason)
	}
	if advice != "" {
		message = append(message, advice)
	}
	return strings.Join(message, "\n\n")
}

// renderPiAdvisoryOnly renders Pi's standalone advisory on the events where the
// bridge has a channel to the model — an injected message after session_start,
// an appended block on tool_result — but no decision channel. Reason is dropped
// for the same reason renderClaudeCodeAdvisoryOnly drops it: there is nowhere
// to put it, so a non-Abstain verdict here is recorded unenforced.
func renderPiAdvisoryOnly(in Input) Rendered {
	if in.Advice == "" {
		return Rendered{Enforced: in.Verdict == Abstain}
	}
	out := piResponse{Advisory: in.Advice}
	return Rendered{Stdout: marshal(out), Enforced: in.Verdict == Abstain, AdviceDelivered: true}
}

// marshal cannot fail here: every field of every response struct is a string.
// Dropping the error keeps Render total rather than handing the security path
// an error case with no sound fallback.
func marshal(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}
