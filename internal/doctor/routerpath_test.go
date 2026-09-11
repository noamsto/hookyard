package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

// writeRouterBinary lays down a file at <root>/bin/hookyard, so its full path
// ends in render.Marker the way a real emitted command's does.
func writeRouterBinary(t *testing.T, root string, executable bool) string {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, "hookyard")
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func routedCommand(router string, engine vocab.Engine, stateDir string) string {
	return fmt.Sprintf("%s route --registered-for %s --event pre_tool --state-dir %s", router, engine, stateDir)
}

// claudeConfig renders a real settings.json naming command, via the actual
// encoder, so expectations are written against real output rather than a
// hand-typed guess at how JSON quoting lands.
func claudeConfig(t *testing.T, dir, command string) string {
	t.Helper()
	path := filepath.Join(dir, "settings.json")
	entries := []render.Entry{{Event: "PreToolUse", Matcher: "Bash", Command: command}}
	if err := render.WriteClaude(path, entries); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRouterPathPassWhenExecutable(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	config := claudeConfig(t, root, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state")))

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func TestRouterPathFailWhenMissing(t *testing.T) {
	root := t.TempDir()
	router := filepath.Join(root, "bin", "hookyard") // never written
	config := claudeConfig(t, root, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state")))

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail", f.Status)
	}
	if !strings.Contains(f.Detail, "missing") {
		t.Errorf("detail = %q, want it to say missing", f.Detail)
	}
}

func TestRouterPathFailWhenNotExecutable(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, false)
	config := claudeConfig(t, root, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state")))

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail", f.Status)
	}
	if !strings.Contains(f.Detail, "not executable") {
		t.Errorf("detail = %q, want it to say not executable", f.Detail)
	}
}

func TestRouterPathFailWhenADirectory(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	router := filepath.Join(binDir, "hookyard")
	if err := os.MkdirAll(router, 0o755); err != nil {
		t.Fatal(err)
	}
	config := claudeConfig(t, root, routedCommand(router, vocab.ClaudeCode, filepath.Join(root, "state")))

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail", f.Status)
	}
	if !strings.Contains(f.Detail, "a directory") {
		t.Errorf("detail = %q, want it to say a directory", f.Detail)
	}
}

func TestRouterPathUnknownWhenNoHookyardEntry(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "settings.json")
	if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Unknown {
		t.Errorf("status = %v, want Unknown", f.Status)
	}
}

func TestRouterPathUnknownWhenConfigUnreadable(t *testing.T) {
	root := t.TempDir()
	// A directory in place of the config file makes os.ReadFile fail
	// deterministically, regardless of the process's privilege level —
	// unlike chmod 0000, which root ignores.
	config := filepath.Join(root, "settings.json")
	if err := os.Mkdir(config, 0o755); err != nil {
		t.Fatal(err)
	}

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Unknown {
		t.Errorf("status = %v, want Unknown", f.Status)
	}
}

// hookyard's own writers always leave one form, so two distinct paths under
// the marker can only come from a hand-edit or a foreign writer — hence the
// hand-typed fixture rather than a real writer, which cannot produce this
// shape.
func TestRouterPathChecksEachDistinctPath(t *testing.T) {
	root := t.TempDir()
	good := writeRouterBinary(t, root, true)
	bad := filepath.Join(root, "other", "bin", "hookyard") // never written

	config := filepath.Join(root, "settings.json")
	raw := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"hooks":[{"command":"%s route --event pre_tool"}]}],`+
		`"PostToolUse":[{"hooks":[{"command":"%s route --event post_tool"}]}]}}`, good, bad)
	if err := os.WriteFile(config, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail", f.Status)
	}
	if !strings.Contains(f.Detail, bad) {
		t.Errorf("detail = %q, want it to name %s", f.Detail, bad)
	}
	if strings.Contains(f.Detail, good+" (") {
		t.Errorf("detail = %q, should not report the good path as bad", f.Detail)
	}
}

func TestRouterPathPassesWhenBothDistinctPathsAreExecutable(t *testing.T) {
	root := t.TempDir()
	a := writeRouterBinary(t, root, true)
	otherRoot := filepath.Join(root, "other")
	b := writeRouterBinary(t, otherRoot, true)

	config := filepath.Join(root, "settings.json")
	raw := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"hooks":[{"command":"%s route --event pre_tool"}]}],`+
		`"PostToolUse":[{"hooks":[{"command":"%s route --event post_tool"}]}]}}`, a, b)
	if err := os.WriteFile(config, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	f := routerPath(vocab.ClaudeCode, config)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, a) || !strings.Contains(f.Detail, b) {
		t.Errorf("detail = %q, want both distinct paths named", f.Detail)
	}
}

func enforcementFinding(t *testing.T, findings []Finding) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Check == "enforcement" {
			return f
		}
	}
	t.Fatal("no enforcement finding in Run's output")
	return Finding{}
}

func TestRunUsesTheSingleRecoveredStateDir(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateDir := filepath.Join(root, "state")
	claudeDir := filepath.Join(root, "claude")

	claudeConfig(t, claudeDir, routedCommand(router, vocab.ClaudeCode, stateDir))
	writeTodaysStream(t, stateDir, `{"enforced":true}`+"\n")

	p := Paths{
		ClaudeConfigDir: claudeDir,
		CodexHome:       filepath.Join(root, "codex"),
		CursorHome:      filepath.Join(root, "cursor"),
		PiAgentDir:      filepath.Join(root, "pi"),
	}
	f := enforcementFinding(t, Run(p, root))
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func TestRunExplicitStateDirBeatsARecoveredOne(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	recovered := filepath.Join(root, "recovered-state")
	explicit := filepath.Join(root, "explicit-state")
	claudeDir := filepath.Join(root, "claude")

	claudeConfig(t, claudeDir, routedCommand(router, vocab.ClaudeCode, recovered))
	// Left empty: if Run used the recovered directory instead of the
	// explicit one, it would find this file and disagree with the assertion
	// below.
	writeTodaysStream(t, explicit, `{"enforced":true}`+"\n"+`{"enforced":false}`+"\n")

	p := Paths{
		ClaudeConfigDir: claudeDir,
		CodexHome:       filepath.Join(root, "codex"),
		CursorHome:      filepath.Join(root, "cursor"),
		PiAgentDir:      filepath.Join(root, "pi"),
		StateDir:        explicit,
	}
	f := enforcementFinding(t, Run(p, root))
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "1/2") {
		t.Errorf("detail = %q, want it to reflect the explicit state dir's stream", f.Detail)
	}
}

func TestRunFailsWhenEnginesDisagreeOnStateDir(t *testing.T) {
	root := t.TempDir()
	router := writeRouterBinary(t, root, true)
	stateA := filepath.Join(root, "state-a")
	stateB := filepath.Join(root, "state-b")

	claudeDir := filepath.Join(root, "claude")
	codexHome := filepath.Join(root, "codex")
	cursorHome := filepath.Join(root, "cursor")

	claudeConfig(t, claudeDir, routedCommand(router, vocab.ClaudeCode, stateA))
	if err := render.WriteCodex(filepath.Join(codexHome, "config.toml"), []render.Entry{
		{Event: "PreToolUse", Matcher: "Bash", Command: routedCommand(router, vocab.Codex, stateB)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := render.WriteCursor(filepath.Join(cursorHome, "hooks.json"), []render.Entry{
		{Event: "preToolUse", Matcher: "Shell", Command: routedCommand(router, vocab.Cursor, stateA)},
	}); err != nil {
		t.Fatal(err)
	}

	p := Paths{
		ClaudeConfigDir: claudeDir,
		CodexHome:       codexHome,
		CursorHome:      cursorHome,
		PiAgentDir:      filepath.Join(root, "pi"),
	}
	f := enforcementFinding(t, Run(p, root))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, stateA) || !strings.Contains(f.Detail, stateB) {
		t.Errorf("detail = %q, want both disagreeing state dirs named", f.Detail)
	}
}

func TestRunFallsBackToDefaultStateDirWhenNoneRecovered(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "default-state")
	// The fresh-machine case: no engine config exists at all, so
	// recoverStateDir finds nothing and Run must fall back to
	// record.DefaultStateDir(). HOOKYARD_STATE_DIR takes priority in that
	// chain, so pointing it here is enough without touching HOME.
	t.Setenv("HOOKYARD_STATE_DIR", stateDir)

	writeTodaysStream(t, stateDir, `{"enforced":true}`+"\n")

	p := Paths{
		ClaudeConfigDir: filepath.Join(root, "claude"),
		CodexHome:       filepath.Join(root, "codex"),
		CursorHome:      filepath.Join(root, "cursor"),
		PiAgentDir:      filepath.Join(root, "pi"),
	}
	f := enforcementFinding(t, Run(p, root))
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func writeTodaysStream(t *testing.T, stateDir, content string) {
	t.Helper()
	path := record.StreamPath(stateDir, time.Now())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
