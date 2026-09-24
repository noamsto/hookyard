package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/installstate"
	"github.com/noamsto/hookyard/internal/render"
)

// claudeHookCommands returns every hook command in a settings.json, by event.
func claudeHookCommands(t *testing.T, path string) map[string][]string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for event, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[event] = append(out[event], h.Command)
			}
		}
	}
	return out
}

// Without Nix, --claude-settings writes the same fixed catalog emit renders,
// merged beside whatever the file already held, and the receipt records it.
func TestRunInstallWritesClaudeSettingsBesideForeignEntries(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := filepath.Join(dir, "claude", "settings.json")
	foreign := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/mine"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func() {
		t.Helper()
		if err := runInstall(io.Discard, manifestPaths{manifestPath}, testRouterPath, stateDir,
			filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), claude,
			[]string{filepath.Join(dir, "pi-settings.json")}, false); err != nil {
			t.Fatal(err)
		}
	}
	run()
	// A second run must replace hookyard's rows, not stack another copy.
	run()

	catalog, err := render.ClaudeCatalogPlan(testRouterPath, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	commands := claudeHookCommands(t, claude)
	marked := 0
	for _, cmds := range commands {
		for _, c := range cmds {
			if strings.Contains(c, render.Marker) {
				marked++
			}
		}
	}
	if marked != len(catalog) {
		t.Errorf("want %d hookyard rows (one per catalog event), got %d: %v", len(catalog), marked, commands)
	}
	if !strings.Contains(strings.Join(commands["PreToolUse"], " "), "/usr/local/bin/mine") {
		t.Errorf("the foreign PreToolUse hook was dropped: %v", commands["PreToolUse"])
	}
	if !strings.Contains(readFile(t, claude), `"model"`) {
		t.Error("a non-hooks key of settings.json was dropped")
	}
	if mode := statMode(t, claude); mode != 0o644 {
		t.Errorf("want the file's own 0644 kept, got %o", mode)
	}

	r, err := installstate.ReadReceipt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.ClaudeSettings != claude {
		t.Errorf("receipt claudeSettings = %q, want %q", r.ClaudeSettings, claude)
	}
}

// Under Nix, ~/.claude/settings.json is a home-manager link: the flag must be
// refused before anything is written, like every other symlinked destination.
func TestRunInstallRefusesASymlinkedClaudeSettings(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	target := filepath.Join(dir, "store-settings.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o444); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, claude); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(dir, "hooks.json")

	err := runInstall(io.Discard, manifestPaths{manifestPath}, testRouterPath, filepath.Join(dir, "state"),
		filepath.Join(dir, "config.toml"), cursor, claude, []string{filepath.Join(dir, "pi-settings.json")}, false)
	if err == nil || !strings.Contains(err.Error(), "--claude-settings") {
		t.Fatalf("want a refusal naming --claude-settings, got %v", err)
	}
	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Errorf("a refused install must not have written the other engines first (stat cursor: %v)", err)
	}
}

// An empty install is how hookyard is turned off: it strips the catalog from
// settings.json and leaves the foreign entries alone.
func TestRunInstallWithNoManifestsStripsClaudeSettings(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(claude, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(paths manifestPaths) error {
		return runInstall(io.Discard, paths, testRouterPath, stateDir,
			filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), claude,
			[]string{filepath.Join(dir, "pi-settings.json")}, false)
	}
	if err := args(manifestPaths{manifestPath}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, claude), render.Marker) {
		t.Fatal("want the catalog written before the strip is tested")
	}
	if err := args(nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, claude)
	if strings.Contains(got, render.Marker) || strings.Contains(got, `"hooks"`) {
		t.Errorf("want every hookyard row and the emptied hooks key gone, got %s", got)
	}
	if !strings.Contains(got, `"theme"`) {
		t.Errorf("the strip dropped a foreign key: %s", got)
	}
}

// With no --claude-settings, install still never touches Claude Code's config.
func TestRunInstallWithoutClaudeSettingsRecordsNone(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	if err := runInstall(io.Discard, manifestPaths{manifestPath}, testRouterPath, stateDir,
		filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), "",
		[]string{filepath.Join(dir, "pi-settings.json")}, false); err != nil {
		t.Fatal(err)
	}
	r, err := installstate.ReadReceipt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.ClaudeSettings != "" {
		t.Errorf("receipt claudeSettings = %q, want empty", r.ClaudeSettings)
	}
}
