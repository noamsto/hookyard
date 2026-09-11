package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	findings := piFindings(p)

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
	findings := piFindings(p)

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
	trust := findByCheck(t, piFindings(p), "workspace trust")
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

	f := piLauncherFindings()
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

	f := piLauncherFindings()
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func TestPiLauncherFindingsUnknownWhenPiAbsentFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	f := piLauncherFindings()
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

	f := piLauncherFindings()
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "binary") {
		t.Errorf("detail = %q, want it to say this is a binary", f.Detail)
	}
}
