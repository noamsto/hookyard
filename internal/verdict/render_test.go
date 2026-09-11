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
			name:     "codex deny drops advice",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Deny, Reason: "r", Advice: "a"},
			stdout:   `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r"}}`,
			enforced: true,
		},
		{
			name:     "codex standalone advice has no slot",
			in:       Input{Engine: vocab.Codex, CanonicalEvent: vocab.PreTool, NativeEvent: "PreToolUse", Verdict: Abstain, Advice: "a"},
			enforced: true,
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
			// No advisory-only response Pi honours has been confirmed, so
			// standalone advice is dropped rather than claimed as delivered.
			name:     "pi standalone advice is not delivered",
			in:       Input{Engine: vocab.Pi, CanonicalEvent: vocab.PreTool, NativeEvent: "tool_call", Verdict: Abstain, Advice: "a"},
			enforced: true,
		},
	}

	for _, c := range cases {
		checkRendered(t, c.name, Render(c.in), c.stdout, c.enforced, c.delivered)
	}
}

func TestRenderOffADecisionSlotPrintsNothing(t *testing.T) {
	for _, engine := range vocab.Engines {
		native, ok := vocab.NativeEvent(engine, vocab.PostTool)
		if !ok {
			t.Fatalf("%s has no native post_tool event", engine)
		}
		for _, v := range lattice {
			in := Input{Engine: engine, CanonicalEvent: vocab.PostTool, NativeEvent: native, Verdict: v, Reason: "r", Advice: "a"}
			// Enforced is true only for abstain: any other verdict was computed
			// and cannot be acted on, which is what the record must show.
			checkRendered(t, fmt.Sprintf("%s post_tool %s", engine, v), Render(in), "", v == Abstain, false)
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

func TestCapabilityTable(t *testing.T) {
	for _, engine := range vocab.Engines {
		for _, event := range vocab.CanonicalEvents {
			native, ok := vocab.NativeEvent(engine, event)
			if !ok {
				t.Fatalf("%s has no native %s event", engine, event)
			}
			want := event == vocab.PreTool
			if got := HasDecisionSlot(engine, event, native); got != want {
				t.Errorf("HasDecisionSlot(%s, %s) = %v, want %v", engine, event, got, want)
			}
			// Codex has no advisory slot on any event.
			wantAdvisory := want && engine != vocab.Codex
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
