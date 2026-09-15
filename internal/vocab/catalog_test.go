package vocab

import "testing"

func TestClaudeCodeCatalogCanonicalRowsMatchNativeEventsInOrder(t *testing.T) {
	if len(ClaudeCodeCatalog) < len(CanonicalEvents) {
		t.Fatalf("catalog has %d rows, want at least %d canonical rows", len(ClaudeCodeCatalog), len(CanonicalEvents))
	}
	for i, canonical := range CanonicalEvents {
		row := ClaudeCodeCatalog[i]
		if row.Routed != canonical {
			t.Errorf("row %d: Routed = %q, want canonical name %q", i, row.Routed, canonical)
		}
		want := nativeEvents[ClaudeCode][canonical]
		if row.Native != want {
			t.Errorf("row %d: Native = %q, want nativeEvents[ClaudeCode][%q] = %q", i, row.Native, canonical, want)
		}
	}
}

func TestClaudeCodeCatalogScopedRowsAreEngineScoped(t *testing.T) {
	for _, row := range ClaudeCodeCatalog[len(CanonicalEvents):] {
		want := "claude-code:" + row.Native
		if row.Routed != want {
			t.Errorf("native %q: Routed = %q, want %q", row.Native, row.Routed, want)
		}
	}
}

func TestClaudeCodeCatalogHasNoDuplicateNatives(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range ClaudeCodeCatalog {
		if seen[row.Native] {
			t.Errorf("native %q appears more than once in the catalog", row.Native)
		}
		seen[row.Native] = true
	}
}

func TestIsClaudeCodeEvent(t *testing.T) {
	for _, row := range ClaudeCodeCatalog {
		if !IsClaudeCodeEvent(row.Native) {
			t.Errorf("IsClaudeCodeEvent(%q) = false, want true", row.Native)
		}
	}
	for _, native := range []string{"Notifcation", "FileChanged"} {
		if IsClaudeCodeEvent(native) {
			t.Errorf("IsClaudeCodeEvent(%q) = true, want false", native)
		}
	}
}
