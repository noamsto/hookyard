package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/installstate"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

// piConfig renders a real settings.json plus bridge via the actual writer,
// so expectations are against real output rather than a hand-typed guess at
// how the bridge's data constant is quoted.
func piConfig(t *testing.T, dir, command string) (settingsPath, bridgePath string) {
	t.Helper()
	settingsPath = filepath.Join(dir, "settings.json")
	entries := []render.Entry{{Event: "pre_tool", Matcher: "bash", Command: command}}
	if err := render.WritePi(settingsPath, entries, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	return settingsPath, render.PiBridgePath(settingsPath)
}

// The trap this pins: settings.json only ever names the bridge file, and
// routerPathPattern run against it recovers that path truncated at
// "hookyard" — the literal render.Marker suffix — dropping "-bridge.ts" and
// reporting a router that does not exist. Only scanning the bridge itself,
// where the real router command lives, gets the right answer.
func TestPiRouterPathMustReadTheBridgeNotSettings(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	settings, bridge := piConfig(t, filepath.Join(root, "pi"), routedCommand(router, vocab.Pi, filepath.Join(root, "state")))

	wrong := routerPath(vocab.Pi, settings)
	truncated := strings.TrimSuffix(bridge, "-bridge.ts")
	if wrong.Status != Fail {
		t.Fatalf("routerPath(settings.json) status = %v, want Fail (the trap); detail=%q", wrong.Status, wrong.Detail)
	}
	if !strings.Contains(wrong.Detail, truncated) {
		t.Errorf("detail = %q, want the truncated path %q named", wrong.Detail, truncated)
	}
	if strings.Contains(wrong.Detail, bridge) {
		t.Errorf("detail = %q, should not recover the real bridge path", wrong.Detail)
	}

	right := routerPath(vocab.Pi, bridge)
	if right.Status != Pass {
		t.Fatalf("routerPath(bridge) status = %v, want Pass; detail=%q", right.Status, right.Detail)
	}
	if !strings.Contains(right.Detail, router) {
		t.Errorf("detail = %q, want the real router path named", right.Detail)
	}
}

func TestPiFindingsRegistersOffSettingsAndRouterOffBridge(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	piDir := filepath.Join(root, "pi")
	piConfig(t, piDir, routedCommand(router, vocab.Pi, filepath.Join(root, "state")))

	p := Paths{PiAgentDir: piDir}
	findings := piFindings(p, "")

	reg := findByCheck(t, findings, "hookyard registered")
	if reg.Status != Pass {
		t.Errorf("registration status = %v, want Pass; detail=%q", reg.Status, reg.Detail)
	}

	rp := findByCheck(t, findings, "router path")
	if rp.Status != Pass {
		t.Errorf("router path status = %v, want Pass; detail=%q", rp.Status, rp.Detail)
	}
	if rp.Detail == "" || !strings.Contains(rp.Detail, router) {
		t.Errorf("router path detail = %q, want it to name %q", rp.Detail, router)
	}
}

func TestPiFindingsNoRegistrationWhenSettingsMissing(t *testing.T) {
	p := Paths{PiAgentDir: filepath.Join(t.TempDir(), "pi")}
	findings := piFindings(p, "")

	reg := findByCheck(t, findings, "hookyard registered")
	if reg.Status != Fail {
		t.Errorf("registration status = %v, want Fail; detail=%q", reg.Status, reg.Detail)
	}
}

// Pi has no Cursor-style per-project marker to check: hookyard's registration
// is a global extension, and Pi's project trust gate does not cover global
// extensions at all, so the check always passes and says why.
func TestPiTrustFindingReportsGlobalExtensionsAsUngated(t *testing.T) {
	p := Paths{PiAgentDir: filepath.Join(t.TempDir(), "pi")}
	trust := findByCheck(t, piFindings(p, ""), "workspace trust")
	if trust.Status != Pass {
		t.Fatalf("trust status = %v, want Pass; detail=%q", trust.Status, trust.Detail)
	}
	if !strings.Contains(trust.Detail, "global") {
		t.Errorf("detail = %q, want it to say the extension is global", trust.Detail)
	}
}

func TestPiDanglingExtensionIsAFinding(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	missing := filepath.Join(dir, "bin", "hookyard-bridge.ts")
	raw, err := json.Marshal(map[string]any{"extensions": []string{missing}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f := danglingExtensions(settings)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, missing) {
		t.Errorf("detail = %q, want the missing file named", f.Detail)
	}
	wantFix := "Run hookyard install to regenerate the Pi bridge and extensions entry."
	if f.Fix != wantFix {
		t.Errorf("fix = %q, want %q", f.Fix, wantFix)
	}
}

func TestPiDanglingForeignExtensionHasNoFix(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	missing := filepath.Join(filepath.Dir(settings), "foreign.js")
	raw, err := json.Marshal(map[string]any{"extensions": []string{missing}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f := danglingExtensions(settings)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if f.Fix != "" {
		t.Errorf("fix = %q, want empty Fix for foreign extension", f.Fix)
	}
}

func TestPiDanglingMixedExtensionsHasNoFix(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	owned := render.PiBridgePath(settings)
	foreign := filepath.Join(dir, "foreign.js")
	raw, err := json.Marshal(map[string]any{"extensions": []string{owned, foreign}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f := danglingExtensions(settings)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if f.Fix != "" {
		t.Errorf("fix = %q, want empty Fix for mixed extensions", f.Fix)
	}
}

func TestPiDanglingExtensionPassesWhenFileExists(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	settings, _ := piConfig(t, filepath.Join(root, "pi"), routedCommand(router, vocab.Pi, filepath.Join(root, "state")))

	f := danglingExtensions(settings)
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

// A stat that fails for any other reason — an unreadable parent here, a dead
// mount in the field — is not a missing file, and saying it is would send the
// reader off to recreate an extension that is sitting right there. doctor is
// the tool nominated to be believed about silent failures, so it does not get
// to guess.
func TestPiDanglingExtensionReportsAnUnreadableEntryAsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a mode-0 directory, so the stat failure this needs cannot be staged")
	}
	dir := t.TempDir()
	sealed := filepath.Join(dir, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(sealed, "hookyard-bridge.ts")
	if err := os.WriteFile(extension, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's own cleanup cannot descend into a mode-0 directory.
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })

	settings := filepath.Join(dir, "settings.json")
	raw, err := json.Marshal(map[string]any{"extensions": []string{extension}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f := danglingExtensions(settings)
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, extension) {
		t.Errorf("detail = %q, want the entry named", f.Detail)
	}
	if strings.Contains(f.Detail, "cannot find") {
		t.Errorf("detail = %q, reports an unreadable entry as one pi cannot find", f.Detail)
	}
}

// A Pi-only install with a non-default --state-dir must recover that
// directory, not silently fall back to record.DefaultStateDir() and read a
// directory nothing writes to.
func TestRunRecoversPiOnlyStateDir(t *testing.T) {
	// Pin the fallback DefaultStateDir() reaches for on a recovery failure to
	// an empty directory. Left ambient, this test passes on any developer
	// machine with a live hookyard install for the wrong reason: the fallback
	// resolves to ~/.local/state/hookyard, which already has today's real
	// event stream, so a completely broken recovery still reports Pass by
	// accident — exactly the silent false negative recoverStateDir's own
	// comment warns about.
	t.Setenv("HOOKYARD_STATE_DIR", t.TempDir())

	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "pi-state")
	piDir := filepath.Join(root, "pi")

	piConfig(t, piDir, routedCommand(router, vocab.Pi, stateDir))
	writeTodaysStream(t, stateDir, `{"enforced":true}`+"\n")

	p := Paths{
		ClaudeConfigDir: filepath.Join(root, "claude"),
		CodexHome:       filepath.Join(root, "codex"),
		CursorHome:      filepath.Join(root, "cursor"),
		PiAgentDir:      piDir,
	}
	f := enforcementFinding(t, Run(p, root))
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func findByCheck(t *testing.T, findings []Finding, check string) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no %q finding among %d findings", check, len(findings))
	return Finding{}
}

// findAllByCheck is findByCheck's multi-dir counterpart: piFindings now
// reports one row per Check per managed settings dir, and findByCheck's
// first-match-wins behavior would let a multi-dir assertion pass vacuously
// while a second dir goes completely unchecked.
func findAllByCheck(t *testing.T, findings []Finding, check string) []Finding {
	t.Helper()
	var out []Finding
	for _, f := range findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

// writeLauncherScript writes a text file at <dir>/pi with the given body and
// makes it executable, mirroring the shape of a real wrapper script such as
// the one this machine's own pi launcher is.
func writeLauncherScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "pi-real")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// withPiOnPath points PATH at a fresh directory containing only a "pi"
// symlink to target, so LookPath and the launcher scan never see the
// developer's ambient PATH or real pi installation.
func withPiOnPath(t *testing.T, target string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "pi")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestPiLauncherFindingsReportsInjectedGuardsAndExtensions(t *testing.T) {
	dir := t.TempDir()
	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		"export PI_AGENT_HOOKS=/opt/example/guard-one.js:/opt/example/guard-two.js\n"+
		`exec /opt/example/real-pi "$@" -e /opt/example/extra-extension.js`+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	for _, want := range []string{
		"/opt/example/guard-one.js",
		"/opt/example/guard-two.js",
		"/opt/example/extra-extension.js",
	} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
	if !strings.Contains(f.Detail, target) {
		t.Errorf("detail = %q, want the symlink resolved to %q", f.Detail, target)
	}
}

func TestPiLauncherFindingsPassesWhenNothingInjected(t *testing.T) {
	dir := t.TempDir()
	target := writeLauncherScript(t, dir, "#!/bin/sh\nexec /opt/example/real-pi \"$@\"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func TestPiLauncherFindingsUnknownWhenPiAbsentFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	f := piLauncherFindings("")
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

func TestPiLauncherFindingsSkipsACompiledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("NUL-byte text heuristic is unix-oriented")
	}
	dir := t.TempDir()
	// A NUL byte in the first bytes is the standard "not text" signal; the
	// rest of the content deliberately looks like an injection so the test
	// fails if the binary check is skipped.
	body := "\x00\x01\x02export PI_AGENT_HOOKS=/opt/example/guard.js"
	target := filepath.Join(dir, "pi-real")
	if err := os.WriteFile(target, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "binary") {
		t.Errorf("detail = %q, want it to say this is a binary", f.Detail)
	}
}

// TestPiLauncherFindingsBuildPackageWithoutOverlapPasses pins the aeye-style
// build-mode package: it is intentional, and a baked table disjoint from the
// yard table is not a double-fire.
func TestPiLauncherFindingsBuildPackageWithoutOverlapPasses(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})
	extension := piBuildPackageFixture(t, filepath.Join(dir, "pkg"), []manifest.Handler{pluginHandler("guard/two", "bin/guard.sh")})

	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		`exec /opt/example/real-pi "$@" -e `+extension+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings(stateDir)
	if f.Status == Fail {
		t.Fatalf("status = Fail, want a non-failing status; detail=%q", f.Detail)
	}
	if !strings.Contains(f.Detail, filepath.Dir(filepath.Dir(extension))) {
		t.Errorf("detail = %q, want it to name the build package root", f.Detail)
	}
}

// TestPiLauncherFindingsBuildPackageOverlapFails pins the build-mode package
// that does double-register: its baked table shares a handler id with the yard
// table, the hazard piDoubleFire also reports, and the id is named.
func TestPiLauncherFindingsBuildPackageOverlapFails(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})
	extension := piBuildPackageFixture(t, filepath.Join(dir, "pkg"), []manifest.Handler{pluginHandler("guard/one", "bin/guard.sh")})

	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		`exec /opt/example/real-pi "$@" -e `+extension+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings(stateDir)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "guard/one") {
		t.Errorf("detail = %q, want it to name the shared handler id", f.Detail)
	}
}

// TestPiLauncherFindingsBuildPackageUnknownWithoutStateDir pins the honest
// answer when no yard table is recoverable: without it, overlap cannot be
// ruled out, so the package must not read as a Pass.
func TestPiLauncherFindingsBuildPackageUnknownWithoutStateDir(t *testing.T) {
	dir := t.TempDir()
	extension := piBuildPackageFixture(t, filepath.Join(dir, "pkg"), []manifest.Handler{pluginHandler("guard/two", "bin/guard.sh")})

	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		`exec /opt/example/real-pi "$@" -e `+extension+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

// TestPiLauncherFindingsPassesOnPlainExtensionsOnly: a wrapper that injects
// only plain -e extensions — no PI_AGENT_HOOKS guard path, no build-mode
// hookyard package — is not the double-fire hazard, so it must not read as a
// Fail.
func TestPiLauncherFindingsPassesOnPlainExtensionsOnly(t *testing.T) {
	dir := t.TempDir()
	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		`exec /opt/example/real-pi "$@" -e /opt/example/plain-extension.js`+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status == Fail {
		t.Fatalf("status = Fail, want a non-failing status; detail=%q", f.Detail)
	}
	if !strings.Contains(f.Detail, "/opt/example/plain-extension.js") {
		t.Errorf("detail = %q, want it to name the plain extension", f.Detail)
	}
}

// TestPiLauncherFindingsGuardsAreFail pins the hazard that stays a Fail:
// PI_AGENT_HOOKS paths are hookyard guards the launcher re-injects, so the
// same guard can fire twice per event.
func TestPiLauncherFindingsGuardsAreFail(t *testing.T) {
	dir := t.TempDir()
	target := writeLauncherScript(t, dir, "#!/bin/sh\n"+
		"export PI_AGENT_HOOKS=/opt/example/guard-one.js\n"+
		`exec /opt/example/real-pi "$@"`+"\n")
	withPiOnPath(t, target)

	f := piLauncherFindings("")
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
}

// pluginHandler is aeyeScript's build-mode counterpart: a fully valid handler
// with a plugin-root-relative exec, so WritePluginTable -> ReadPluginTable
// round-trips without validateStatic refusing it.
func pluginHandler(id, exec string) manifest.Handler {
	return manifest.Handler{
		ID:      id,
		Exec:    exec,
		Events:  []string{"post_tool"},
		Engines: []string{"pi"},
	}
}

// piBuildPackageFixture writes a build-mode pi package under root, in the
// shape build-pi produces (decomposition: "Pi build package on disk"): the
// extension file at <root>/extensions/hookyard.ts and the baked table at
// <root>/hookyard/table.json. It returns the extension path, the value a
// settings.json extensions[] entry would name.
func piBuildPackageFixture(t *testing.T, root string, handlers []manifest.Handler) string {
	t.Helper()
	extensionsDir := filepath.Join(root, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(extensionsDir, "hookyard.ts")
	if err := os.WriteFile(extension, []byte("export default {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tableDir := filepath.Join(root, "hookyard")
	if err := os.MkdirAll(tableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manifest.WritePluginTable(filepath.Join(tableDir, "table.json"), handlers); err != nil {
		t.Fatal(err)
	}
	return extension
}

func writePiSettingsExtensions(t *testing.T, path string, extensions []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"extensions": extensions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiDoubleFirePassesWhenNoBuildPackageExists(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	settings := filepath.Join(dir, "pi", "settings.json")
	writePiSettingsExtensions(t, settings, nil)

	f := piDoubleFire(stateDir, settings)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "no build-mode pi package found") {
		t.Errorf("detail = %q, want it to say no package was found", f.Detail)
	}
}

func TestPiDoubleFirePassesOnDisjointHandlerIDs(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	pkgRoot := filepath.Join(dir, "pkg")
	extension := piBuildPackageFixture(t, pkgRoot, []manifest.Handler{pluginHandler("guard/two", "bin/guard.sh")})

	settings := filepath.Join(dir, "pi", "settings.json")
	writePiSettingsExtensions(t, settings, []string{extension})

	f := piDoubleFire(stateDir, settings)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "no handler id is registered in both") {
		t.Errorf("detail = %q, want it to say nothing collided", f.Detail)
	}
}

func TestPiDoubleFireFailsOnASharedHandlerID(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	pkgRoot := filepath.Join(dir, "pkg")
	extension := piBuildPackageFixture(t, pkgRoot, []manifest.Handler{pluginHandler("guard/one", "bin/guard.sh")})

	settings := filepath.Join(dir, "pi", "settings.json")
	writePiSettingsExtensions(t, settings, []string{extension})

	f := piDoubleFire(stateDir, settings)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "guard/one") {
		t.Errorf("detail = %q, want the colliding id named", f.Detail)
	}
	yardPath := filepath.Join(stateDir, "table.json")
	pkgTablePath := filepath.Join(pkgRoot, "hookyard", "table.json")
	if !strings.Contains(f.Detail, yardPath) {
		t.Errorf("detail = %q, want the yard table path named", f.Detail)
	}
	if !strings.Contains(f.Detail, pkgTablePath) {
		t.Errorf("detail = %q, want the build package's table path named", f.Detail)
	}
	// Retiring one of the two registrations is a human decision — yard-mode
	// vs. build-mode, and there is no hookyard uninstall to name — so unlike
	// most Fail findings this one has no single safe command to suggest.
	if f.Fix != "" {
		t.Errorf("fix = %q, want empty Fix for a same-ID collision", f.Fix)
	}
}

func TestPiDoubleFireUnknownWhenPackageTableUnreadable(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	pkgRoot := filepath.Join(dir, "pkg")
	extensionsDir := filepath.Join(pkgRoot, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(extensionsDir, "hookyard.ts")
	if err := os.WriteFile(extension, []byte("export default {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tableDir := filepath.Join(pkgRoot, "hookyard")
	if err := os.MkdirAll(tableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tableDir, "table.json"), []byte("not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := filepath.Join(dir, "pi", "settings.json")
	writePiSettingsExtensions(t, settings, []string{extension})

	f := piDoubleFire(stateDir, settings)
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

// A third-party extension sitting beside pi's own must not be treated as a
// hookyard build package just because it is a file under some extensions/
// directory: its grandparent has no hookyard/table.json, so it does not
// qualify and contributes nothing rather than an error.
func TestPiDoubleFireIgnoresANonHookyardExtensionsEntry(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	otherDir := filepath.Join(dir, "other-extension", "extensions")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(otherDir, "unrelated.ts")
	if err := os.WriteFile(other, []byte("export default {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := filepath.Join(dir, "pi", "settings.json")
	writePiSettingsExtensions(t, settings, []string{other})

	f := piDoubleFire(stateDir, settings)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

// piHandler is aeyeScript's pi-scoped counterpart: ReadTable enforces
// ExecAbsolute, unlike pluginHandler's plugin-root-relative exec, so the
// drift check's yard-mode fixtures need their own absolute-exec handler.
func piHandler(id, event, exec string) manifest.Handler {
	return manifest.Handler{
		ID:      id,
		Exec:    exec,
		Events:  []string{event},
		Engines: []string{"pi"},
	}
}

// piBridgeFixture renders settings.json plus a bridge naming exactly entries,
// via the real writer, so a multi-entry fixture is never a hand-typed guess
// at how WritePi encodes bin/args.
func piBridgeFixture(t *testing.T, dir string, entries []render.Entry) (settingsPath, bridgePath string) {
	t.Helper()
	settingsPath = filepath.Join(dir, "settings.json")
	if err := render.WritePi(settingsPath, entries, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	return settingsPath, render.PiBridgePath(settingsPath)
}

func TestPiBridgeDriftPassesWhenBridgeMatchesTheTable(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	handlers := []manifest.Handler{piHandler("guard/one", "post_tool", "/nix/store/x/guard.sh")}
	writeTable(t, stateDir, handlers)

	router := writeRouterBinary(t, dir, true)
	plan, err := render.BuildPlan(handlers, router, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, bridge := piBridgeFixture(t, filepath.Join(dir, "pi"), plan[vocab.Pi])

	f := piBridgeDrift(stateDir, bridge)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

// A handler in the table with no corresponding bridge entry is exactly
// "registered but can never fire".
func TestPiBridgeDriftFailsWhenATableHandlerHasNoBridgeEntry(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	handlers := []manifest.Handler{
		piHandler("guard/pre", "pre_tool", "/nix/store/x/pre.sh"),
		piHandler("guard/post", "post_tool", "/nix/store/x/post.sh"),
	}
	writeTable(t, stateDir, handlers)

	router := writeRouterBinary(t, dir, true)
	plan, err := render.BuildPlan(handlers, router, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Install only the pre_tool ("tool_call") entry: post_tool
	// ("tool_result") stays in the table with no matching bridge entry.
	var preOnly []render.Entry
	for _, e := range plan[vocab.Pi] {
		if e.Event == "tool_call" {
			preOnly = append(preOnly, e)
		}
	}
	if len(preOnly) != 1 {
		t.Fatalf("test setup: want exactly one tool_call entry in the plan, got %d", len(preOnly))
	}
	_, bridge := piBridgeFixture(t, filepath.Join(dir, "pi"), preOnly)

	f := piBridgeDrift(stateDir, bridge)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "tool_result") {
		t.Errorf("detail = %q, want the orphaned handler's native event named", f.Detail)
	}
	if want := "Run hookyard install to regenerate the Pi bridge."; f.Fix != want {
		t.Errorf("fix = %q, want %q", f.Fix, want)
	}
}

// A duplicate (event, matcher) pair in the installed bridge is the shape the
// session_start + pi:session_start aliasing hazard produces, and this check
// is the only place that surfaces it.
func TestPiBridgeDriftFailsOnADuplicateEntry(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	handlers := []manifest.Handler{piHandler("guard/one", "post_tool", "/nix/store/x/guard.sh")}
	writeTable(t, stateDir, handlers)

	router := writeRouterBinary(t, dir, true)
	plan, err := render.BuildPlan(handlers, router, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	doubled := append(plan[vocab.Pi], plan[vocab.Pi]...)
	_, bridge := piBridgeFixture(t, filepath.Join(dir, "pi"), doubled)

	f := piBridgeDrift(stateDir, bridge)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "registered 2 times") {
		t.Errorf("detail = %q, want the duplicate count named", f.Detail)
	}
}

// A DATA that will not parse is Unknown, not Fail — the same
// absence-vs-unreadability split danglingExtensions draws.
func TestPiBridgeDriftUnknownWhenDataCannotBeParsed(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	writeTable(t, stateDir, []manifest.Handler{piHandler("guard/one", "post_tool", "/nix/store/x/guard.sh")})

	bridge := filepath.Join(dir, "pi", "bin", "hookyard-bridge.ts")
	if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, []byte("const DATA = {not valid json};\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := piBridgeDrift(stateDir, bridge)
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

// The hazard this pins is the one a machine installed before render's
// checkRouterPath is already in: piBridgeEntries split the emitted command on
// " ", so a whitespace-bearing router path reached the bridge as a bin of
// "<root>/my" with the rest of the path sitting in args[0]. The fixture is
// built by the real writer from such a command rather than hand-typed, so it
// is byte-for-byte what that install produced.
func TestPiBridgeExecFailsOnAWhitespaceSplitInvocation(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "my dir", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	router := filepath.Join(binDir, "hookyard")
	if err := os.WriteFile(router, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	piDir := filepath.Join(root, "pi")
	piConfig(t, piDir, routedCommand(router, vocab.Pi, filepath.Join(root, "state")))

	findings := piFindings(Paths{PiAgentDir: piDir}, "")

	f := findByCheck(t, findings, "bridge invocation is executable")
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, filepath.Join(root, "my")+" ") {
		t.Errorf("detail = %q, want the truncated bin execFile receives named", f.Detail)
	}
	if !strings.Contains(f.Detail, "hookyard install") || !strings.Contains(f.Detail, "whitespace") {
		t.Errorf("detail = %q, want the repair and its cause named", f.Detail)
	}
	if want := "Run hookyard install."; f.Fix != want {
		t.Errorf("fix = %q, want %q", f.Fix, want)
	}

	// Why this check has to exist at all: routerPathPattern needs a slash
	// before the marker, and the split leaves "dir/bin/hookyard" with none, so
	// the grep matches nothing and the router-path finding shrugs.
	if rp := findByCheck(t, findings, "router path"); rp.Status != Unknown {
		t.Errorf("router path status = %v, want Unknown (the gap this check closes); detail=%q", rp.Status, rp.Detail)
	}
}

func TestPiBridgeExecPassesWhenTheRouterIsExecutable(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	_, bridge := piConfig(t, filepath.Join(root, "pi"), routedCommand(router, vocab.Pi, filepath.Join(root, "state")))

	f := piBridgeExec(bridge)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

// A bridge doctor cannot read or cannot parse is Unknown, not Fail — the same
// absence-vs-unreadability split danglingExtensions draws.
func TestPiBridgeExecUnknownWhenTheBridgeCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	if f := piBridgeExec(filepath.Join(dir, "bin", "hookyard-bridge.ts")); f.Status != Unknown {
		t.Errorf("missing bridge status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}

	bridge := filepath.Join(dir, "pi", "bin", "hookyard-bridge.ts")
	if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, []byte("const DATA = {not valid json};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f := piBridgeExec(bridge); f.Status != Unknown {
		t.Errorf("unparsable DATA status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

// A built package's bin is render.PluginLauncher, relative to a package root
// the bridge resolves at load. Statting it from doctor's working directory
// would report a missing router that is perfectly present.
func TestPiBridgeExecIgnoresABuildModePackagesRelativeBin(t *testing.T) {
	source, err := render.PiPluginBridge([]manifest.Handler{piHandler("guard/one", "post_tool", "bin/guard.sh")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(t.TempDir(), "extensions", "hookyard.ts")
	if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, source, 0o644); err != nil {
		t.Fatal(err)
	}

	f := piBridgeExec(bridge)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "relative") {
		t.Errorf("detail = %q, want it to say why nothing was stat'd", f.Detail)
	}
}

// piMultiDirWitness writes a witness naming exactly the given settings paths
// as hookyard's managed pi settings dirs, so managedPiSettingsPaths resolves
// to more than the single ambient PiAgentDir a bare Paths would fall back to.
func piMultiDirWitness(t *testing.T, path string, settings ...string) {
	t.Helper()
	writeWitness(t, path, installstate.Witness{
		Schema:   installstate.Schema,
		Identity: installstate.Identity{PiSettings: settings},
	})
}

// A settings dir hookyard doesn't yet know about (no witness, or a witness
// naming a different dir) passes every per-dir check by being absent from
// all of them; the multi-dir loop this test exercises is what makes a second
// managed dir show up in piFindings' output at all.
func TestPiFindingsCoverEveryManagedDir(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "state")

	dir1 := filepath.Join(root, "pi1")
	dir2 := filepath.Join(root, "pi2")
	settings1, _ := piConfig(t, dir1, routedCommand(router, vocab.Pi, stateDir))
	settings2, bridge2 := piConfig(t, dir2, routedCommand(router, vocab.Pi, stateDir))

	witness := filepath.Join(root, "generation.json")
	piMultiDirWitness(t, witness, settings1, settings2)

	p := Paths{PiAgentDir: dir1, GenerationWitness: witness}
	findings := piFindings(p, "")

	regs := findAllByCheck(t, findings, "hookyard registered")
	if len(regs) != 2 {
		t.Fatalf("got %d \"hookyard registered\" findings, want 2 (one per managed dir): %+v", len(regs), regs)
	}
	for _, r := range regs {
		if r.Status != Pass {
			t.Errorf("registration %+v status = %v, want Pass", r, r.Status)
		}
	}

	// Corrupt only dir2's bridge: dir1's per-dir findings must stay exactly
	// as they were, proving the loop doesn't leak state across dirs.
	if err := os.Remove(bridge2); err != nil {
		t.Fatal(err)
	}
	findings = piFindings(p, "")

	exts := findAllByCheck(t, findings, "extensions targets exist")
	if len(exts) != 2 {
		t.Fatalf("got %d \"extensions targets exist\" findings, want 2: %+v", len(exts), exts)
	}
	var sawDir1Pass, sawDir2Fail bool
	for _, e := range exts {
		switch {
		case strings.Contains(e.Detail, settings1):
			if e.Status != Pass {
				t.Errorf("dir1 extensions status = %v, want Pass; detail=%q", e.Status, e.Detail)
			}
			sawDir1Pass = true
		case strings.Contains(e.Detail, bridge2):
			if e.Status != Fail {
				t.Errorf("dir2 extensions status = %v, want Fail (its bridge was removed); detail=%q", e.Status, e.Detail)
			}
			sawDir2Fail = true
		default:
			t.Errorf("finding %+v names neither settings dir", e)
		}
	}
	if !sawDir1Pass || !sawDir2Fail {
		t.Fatalf("did not observe both dirs' expected status: dir1 Pass=%v, dir2 Fail=%v", sawDir1Pass, sawDir2Fail)
	}
}

// A build-mode collision in one managed dir must not spill into another
// managed dir's own double-fire check: each dir compares only its own
// settings.json extensions[] against the shared yard table.
func TestPiDoubleFireIsPerDirNotShared(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("guard/one", "/nix/store/x/guard.sh")})

	// dir1 has a build-mode package colliding with the yard table.
	dir1 := filepath.Join(root, "pi1")
	pkgRoot := filepath.Join(root, "pkg")
	extension := piBuildPackageFixture(t, pkgRoot, []manifest.Handler{pluginHandler("guard/one", "bin/guard.sh")})
	settings1 := filepath.Join(dir1, "settings.json")
	writePiSettingsExtensions(t, settings1, []string{extension})

	// dir2 has no build-mode package at all.
	dir2 := filepath.Join(root, "pi2")
	settings2, _ := piConfig(t, dir2, routedCommand(router, vocab.Pi, stateDir))

	witness := filepath.Join(root, "generation.json")
	piMultiDirWitness(t, witness, settings1, settings2)

	p := Paths{PiAgentDir: dir1, GenerationWitness: witness}
	findings := piFindings(p, stateDir)

	fires := findAllByCheck(t, findings, "double-registered handlers")
	if len(fires) != 2 {
		t.Fatalf("got %d \"double-registered handlers\" findings, want 2: %+v", len(fires), fires)
	}
	var sawDir1Fail, sawDir2Pass bool
	for _, f := range fires {
		switch {
		case strings.Contains(f.Detail, "guard/one"):
			if f.Status != Fail {
				t.Errorf("dir1 double-fire status = %v, want Fail; detail=%q", f.Status, f.Detail)
			}
			sawDir1Fail = true
		case strings.Contains(f.Detail, settings2):
			if f.Status != Pass {
				t.Errorf("dir2 double-fire status = %v, want Pass (no build-mode package there); detail=%q", f.Status, f.Detail)
			}
			sawDir2Pass = true
		default:
			t.Errorf("finding %+v matched neither dir's expected shape", f)
		}
	}
	if !sawDir1Fail || !sawDir2Pass {
		t.Fatalf("did not observe both dirs' expected status: dir1 Fail=%v, dir2 Pass=%v", sawDir1Fail, sawDir2Pass)
	}
}

func TestPiUnmanagedDir(t *testing.T) {
	piAgentDir := filepath.Join(t.TempDir(), "pi")
	ambient := filepath.Join(piAgentDir, "settings.json")

	pass := piUnmanagedDir(piAgentDir, []string{ambient, "/other/settings.json"})
	if pass.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", pass.Status, pass.Detail)
	}

	fail := piUnmanagedDir(piAgentDir, []string{"/other/settings.json"})
	if fail.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", fail.Status, fail.Detail)
	}
	if fail.Fix == "" {
		t.Errorf("fix = %q, want a non-empty Fix for an unmanaged dir", fail.Fix)
	}
}

// On a plain non-Nix machine there is no witness at all, so
// managedPiSettingsPaths must fall back to the single ambient dir doctor has
// always checked, and piUnmanagedDir must never turn that fallback into a
// false "unmanaged dir" problem.
func TestPiUnmanagedDirFallsBackWithoutAWitness(t *testing.T) {
	piAgentDir := filepath.Join(t.TempDir(), "pi")
	p := Paths{PiAgentDir: piAgentDir}

	managed := managedPiSettingsPaths(p)
	want := []string{filepath.Join(piAgentDir, "settings.json")}
	if len(managed) != len(want) || managed[0] != want[0] {
		t.Fatalf("managedPiSettingsPaths = %v, want %v", managed, want)
	}

	f := piUnmanagedDir(piAgentDir, managed)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}
