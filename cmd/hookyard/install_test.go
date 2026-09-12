package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/envelope"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
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
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "cursor-target")
	if err := os.Mkdir(cursor, 0o755); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(dir, "pi-settings.json")

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, false)
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
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, false); err != nil {
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
				filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"),
				filepath.Join(dir, "pi-settings.json"), false)
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
				filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"),
				filepath.Join(dir, "pi-settings.json"), false)
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
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("want no state dir created by --dry-run, got stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "table.json")); !os.IsNotExist(err) {
		t.Errorf("want no table.json written by --dry-run, got stat err: %v", err)
	}
	// Pi's bridge is the one artifact a dry run could leave behind that is not
	// a config file, so it is named rather than covered by the state-dir check.
	if _, err := os.Stat(render.PiBridgePath(pi)); !os.IsNotExist(err) {
		t.Errorf("want no Pi bridge written by --dry-run, got stat err: %v", err)
	}
}

// symlinkedDestination stands in for a path something else manages — on a
// Nix machine home-manager places engine configs as store links.
func symlinkedDestination(t *testing.T, dir, name string) string {
	t.Helper()
	target := filepath.Join(dir, "managed-elsewhere-"+name)
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, name)
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
	codex := symlinkedDestination(t, dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, false)
	if err == nil {
		t.Fatal("want a refusal for the symlinked destination, got nil")
	}
	if !strings.Contains(err.Error(), codex) {
		t.Errorf("error does not name the symlinked path: %v", err)
	}

	for _, path := range []string{cursor, pi, render.PiBridgePath(pi), stateDir, filepath.Join(stateDir, "table.json")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("want %s left unwritten by the refused install, got stat err: %v", path, statErr)
		}
	}
	info, statErr := os.Lstat(codex)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the destination is no longer a symlink: %v", info.Mode())
	}
}

// The bridge is the second destination Pi contributes, and its refusal has to
// arrive before the table, exactly as a symlinked engine config's does.
func TestRunInstallRefusesASymlinkedPiBridgeBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeAllEnginesManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	bridge := render.PiBridgePath(pi)
	if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "managed-elsewhere.ts")
	if err := os.WriteFile(target, []byte("// managed elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, bridge); err != nil {
		t.Fatal(err)
	}

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, false)
	if err == nil {
		t.Fatal("want a refusal for the symlinked bridge, got nil")
	}
	if !strings.Contains(err.Error(), bridge) {
		t.Errorf("error does not name the symlinked bridge: %v", err)
	}

	for _, path := range []string{codex, cursor, pi, stateDir, filepath.Join(stateDir, "table.json")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("want %s left unwritten by the refused install, got stat err: %v", path, statErr)
		}
	}
	info, statErr := os.Lstat(bridge)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the bridge is no longer a symlink: %v", info.Mode())
	}
}

// A link one segment up is the same hazard with none of the visibility: the
// pre-flight over the final path sees an ordinary missing file, MkdirAll walks
// the link without a word, and the bridge lands wherever it points — executable
// code, redirected, with the install reporting success. bin/ is a segment
// hookyard invents and creates, so nothing a user manages can already be there.
func TestRunInstallRefusesASymlinkedPiBinDirectoryBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeAllEnginesManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	bridge := render.PiBridgePath(pi)
	elsewhere := filepath.Join(dir, "owned-by-something-else")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Dir(bridge)); err != nil {
		t.Fatal(err)
	}

	err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex, cursor, pi, false)
	if err == nil {
		t.Fatal("want a refusal for the symlinked bin/, got nil")
	}
	if !strings.Contains(err.Error(), filepath.Dir(bridge)) {
		t.Errorf("error does not name the symlinked directory: %v", err)
	}

	// The last path is the one the link would have redirected the bridge to.
	for _, path := range []string{codex, cursor, pi, stateDir, filepath.Join(elsewhere, filepath.Base(bridge))} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("want %s left unwritten by the refused install, got stat err: %v", path, statErr)
		}
	}
}

// --dry-run writes nothing, so it must keep reporting the plan on a machine
// whose destinations are exactly the symlinks an install refuses.
func TestRunInstallDryRunSucceedsAgainstASymlinkedDestination(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	codex := symlinkedDestination(t, dir, "config.toml")

	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, stateDir, codex,
		filepath.Join(dir, "hooks.json"), filepath.Join(dir, "pi-settings.json"), true); err != nil {
		t.Fatalf("want --dry-run to survive a symlinked destination, got %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("want no state dir created by --dry-run, got stat err: %v", err)
	}
}

// writeAllEnginesManifest is writeTestManifest's counterpart for the
// --allow-empty tests below, which need a first install that actually puts a
// hookyard row in every engine config so the second, empty install has
// something to strip.
func writeAllEnginesManifest(t *testing.T, dir string) string {
	t.Helper()
	exec := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"handlers":[{"id":"a","exec":"` + exec + `","events":["pre_tool"],` +
		`"engines":["claude-code","codex","cursor","pi"],"match":["Bash"]}]}`
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// install() (not runInstall) is what §-test-safety above is about: it builds
// its target flags' defaults from the real home directory before parsing, so
// every call here must pass all four target flags plus --router-path. Missing
// --pi-settings is the expensive one: the default resolves under the
// developer's real ~/.pi/agent, where this test would rewrite settings.json and
// then, on the --allow-empty pass, delete a bridge it does not own — and fail
// outright where home-manager makes that file a store link.
//
// Claude Code is not among the configs checked here: install never writes
// settings.json (R2), so a manifest naming claude-code has nothing here to
// strip in the first place.
func TestInstallAllowEmptyStripsHookyardRowsFromEveryConfig(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeAllEnginesManifest(t, dir)
	stateDir := filepath.Join(dir, "state")
	codex := filepath.Join(dir, "config.toml")
	cursor := filepath.Join(dir, "hooks.json")
	pi := filepath.Join(dir, "pi-settings.json")

	if err := os.WriteFile(codex, []byte("[mcp_servers.context7]\ncommand = \"context7-mcp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursor, []byte(`{"version":1,"hooks":{"preToolUse":[{"command":"/usr/bin/foreign-cursor-hook"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pi, []byte(`{"extensions":["/usr/lib/foreign-pi-extension.ts"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	targetFlags := []string{
		"--router-path", testRouterPath,
		"--state-dir", stateDir,
		"--codex-config", codex,
		"--cursor-hooks", cursor,
		"--pi-settings", pi,
	}

	if err := install(append([]string{"--manifest", manifestPath}, targetFlags...)); err != nil {
		t.Fatal(err)
	}
	// Pi's registration is split across two files — the entry in settings.json
	// names the bridge, and the router command lives inside the bridge — so
	// both are read here.
	bridge := render.PiBridgePath(pi)
	for _, want := range []string{"--registered-for codex", "--registered-for cursor", "--registered-for pi"} {
		if got := readFile(t, codex) + readFile(t, cursor) + readFile(t, pi) + readFile(t, bridge); !strings.Contains(got, want) {
			t.Fatalf("setup: first install did not register %q, got:\n%s", want, got)
		}
	}

	if err := install(append([]string{"--allow-empty"}, targetFlags...)); err != nil {
		t.Fatal(err)
	}

	codexGot := readFile(t, codex)
	cursorGot := readFile(t, cursor)
	piGot := readFile(t, pi)

	for _, got := range []string{codexGot, cursorGot, piGot} {
		if strings.Contains(got, "hookyard") {
			t.Errorf("a hookyard row survived the empty install\n--- got ---\n%s", got)
		}
	}
	if !strings.Contains(codexGot, `[mcp_servers.context7]`) {
		t.Errorf("empty install dropped the foreign Codex entry\n--- got ---\n%s", codexGot)
	}
	if !strings.Contains(cursorGot, "foreign-cursor-hook") {
		t.Errorf("empty install dropped the foreign Cursor entry\n--- got ---\n%s", cursorGot)
	}
	if !strings.Contains(piGot, "foreign-pi-extension.ts") {
		t.Errorf("empty install dropped the foreign Pi extension\n--- got ---\n%s", piGot)
	}
	// Stripping the entry alone would leave executable code on disk that
	// nothing registers and --allow-empty claims to have removed.
	if _, err := os.Stat(bridge); !os.IsNotExist(err) {
		t.Errorf("want the Pi bridge deleted by the empty install, got stat err: %v", err)
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
		"--codex-config", filepath.Join(dir, "config.toml"),
		"--cursor-hooks", filepath.Join(dir, "hooks.json"),
		"--pi-settings", filepath.Join(dir, "pi-settings.json"),
	})
	if err == nil {
		t.Fatal("want an error for a manifest-less install without --allow-empty, got nil")
	}
	if !strings.Contains(err.Error(), "no --manifest given") {
		t.Errorf("want the bare-invocation message, got: %v", err)
	}
}

// A machine with no pi on PATH is not an edge case but the ordinary
// home-manager activation order, and pi_version is the only key
// envelope.Detect keys on for Pi — so the fallback that keeps it non-empty is
// what keeps every Pi hook from silently becoming a no-op.
func TestRunInstallWithoutPiOnPATHStillEmitsADetectablePiVersion(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on it, least of all pi

	dir := t.TempDir()
	manifestPath := writeAllEnginesManifest(t, dir)
	pi := filepath.Join(dir, "pi-settings.json")
	if err := runInstall(manifestPaths{manifestPath}, testRouterPath, filepath.Join(dir, "state"),
		filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json"),
		pi, false); err != nil {
		t.Fatal(err)
	}

	var data struct {
		PiVersion json.RawMessage `json:"pi_version"`
	}
	if err := json.Unmarshal([]byte(bridgeConstant(t, readFile(t, render.PiBridgePath(pi)))), &data); err != nil {
		t.Fatalf("the bridge's data constant is not JSON: %v", err)
	}
	if got := string(data.PiVersion); got != `"unknown"` {
		t.Errorf("pi_version = %s, want \"unknown\": every failure path has to land on a non-empty value", got)
	}

	// The payload the bridge builds out of that constant, reduced to the keys
	// detection turns on.
	engine, err := envelope.Detect(map[string]json.RawMessage{
		"hook_event_name": json.RawMessage(`"tool_call"`),
		"pi_version":      data.PiVersion,
	})
	if err != nil {
		t.Fatalf("Detect on the emitted payload: %v", err)
	}
	if engine != vocab.Pi {
		t.Errorf("Detect = %q, want %q", engine, vocab.Pi)
	}
}

func bridgeConstant(t *testing.T, source string) string {
	t.Helper()
	const prefix = "const DATA = "
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), ";")
		}
	}
	t.Fatalf("no %q line in the written bridge:\n%s", prefix, source)
	return ""
}
