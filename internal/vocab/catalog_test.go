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

func TestPiCatalogCanonicalRowsMatchNativeEventsInOrder(t *testing.T) {
	if len(PiCatalog) < len(CanonicalEvents) {
		t.Fatalf("catalog has %d rows, want at least %d canonical rows", len(PiCatalog), len(CanonicalEvents))
	}
	for i, canonical := range CanonicalEvents {
		native := PiCatalog[i]
		want := nativeEvents[Pi][canonical]
		if native != want {
			t.Errorf("row %d: native = %q, want nativeEvents[Pi][%q] = %q", i, native, canonical, want)
		}
	}
}

// The scoped rows (beyond the six canonical) must be genuinely additional —
// reachable only as "pi:<native>" — not a restatement of a canonical native
// under another name.
func TestPiCatalogScopedRowsAreEngineScoped(t *testing.T) {
	canonicalNatives := map[string]bool{}
	for _, canonical := range CanonicalEvents {
		canonicalNatives[nativeEvents[Pi][canonical]] = true
	}
	for _, native := range PiCatalog[len(CanonicalEvents):] {
		if canonicalNatives[native] {
			t.Errorf("native %q duplicates a canonical row instead of being genuinely scoped", native)
		}
	}
}

func TestPiCatalogHasNoDuplicateNatives(t *testing.T) {
	seen := map[string]bool{}
	for _, native := range PiCatalog {
		if seen[native] {
			t.Errorf("native %q appears more than once in the catalog", native)
		}
		seen[native] = true
	}
}

func TestIsPiEvent(t *testing.T) {
	for _, native := range PiCatalog {
		if !IsPiEvent(native) {
			t.Errorf("IsPiEvent(%q) = false, want true", native)
		}
	}
	for _, native := range []string{"before_agent_start", "session_compact"} {
		if IsPiEvent(native) {
			t.Errorf("IsPiEvent(%q) = true, want false", native)
		}
	}
}

// D1: both pi's settle boundary (the new canonical TurnEnd native) and its
// per-turn turn_end (now scoped-only) must remain routable natives.
func TestIsPiEventTurnEndAndAgentBeforeSettle(t *testing.T) {
	for _, native := range []string{"turn_end", "agent_before_settle"} {
		if !IsPiEvent(native) {
			t.Errorf("IsPiEvent(%q) = false, want true", native)
		}
	}
}

// agent_settled is the terminal idle signal a dashboard reads; if it ever fell
// out of the catalog a manifest naming pi:agent_settled would fail validation
// and the signal would silently stop being routable.
func TestPiCatalogIncludesAgentSettled(t *testing.T) {
	if !IsPiEvent("agent_settled") {
		t.Error("IsPiEvent(\"agent_settled\") = false, want true")
	}
	if PiCatalog[len(PiCatalog)-1] != "turn_end" {
		t.Errorf("PiCatalog last row = %q, want the scoped turn_end row", PiCatalog[len(PiCatalog)-1])
	}
}
