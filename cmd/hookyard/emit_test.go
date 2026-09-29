package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/render"
)

// renderClaudeOverlay with no base is exactly ClaudeSettings over
// ClaudeCatalogPlan's rows and nothing else — the golden case emit's whole
// pipeline reduces to once --manifest is gone (R-B).
func TestRenderClaudeOverlayMatchesClaudeCatalogPlanWithNoBase(t *testing.T) {
	stateDir := "/var/lib/hookyard"
	got, err := renderClaudeOverlay(testRouterPath, stateDir, "")
	if err != nil {
		t.Fatal(err)
	}

	entries, err := render.ClaudeCatalogPlan(testRouterPath, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := render.ClaudeSettings(nil, entries)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != string(want) {
		t.Errorf("renderClaudeOverlay() =\n%s\nwant\n%s", got, want)
	}
}

// A base carrying its own statusLine, an inherited PreToolUse hook and a
// stale hookyard-marked row (from an older emit run) must come out with the
// first two kept, the stale row stripped, and the catalog rows present.
func TestRenderClaudeOverlayKeepsForeignFieldsAndStripsStaleRows(t *testing.T) {
	dir := t.TempDir()
	base := `{
  "statusLine": {"type": "command", "command": "/nix/store/statusline/bin/statusline"},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/nix/store/guards/bin/git-guard"}]}
    ],
    "Notification": [
      {"hooks": [{"type": "command", "command": "/old/store/path/bin/hookyard route --registered-for claude-code --event claude-code:Notification", "timeout": 5}]}
    ]
  }
}`
	basePath := filepath.Join(dir, "base.json")
	if err := os.WriteFile(basePath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := renderClaudeOverlay(testRouterPath, filepath.Join(dir, "state"), basePath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)

	for _, want := range []string{`"statusLine"`, "/nix/store/guards/bin/git-guard"} {
		if !strings.Contains(out, want) {
			t.Errorf("base content dropped: %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "/old/store/path") {
		t.Errorf("stale hookyard-marked row survived\n--- got ---\n%s", out)
	}
	for _, native := range []string{
		"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
		"PreCompact", "Stop", "Notification", "SessionEnd",
	} {
		if !strings.Contains(out, `"`+native+`"`) {
			t.Errorf("catalog row missing for %s\n--- got ---\n%s", native, out)
		}
	}
}

// An old invocation's --manifest must fail on the unknown flag rather than
// being silently ignored (R-B).
func TestEmitRejectsManifestFlag(t *testing.T) {
	dir := t.TempDir()
	err := emit([]string{
		"--engine", "claude-code",
		"--router-path", testRouterPath,
		"--state-dir", filepath.Join(dir, "state"),
		"--manifest", "x",
	})
	if err == nil {
		t.Fatal("want an error for the removed --manifest flag, got nil")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("want the error to mention manifest, got: %v", err)
	}
}
