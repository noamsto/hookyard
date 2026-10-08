package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func guardInput(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantDeny(t *testing.T, res result) {
	t.Helper()
	wantExit(t, res, 0)
	var out struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("stdout %q is not a deny: %v", res.stdout, err)
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" || out.HookSpecificOutput.PermissionDecisionReason == "" {
		t.Fatalf("not a deny with a reason: %q", res.stdout)
	}
}

func TestHookWriteGuardDenies(t *testing.T) {
	sb := newSandbox(t, "work")
	attest := filepath.Join(sb.personal, ".attest", "entry")
	fact := filepath.Join(sb.personal, "repo", "fact.md")
	claim := "---\nname: n\nmetadata:\n  confidence: reviewed\n---\nb\n"
	tests := []struct{ name, tool, input string }{
		{"attest entry Write", "Write", guardInput(t, map[string]any{"file_path": attest, "content": "x\n"})},
		{"attest entry Bash", "Bash", guardInput(t, map[string]any{"command": "echo x > " + attest})},
		{"index Write", "Write", guardInput(t, map[string]any{"file_path": filepath.Join(sb.personal, "MEMORY.md"), "content": "x\n"})},
		{"index Bash", "Bash", guardInput(t, map[string]any{"command": "echo x | tee " + filepath.Join(sb.work, "MEMORY.md")})},
		{"state dir Write", "Write", guardInput(t, map[string]any{"file_path": filepath.Join(sb.state, "local", "personal", "x.md"), "content": "x\n"})},
		{"state dir Bash", "Bash", guardInput(t, map[string]any{"command": "touch " + filepath.Join(sb.state, "provenance", "s") + " && rm -f x"})},
		{"state dir MultiEdit", "MultiEdit", guardInput(t, map[string]any{"file_path": filepath.Join(sb.state, "quarantine", "x.md"),
			"edits": []map[string]string{{"old_string": "a", "new_string": "b"}}})},
		{"fact reviewed Write", "Write", guardInput(t, map[string]any{"file_path": fact, "content": claim})},
		{"fact reviewed Edit", "Edit", guardInput(t, map[string]any{"file_path": fact, "old_string": "proposed", "new_string": "  confidence: reviewed"})},
		{"fact Bash", "Bash", guardInput(t, map[string]any{"command": "sed -i s/a/b/ " + fact})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantDeny(t, sb.run(sb.toolEnvelope("pre_tool", "sess-1", tt.tool, tt.input, `"ok"`)))
		})
	}
}

func TestHookWriteGuardAllows(t *testing.T) {
	sb := newSandbox(t, "personal")
	fact := filepath.Join(sb.personal, "repo", "fact.md")
	tests := []struct{ name, tool, input string }{
		{"unrelated Write", "Write", guardInput(t, map[string]any{"file_path": filepath.Join(sb.dir, "repo", "main.go"), "content": "package main\n"})},
		{"unrelated Bash write", "Bash", guardInput(t, map[string]any{"command": "echo x > " + filepath.Join(sb.dir, "out.txt")})},
		{"cat index", "Bash", guardInput(t, map[string]any{"command": "cat " + filepath.Join(sb.personal, "MEMORY.md")})},
		{"rg state", "Bash", guardInput(t, map[string]any{"command": "rg needle " + sb.state})},
		{"cat attest", "Bash", guardInput(t, map[string]any{"command": "cat " + filepath.Join(sb.personal, ".attest", "entry")})},
		{"fact proposed Write", "Write", guardInput(t, map[string]any{"file_path": fact, "content": "---\nmetadata:\n  confidence: proposed\n---\nb\n"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb.send(sb.toolEnvelope("pre_tool", "sess-1", tt.tool, tt.input, `"ok"`))
		})
	}
}
