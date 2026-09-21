package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

// liveE2EBudget bounds one real model turn plus a real process launch:
// generous for a loaded machine, but short enough that a hung claude process
// fails this test instead of the suite.
const liveE2EBudget = 90 * time.Second

// liveProbePrompt asks for the same single Bash tool call
// docs/design/fixtures/hook-payloads/README.md records capturing live payloads
// with, but names a filesystem side effect rather than an echo, so whether the
// denied call actually ran is a file that either exists or does not.
const liveProbePrompt = "Run the shell command: touch SIDE-EFFECT.txt"

// liveProbeSideEffect is the file liveProbePrompt would create, relative to
// the directory the probe runs in.
const liveProbeSideEffect = "SIDE-EFFECT.txt"

// TestLiveClaudeCodeRefusesTheDeniedToolCall proves enforcement rather than
// verdict shape. Every other test here asserts that hookyard renders the JSON
// bytes an engine is documented to read; none drives a real engine, so none
// can tell a correct rendering from one the engine happens to ignore. This
// builds the overlay by running `hookyard emit --base` over a base carrying
// its own, non-hookyard PreToolUse hook — the artifact this change actually
// ships, not a hand-written equivalent of it — passes it to a real claude
// binary as --settings, and checks two things: that the deny handler's tool
// call was actually refused, and that the base's own hook still fired.
//
// That second assertion is the load-bearing half. A version of this test that
// only checked hookyard's own hook would pass against the exact merge bug
// #29 exists to fix: the old merge decoded and re-encoded every inherited
// hook through a struct that always writes a timeout field, and an entry
// carrying `"timeout": 0` is — measured twice against the live claude
// binary — silently never run. The base file below deliberately declares no
// timeout on its own hook, the shape any hand-written settings.json actually
// has, so a regression back to that merge would leave hookyard's own deny
// firing (proving nothing wrong with hookyard) while the base's hook goes
// silently dead (the actual bug).
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
//
// Not accounted for below: claude on PATH here may itself be a launcher that
// passes its own `--plugin-dir` trees, each with hooks of its own. That is
// pre-existing exposure in this test, not something emit changes, so it is
// noted rather than controlled for.
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

	// install still sets up the router's own state (table.json under
	// stateDir) and the three engines that stay on install (§4.2); it is
	// never given anywhere to put a Claude Code settings.json, because that
	// flag no longer exists — emit below is the only path that produces
	// Claude Code's overlay.
	install := exec.Command(hookyardBin, "install",
		"--manifest", manifestPath,
		"--router-path", hookyardBin,
		"--state-dir", stateDir,
		"--codex-config", filepath.Join(root, "unused-codex", "config.toml"),
		"--cursor-hooks", filepath.Join(root, "unused-cursor", "hooks.json"),
		// Without this, install() resolves the real $PI_CODING_AGENT_DIR (or
		// ~/.pi/agent) default before this test's flags are even parsed, and
		// this arm would rewrite the developer's actual Pi settings.json.
		"--pi-settings", filepath.Join(root, "unused-pi", "settings.json"),
	)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("hookyard install: %v\n%s", err, out)
	}

	baseMarker := filepath.Join(root, "base-hook-fired")
	basePath := liveWriteClaudeBaseWithOwnHook(t, root, baseMarker)
	overlayPath := liveEmitClaudeOverlay(t, hookyardBin, root, hookyardBin, stateDir, basePath)

	// claudeConfigDir/settings.json is never written: emit's whole point is
	// that Claude Code's overlay comes from --settings rather than from
	// anything hookyard puts under CLAUDE_CONFIG_DIR, so "carries no
	// hookyard entry" holds simply because nothing here ever creates the
	// file at all.
	ctx, cancel := context.WithTimeout(context.Background(), liveE2EBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, claudeBin, "-p", "--model", "claude-haiku-4-5-20251001",
		"--settings", overlayPath, liveProbePrompt)
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

	if rec.Reason != reasonToken {
		t.Fatalf("hookyard recorded the deny with reason %q, want the handler's own %q — the reason the "+
			"engine was handed is not the one the handler returned\n--- claude output ---\n%s",
			rec.Reason, reasonToken, output)
	}

	// Enforcement, not verdict shape: every assertion above would still hold
	// if claude had recorded the deny and run the command anyway.
	if _, statErr := os.Stat(filepath.Join(projectDir, liveProbeSideEffect)); statErr == nil {
		t.Fatalf("the denied shell command ran anyway: %s exists despite an enforced deny\n"+
			"--- claude output ---\n%s", liveProbeSideEffect, output)
	}

	// The load-bearing assertion (see the test's doc comment): the base's own
	// PreToolUse hook, carrying no timeout field of its own, must have fired
	// too. If emit's merge regressed to re-encoding inherited hooks through a
	// struct that always writes timeout, this hook would have gone dead at
	// timeout:0 while every assertion above still passed.
	if _, statErr := os.Stat(baseMarker); os.IsNotExist(statErr) {
		t.Fatalf("the base's own PreToolUse hook never fired: emit's merge is dropping or disabling an "+
			"inherited hook it did not write itself\n--- claude output ---\n%s", output)
	}

	t.Logf("claude refused the probe call and the base's own hook still fired; full output:\n%s", output)
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

// liveWriteClaudeBaseWithOwnHook writes a settings.json-shaped file carrying
// one PreToolUse hook of its own, matched on Bash and touching marker,
// deliberately declaring no timeout field — the shape a hand-written
// settings.json actually has, and the shape whose absence the old merge used
// to fill in as 0 (§4.2b). This is the base emit merges hookyard's own entry
// into, standing in for the consumer's real settings.json.
func liveWriteClaudeBaseWithOwnHook(t *testing.T, dir, marker string) string {
	t.Helper()
	base := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{{
				"matcher": "Bash",
				"hooks": []map[string]any{{
					"type":    "command",
					"command": "touch " + marker,
				}},
			}},
		},
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base settings: %v", err)
	}
	path := filepath.Join(dir, "claude-base-settings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write base settings: %v", err)
	}
	return path
}

// liveEmitClaudeOverlay runs the real `hookyard emit` subcommand — the
// artifact a Nix derivation would place at Claude Code's --settings path —
// over basePath, and saves its stdout to a file so it can be passed to claude
// as --settings itself. Driving emit rather than calling render.ClaudeSettings
// directly is the point of this test: a hand-written overlay would pass
// against bugs the CLI's own flag wiring could still have.
func liveEmitClaudeOverlay(t *testing.T, hookyardBin, root, routerPath, stateDir, basePath string) string {
	t.Helper()
	emit := exec.Command(hookyardBin, "emit",
		"--engine", "claude-code",
		"--router-path", routerPath,
		"--state-dir", stateDir,
		"--base", basePath,
	)
	out, err := emit.Output()
	if err != nil {
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("hookyard emit: %v\n%s", err, stderr)
	}
	path := filepath.Join(root, "claude-settings-overlay.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write emitted overlay: %v", err)
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

// TestLiveClaudeCodePluginRefusesTheDeniedToolCall is
// TestLiveClaudeCodeRefusesTheDeniedToolCall's build-mode counterpart: instead
// of a hand-run `hookyard emit` overlay merged over a base settings.json, this
// drives the real `hookyard build` artifact — a self-contained plugin
// directory carrying its own launcher, bundled binary, and table.json — via
// claude's own `--plugin-dir`. It proves the same thing emit's sibling test
// proves (a real claude process actually enforces the deny, not just that
// hookyard renders JSON an engine is documented to read), but through the
// distribution path build mode ships instead of yard mode's.
//
// Gated identically: HOOKYARD_E2E=1 and claude on PATH, for the same reasons
// (no credentials in the repo gate, claude not a build dependency).
func TestLiveClaudeCodePluginRefusesTheDeniedToolCall(t *testing.T) {
	if os.Getenv("HOOKYARD_E2E") != "1" {
		t.Skip("set HOOKYARD_E2E=1 to run this test against a live claude binary")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not found on PATH")
	}

	hookyardBin := liveBuildHookyard(t)
	root := t.TempDir()

	claudeConfigDir := filepath.Join(root, "claude-config")
	projectDir := filepath.Join(root, "project")
	pluginDir := filepath.Join(root, "plugin")
	stateDir := filepath.Join(root, "state")
	xdgStateDir := filepath.Join(root, "xdg-state")
	for _, dir := range []string{claudeConfigDir, projectDir, pluginDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	projectDir, err = filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatalf("resolve project dir: %v", err)
	}

	liveSeedClaudeTrust(t, claudeConfigDir, projectDir)
	liveLinkClaudeAuth(t, claudeConfigDir)

	reasonToken := fmt.Sprintf("hookyard-e2e-build-deny-%d", time.Now().UnixNano())
	firedMarker := filepath.Join(root, "handler-fired")
	manifestPath := liveWritePluginDenyHandlerAndManifest(t, root, pluginDir, firedMarker, reasonToken)

	build := exec.Command(hookyardBin, "build",
		"--engine", "claude-code",
		"--manifest", manifestPath,
		"--out", pluginDir,
		"--name", "hookyard-live-probe",
	)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("hookyard build: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveE2EBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, claudeBin, "-p", "--model", "claude-haiku-4-5-20251001",
		"--plugin-dir", pluginDir, liveProbePrompt)
	probe.Dir = projectDir
	probe.Env = append(os.Environ(),
		"CLAUDE_CONFIG_DIR="+claudeConfigDir,
		"HOOKYARD_STATE_DIR="+stateDir,
		"XDG_STATE_HOME="+xdgStateDir,
	)
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
	if rec.Reason != reasonToken {
		t.Fatalf("hookyard recorded the deny with reason %q, want the handler's own %q — the reason the "+
			"engine was handed is not the one the handler returned\n--- claude output ---\n%s",
			rec.Reason, reasonToken, output)
	}

	// Enforcement, not verdict shape: every assertion above would still hold
	// if claude had recorded the deny and run the command anyway.
	if _, statErr := os.Stat(filepath.Join(projectDir, liveProbeSideEffect)); statErr == nil {
		t.Fatalf("the denied shell command ran anyway: %s exists despite an enforced deny\n"+
			"--- claude output ---\n%s", liveProbeSideEffect, output)
	}

	t.Logf("claude refused the probe call through the built plugin; full output:\n%s", output)
}

// liveWritePluginDenyHandlerAndManifest writes a build-mode handler
// (plugin-root-relative exec, per manifest.ExecPluginRelative) under pluginDir
// and a manifest naming it, mirroring setupBuildPlugin in build_test.go. The
// handler touches firedMarker before printing its deny JSON, same contract as
// liveWriteDenyHandler's yard-mode script. The manifest is written outside
// pluginDir, since an author's manifest does not itself ship inside the
// plugin build reads it to produce.
func liveWritePluginDenyHandlerAndManifest(t *testing.T, manifestDir, pluginDir, firedMarker, reason string) string {
	t.Helper()
	handlerRel := filepath.Join("handlers", "deny")
	handlerPath := filepath.Join(pluginDir, handlerRel)
	if err := os.MkdirAll(filepath.Dir(handlerPath), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(handlerPath), err)
	}
	script := "#!/bin/sh\n" +
		"touch " + firedMarker + "\n" +
		"printf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"" + reason + "\"}}'\n"
	if err := os.WriteFile(handlerPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write deny handler: %v", err)
	}

	m := manifest.Manifest{Handlers: []manifest.Handler{{
		ID:      "e2e-build-deny",
		Exec:    filepath.ToSlash(handlerRel),
		Events:  []string{"pre_tool"},
		Engines: []string{"claude-code"},
		Match:   []string{"Bash"},
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestPath := filepath.Join(manifestDir, "hookyard.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return manifestPath
}

// The Pi arm. Unlike Claude Code above, Pi's capture runs fully offline
// against a local model with no credentials and no API cost
// (docs/design/fixtures/hook-payloads/README.md, "How they were captured"),
// which is what makes it automatable at all rather than manual PR evidence.

// livePiModelEndpoint is the local Lemonade server the fixture capture run
// used and this arm reuses: no credentials, no API cost, and reachable
// without any network access beyond localhost even under PI_OFFLINE=1.
const livePiModelEndpoint = "http://127.0.0.1:13305/v1"

// livePiProbeModel names one model this arm's scratch models.json points at.
// It must already be pulled by the local server; the arm has no way to pull
// one itself and skips rather than guessing if the server is unreachable.
const livePiProbeModel = "Qwen3-Coder-30B-A3B-Instruct-GGUF"

// livePiBudget is wider than liveE2EBudget: Claude Code answers a hosted API,
// this arm answers a local model server, and observed latency against it
// ranged from single-digit seconds to well over two minutes under load. A
// tight budget here would flake on exactly the machines this arm exists to
// run on without a hosted API key.
const livePiBudget = 5 * time.Minute

// livePiProbePrompt asks for a real filesystem side effect rather than
// liveProbePrompt's echo, because two of this arm's three assertions are read
// off whether a file exists afterward, not off captured stdout.
const livePiProbePrompt = "Run the shell command: touch SIDE-EFFECT.txt"

// liveRequirePi skips on any of three conditions, each with its own message:
// HOOKYARD_E2E alone would make this arm fail outright, and HOOKYARD_E2E plus
// pi-on-PATH alone would still burn the whole per-test budget spawning pi
// against a model server that was never started. It returns pi's resolved
// path once all three hold.
func liveRequirePi(t *testing.T) string {
	t.Helper()
	if os.Getenv("HOOKYARD_E2E") != "1" {
		t.Skip("set HOOKYARD_E2E=1 to run this test against a live pi binary")
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi binary not found on PATH")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(livePiModelEndpoint + "/models")
	if err != nil {
		t.Skipf("no local model endpoint at %s/models: %v", livePiModelEndpoint, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("local model endpoint at %s/models returned %s", livePiModelEndpoint, resp.Status)
	}
	return piBin
}

// livePiSetup builds the scratch layout every test below shares: a hookyard
// binary, a scratch PI_CODING_AGENT_DIR seeded with a local-provider
// settings.json and models.json, a project directory to run pi from, and a
// state directory for hookyard's own records. Each caller still runs its own
// install with its own --router-path, since that is the one thing the three
// tests vary.
func livePiSetup(t *testing.T) (hookyardBin, root, agentDir, projectDir, stateDir string) {
	t.Helper()
	hookyardBin = liveBuildHookyard(t)
	root = t.TempDir()
	agentDir = filepath.Join(root, "pi-agent")
	projectDir = filepath.Join(root, "project")
	stateDir = filepath.Join(root, "state")
	liveSeedPiAgentDir(t, agentDir)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	return hookyardBin, root, agentDir, projectDir, stateDir
}

// liveSeedPiAgentDir writes a settings.json and models.json shaped like
// ~/.pi/agent's own (README, "How they were captured"), pointing at the local
// Lemonade server. It is a from-scratch seed, never a read of the developer's
// real config: the local provider needs no real credential, so apiKey is the
// literal string the server itself accepts, and ~/.pi/agent/auth.json is
// never opened.
func liveSeedPiAgentDir(t *testing.T, agentDir string) {
	t.Helper()
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", agentDir, err)
	}
	livePiWriteJSON(t, filepath.Join(agentDir, "settings.json"), map[string]string{
		"defaultModel":    livePiProbeModel,
		"defaultProvider": "Lemonade",
	})
	livePiWriteJSON(t, filepath.Join(agentDir, "models.json"), map[string]any{
		"providers": map[string]any{
			"Lemonade": map[string]any{
				"api":     "openai-completions",
				"apiKey":  "lemonade",
				"baseUrl": livePiModelEndpoint,
				"models":  []map[string]string{{"id": livePiProbeModel}},
			},
		},
	})
}

func livePiWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// livePiEnv is the environment every pi invocation in this arm runs under:
// PI_OFFLINE=1 keeps it off the network beyond the local model server, and
// PI_AGENT_HOOKS is forced empty rather than merely unset — a machine running
// this arm may have the Nix wrapper's own guard list live in its ambient
// environment (docs/design/hookyard.md §8's coexistence hazard), and
// inheriting it would let that bridge fire alongside hookyard's own and make
// which one produced a given record ambiguous. Both env.Environ() entries are
// filtered out first because a duplicate key's winner is unspecified.
func livePiEnv(agentDir string) []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		if strings.HasPrefix(kv, "PI_CODING_AGENT_DIR=") ||
			strings.HasPrefix(kv, "PI_OFFLINE=") ||
			strings.HasPrefix(kv, "PI_AGENT_HOOKS=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"PI_CODING_AGENT_DIR="+agentDir,
		"PI_OFFLINE=1",
		"PI_AGENT_HOOKS=",
	)
}

// livePiWriteManifest mirrors liveWriteManifest, scoped to Pi's own tool
// vocabulary rather than Claude Code's — the manifest itself still declares
// Bash in the normalized spelling, same as every engine's manifest, and
// vocab.NativeMatcher is what lowercases it to "bash" for Pi (§7).
func livePiWriteManifest(t *testing.T, dir, handlerPath string) string {
	t.Helper()
	m := manifest.Manifest{Handlers: []manifest.Handler{{
		ID:      "e2e-pi-deny",
		Exec:    handlerPath,
		Events:  []string{"pre_tool"},
		Engines: []string{"pi"},
		Match:   []string{"Bash"},
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "hookyard-pi.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// livePiWriteMultiEventManifest is livePiWriteManifest's sibling for the
// fixture-shape test: one handler registered for every event that test
// captures — session_start, pre_tool, post_tool and pi:session_shutdown —
// rather than pre_tool alone, since a manifest that never asks pi to fire the
// other three would leave their fixtures unverifiable no matter what the
// capture router does. No Match: an unset matcher renders empty for every
// event (render.buildPlan's everyTool case), and a non-empty one would
// silently drop session_start/session_shutdown, whose events carry no
// toolName for the bridge's matcher check to compare against.
func livePiWriteMultiEventManifest(t *testing.T, dir, handlerPath string) string {
	t.Helper()
	m := manifest.Manifest{Handlers: []manifest.Handler{{
		ID:      "e2e-pi-fixture-shape",
		Exec:    handlerPath,
		Events:  []string{"session_start", "pre_tool", "post_tool", "pi:session_shutdown"},
		Engines: []string{"pi"},
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "hookyard-pi.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// livePiWritePreAndPostManifest registers two handlers on the same tool
// rather than livePiWriteManifest's one: a pre_tool advisory and a post_tool
// advisory, both matching Bash, so a single call fires both and this suite can
// prove the bridge orders the pre one ahead of the post one on the shared
// tool_result.
func livePiWritePreAndPostManifest(t *testing.T, dir, preHandlerPath, postHandlerPath string) string {
	t.Helper()
	m := manifest.Manifest{Handlers: []manifest.Handler{
		{
			ID:      "e2e-pi-pre",
			Exec:    preHandlerPath,
			Events:  []string{"pre_tool"},
			Engines: []string{"pi"},
			Match:   []string{"Bash"},
		},
		{
			ID:      "e2e-pi-post",
			Exec:    postHandlerPath,
			Events:  []string{"post_tool"},
			Engines: []string{"pi"},
			Match:   []string{"Bash"},
		},
	}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "hookyard-pi.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// livePiInstall runs `hookyard install` with routerPath as the one path each
// test varies, and every other destination pointed at scratch files under
// root so no real engine config is ever touched. agentDirs registers the
// bridge into every listed pi settings.json in a single install call, which
// is how a dispatched pi worker's non-default PI_CODING_AGENT_DIR gets
// covered alongside the default one.
func livePiInstall(t *testing.T, hookyardBin, root string, agentDirs []string, stateDir, manifestPath, routerPath string) {
	t.Helper()
	args := []string{"install",
		"--manifest", manifestPath,
		"--router-path", routerPath,
		"--state-dir", stateDir,
	}
	for _, dir := range agentDirs {
		args = append(args, "--pi-settings", filepath.Join(dir, "settings.json"))
	}
	args = append(args,
		"--codex-config", filepath.Join(root, "unused-codex", "config.toml"),
		"--cursor-hooks", filepath.Join(root, "unused-cursor", "hooks.json"),
	)
	install := exec.Command(hookyardBin, args...)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("hookyard install: %v\n%s", err, out)
	}
}

// TestLivePiRefusesTheDeniedToolCall is the deny-enforcement half of §5's
// guarantee: an install with a handler that unconditionally denies, run
// against a real pi process, must actually stop the shell command from
// running — not just render a verdict hookyard's own tests already check the
// shape of.
func TestLivePiRefusesTheDeniedToolCall(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, stateDir := livePiSetup(t)

	reasonToken := fmt.Sprintf("hookyard-e2e-pi-deny-%d", time.Now().UnixNano())
	handlerPath := liveWriteDenyHandler(t, root, filepath.Join(root, "handler-fired"), reasonToken)
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, hookyardBin)

	sideEffect := filepath.Join(projectDir, "SIDE-EFFECT.txt")

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, runErr := probe.CombinedOutput()

	if _, statErr := os.Stat(sideEffect); statErr == nil {
		t.Fatalf("the denied tool call ran anyway: %s exists\n--- pi output ---\n%s", sideEffect, output)
	}

	rec, ok := liveFindBashDenyRecord(t, stateDir)
	if !ok {
		t.Fatalf("hookyard's own record has no Bash pre_tool entry for the probe call "+
			"(pi run error: %v)\n--- pi output ---\n%s", runErr, output)
	}
	if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
		t.Fatalf("hookyard did not render an enforced deny for the probe call (verdict=%q enforced=%v)\n"+
			"--- pi output ---\n%s", rec.Verdict, rec.Enforced, output)
	}

	t.Logf("pi refused the probe call; full output:\n%s", output)
}

// TestLivePiRefusesTheDeniedToolCallInASecondSettingsDir proves §5's
// deny-enforcement guarantee holds for a SECOND, non-default pi settings dir
// registered in the same install — the scenario a dispatched pi worker
// process hits when $PI_CODING_AGENT_DIR points somewhere other than the
// first configured dir. It is TestLivePiRefusesTheDeniedToolCall with a
// second agent dir installed alongside the first, and the probe run against
// that second dir instead.
func TestLivePiRefusesTheDeniedToolCallInASecondSettingsDir(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, stateDir := livePiSetup(t)

	agentDir2 := filepath.Join(root, "pi-agent-2")
	liveSeedPiAgentDir(t, agentDir2)

	reasonToken := fmt.Sprintf("hookyard-e2e-pi-deny-2nd-dir-%d", time.Now().UnixNano())
	handlerPath := liveWriteDenyHandler(t, root, filepath.Join(root, "handler-fired"), reasonToken)
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir, agentDir2}, stateDir, manifestPath, hookyardBin)

	sideEffect := filepath.Join(projectDir, "SIDE-EFFECT.txt")

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir2)
	output, runErr := probe.CombinedOutput()

	if _, statErr := os.Stat(sideEffect); statErr == nil {
		t.Fatalf("the denied tool call ran anyway: %s exists\n--- pi output ---\n%s", sideEffect, output)
	}

	rec, ok := liveFindBashDenyRecord(t, stateDir)
	if !ok {
		t.Fatalf("hookyard's own record has no Bash pre_tool entry for the probe call "+
			"(pi run error: %v)\n--- pi output ---\n%s", runErr, output)
	}
	if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
		t.Fatalf("hookyard did not render an enforced deny for the probe call (verdict=%q enforced=%v)\n"+
			"--- pi output ---\n%s", rec.Verdict, rec.Enforced, output)
	}

	t.Logf("pi refused the probe call against the second settings dir; full output:\n%s", output)
}

// TestLivePiFailsOpenWhenTheRouterBinaryIsAbsent proves §5's fail-open
// guarantee against a real pi process rather than a unit test's mocked
// execFile: a router path that does not exist must not block the tool call.
// Getting this backwards would mean hookyard fails closed on its fourth
// engine, which is the one inversion design doc §5 exists to rule out.
func TestLivePiFailsOpenWhenTheRouterBinaryIsAbsent(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, stateDir := livePiSetup(t)

	handlerPath := liveWriteDenyHandler(t, root, filepath.Join(root, "handler-fired"), "unused-if-fail-open-holds")
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	// Contains render.Marker, so BuildPlan accepts it as a router path, but
	// nothing is ever written there — the absent-router case §5 governs.
	missingRouter := filepath.Join(root, "does-not-exist", "bin", "hookyard")
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, missingRouter)

	sideEffect := filepath.Join(projectDir, "SIDE-EFFECT.txt")

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s", err, output)
	}

	if _, statErr := os.Stat(sideEffect); statErr != nil {
		t.Fatalf("the tool call did not run with the router binary absent — hookyard failed CLOSED on Pi, "+
			"inverting §5's guarantee (stat: %v)\n--- pi output ---\n%s", statErr, output)
	}
}

// liveWritePiCaptureRouter writes a router that is not hookyard at all: it
// only records the exact bytes the bridge piped to its stdin, so this test
// can compare what the bridge actually sends against the committed fixtures
// without going through hookyard's own decode/render round trip, which would
// hide a drift between the two. It captures into captureDir, one file per
// invocation named after the invoking shell's own PID, because the bridge
// calls this same script once per registered event — a single `cat >` target
// would leave only the last firing's bytes behind, and the script has no
// argv-parsing of its own to name the event that fired it. Its path still has
// to satisfy render.BuildPlan's Marker check, the same requirement any real
// router path meets.
func liveWritePiCaptureRouter(t *testing.T, root, captureDir string) string {
	t.Helper()
	path := filepath.Join(root, "capture-router", "bin", "hookyard")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", captureDir, err)
	}
	script := "#!/bin/sh\ncat > " + captureDir + "/$$.json\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write capture router: %v", err)
	}
	return path
}

// liveReadPiCaptures reads every payload liveWritePiCaptureRouter's router
// wrote into dir and keys each by its own hook_event_name — the bridge's
// native pi event name — since that is the only thing identifying which
// firing produced it; the capture filename carries nothing but a PID.
func liveReadPiCaptures(t *testing.T, dir string) map[string]map[string]json.RawMessage {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read capture dir %s: %v", dir, err)
	}
	captures := map[string]map[string]json.RawMessage{}
	for _, entry := range entries {
		capturePath := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatalf("read %s: %v", capturePath, err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("captured payload %s is not valid JSON: %v\n%s", capturePath, err, raw)
		}
		var name string
		if err := json.Unmarshal(payload["hook_event_name"], &name); err != nil {
			t.Fatalf("captured payload %s carries no hook_event_name: %v\n%s", capturePath, err, raw)
		}
		if _, dup := captures[name]; dup {
			t.Fatalf("more than one captured payload names hook_event_name %q; expected one firing per event", name)
		}
		captures[name] = payload
	}
	return captures
}

// TestLivePiPayloadMatchesTheCommittedFixtureShape is the automated binding
// between the generated bridge (internal/render/pi_bridge.ts's `extras`
// table) and the committed fixtures: without it the generator can drift from
// docs/design/fixtures/hook-payloads/pi-*.json with go build, go vet, go test
// and the nix syntax gate all green, since none of those runs the bridge
// against a real pi process. It covers every event that table carries an
// entry for — session_start, tool_call, tool_result and session_shutdown —
// not tool_call alone, so a bridge that registers one of the other three and
// never actually fires it fails here instead of going quiet on someone's
// machine.
func TestLivePiPayloadMatchesTheCommittedFixtureShape(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, stateDir := livePiSetup(t)

	handlerPath := liveWriteDenyHandler(t, root, filepath.Join(root, "handler-fired"), "unused-not-invoked")
	manifestPath := livePiWriteMultiEventManifest(t, root, handlerPath)
	captureDir := filepath.Join(root, "captured-payloads")
	captureRouter := liveWritePiCaptureRouter(t, root, captureDir)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, captureRouter)

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s", err, output)
	}

	captures := liveReadPiCaptures(t, captureDir)
	liveAssertPayloadMatchesFixture(t, captures, "session_start", "pi-session_start.json")
	liveAssertPayloadMatchesFixture(t, captures, "tool_call", "pi-tool_call.json")
	liveAssertPayloadMatchesFixture(t, captures, "tool_result", "pi-tool_result.json")
	liveAssertPayloadMatchesFixture(t, captures, "session_shutdown", "pi-session_shutdown.json")
}

// liveAssertPayloadMatchesFixture isolates nativeEvent's captured payload
// from captures and compares its key set against fixtureName, the committed
// fixture naming that event.
func liveAssertPayloadMatchesFixture(t *testing.T, captures map[string]map[string]json.RawMessage, nativeEvent, fixtureName string) {
	t.Helper()
	got, ok := captures[nativeEvent]
	if !ok {
		t.Fatalf("the bridge never fired %s: no captured payload names it in hook_event_name", nativeEvent)
	}

	fixturePath := filepath.Join("..", "..", "docs", "design", "fixtures", "hook-payloads", fixtureName)
	fixtureRaw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", fixturePath, err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(fixtureRaw, &want); err != nil {
		t.Fatalf("%s is not valid JSON: %v", fixturePath, err)
	}

	if diff := liveKeySetDiff(got, want); diff != "" {
		t.Fatalf("%s payload key set does not match %s: %s", nativeEvent, fixturePath, diff)
	}
}

// liveKeySetDiff reports the symmetric difference between two payloads' key
// sets, or "" when they match exactly.
func liveKeySetDiff(got, want map[string]json.RawMessage) string {
	var missing, extra []string
	for k := range want {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Sprintf("missing %v, extra %v", missing, extra)
}

// livePiBuildPluginPackage runs `hookyard build --engine pi` into its own
// scratch plugin root, then replaces the bundled binary launcher.sh execs
// with a stand-in that records its argv to argvCapturePath — the same
// substitution liveWritePiCaptureRouter makes for yard mode's router, applied
// here to build mode's bundled binary instead of a --router-path. launcher.sh
// itself is left exactly as build wrote it, so a passing test still proves
// build's own OS/arch dispatch resolves to a real, executable file.
func livePiBuildPluginPackage(t *testing.T, hookyardBin, root string) (pluginRoot, argvCapturePath string) {
	t.Helper()
	pluginRoot = filepath.Join(root, "pi-package")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", pluginRoot, err)
	}
	writeBuildFile(t, filepath.Join(pluginRoot, "handlers", "guard.sh"), "#!/bin/sh\n", 0o755)
	manifestPath := filepath.Join(root, "pi-plugin-manifest.json")
	writeBuildFile(t, manifestPath, `{"handlers":[`+
		`{"id":"e2e-pi-plugin","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["pi"],"match":["Bash"]}]}`, 0o644)

	build := exec.Command(hookyardBin, "build",
		"--engine", "pi",
		"--manifest", manifestPath,
		"--out", pluginRoot,
		"--name", "hookyard-e2e",
	)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("hookyard build --engine pi: %v\n%s", err, out)
	}

	// The bundled binary this overwrites is the one file build's own launcher.sh
	// execs once it has picked the host's OS/arch, so replacing it is what turns
	// "the package routes" into something this test can observe without
	// spawning hookyard's real route command.
	argvCapturePath = filepath.Join(root, "plugin-router-argv.txt")
	binaryPath := filepath.Join(pluginRoot, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
	script := "#!/bin/sh\necho \"$@\" > " + argvCapturePath + "\ncat > /dev/null\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("overwrite bundled binary %s: %v", binaryPath, err)
	}
	return pluginRoot, argvCapturePath
}

// livePiRegisterExtension adds extensionPath to the scratch agent dir's
// settings.json extensions[] — the mechanism a built package's own
// extensions/hookyard.ts loads through, the same array yard mode's bridge is
// registered in, just pointed at a package instead of hookyard install
// writing it.
func livePiRegisterExtension(t *testing.T, agentDir, extensionPath string) {
	t.Helper()
	settingsPath := filepath.Join(agentDir, "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("%s is not valid JSON: %v", settingsPath, err)
	}
	settings["extensions"] = []string{extensionPath}
	livePiWriteJSON(t, settingsPath, settings)
}

// TestLivePiBuiltPackageInvokesTheRouterWithPluginRoot is build mode's
// automated proof that a package `hookyard build --engine pi` produces is not
// merely well-formed on disk but actually loads in a real pi process and
// routes through it (SPEC AC4): registered in extensions[] rather than
// installed through yard mode's settings.json + bridge pair, and its bundled
// binary invoked with --plugin-root, the one argument only a package's own
// bridge appends (pi_bridge.ts's `base` branch).
func TestLivePiBuiltPackageInvokesTheRouterWithPluginRoot(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, _ := livePiSetup(t)

	pluginRoot, argvCapturePath := livePiBuildPluginPackage(t, hookyardBin, root)
	livePiRegisterExtension(t, agentDir, filepath.Join(pluginRoot, "extensions", "hookyard.ts"))

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s", err, output)
	}

	argv, err := os.ReadFile(argvCapturePath)
	if err != nil {
		t.Fatalf("the built package's router was never invoked: %v\n--- pi output ---\n%s", err, output)
	}
	if !strings.Contains(string(argv), "--plugin-root") {
		t.Fatalf("router argv carries no --plugin-root, so the built package's bridge did not resolve its own "+
			"root: %q\n--- pi output ---\n%s", argv, output)
	}
}

// livePiAdvisoryPrompt is the prompt every test below runs. It has to match
// nothing about the scripted model — fake-llm.py's real-pi probe (see
// docs/design/fixtures/pi-pre-tool-advisory/probe.sh) used the same literal
// text, and reusing it keeps this arm's transcript comparable to the fixture
// dir's captured samples by eye.
const livePiAdvisoryPrompt = "Run the probe."

// livePiAdvisoryBudget bounds one real pi process talking to an in-process
// scripted model rather than a hosted API or a local GPU server: the model
// side is instant, so this only needs to cover pi's own startup and tool-loop
// overhead.
const livePiAdvisoryBudget = 2 * time.Minute

// liveRequirePiBinary is liveRequirePi's counterpart for the tests below: they
// script their own model server in-process (liveNewAdvisoryModelServer)
// instead of depending on a local model endpoint someone else started, so
// there is no third condition to skip on.
func liveRequirePiBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv("HOOKYARD_E2E") != "1" {
		t.Skip("set HOOKYARD_E2E=1 to run this test against a live pi binary")
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi binary not found on PATH")
	}
	return piBin
}

// liveScriptedPiLayout builds the scratch layout the tests below share, which
// is deliberately not livePiSetup: livePiSetup's liveSeedPiAgentDir seeds a
// Lemonade-pointing models.json, and these tests need one pointed at their own
// in-process scripted server instead. Building the layout by hand keeps that
// seeding out of livePiSetup, which every other pi test still depends on.
func liveScriptedPiLayout(t *testing.T) (hookyardBin, root, agentDir, projectDir, stateDir string) {
	t.Helper()
	hookyardBin = liveBuildHookyard(t)
	root = t.TempDir()
	agentDir = filepath.Join(root, "pi-agent")
	projectDir = filepath.Join(root, "project")
	stateDir = filepath.Join(root, "state")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	return hookyardBin, root, agentDir, projectDir, stateDir
}

// livePiSeedScriptedAgentDir writes a settings.json and models.json pointed at
// an in-process scripted server rather than a real model, mirroring the shape
// docs/design/fixtures/pi-pre-tool-advisory/probe.sh writes for the same
// purpose (its models.json, plus --provider/--model on pi's own argv; this
// uses settings.json's defaultProvider/defaultModel instead, since these tests
// have no reason to also vary pi's own CLI flags). The compat block disables
// three OpenAI-completions extensions the fixture dir's fake-llm.py does not
// implement — pi would otherwise ask a scripted server for behavior nothing
// here scripts.
func livePiSeedScriptedAgentDir(t *testing.T, agentDir, baseURL string) {
	t.Helper()
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", agentDir, err)
	}
	livePiWriteJSON(t, filepath.Join(agentDir, "settings.json"), map[string]string{
		"defaultProvider": "probe",
		"defaultModel":    "probe",
	})
	livePiWriteJSON(t, filepath.Join(agentDir, "models.json"), map[string]any{
		"providers": map[string]any{
			"probe": map[string]any{
				"baseUrl": baseURL,
				"api":     "openai-completions",
				"apiKey":  "probe",
				"compat": map[string]any{
					"supportsDeveloperRole":    false,
					"supportsReasoningEffort":  false,
					"supportsUsageInStreaming": false,
				},
				"models": []map[string]string{{"id": "probe"}},
			},
		},
	})
}

// advisoryMessage is one OpenAI-shaped chat message as this arm's scripted
// model server both reads (on the way in) and records (for the assertions
// below). Content is left as raw JSON rather than decoded into a string,
// because pi sends it as a plain string on some messages and as an array of
// {type, text} blocks on others (advisoryContentContains reads either).
type advisoryMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// advisoryCapture is the scripted model server's own record of every request
// pi sent it, keyed by nothing but arrival order: one entry per POST
// */chat/completions call, system-role messages already dropped (identical
// noise on every request, the same reason
// docs/design/fixtures/pi-pre-tool-advisory/fake-llm.py drops them from its
// own sample-*.jsonl). It is what "what reached the model" means below — read
// off the wire, never inferred from pi's own stdout.
type advisoryCapture struct {
	mu       sync.Mutex
	requests [][]advisoryMessage
}

func (c *advisoryCapture) record(msgs []advisoryMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, msgs)
}

func (c *advisoryCapture) snapshot() [][]advisoryMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]advisoryMessage, len(c.requests))
	copy(out, c.requests)
	return out
}

// dump renders every captured request for a t.Fatalf/t.Logf body, so a
// failure or a passing run's log both show the exact conversation pi sent
// rather than a boolean.
func (c *advisoryCapture) dump() string {
	raw, err := json.MarshalIndent(c.snapshot(), "", "  ")
	if err != nil {
		return fmt.Sprintf("(failed to marshal captured requests: %v)", err)
	}
	return string(raw)
}

// advisoryContentContains reads one message's content the two shapes pi sends
// it in — a plain string, or an array of {type, text} blocks — and reports
// whether substr appears in the text either shape carries. A content this
// cannot parse as either (null, on an assistant tool_calls message, notably)
// is not an error: it just contains nothing.
func advisoryContentContains(raw json.RawMessage, substr string) bool {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.Contains(asString, substr)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		for _, part := range parts {
			if strings.Contains(part.Text, substr) {
				return true
			}
		}
	}
	return false
}

// advisoryContentText flattens one message's content to plain text for a
// substring-position check. A live pi 0.86.1 tool-role message carries
// content as a plain string already, its blocks newline-joined by pi itself
// before this ever reaches the model (confirmed against a real process,
// TestLivePiOrdersPreAndPostToolAdvisoriesOnOneCall): "<exec output>\n[hookyard
// advisory] <post>\n[hookyard advisory] <pre>". The array-of-{type,text}-blocks
// shape is flattened the same way, joined on "\n", so a caller can look for
// ordering with strings.Index regardless of which shape a given message
// arrived in.
func advisoryContentText(raw json.RawMessage) (string, bool) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p.Text
		}
		return strings.Join(texts, "\n"), true
	}
	return "", false
}

func (c *advisoryCapture) toolMessageContainsBoth(a, b string) bool {
	for _, msgs := range c.snapshot() {
		for _, m := range msgs {
			if m.Role == "tool" && advisoryContentContains(m.Content, a) && advisoryContentContains(m.Content, b) {
				return true
			}
		}
	}
	return false
}

func (c *advisoryCapture) nonToolMessageContains(substr string) bool {
	for _, msgs := range c.snapshot() {
		for _, m := range msgs {
			if m.Role != "tool" && advisoryContentContains(m.Content, substr) {
				return true
			}
		}
	}
	return false
}

func (c *advisoryCapture) anyMessageContains(substr string) bool {
	for _, msgs := range c.snapshot() {
		for _, m := range msgs {
			if advisoryContentContains(m.Content, substr) {
				return true
			}
		}
	}
	return false
}

// writeAdvisorySSEChunk writes one OpenAI chat-completions streaming chunk,
// shaped exactly like docs/design/fixtures/pi-pre-tool-advisory/fake-llm.py's
// own chunk() helper — id/object/created/model are fixed placeholders, since
// nothing here reads them back. finish is "" for every chunk but the last one
// in a response, which is what turns finish_reason into JSON null rather than
// the empty string (an OpenAI-compatible client reads absence, not "", as
// "not finished yet").
func writeAdvisorySSEChunk(w http.ResponseWriter, delta map[string]any, finish string) {
	var finishReason any
	if finish != "" {
		finishReason = finish
	}
	chunk := map[string]any{
		"id":      "chatcmpl-PROBE",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "probe",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		panic(fmt.Sprintf("marshal SSE chunk: %v", err))
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
}

// livePiPrintfRanCommand is the shell command the scripted bash tool call
// runs by default: printf's own %s-%s differs from the command's source
// text, so a substring match on the tool's captured output can tell "the
// command ran" apart from "the command's text merely appeared in a message"
// (e.g. an assistant tool_calls message, which carries the command as a JSON
// string) — deliberately not `echo <execTok>-ran`, which would not have that
// property.
func livePiPrintfRanCommand(execTok string) string {
	return fmt.Sprintf("printf '%%s-%%s' %s ran", execTok)
}

// liveNewAdvisoryModelServer starts an in-process OpenAI-compatible chat
// server scripted by conversation state rather than by request count: pi on
// PATH may be a Nix wrapper that adds its own extensions when CREW_WORKER_ID
// is unset (§8's coexistence hazard), which changes how many requests a run
// makes. Until a tool-role message appears it answers with one bash tool call
// running command; after that, plain text, which ends pi's turn. Like
// docs/design/fixtures/pi-pre-tool-advisory/fake-llm.py, it ignores paths.
func liveNewAdvisoryModelServer(t *testing.T, command string) (*httptest.Server, *advisoryCapture) {
	t.Helper()
	capture := &advisoryCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"probe","object":"model"}]}`)
			return
		}

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read scripted model request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body struct {
			Messages []advisoryMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("scripted model request body is not valid JSON: %v\n%s", err, raw)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		hasToolMessage := false
		kept := make([]advisoryMessage, 0, len(body.Messages))
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasToolMessage = true
			}
			if m.Role == "system" {
				continue
			}
			kept = append(kept, m)
		}
		capture.record(kept)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if !hasToolMessage {
			args, err := json.Marshal(map[string]string{
				"command": command,
			})
			if err != nil {
				t.Errorf("marshal scripted bash tool arguments: %v", err)
			}
			writeAdvisorySSEChunk(w, map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0,
					"id":    "PROBE-TOOLCALL-01",
					"type":  "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": string(args),
					},
				}},
			}, "")
			writeAdvisorySSEChunk(w, map[string]any{}, "tool_calls")
		} else {
			writeAdvisorySSEChunk(w, map[string]any{"role": "assistant", "content": "done"}, "")
			writeAdvisorySSEChunk(w, map[string]any{}, "stop")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, capture
}

// livePiWriteAdvisoryHandler is liveWriteDenyHandler's generalization: that
// helper's reason is one fixed field in one fixed shape, and the tests below
// need a handler that can also print additionalContext alongside — or instead
// of — a permissionDecision. body is the handler's entire stdout, already
// shaped as hookyard's handler wire protocol (§7's hookSpecificOutput
// wrapper, the same shape internal/router/handler.go's handlerOutput decodes
// and every engine's own render reads back). name picks the script's own
// filename, distinct from any other handler sharing dir — a test installing
// two handlers at once (one on pre_tool, one on post_tool) would otherwise
// have the second call's write silently clobber the first's script.
func livePiWriteAdvisoryHandler(t *testing.T, dir, name, firedMarker, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"touch " + firedMarker + "\n" +
		"printf '%s' '" + body + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write advisory handler: %v", err)
	}
	return path
}

// TestLivePiDeliversAStandalonePreToolAdvisoryWithTheCallsResult is the
// delivery half of docs/design/hookyard.md §11.1's amendment: a handler that
// allows by abstaining and returns only additionalContext has no field in
// pi's tool_call reply to carry it on (render.renderPi's default arm), so the
// bridge stashes it and appends it to that same call's own tool_result — the
// request immediately following execution, beside the result, the same place
// Claude Code's pre_tool additionalContext lands. This proves that delivery
// through the real installed bridge (pi_bridge.ts, not a hand-written
// stand-in) against a real pi process, deterministically: the scripted model
// in liveNewAdvisoryModelServer replaces
// docs/design/fixtures/pi-pre-tool-advisory/fake-llm.py's manual, one-off
// probe with something this suite can run unattended, without Lemonade or any
// other local model.
//
// Two things distinguish "delivered" from "not delivered" here, both read off
// the wire rather than off pi's own stdout: the advisory text must appear on
// the SAME tool-role message as the executed command's own output, and it
// must never appear on any non-tool message, which would mean the bridge
// queued it as a separate delivery instead of appending it to the result.
func TestLivePiDeliversAStandalonePreToolAdvisoryWithTheCallsResult(t *testing.T) {
	piBin := liveRequirePiBinary(t)
	hookyardBin, root, agentDir, projectDir, stateDir := liveScriptedPiLayout(t)

	execTok := fmt.Sprintf("hookyard-e2e-pi-exec-%d", time.Now().UnixNano())
	adviceTok := fmt.Sprintf("hookyard-e2e-pi-advice-%d", time.Now().UnixNano())

	server, capture := liveNewAdvisoryModelServer(t, livePiPrintfRanCommand(execTok))
	livePiSeedScriptedAgentDir(t, agentDir, server.URL+"/v1")

	body := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"` + adviceTok + `"}}`
	handlerPath := livePiWriteAdvisoryHandler(t, root, "advisory-handler.sh", filepath.Join(root, "handler-fired"), body)
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, hookyardBin)

	ctx, cancel := context.WithTimeout(context.Background(), livePiAdvisoryBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", "--no-session", livePiAdvisoryPrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s\n--- recorded requests ---\n%s", err, output, capture.dump())
	}

	execOutput := execTok + "-ran"
	advisoryText := "[hookyard advisory] " + adviceTok

	if !capture.toolMessageContainsBoth(execOutput, advisoryText) {
		t.Fatalf("no tool-role message carries both the executed output %q and the advisory %q; a standalone "+
			"pre_tool advisory must reach the model appended to the call's own tool result\n"+
			"--- recorded requests ---\n%s\n--- pi output ---\n%s", execOutput, advisoryText, capture.dump(), output)
	}
	if capture.nonToolMessageContains(adviceTok) {
		t.Fatalf("advisory token %q appears on a non-tool message; it must reach the model only appended to the "+
			"call's own tool result, never as a separate delivery\n--- recorded requests ---\n%s\n--- pi output ---\n%s",
			adviceTok, capture.dump(), output)
	}

	rec, ok := liveFindBashDenyRecord(t, stateDir)
	if !ok {
		t.Fatalf("the advisory handler fired, but hookyard's own record has no Bash pre_tool entry for it — "+
			"a hookyard-side problem, not an engine one\n--- pi output ---\n%s", output)
	}
	if len(rec.Handlers) == 0 || rec.Handlers[0].Outcome != record.OutcomeAdvise ||
		rec.Handlers[0].Delivered == nil || !*rec.Handlers[0].Delivered {
		t.Fatalf("hookyard's own record does not show the advisory as delivered: %+v\n"+
			"--- recorded requests ---\n%s\n--- pi output ---\n%s", rec.Handlers, capture.dump(), output)
	}

	t.Logf("recorded requests:\n%s\n--- pi output ---\n%s", capture.dump(), output)
}

// TestLivePiDeliversAPreToolAdvisoryWhenTheToolFails is the standalone
// test's counterpart for a call that fails: pi's afterToolCall fires
// tool_result for a call that threw too — the failure just becomes an
// isError result — so the bridge's flush must not skip it. The scripted
// command still prints its own output before exiting non-zero, so the same
// exec-output-plus-advisory check applies.
func TestLivePiDeliversAPreToolAdvisoryWhenTheToolFails(t *testing.T) {
	piBin := liveRequirePiBinary(t)
	hookyardBin, root, agentDir, projectDir, stateDir := liveScriptedPiLayout(t)

	execTok := fmt.Sprintf("hookyard-e2e-pi-exec-fail-%d", time.Now().UnixNano())
	adviceTok := fmt.Sprintf("hookyard-e2e-pi-advice-fail-%d", time.Now().UnixNano())

	server, capture := liveNewAdvisoryModelServer(t, livePiPrintfRanCommand(execTok)+"; exit 3")
	livePiSeedScriptedAgentDir(t, agentDir, server.URL+"/v1")

	body := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"` + adviceTok + `"}}`
	handlerPath := livePiWriteAdvisoryHandler(t, root, "advisory-handler.sh", filepath.Join(root, "handler-fired"), body)
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, hookyardBin)

	ctx, cancel := context.WithTimeout(context.Background(), livePiAdvisoryBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", "--no-session", livePiAdvisoryPrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s\n--- recorded requests ---\n%s", err, output, capture.dump())
	}

	execOutput := execTok + "-ran"
	advisoryText := "[hookyard advisory] " + adviceTok

	if !capture.toolMessageContainsBoth(execOutput, advisoryText) {
		t.Fatalf("no tool-role message carries both the failed call's own output %q and the advisory %q — a "+
			"failed call must still get its advisory\n--- recorded requests ---\n%s\n--- pi output ---\n%s",
			execOutput, advisoryText, capture.dump(), output)
	}

	t.Logf("recorded requests:\n%s\n--- pi output ---\n%s", capture.dump(), output)
}

// TestLivePiOrdersPreAndPostToolAdvisoriesOnOneCall proves pi_bridge.ts's
// registration-order guarantee end to end: the bridge registers its
// tool_call-stash flush after the per-entry loop specifically so hookyard's
// own post_tool advisory — built from the tool's original content — lands on
// the same call's tool_result ahead of the pre_tool advisory the flush
// appends afterward — exec output first, then the post-tool advisory, then
// the pre-tool one, each exactly once.
func TestLivePiOrdersPreAndPostToolAdvisoriesOnOneCall(t *testing.T) {
	piBin := liveRequirePiBinary(t)
	hookyardBin, root, agentDir, projectDir, stateDir := liveScriptedPiLayout(t)

	execTok := fmt.Sprintf("hookyard-e2e-pi-exec-order-%d", time.Now().UnixNano())
	preTok := fmt.Sprintf("hookyard-e2e-pi-pre-%d", time.Now().UnixNano())
	postTok := fmt.Sprintf("hookyard-e2e-pi-post-%d", time.Now().UnixNano())

	server, capture := liveNewAdvisoryModelServer(t, livePiPrintfRanCommand(execTok))
	livePiSeedScriptedAgentDir(t, agentDir, server.URL+"/v1")

	preBody := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"` + preTok + `"}}`
	postBody := `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"` + postTok + `"}}`
	preHandlerPath := livePiWriteAdvisoryHandler(t, root, "pre-handler.sh", filepath.Join(root, "pre-handler-fired"), preBody)
	postHandlerPath := livePiWriteAdvisoryHandler(t, root, "post-handler.sh", filepath.Join(root, "post-handler-fired"), postBody)
	manifestPath := livePiWritePreAndPostManifest(t, root, preHandlerPath, postHandlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, hookyardBin)

	ctx, cancel := context.WithTimeout(context.Background(), livePiAdvisoryBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", "--no-session", livePiAdvisoryPrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s\n--- recorded requests ---\n%s", err, output, capture.dump())
	}

	execOutput := execTok + "-ran"
	preText := "[hookyard advisory] " + preTok
	postText := "[hookyard advisory] " + postTok

	ordered := false
	for _, msgs := range capture.snapshot() {
		for _, m := range msgs {
			if m.Role != "tool" {
				continue
			}
			text, ok := advisoryContentText(m.Content)
			if !ok {
				continue
			}
			if strings.Count(text, execOutput) != 1 || strings.Count(text, preText) != 1 ||
				strings.Count(text, postText) != 1 {
				continue
			}
			execIdx := strings.Index(text, execOutput)
			preIdx := strings.Index(text, preText)
			postIdx := strings.Index(text, postText)
			if execIdx < postIdx && postIdx < preIdx {
				ordered = true
			}
		}
	}
	if !ordered {
		t.Fatalf("no tool-role message carries the exec output, then the post-tool advisory, then the "+
			"pre-tool advisory, each exactly once, in that order\n--- recorded requests ---\n%s\n--- pi output ---\n%s",
			capture.dump(), output)
	}

	t.Logf("recorded requests:\n%s\n--- pi output ---\n%s", capture.dump(), output)
}

// TestLivePiDeliversADenyAdvisoryOnlyThroughTheReason is
// TestLivePiDeliversAStandalonePreToolAdvisoryWithTheCallsResult's deny-side
// counterpart: render.renderPiDeny joins a deny's reason and advice into
// Pi's one reason field and never also appends it to the call's own tool
// result (§11.1, "delivered exactly once"), so this proves that against a
// real pi process the advisory rides only the block reason — reaching the
// model as the tool's own result, the same request the call itself was made
// in, per the fixture dir's ext-block.ts probe — and never as a second
// message appended to the call's own tool result.
func TestLivePiDeliversADenyAdvisoryOnlyThroughTheReason(t *testing.T) {
	piBin := liveRequirePiBinary(t)
	hookyardBin, root, agentDir, projectDir, stateDir := liveScriptedPiLayout(t)

	execTok := fmt.Sprintf("hookyard-e2e-pi-exec-deny-%d", time.Now().UnixNano())
	reasonTok := fmt.Sprintf("hookyard-e2e-pi-reason-%d", time.Now().UnixNano())
	adviceTok := fmt.Sprintf("hookyard-e2e-pi-advice-deny-%d", time.Now().UnixNano())

	server, capture := liveNewAdvisoryModelServer(t, livePiPrintfRanCommand(execTok))
	livePiSeedScriptedAgentDir(t, agentDir, server.URL+"/v1")

	body := `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"` + reasonTok +
		`","additionalContext":"` + adviceTok + `"}}`
	handlerPath := livePiWriteAdvisoryHandler(t, root, "advisory-handler.sh", filepath.Join(root, "handler-fired"), body)
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	livePiInstall(t, hookyardBin, root, []string{agentDir}, stateDir, manifestPath, hookyardBin)

	ctx, cancel := context.WithTimeout(context.Background(), livePiAdvisoryBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", "--no-session", livePiAdvisoryPrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s\n--- recorded requests ---\n%s", err, output, capture.dump())
	}

	execOutput := execTok + "-ran"

	if !capture.toolMessageContainsBoth(reasonTok, adviceTok) {
		t.Fatalf("no tool-role message carries both the deny reason %q and the advisory %q; a deny's advice "+
			"must ride the block reason\n--- recorded requests ---\n%s\n--- pi output ---\n%s",
			reasonTok, adviceTok, capture.dump(), output)
	}
	if capture.anyMessageContains(execOutput) {
		t.Fatalf("the denied tool call ran anyway: %q appears in a recorded message\n"+
			"--- recorded requests ---\n%s\n--- pi output ---\n%s", execOutput, capture.dump(), output)
	}
	if capture.nonToolMessageContains(adviceTok) {
		t.Fatalf("advisory %q also appears on a non-tool message; on a deny it must be delivered exactly once, "+
			"folded into the block reason, never separately appended to a tool result\n"+
			"--- recorded requests ---\n%s\n--- pi output ---\n%s",
			adviceTok, capture.dump(), output)
	}

	t.Logf("recorded requests:\n%s\n--- pi output ---\n%s", capture.dump(), output)
}
