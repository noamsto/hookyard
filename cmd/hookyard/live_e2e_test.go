package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// payloads from all four engines. It is reused verbatim here because it is
// already known to reliably produce one Bash tool call, not because the
// wording matters on its own.
const liveProbePrompt = "Run the shell command: echo hookyard-probe"

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
	// flag no longer exists (R2) — emit below is the only path that produces
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
	overlayPath := liveEmitClaudeOverlay(t, hookyardBin, root, manifestPath, hookyardBin, stateDir, basePath)

	// claudeConfigDir/settings.json is never written: emit's whole point is
	// that Claude Code's overlay comes from --settings rather than from
	// anything hookyard puts under CLAUDE_CONFIG_DIR (A2), so "carries no
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

	if !strings.Contains(string(output), reasonToken) {
		t.Fatalf("hookyard recorded an enforced deny, but claude's own output never surfaced the deny "+
			"reason (%s): the shell call may have run anyway despite the recorded deny\n"+
			"--- claude output ---\n%s", reasonToken, output)
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
func liveEmitClaudeOverlay(t *testing.T, hookyardBin, root, manifestPath, routerPath, stateDir, basePath string) string {
	t.Helper()
	emit := exec.Command(hookyardBin, "emit",
		"--engine", "claude-code",
		"--manifest", manifestPath,
		"--router-path", routerPath,
		"--state-dir", stateDir,
		"--base", basePath,
	)
	out, err := emit.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
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

// livePiInstall runs `hookyard install` with routerPath as the one path each
// test varies, and every other destination pointed at scratch files under
// root so no real engine config is ever touched.
func livePiInstall(t *testing.T, hookyardBin, root, agentDir, stateDir, manifestPath, routerPath string) {
	t.Helper()
	install := exec.Command(hookyardBin, "install",
		"--manifest", manifestPath,
		"--router-path", routerPath,
		"--state-dir", stateDir,
		"--pi-settings", filepath.Join(agentDir, "settings.json"),
		"--codex-config", filepath.Join(root, "unused-codex", "config.toml"),
		"--cursor-hooks", filepath.Join(root, "unused-cursor", "hooks.json"),
	)
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
	livePiInstall(t, hookyardBin, root, agentDir, stateDir, manifestPath, hookyardBin)

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
	livePiInstall(t, hookyardBin, root, agentDir, stateDir, manifestPath, missingRouter)

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
// can compare what the bridge actually sends against the committed fixture
// without going through hookyard's own decode/render round trip, which would
// hide a drift between the two. Its path still has to satisfy
// render.BuildPlan's Marker check, the same requirement any real router path
// meets.
func liveWritePiCaptureRouter(t *testing.T, root, capturePath string) string {
	t.Helper()
	path := filepath.Join(root, "capture-router", "bin", "hookyard")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	script := "#!/bin/sh\ncat > " + capturePath + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write capture router: %v", err)
	}
	return path
}

// TestLivePiPayloadMatchesTheCommittedFixtureShape is the automated binding
// between the generated bridge (internal/render/pi_bridge.ts's tool_call
// entry in the `extras` table) and the committed fixtures: without it the
// generator can drift from docs/design/fixtures/hook-payloads/pi-tool_call.json
// with go build, go vet, go test and the nix syntax gate all green, since none
// of those runs the bridge against a real pi process.
func TestLivePiPayloadMatchesTheCommittedFixtureShape(t *testing.T) {
	piBin := liveRequirePi(t)
	hookyardBin, root, agentDir, projectDir, stateDir := livePiSetup(t)

	handlerPath := liveWriteDenyHandler(t, root, filepath.Join(root, "handler-fired"), "unused-not-invoked")
	manifestPath := livePiWriteManifest(t, root, handlerPath)
	capturePath := filepath.Join(root, "captured-payload.json")
	captureRouter := liveWritePiCaptureRouter(t, root, capturePath)
	livePiInstall(t, hookyardBin, root, agentDir, stateDir, manifestPath, captureRouter)

	ctx, cancel := context.WithTimeout(context.Background(), livePiBudget)
	defer cancel()
	probe := exec.CommandContext(ctx, piBin, "-p", "--approve", livePiProbePrompt)
	probe.Dir = projectDir
	probe.Env = livePiEnv(agentDir)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("pi run: %v\n--- pi output ---\n%s", err, output)
	}

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the capture router was never invoked: %v\n--- pi output ---\n%s", err, output)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("captured payload is not valid JSON: %v\n%s", err, captured)
	}

	fixturePath := filepath.Join("..", "..", "docs", "design", "fixtures", "hook-payloads", "pi-tool_call.json")
	fixtureRaw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", fixturePath, err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(fixtureRaw, &want); err != nil {
		t.Fatalf("%s is not valid JSON: %v", fixturePath, err)
	}

	if diff := liveKeySetDiff(got, want); diff != "" {
		t.Fatalf("bridge payload key set does not match %s: %s\ncaptured: %s", fixturePath, diff, captured)
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
