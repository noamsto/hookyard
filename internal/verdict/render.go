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

// piResponse is the bridge's wire shape: binary and reason-only, with no
// decision field at all — the bridge blocks by returning this, and returns
// nothing (allow) otherwise (§7).
type piResponse struct {
	Block  bool   `json:"block"`
	Reason string `json:"reason,omitempty"`
}

// Render turns a consolidated decision into the bytes one engine expects on
// one event. It is total: an (engine, event) pair with no decision slot prints
// nothing rather than failing, so no caller needs a fallback on this path.
// Stdout is nil whenever nothing is printed.
func Render(in Input) Rendered {
	if !HasDecisionSlot(in.Engine, in.CanonicalEvent, in.NativeEvent) {
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
// as Codex's does, printing nothing and recording Enforced false: not
// blocking already is allow, so there is nothing this render step could add.
func renderPi(in Input) Rendered {
	switch in.Verdict {
	case Deny:
		return renderPiDeny(in.Reason, in.Advice)
	case Ask:
		return renderPiDeny(piAskDegradedReason(in.Reason), in.Advice)
	default:
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

// renderPiDeny joins reason and advice into Pi's one reason field, the same
// way renderCursor joins them into user_message: the block reason is the only
// slot observed reaching the model, and standalone advice has no path of its
// own (§7), so advice only reaches the model riding along a block.
func renderPiDeny(reason, advice string) Rendered {
	var message []string
	if reason != "" {
		message = append(message, reason)
	}
	if advice != "" {
		message = append(message, advice)
	}
	out := piResponse{Block: true, Reason: strings.Join(message, "\n\n")}
	return Rendered{Stdout: marshal(out), Enforced: true, AdviceDelivered: advice != ""}
}

// marshal cannot fail here: every field of every response struct is a string.
// Dropping the error keeps Render total rather than handing the security path
// an error case with no sound fallback.
func marshal(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}
