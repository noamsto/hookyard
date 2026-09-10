package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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

// symlinkedDestination stands in for a path something else manages — on a
// Nix machine home-manager places ~/.claude/settings.json as a store link.
func symlinkedDestination(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(dir, "managed-elsewhere.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// The refusal is a pre-flight over all three destinations, so a symlink at
// one engine must leave the other two — and the table — untouched. Anything
// less is a half-applied failed install.
func TestRunInstallRefusesASymlinkedDestinationBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := symlinkedDestination(t, dir)
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, claude, codex, cursor, false)
	if err == nil {
		t.Fatal("want a refusal for the symlinked destination, got nil")
	}
	if !strings.Contains(err.Error(), claude) {
		t.Errorf("error does not name the symlinked path: %v", err)
	}

	for _, path := range []string{codex, cursor, stateDir, filepath.Join(stateDir, "table.json")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("want %s left unwritten by the refused install, got stat err: %v", path, statErr)
		}
	}
	info, statErr := os.Lstat(claude)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the destination is no longer a symlink: %v", info.Mode())
	}
}

// --dry-run writes nothing, so it must keep reporting the plan on a machine
// whose destinations are exactly the symlinks an install refuses.
func TestRunInstallDryRunSucceedsAgainstASymlinkedDestination(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := symlinkedDestination(t, dir)

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, claude,
		filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"), true); err != nil {
		t.Fatalf("want --dry-run to survive a symlinked destination, got %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("want no state dir created by --dry-run, got stat err: %v", err)
	}
}

// writeAllEnginesManifest is writeTestManifest's counterpart for the
// --allow-empty tests below, which need a first install that actually puts a
// hookyard row in all three engine configs so the second, empty install has
// something to strip.
func writeAllEnginesManifest(t *testing.T, dir string) string {
	t.Helper()
	exec := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"handlers":[{"id":"a","exec":"` + exec + `","events":["pre_tool"],` +
		`"engines":["claude-code","codex","cursor"],"match":["Bash"]}]}`
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// install() (not runInstall) is what §-test-safety above is about: it builds
// its target flags' defaults from the real home directory before parsing, so
// every call here must pass all four target flags plus --router-path.
func TestInstallAllowEmptyStripsHookyardRowsFromAllThreeConfigs(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeAllEnginesManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	claude := filepath.Join(dir, "claude.json")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")

	if err := os.WriteFile(claude, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Foreign","hooks":[{"type":"command","command":"/usr/bin/foreign-claude-hook"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("[mcp_servers.context7]\ncommand = \"context7-mcp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursor, []byte(`{"version":1,"hooks":{"preToolUse":[{"command":"/usr/bin/foreign-cursor-hook"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	targetFlags := []string{
		"--router-path", testRouterPath,
		"--state-dir", stateDir,
		"--claude-settings", claude,
		"--codex-config", codex,
		"--cursor-hooks", cursor,
	}

	if err := install(append([]string{"--manifest", manifestPath}, targetFlags...)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--registered-for claude-code", "--registered-for codex", "--registered-for cursor"} {
		if got := readFile(t, claude) + readFile(t, codex) + readFile(t, cursor); !strings.Contains(got, want) {
			t.Fatalf("setup: first install did not register %q, got:\n%s", want, got)
		}
	}

	if err := install(append([]string{"--allow-empty"}, targetFlags...)); err != nil {
		t.Fatal(err)
	}

	claudeGot := readFile(t, claude)
	codexGot := readFile(t, codex)
	cursorGot := readFile(t, cursor)

	for _, got := range []string{claudeGot, codexGot, cursorGot} {
		if strings.Contains(got, "hookyard") {
			t.Errorf("a hookyard row survived the empty install\n--- got ---\n%s", got)
		}
	}
	if !strings.Contains(claudeGot, "foreign-claude-hook") {
		t.Errorf("empty install dropped the foreign Claude Code entry\n--- got ---\n%s", claudeGot)
	}
	if !strings.Contains(codexGot, `[mcp_servers.context7]`) {
		t.Errorf("empty install dropped the foreign Codex entry\n--- got ---\n%s", codexGot)
	}
	if !strings.Contains(cursorGot, "foreign-cursor-hook") {
		t.Errorf("empty install dropped the foreign Cursor entry\n--- got ---\n%s", cursorGot)
	}

	handlers, err := manifest.ReadTable(filepath.Join(stateDir, "table.json"))
	if err != nil {
		t.Fatalf("ReadTable on the emptied table: %v", err)
	}
	if len(handlers) != 0 {
		t.Errorf("want zero handlers in the table, got %d", len(handlers))
	}
}

func TestInstallWithoutManifestOrAllowEmptyStillErrors(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	err := install([]string{
		"--router-path", testRouterPath,
		"--state-dir", stateDir,
		"--claude-settings", filepath.Join(dir, "claude.json"),
		"--codex-config", filepath.Join(dir, "config.toml"),
		"--cursor-hooks", filepath.Join(dir, "hooks.json"),
	})
	if err == nil {
		t.Fatal("want an error for a manifest-less install without --allow-empty, got nil")
	}
	if !strings.Contains(err.Error(), "no --manifest given") {
		t.Errorf("want the bare-invocation message, got: %v", err)
	}
}
