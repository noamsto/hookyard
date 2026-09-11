package vocab

import "testing"

func TestBuildInboundToolsInvertsEveryRealEngineColumn(t *testing.T) {
	if _, err := buildInboundTools(nativeTools); err != nil {
		t.Fatalf("nativeTools should invert cleanly, got %v", err)
	}
}

// A builder that never checks for collisions would pass the happy-path test
// above too, so this asserts the failure directly.
func TestBuildInboundToolsReportsACollision(t *testing.T) {
	synthetic := map[Engine]map[string]string{
		Cursor: {"Write": "Write", "Edit": "Write"},
	}
	_, err := buildInboundTools(synthetic)
	if err == nil {
		t.Fatal("want an error for a non-injective column, got nil")
	}
}

func TestInboundToolMapsKnownNativesPerEngine(t *testing.T) {
	tests := []struct {
		engine Engine
		native string
		want   string
	}{
		{Codex, "Bash", "Bash"},
		{Codex, "apply_patch", "Write"},
		{Codex, "view_image", "Read"},
		{Cursor, "Read", "Read"},
		{Cursor, "Write", "Write"},
		{Cursor, "Shell", "Bash"},
		{Cursor, "Grep", "Grep"},
		{ClaudeCode, "Read", "Read"},
		{ClaudeCode, "Write", "Write"},
		{ClaudeCode, "Bash", "Bash"},
		{ClaudeCode, "Grep", "Grep"},
		{ClaudeCode, "Glob", "Glob"},
		{Pi, "read", "Read"},
		{Pi, "write", "Write"},
		{Pi, "bash", "Bash"},
		{Pi, "grep", "Grep"},
		{Pi, "find", "Glob"},
	}
	for _, tt := range tests {
		if got := InboundTool(tt.engine, tt.native); got != tt.want {
			t.Errorf("InboundTool(%s, %q) = %q, want %q", tt.engine, tt.native, got, tt.want)
		}
	}
}

// A native name hookyard has no mapping for must pass through unchanged, not
// vanish: dropping it would turn "no mapping" into "no tool at all".
func TestInboundToolPassesUnknownNativesThroughUnchanged(t *testing.T) {
	if got := InboundTool(Codex, "exec_command"); got != "exec_command" {
		t.Errorf("want the unknown native passed through unchanged, got %q", got)
	}
}

// Cursor's preToolUse and Claude Code's PreToolUse differ only in case; a flat
// map would resolve one engine's payload against the other's spelling.
func TestInboundEventIsScopedPerEngine(t *testing.T) {
	if got, ok := InboundEvent(Cursor, "preToolUse"); !ok || got != PreTool {
		t.Errorf("Cursor preToolUse = (%q, %v), want (%q, true)", got, ok, PreTool)
	}
	if _, ok := InboundEvent(Cursor, "PreToolUse"); ok {
		t.Error("Claude Code's spelling should not resolve when asked as Cursor")
	}
}

func TestInboundEventUnrecognizedNativeReturnsFalse(t *testing.T) {
	if got, ok := InboundEvent(Cursor, "beforeShellExecution"); ok || got != "" {
		t.Errorf("beforeShellExecution = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestProtocol(t *testing.T) {
	tests := []struct {
		native string
		want   string
	}{
		{"beforeShellExecution", "shell"},
		{"afterShellExecution", "shell"},
		{"beforeMCPExecution", "mcp"},
		{"afterMCPExecution", "mcp"},
		{"beforeReadFile", "file"},
		{"afterFileEdit", "file"},
		{"beforeTabFileRead", "file"},
		{"afterTabFileEdit", "file"},
		{"preToolUse", ""}, // Cursor's deployed event, not protocol-split
	}
	for _, tt := range tests {
		if got := Protocol(tt.native); got != tt.want {
			t.Errorf("Protocol(%q) = %q, want %q", tt.native, got, tt.want)
		}
	}
}
