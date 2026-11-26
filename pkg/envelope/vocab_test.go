package envelope

import (
	"slices"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

func TestReexportedVocabMatchesInternal(t *testing.T) {
	if !slices.Equal(Engines(), vocab.Engines) {
		t.Errorf("Engines() = %v, want %v", Engines(), vocab.Engines)
	}
	if !slices.Equal(CanonicalEvents(), vocab.CanonicalEvents) {
		t.Errorf("CanonicalEvents() = %v, want %v", CanonicalEvents(), vocab.CanonicalEvents)
	}
	if ClaudeCode != vocab.ClaudeCode || Codex != vocab.Codex || Cursor != vocab.Cursor || Pi != vocab.Pi {
		t.Error("engine constants drifted from internal/vocab")
	}
	if SessionStart != vocab.SessionStart || TurnEnd != vocab.TurnEnd || PreTool != vocab.PreTool {
		t.Error("canonical event constants drifted from internal/vocab")
	}
}
