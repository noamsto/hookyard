package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

// claudeOverlayJSON builds a --settings overlay through the real encoder, the
// same one `hookyard emit` writes with, so these expectations are against the
// document Nix will actually place rather than a hand-typed guess at it.
func claudeOverlayJSON(t *testing.T, command string) []byte {
	t.Helper()
	entries := []render.Entry{{Event: "PreToolUse", Matcher: "Bash", Command: command}}
	out, err := render.ClaudeSettings(nil, entries)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, path string, raw []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// claudePaths points doctor at an empty config dir, so settings.json is absent
// — which is the expected state now that hookyard does not write it.
func claudePaths(t *testing.T, flags ...string) Paths {
	t.Helper()
	return Paths{ClaudeConfigDir: t.TempDir(), ClaudeSettingsFlags: flags}
}

func TestClaudeRegistrationPassesOffTheLauncherOverlay(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	overlay := writeFile(t, filepath.Join(root, "overlay.json"),
		claudeOverlayJSON(t, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state"))))

	f := claudeRegistration(resolveClaudeSources(claudePaths(t, overlay)))
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, overlay) || !strings.Contains(f.Detail, "--settings") {
		t.Errorf("detail = %q, want it to name the --settings overlay", f.Detail)
	}
	// §4.3b: --settings is last-wins, so the claim has to stop at what the
	// launcher passes. Claiming a given invocation will run the hooks would be
	// wrong the moment a caller passes its own --settings.
	if !strings.Contains(f.Detail, "displaces it") {
		t.Errorf("detail = %q, want the narrow claim about a displacing --settings", f.Detail)
	}
}

// The wording here is the first of the two §4.4 calls out as most likely to
// drift, and the Fail is load-bearing twice over: the entry is stale, and with
// the overlay registered too every handler runs twice (§8 unions hook sources).
func TestClaudeRegistrationFailsOnAStaleMarkerInSettingsJSON(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	command := routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state"))
	configDir := filepath.Join(root, "claude")
	settings := claudeConfig(t, configDir, command)
	overlay := writeFile(t, filepath.Join(root, "overlay.json"), claudeOverlayJSON(t, command))

	f := claudeRegistration(resolveClaudeSources(Paths{
		ClaudeConfigDir:     configDir,
		ClaudeSettingsFlags: []string{overlay},
	}))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, settings) {
		t.Errorf("detail = %q, want it to name %s", f.Detail, settings)
	}
	if !strings.Contains(f.Detail, "remove it") || !strings.Contains(f.Detail, "twice") {
		t.Errorf("detail = %q, want removal advice and the double-fire risk", f.Detail)
	}
	if strings.Contains(f.Detail, "hookyard install") {
		t.Errorf("detail = %q, must not advise an install that cannot write this file", f.Detail)
	}
}

// The second wording §4.4 pins: the repair names the merged overlay option,
// never an install and never the hooks-only file, which would drop the rest of
// the overlay (R4b).
func TestClaudeRegistrationFailsWithTheMergedOverlayRepair(t *testing.T) {
	root := t.TempDir()
	overlay := writeFile(t, filepath.Join(root, "overlay.json"), []byte(`{"permissions":{}}`))

	f := claudeRegistration(resolveClaudeSources(claudePaths(t, overlay)))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "programs.hookyard.claudeOverlay.merged") {
		t.Errorf("detail = %q, want the claudeOverlay.merged repair", f.Detail)
	}
	if strings.Contains(f.Detail, "hookyard install") || strings.Contains(f.Detail, "claudeHooks") {
		t.Errorf("detail = %q, must name neither an install nor claudeHooks", f.Detail)
	}
}

// Unknown, not Fail: an unreadable launcher is exactly what the package's
// three-valued Status reserves the third value for.
func TestClaudeRegistrationUnknownWhenTheLauncherIsUnreadable(t *testing.T) {
	t.Run("no --settings recovered", func(t *testing.T) {
		f := claudeRegistration(resolveClaudeSources(claudePaths(t)))
		if f.Status != Unknown {
			t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
		}
		if !strings.Contains(f.Detail, "cannot see which settings file") {
			t.Errorf("detail = %q, want it to say the settings file is not visible", f.Detail)
		}
	})

	// The four launcher cases are distinct repairs, so the detail has to say
	// which one it hit rather than leaving the operator to guess.
	t.Run("the launcher case is named", func(t *testing.T) {
		p := claudePaths(t)
		p.ClaudeLauncherUnread = "/nix/store/x/bin/claude is a compiled binary, not a wrapper script"
		f := claudeRegistration(resolveClaudeSources(p))
		if f.Status != Unknown {
			t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
		}
		if !strings.Contains(f.Detail, "compiled binary") {
			t.Errorf("detail = %q, want it to name why the launcher was not read", f.Detail)
		}
	})

	t.Run("value resolves to neither a file nor JSON", func(t *testing.T) {
		value := filepath.Join(t.TempDir(), "never-written.json")
		f := claudeRegistration(resolveClaudeSources(claudePaths(t, value)))
		if f.Status != Unknown {
			t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
		}
		if !strings.Contains(f.Detail, value) {
			t.Errorf("detail = %q, want it to name what it could not read", f.Detail)
		}
	})
}

// claude --help declares --settings <file-or-json>, so the recovered value can
// be the document itself.
func TestClaudeRegistrationReadsAnInlineSettingsValue(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	inline := string(claudeOverlayJSON(t, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state"))))

	c := resolveClaudeSources(claudePaths(t, inline))
	if f := claudeRegistration(c); f.Status != Pass {
		t.Fatalf("registration status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if f := claudeRouterPath(c); f.Status != Pass {
		t.Fatalf("router path status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

// A10: the gate fires on disableAllHooks in user *or* flag settings (§8), so
// reading only settings.json and reporting Pass is a fail-open.
func TestClaudeHooksEnabledReadsEverySource(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "claude")
	settings := writeFile(t, filepath.Join(configDir, "settings.json"), []byte(`{"disableAllHooks":false}`))
	overlay := writeFile(t, filepath.Join(root, "overlay.json"), []byte(`{"disableAllHooks":true}`))

	f := claudeHooksEnabled(resolveClaudeSources(Paths{
		ClaudeConfigDir:     configDir,
		ClaudeSettingsFlags: []string{overlay},
	}))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, overlay) {
		t.Errorf("detail = %q, want it to name the source that sets the flag", f.Detail)
	}
	if strings.Contains(f.Detail, settings) {
		t.Errorf("detail = %q, names the file that does not set it", f.Detail)
	}
}

// The disclaimer was honest while the overlay was invisible. Doctor now holds
// its bytes, so keeping it would be a false statement in the tool whose job is
// to be believed.
func TestClaudeHooksEnabledDropsTheOverlayDisclaimer(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "claude")
	settings := writeFile(t, filepath.Join(configDir, "settings.json"), []byte(`{}`))
	overlay := writeFile(t, filepath.Join(root, "overlay.json"), []byte(`{}`))

	f := claudeHooksEnabled(resolveClaudeSources(Paths{
		ClaudeConfigDir:     configDir,
		ClaudeSettingsFlags: []string{overlay},
	}))
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if strings.Contains(f.Detail, "not visible here") {
		t.Errorf("detail = %q, still carries the disclaimer", f.Detail)
	}
	if !strings.Contains(f.Detail, settings) || !strings.Contains(f.Detail, overlay) {
		t.Errorf("detail = %q, want both sources named as read", f.Detail)
	}
}

// A --settings value doctor could not resolve keeps a narrower disclaimer, so
// the Pass still says what it did not see.
func TestClaudeHooksEnabledNamesWhatItCouldNotRead(t *testing.T) {
	value := filepath.Join(t.TempDir(), "never-written.json")

	f := claudeHooksEnabled(resolveClaudeSources(claudePaths(t, value)))
	if !strings.Contains(f.Detail, value) {
		t.Errorf("detail = %q, want the unresolved source named", f.Detail)
	}
}

// The router-path and --state-dir scans follow the marker into the overlay.
// settings.json holds nothing to recover, and falling back to it would report
// a router and a state directory nothing runs or writes.
func TestClaudeScansFollowTheOverlay(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "state")
	configDir := filepath.Join(root, "claude")
	writeFile(t, filepath.Join(configDir, "settings.json"), []byte(`{}`))
	overlay := writeFile(t, filepath.Join(root, "overlay.json"),
		claudeOverlayJSON(t, routedCommand(router, vocab.ClaudeCode, stateDir)))

	p := Paths{ClaudeConfigDir: configDir, ClaudeSettingsFlags: []string{overlay}}
	c := resolveClaudeSources(p)

	f := claudeRouterPath(c)
	if f.Status != Pass {
		t.Fatalf("router path status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, router) {
		t.Errorf("detail = %q, want the router named", f.Detail)
	}
	if got := recoverStateDir(p, c); !reflect.DeepEqual(got, []string{stateDir}) {
		t.Errorf("recoverStateDir = %v, want [%s]", got, stateDir)
	}
}

// Claude's arm is a separate path precisely so the other three keep the shared
// registration()/routerPath() wording — including "run hookyard install",
// which is still the right advice for them. This pins every string.
func TestOtherEnginesFindingsAreUnchanged(t *testing.T) {
	// piLauncherFindings resolves pi on PATH; an empty PATH makes the fixture
	// independent of whatever is installed on the machine running the test.
	t.Setenv("PATH", t.TempDir())

	root := t.TempDir()
	dir := filepath.Join(root, "project")
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "state")
	executable := router + ": executable"

	codexHome := filepath.Join(root, "codex")
	codexConfig := writeFile(t, filepath.Join(codexHome, "config.toml"), []byte(
		"[projects.\""+dir+"\"]\ntrust_level = \"trusted\"\n\n"+
			"[hooks.state.reviewed]\naccepted = true\n\n"+
			"[[hooks.pre_tool]]\ncommand = \""+routedCommand(router, vocab.Codex, stateDir)+"\"\n"))

	cursorHome := filepath.Join(root, "cursor")
	cursorMarker := writeFile(t, filepath.Join(cursorHome, "projects", cursorProjectSlug(dir), ".workspace-trusted"), nil)
	// No hookyard entry: the arm that carries the shared repair advice.
	cursorHooks := writeFile(t, filepath.Join(cursorHome, "hooks.json"), []byte(`{"version":1,"hooks":{}}`))

	piAgentDir := filepath.Join(root, "pi", "agent")
	piSettings, piBridge := piConfig(t, piAgentDir, routedCommand(router, vocab.Pi, stateDir))

	p := Paths{CodexHome: codexHome, CursorHome: cursorHome, PiAgentDir: piAgentDir}

	want := []Finding{
		{vocab.Codex, "workspace trust", Pass, "trusted in " + codexConfig},
		{vocab.Codex, "hook trust", Pass, "1 reviewed hook entries in " + codexConfig},
		{vocab.Codex, "hookyard registered", Pass, "present in " + codexConfig},
		{vocab.Codex, "router path", Pass, executable},
		{vocab.Cursor, "workspace trust", Pass, "trusted, per " + cursorMarker},
		{vocab.Cursor, "hookyard registered", Fail, "no hookyard entry in " + cursorHooks + "; run hookyard install"},
		{vocab.Cursor, "router path", Unknown, "no hookyard entry in " + cursorHooks + " to check"},
		{vocab.Pi, "workspace trust", Pass, piBridge + " is a global extension; Pi's project trust gate (" +
			filepath.Join(piAgentDir, "trust.json") + ") does not gate global extensions, so it runs regardless of trust state"},
		{vocab.Pi, "extensions targets exist", Pass, "every extensions[] entry in " + piSettings + " resolves to a file"},
		{vocab.Pi, "hookyard registered", Pass, "present in " + piSettings},
		{vocab.Pi, "router path", Pass, executable},
		{vocab.Pi, "launcher wrapper", Unknown, "no pi on PATH to check for an injected launcher"},
	}

	var got []Finding
	got = append(got, codexFindings(p, dir)...)
	got = append(got, cursorFindings(p, dir)...)
	got = append(got, piFindings(p)...)

	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("finding %d\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// withClaudeOnPath points PATH at a fresh directory holding only a "claude"
// symlink to target, mirroring withPiOnPath: the launcher scan must never
// reach the developer's real claude, whose wrapper does pass --settings and
// would make every case below pass for the wrong reason.
func withClaudeOnPath(t *testing.T, target string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "claude")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func writeClaudeLauncher(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "claude-real")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The reason strings are the whole point of the second return: doctor's Unknown
// arm is the one place an operator learns why hookyard cannot see the overlay,
// and "this machine has no Nix wrapper" and "your overlay is not wired" send
// them to different places. Asserting only that *a* reason arrives would pass
// with all five collapsed to one string, which is the defect this covers.
func TestClaudeLauncherSettingsNamesWhyItFoundNothing(t *testing.T) {
	t.Run("absent from PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		values, reason := claudeLauncherSettings()
		if values != nil {
			t.Fatalf("values = %q, want none", values)
		}
		if !strings.Contains(reason, "PATH") {
			t.Errorf("reason = %q, want it to say claude is not on PATH", reason)
		}
	})

	t.Run("a compiled binary", func(t *testing.T) {
		dir := t.TempDir()
		// The NUL byte is the "not text" signal; the rest deliberately looks
		// like a wrapper so this fails if the binary check is skipped.
		target := writeClaudeLauncher(t, dir, "\x00\x01\x02exec claude --settings /nix/store/x.json")
		withClaudeOnPath(t, target)

		values, reason := claudeLauncherSettings()
		if values != nil {
			t.Fatalf("values = %q, want none from a compiled binary", values)
		}
		if !strings.Contains(reason, "compiled binary") {
			t.Errorf("reason = %q, want it to name the compiled binary", reason)
		}
	})

	t.Run("a wrapper passing no --settings", func(t *testing.T) {
		dir := t.TempDir()
		target := writeClaudeLauncher(t, dir, "#!/bin/sh\nexec /opt/example/real-claude \"$@\"\n")
		withClaudeOnPath(t, target)

		values, reason := claudeLauncherSettings()
		if values != nil {
			t.Fatalf("values = %q, want none", values)
		}
		if !strings.Contains(reason, "--settings") {
			t.Errorf("reason = %q, want it to say the wrapper passes no --settings", reason)
		}
		if !strings.Contains(reason, target) {
			t.Errorf("reason = %q, want it to name the resolved launcher %q", reason, target)
		}
	})

	t.Run("a wrapper that does pass one", func(t *testing.T) {
		dir := t.TempDir()
		target := writeClaudeLauncher(t, dir, "#!/bin/sh\n"+
			`exec -a claude /opt/example/real-claude --settings /nix/store/overlay.json --plugin-dir /p "$@"`+"\n")
		withClaudeOnPath(t, target)

		values, reason := claudeLauncherSettings()
		if reason != "" {
			t.Fatalf("reason = %q, want none when the overlay was found", reason)
		}
		if len(values) != 1 || values[0] != "/nix/store/overlay.json" {
			t.Errorf("values = %q, want the one --settings path", values)
		}
	})
}
