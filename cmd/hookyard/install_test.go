package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRouterPath carries the /bin/hookyard marker BuildPlan requires; tests
// that are not exercising the marker check use it as-is.
const testRouterPath = "/nix/store/abc/bin/hookyard"

// writeTestManifest writes a manifest whose exec points at a real executable
// file, so manifest.execIsRunnable's stat succeeds and tests exercise the
// rule under test rather than that check.
func writeTestManifest(t *testing.T, dir string) string {
	t.Helper()
	exec := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"handlers":[{"id":"a","exec":"` + exec + `","events":["pre_tool"],"engines":["cursor"],"match":["Bash"]}]}`
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// A writer for one engine failing must not have undone the table write that
// already happened: the table precedes the engine configs precisely so a
// later failure leaves it in place.
func TestRunInstallLeavesTheTableBehindWhenAnEngineWriterFails(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := filepath.Join(dir, "claude.json")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "cursor-target")
	if err := os.Mkdir(cursor, 0o755); err != nil {
		t.Fatal(err)
	}

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, claude, codex, cursor, false)
	if err == nil {
		t.Fatal("want an error from the unwritable cursor target, got nil")
	}

	if mode := statMode(t, stateDir); mode != 0o700 {
		t.Errorf("want state dir at 0700, got %o", mode)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "table.json")); err != nil {
		t.Errorf("want table.json to survive the failed engine write: %v", err)
	}
}

// A state directory an earlier, looser tool left at 0755 must be tightened,
// not trusted.
func TestRunInstallTightensAPreexistingStateDir(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude.json")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, claude, codex, cursor, false); err != nil {
		t.Fatal(err)
	}

	if mode := statMode(t, stateDir); mode != 0o700 {
		t.Errorf("want state dir tightened to 0700, got %o", mode)
	}
}

// render.command formats one unquoted string that each engine's own shell
// splits itself, so a path with whitespace or a shell metacharacter in it
// must be rejected before anything is written.
func TestRunInstallRejectsShellUnsafePaths(t *testing.T) {
	unsafe := []string{"has space", "semi;colon", "dollar$sign", "back`tick"}

	for _, bad := range unsafe {
		t.Run("router-path/"+bad, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := writeTestManifest(t, dir)
			stateDir := filepath.Join(dir, "state")

			err := runInstall(manifestPaths{manifestPath}, testRouterPath+bad, stateDir,
				filepath.Join(dir, "claude.json"), filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), false)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "--router-path") {
				t.Errorf("want the error to name --router-path, got: %v", err)
			}
			if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
				t.Errorf("want no state dir written, got stat err: %v", statErr)
			}
		})

		t.Run("state-dir/"+bad, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := writeTestManifest(t, dir)
			stateDir := filepath.Join(dir, "state"+bad)

			err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir,
				filepath.Join(dir, "claude.json"), filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), false)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "--state-dir") {
				t.Errorf("want the error to name --state-dir, got: %v", err)
			}
			if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
				t.Errorf("want no state dir written, got stat err: %v", statErr)
			}
		})
	}
}

// --dry-run must stay read-only: nothing on disk names hookyard's own state
// until a real install runs.
func TestRunInstallDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := filepath.Join(dir, "claude.json")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, claude, codex, cursor, true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("want no state dir created by --dry-run, got stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "table.json")); !os.IsNotExist(err) {
		t.Errorf("want no table.json written by --dry-run, got stat err: %v", err)
	}
}
