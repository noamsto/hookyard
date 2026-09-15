package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

func TestPluginPlanRendersTheLauncherCommand(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"claude-code", "codex"}, Match: []string{"Bash"}},
	}
	entries, err := PluginPlan(handlers, vocab.ClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	want := Entry{
		Event:   "PreToolUse",
		Matcher: "Bash",
		Command: `"${CLAUDE_PLUGIN_ROOT}/bin/hookyard" route --registered-for claude-code --event pre_tool --plugin-root "${CLAUDE_PLUGIN_ROOT}"`,
	}
	if entries[0] != want {
		t.Errorf("got %+v, want %+v", entries[0], want)
	}
}

// The marker is how build's own hooks.json merge finds hookyard's entries
// again on a re-run, the same discipline yard mode's command relies on.
func TestPluginPlanCommandCarriesTheMarker(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"claude-code"}, Match: []string{"Bash"}},
	}
	entries, err := PluginPlan(handlers, vocab.ClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Command; !strings.Contains(got, Marker) {
		t.Errorf("command %q does not contain marker %q", got, Marker)
	}
}

func TestPluginPlanRefusesAnUnsupportedEngine(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"codex"}, Match: []string{"Bash"}},
	}
	if _, err := PluginPlan(handlers, vocab.Codex); err == nil {
		t.Fatal("want an error for an engine build mode does not support yet, got nil")
	}
}

func TestPluginPlanEntriesRenderIntoValidClaudeSettings(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"claude-code"}, Match: []string{"Bash"}},
	}
	entries, err := PluginPlan(handlers, vocab.ClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ClaudeSettings(nil, entries)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}
	if len(parsed.Hooks.PreToolUse) != 1 || len(parsed.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("want one PreToolUse group with one hook, got %+v", parsed.Hooks.PreToolUse)
	}
	hook := parsed.Hooks.PreToolUse[0].Hooks[0]
	if hook.Command != entries[0].Command {
		t.Errorf("got command %q, want %q", hook.Command, entries[0].Command)
	}
	if hook.Timeout != EmittedTimeoutSeconds {
		t.Errorf("got timeout %d, want %d", hook.Timeout, EmittedTimeoutSeconds)
	}
}
