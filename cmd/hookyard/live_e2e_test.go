package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

// liveE2EBudget bounds one real model turn plus a real process launch:
// generous for a loaded machine, but short enough that a hung claude process
// fails this test instead of the suite.
const liveE2EBudget = 90 * time.Second

// liveProbePrompt is the exact prompt
// docs/design/fixtures/hook-payloads/README.md records using to capture live
// payloads from all three engines. It is reused verbatim here because it is
// already known to reliably produce one Bash tool call, not because the
// wording matters on its own.
const liveProbePrompt = "Run the shell command: echo hookyard-probe"

// TestLiveClaudeCodeRefusesTheDeniedToolCall proves enforcement rather than
// verdict shape. Every other test here asserts that hookyard renders the JSON
// bytes an engine is documented to read; none drives a real engine, so none
// can tell a correct rendering from one the engine happens to ignore. This
// installs hookyard into a scratch Claude Code configuration with a handler
// that unconditionally denies, runs the real claude binary on
// liveProbePrompt, and checks that the shell call was actually refused.
//
// It is gated behind HOOKYARD_E2E because the repo gate has no credentials to
// authenticate a real API call, and behind claude's presence on PATH because
// the engine is not a build dependency. Running it requires the environment
// claude itself authenticates from (e.g. ANTHROPIC_API_KEY); it never reads
// or copies that credential, it only lets the child process inherit the
// environment it is given.
//
// Claude Code is the engine covered, chosen over Codex and Cursor because it
// is the one with a documented, scriptable non-interactive mode (`claude -p`).
// Codex's per-entry hook-trust hashing and Cursor's `cursor-agent` trust flow
// are both unverified here, and a live check for either means nothing until
// they are. The shape below — scratch config, a deny handler, a probe prompt,
// checking both hookyard's own record and the engine's own output —
// generalizes to either once that groundwork exists.
func TestLiveClaudeCodeRefusesTheDeniedToolCall(t *testing.T) {
	if os.Getenv("HOOKYARD_E2E") != "1" {
		t.Skip("set HOOKYARD_E2E=1 to run this test against a live claude binary")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not found on PATH")
	}

	hookyardBin := liveBuildHookyard(t)
	root := t.TempDir()

	// A scratch CLAUDE_CONFIG_DIR and a scratch project directory, exactly as
	// the README describes the capture run doing it: nothing hookyard writes
	// or claude reads for this test lives under the developer's real
	// ~/.claude.
	claudeConfigDir := filepath.Join(root, "claude-config")
	projectDir := filepath.Join(root, "project")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(claudeConfigDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", claudeConfigDir, err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	projectDir, err = filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatalf("resolve project dir: %v", err)
	}

	// All three engines refuse to load hooks in a directory they have not
	// trusted (README, "How they were captured"). claude -p skips the
	// interactive trust dialog, but the captured payloads were themselves
	// produced against a pre-seeded trust record (docs/design/hookyard.md),
	// so this does the same rather than leaning on undocumented -p behaviour
	// alone.
	liveSeedClaudeTrust(t, claudeConfigDir, projectDir)
	liveLinkClaudeAuth(t, claudeConfigDir)

	reasonToken := fmt.Sprintf("hookyard-e2e-deny-%d", time.Now().UnixNano())
	firedMarker := filepath.Join(root, "handler-fired")
	handlerPath := liveWriteDenyHandler(t, root, firedMarker, reasonToken)
	manifestPath := liveWriteManifest(t, root, handlerPath)

	install := exec.Command(hookyardBin, "install",
		"--manifest", manifestPath,
		"--router-path", hookyardBin,
		"--state-dir", stateDir,
		"--claude-settings", filepath.Join(claudeConfigDir, "settings.json"),
		"--codex-config", filepath.Join(root, "unused-codex", "config.toml"),
		"--cursor-hooks", filepath.Join(root, "unused-cursor", "hooks.json"),
	)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("hookyard install: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveE2EBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, claudeBin, "-p", "--model", "claude-haiku-4-5-20251001", liveProbePrompt)
	probe.Dir = projectDir
	probe.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+claudeConfigDir)
	output, runErr := probe.CombinedOutput()

	if _, statErr := os.Stat(firedMarker); os.IsNotExist(statErr) {
		t.Fatalf("the deny handler was never invoked: claude never fired the PreToolUse hook at all "+
			"(claude run error: %v)\n--- claude output ---\n%s", runErr, output)
	}

	rec, ok := liveFindBashDenyRecord(t, stateDir)
	if !ok {
		t.Fatalf("the deny handler fired, but hookyard's own record has no Bash pre_tool entry for it — "+
			"a hookyard-side problem, not an engine one\n--- claude output ---\n%s", output)
	}
	if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
		t.Fatalf("hookyard did not render an enforced deny for the probe call (verdict=%q enforced=%v) — "+
			"a hookyard-side problem, not an engine one\n--- claude output ---\n%s", rec.Verdict, rec.Enforced, output)
	}

	if !strings.Contains(string(output), reasonToken) {
		t.Fatalf("hookyard recorded an enforced deny, but claude's own output never surfaced the deny "+
			"reason (%s): the shell call may have run anyway despite the recorded deny\n"+
			"--- claude output ---\n%s", reasonToken, output)
	}

	t.Logf("claude refused the probe call; full output:\n%s", output)
}

// liveBuildHookyard compiles cmd/hookyard at a path containing
// render.Marker ("/bin/hookyard"): BuildPlan, which `hookyard install` calls,
// refuses any router path that doesn't, since that marker is how a later
// install finds its own entries again to strip them.
func liveBuildHookyard(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin", "hookyard")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(bin), err)
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// liveSeedClaudeTrust pre-seeds the trust record doctor.go reads
// (.claude.json's projects[dir].hasTrustDialogAccepted) inside the scratch
// CLAUDE_CONFIG_DIR, rather than the real one, so claude does not depend on
// an interactive trust prompt to load hooks in dir.
func liveSeedClaudeTrust(t *testing.T, claudeConfigDir, dir string) {
	t.Helper()
	state := map[string]any{
		"projects": map[string]any{
			dir: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal trust record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeConfigDir, ".claude.json"), raw, 0o600); err != nil {
		t.Fatalf("write trust record: %v", err)
	}
}

// liveLinkClaudeAuth points the scratch config directory at the developer's
// existing credentials by symlink, which is how the captured payloads reached
// authentication too (README, "How they were captured"): linked, never
// copied, and never read by this test. Without it claude exits "Not logged
// in" against a scratch CLAUDE_CONFIG_DIR and the run cannot say anything
// about enforcement. A machine authenticating some other way — an API key in
// the environment, say — simply has no file to link, so this is best-effort
// rather than a precondition.
func liveLinkClaudeAuth(t *testing.T, claudeConfigDir string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	real := filepath.Join(home, ".claude", ".credentials.json")
	if _, err := os.Lstat(real); err != nil {
		return
	}
	if err := os.Symlink(real, filepath.Join(claudeConfigDir, ".credentials.json")); err != nil {
		t.Logf("could not link existing credentials: %v", err)
	}
}

// liveWriteDenyHandler writes a handler that always denies with reason,
// touching firedMarker before it prints anything. A missing marker after the
// run means the engine never invoked the hook at all — independent of, and
// checked before, anything hookyard itself recorded.
func liveWriteDenyHandler(t *testing.T, dir, firedMarker, reason string) string {
	t.Helper()
	path := filepath.Join(dir, "deny-handler.sh")
	script := "#!/bin/sh\n" +
		"touch " + firedMarker + "\n" +
		"printf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"" + reason + "\"}}'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write deny handler: %v", err)
	}
	return path
}

// liveWriteManifest writes a one-handler manifest scoped to Claude Code's
// Bash tool on pre_tool, so the router only denies the probe's own shell
// call rather than every tool call claude might otherwise make.
func liveWriteManifest(t *testing.T, dir, handlerPath string) string {
	t.Helper()
	m := manifest.Manifest{Handlers: []manifest.Handler{{
		ID:      "e2e-deny",
		Exec:    handlerPath,
		Events:  []string{"pre_tool"},
		Engines: []string{"claude-code"},
		Match:   []string{"Bash"},
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// liveFindBashDenyRecord scans stateDir's stream file for the record
// hookyard wrote for the probe's Bash call. Unlike route_test.go's
// readRecord, it does not assume exactly one line: a live model can make
// tool calls this test did not ask for, and each one gets its own record.
func liveFindBashDenyRecord(t *testing.T, stateDir string) (record.Record, bool) {
	t.Helper()
	raw, err := os.ReadFile(record.StreamPath(stateDir, time.Now()))
	if err != nil {
		return record.Record{}, false
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec record.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record line %q: %v", line, err)
		}
		if rec.ToolName == "Bash" {
			return rec, true
		}
	}
	return record.Record{}, false
}
