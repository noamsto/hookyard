package vocab

import "testing"

// D1 remaps canonical TurnEnd onto pi's settle boundary, not its per-turn
// native; NativeEvent must resolve both the canonical name and the
// engine-scoped pi:turn_end escape hatch to the right native spellings.
func TestNativeEventPiTurnEndRemap(t *testing.T) {
	if got, ok := NativeEvent(Pi, TurnEnd); !ok || got != "agent_before_settle" {
		t.Errorf("NativeEvent(Pi, TurnEnd) = (%q, %v), want (%q, true)", got, ok, "agent_before_settle")
	}
	if got, ok := NativeEvent(Pi, "pi:turn_end"); !ok || got != "turn_end" {
		t.Errorf(`NativeEvent(Pi, "pi:turn_end") = (%q, %v), want (%q, true)`, got, ok, "turn_end")
	}
}
