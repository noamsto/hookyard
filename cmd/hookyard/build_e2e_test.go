package main

// This file exercises the build pipeline through the real, built artifact:
// compile hookyard, run `hookyard build` against a plugin root whose path
// contains a space, then fire its generated hooks/hooks.json commands exactly
// the way Claude Code would — through `sh -c`, on the bundled launcher,
// through the bundled binary — with the same fixtures route_e2e_test.go
// drives against yard mode.

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

// e2eDenyToolUseID is the DENY fixture's own tool_use_id
// (docs/design/fixtures/hook-payloads/claude-PreToolUse-DENY.json). The deny
// handler below greps stdin for it verbatim rather than parsing JSON, so a
// fixture that merely shares the same tool_name still proves the handler ran
// and chose not to deny, not that it never ran at all.
const e2eDenyToolUseID = "toolu_018uKtWoU2752c5uZHS47Q3e"

// e2eDenyHandlerScript denies only when stdin contains e2eDenyToolUseID.
var e2eDenyHandlerScript = "#!/bin/sh\ninput=\"$(cat)\"\ncase \"$input\" in\n*" +
	e2eDenyToolUseID +
	"*)\n\tprintf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"build-e2e deny\"}}'\n" +
	"\t;;\n*)\n\texit 0\n\t;;\nesac\n"

// e2eDecoyDenyHandlerScript lives under the "agent cwd" a built command runs
// with, never under the plugin root. If a regression ever made the launcher
// or route resolve an exec against cmd.Dir instead of CLAUDE_PLUGIN_ROOT, this
// is what would run instead — an allow carrying a reason that can never match
// what the real deny handler prints.
const e2eDecoyDenyHandlerScript = "#!/bin/sh\n" +
	"printf '%s' '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\",\"permissionDecisionReason\":\"decoy\"}}'\n"

// claudeHooksDoc is the shape ClaudeSettings writes (internal/render/claude.go),
// read back just far enough to find the command for one (event, tool).
type claudeHooksDoc struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// commandFor finds the command hookyard build registered for event on
// toolName: the matcher group whose regex matches, or the unmatchered
// (every-tool) group.
func commandFor(t *testing.T, doc []byte, event, toolName string) string {
	t.Helper()
	var parsed claudeHooksDoc
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	for _, group := range parsed.Hooks[event] {
		if group.Matcher == "" {
			return group.Hooks[0].Command
		}
		if regexp.MustCompile("^(" + group.Matcher + ")$").MatchString(toolName) {
			return group.Hooks[0].Command
		}
	}
	t.Fatalf("no %s hook group in %s matches tool %q", event, doc, toolName)
	return ""
}

// setupE2EPluginSources lays out a plugin root whose path contains a space —
// the same trap an install path with a space in it would spring on a
// double-quote omitted anywhere in the emitted command — with a deny handler
// and a post handler, plus a manifest naming both plugin-root-relative. The
// manifest lives outside root, matching how an author invokes build against
// their own tree.
func setupE2EPluginSources(t *testing.T) (out, manifestPath string) {
	t.Helper()
	out = filepath.Join(t.TempDir(), "plugin root")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir plugin root: %v", err)
	}
	writeBuildFile(t, filepath.Join(out, "handlers", "deny"), e2eDenyHandlerScript, 0o755)
	writeBuildFile(t, filepath.Join(out, "handlers", "post"), "#!/bin/sh\nexit 0\n", 0o755)

	manifestPath = filepath.Join(t.TempDir(), "hookyard.json")
	writeBuildFile(t, manifestPath, `{"handlers":[`+
		`{"id":"deny","exec":"handlers/deny","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]},`+
		`{"id":"post","exec":"handlers/post","events":["post_tool"],"engines":["claude-code"]}]}`, 0o644)
	return out, manifestPath
}

func runHookyardBuild(t *testing.T, bin, manifestPath, out string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "build",
		"--engine", "claude-code",
		"--manifest", manifestPath,
		"--out", out,
		"--name", "e2e-probe")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hookyard build: %v\n%s", err, output)
	}
}

func bundledBinaryPath(out string) string {
	return filepath.Join(out, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
}

// assertStaticBinary asserts the copied binary carries no ELF PT_INTERP
// program header: a future cgo dependency shipping a store-linked binary
// would fail on any end user's machine that lacks that interpreter.
func assertStaticBinary(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("PT_INTERP is a linux ELF concept")
	}
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("open bundled binary %s as ELF: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Errorf("bundled binary %s has a PT_INTERP program header, so it is not static", path)
		}
	}
}

// runBuiltCommand runs one hooks.json command the way Claude Code would: a
// shell, a fixed three-variable environment, and a separate working
// directory holding a decoy handler that must never be reached.
func runBuiltCommand(t *testing.T, command, agentCwd, pluginRoot, stateDir, stdin string) (stdout string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = agentCwd
	cmd.Env = []string{
		"CLAUDE_PLUGIN_ROOT=" + pluginRoot,
		"HOOKYARD_STATE_DIR=" + stateDir,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if err == nil {
		return stdoutBuf.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run built command %q: %v (stderr: %s)", command, err, stderrBuf.String())
	}
	return stdoutBuf.String(), exitErr.ExitCode()
}

// assertDenyJSON asserts stdout is Claude Code's deny shape carrying reason.
func assertDenyJSON(t *testing.T, stdout, reason string) {
	t.Helper()
	var parsed struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("parse deny stdout %q: %v", stdout, err)
	}
	if parsed.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("permissionDecision = %q, want deny", parsed.HookSpecificOutput.PermissionDecision)
	}
	if parsed.HookSpecificOutput.PermissionDecisionReason != reason {
		t.Errorf("permissionDecisionReason = %q, want %q", parsed.HookSpecificOutput.PermissionDecisionReason, reason)
	}
}

// readStreamRecords reads every record a built command run appended, via the
// same StreamPath the writer used, taken after the runs complete.
func readStreamRecords(t *testing.T, stateDir string) []record.Record {
	t.Helper()
	raw, err := os.ReadFile(record.StreamPath(stateDir, time.Now()))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	recs := make([]record.Record, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var rec record.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestBuildE2EThroughBuiltHooks(t *testing.T) {
	bin := buildRouteBinary(t)
	out, manifestPath := setupE2EPluginSources(t)
	runHookyardBuild(t, bin, manifestPath, out)

	t.Run("bundled binary is static", func(t *testing.T) {
		assertStaticBinary(t, bundledBinaryPath(out))
	})

	doc, err := os.ReadFile(filepath.Join(out, "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	preCmd := commandFor(t, doc, "PreToolUse", "Bash")
	postCmd := commandFor(t, doc, "PostToolUse", "Bash")

	agentCwd := t.TempDir()
	writeBuildFile(t, filepath.Join(agentCwd, "handlers", "deny"), e2eDecoyDenyHandlerScript, 0o755)

	// runFixtures drives all three claude-code fixtures against stateDir,
	// asserting each fixture's own shape regardless of whether the record
	// lands (that assertion belongs to each caller, since it differs between
	// an absent and a pre-existing state dir).
	runFixtures := func(t *testing.T, stateDir string) {
		t.Helper()
		denyOut, code := runBuiltCommand(t, preCmd, agentCwd, out, stateDir, readFixture(t, "claude-PreToolUse-DENY.json"))
		if code != 0 {
			t.Fatalf("DENY fixture: want exit 0, got %d", code)
		}
		assertDenyJSON(t, denyOut, "build-e2e deny")

		allowOut, code := runBuiltCommand(t, preCmd, agentCwd, out, stateDir, readFixture(t, "claude-PreToolUse.json"))
		if code != 0 {
			t.Fatalf("non-deny PreToolUse fixture: want exit 0, got %d", code)
		}
		if allowOut != "" {
			t.Errorf("non-deny PreToolUse fixture: want nothing printed, got %q", allowOut)
		}

		postOut, code := runBuiltCommand(t, postCmd, agentCwd, out, stateDir, readFixture(t, "claude-PostToolUse.json"))
		if code != 0 {
			t.Fatalf("PostToolUse fixture: want exit 0, got %d", code)
		}
		if strings.Contains(postOut, "deny") {
			t.Errorf("PostToolUse fixture: want no deny in output, got %q", postOut)
		}
	}

	t.Run("nonexistent state dir stays absent", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		runFixtures(t, stateDir)
		if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
			t.Errorf("want %s to stay absent (build mode never creates a state dir), stat err = %v", stateDir, err)
		}
	})

	t.Run("existing state dir receives records", func(t *testing.T) {
		stateDir := t.TempDir()
		runFixtures(t, stateDir)

		recs := readStreamRecords(t, stateDir)
		if len(recs) != 3 {
			t.Fatalf("want 3 records, got %d: %+v", len(recs), recs)
		}
		denies := 0
		for _, rec := range recs {
			if rec.Verdict == record.OutcomeDeny {
				denies++
				if !rec.Enforced {
					t.Errorf("want the deny record enforced, got %+v", rec)
				}
			}
		}
		if denies != 1 {
			t.Errorf("want exactly 1 record with verdict %q, got %d among %+v", record.OutcomeDeny, denies, recs)
		}
	})

	t.Run("idempotent rebuild keeps a foreign hook", func(t *testing.T) {
		hooksPath := filepath.Join(out, "hooks", "hooks.json")
		addForeignPreToolUseHook(t, hooksPath)

		runHookyardBuild(t, bin, manifestPath, out)
		first, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatalf("read hooks.json after rebuild: %v", err)
		}
		assertForeignHookKeptOnce(t, first)

		runHookyardBuild(t, bin, manifestPath, out)
		second, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatalf("read hooks.json after second rebuild: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("want a byte-identical rebuild once the foreign hook is already present\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// addForeignPreToolUseHook injects a hook group whose command never contains
// render.Marker ("/bin/hookyard"), the same rule a hand-written author hook
// relies on to survive a rebuild's strip. "/usr/bin/true" here is only ever
// data inside hooks.json — this test never execs it.
func addForeignPreToolUseHook(t *testing.T, hooksPath string) {
	t.Helper()
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read %s: %v", hooksPath, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse %s: %v", hooksPath, err)
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(root["hooks"], &hooks); err != nil {
		t.Fatalf("parse %s hooks key: %v", hooksPath, err)
	}
	var groups []json.RawMessage
	if err := json.Unmarshal(hooks["PreToolUse"], &groups); err != nil {
		t.Fatalf("parse %s PreToolUse groups: %v", hooksPath, err)
	}
	groups = append(groups, json.RawMessage(`{"hooks":[{"type":"command","command":"/usr/bin/true"}]}`))

	newGroups, err := json.Marshal(groups)
	if err != nil {
		t.Fatal(err)
	}
	hooks["PreToolUse"] = newGroups
	newHooks, err := json.Marshal(hooks)
	if err != nil {
		t.Fatal(err)
	}
	root["hooks"] = newHooks
	newRoot, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, newRoot, 0o644); err != nil {
		t.Fatalf("write %s: %v", hooksPath, err)
	}
}

func assertForeignHookKeptOnce(t *testing.T, doc []byte) {
	t.Helper()
	var parsed claudeHooksDoc
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	foreign, hookyard := 0, 0
	for _, group := range parsed.Hooks["PreToolUse"] {
		for _, h := range group.Hooks {
			switch {
			case h.Command == "/usr/bin/true":
				foreign++
			case strings.Contains(h.Command, "/bin/hookyard"):
				hookyard++
			}
		}
	}
	if foreign != 1 {
		t.Errorf("want the foreign hook kept exactly once, found %d", foreign)
	}
	if hookyard != 1 {
		t.Errorf("want hookyard's own group exactly once, found %d", hookyard)
	}
}

// TestBuildE2EMissingHandlerStillDenies builds its own plugin root — never
// rebuilt — with a second pre_tool handler whose exec is removed after the
// build succeeds. A missing exec must fail only that one handler: the deny
// handler beside it still has to win the lattice, and the missing one has to
// show up in the record as its own error.
func TestBuildE2EMissingHandlerStillDenies(t *testing.T) {
	bin := buildRouteBinary(t)
	out := filepath.Join(t.TempDir(), "plugin root")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir plugin root: %v", err)
	}
	writeBuildFile(t, filepath.Join(out, "handlers", "deny"), e2eDenyHandlerScript, 0o755)
	goneScript := filepath.Join(out, "handlers", "gone")
	writeBuildFile(t, goneScript, "#!/bin/sh\nexit 0\n", 0o755)

	manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
	writeBuildFile(t, manifestPath, `{"handlers":[`+
		`{"id":"deny","exec":"handlers/deny","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]},`+
		`{"id":"gone","exec":"handlers/gone","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]}]}`, 0o644)

	runHookyardBuild(t, bin, manifestPath, out)

	if err := os.Remove(goneScript); err != nil {
		t.Fatalf("remove %s: %v", goneScript, err)
	}

	doc, err := os.ReadFile(filepath.Join(out, "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	preCmd := commandFor(t, doc, "PreToolUse", "Bash")

	agentCwd := t.TempDir()
	writeBuildFile(t, filepath.Join(agentCwd, "handlers", "deny"), e2eDecoyDenyHandlerScript, 0o755)
	stateDir := t.TempDir()

	stdout, code := runBuiltCommand(t, preCmd, agentCwd, out, stateDir, readFixture(t, "claude-PreToolUse-DENY.json"))
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	assertDenyJSON(t, stdout, "build-e2e deny")

	recs := readStreamRecords(t, stateDir)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Verdict != record.OutcomeDeny || !rec.Enforced {
		t.Errorf("want an enforced deny despite the missing handler, got %q enforced=%v", rec.Verdict, rec.Enforced)
	}

	var denyHandler, goneHandler *record.RecordHandler
	for i := range rec.Handlers {
		switch rec.Handlers[i].Name {
		case "deny":
			denyHandler = &rec.Handlers[i]
		case "gone":
			goneHandler = &rec.Handlers[i]
		}
	}
	if denyHandler == nil || denyHandler.Outcome != record.OutcomeDeny {
		t.Errorf("want handler %q outcome %q, got %+v", "deny", record.OutcomeDeny, denyHandler)
	}
	if goneHandler == nil || goneHandler.Outcome != record.OutcomeError {
		t.Errorf("want handler %q outcome %q, got %+v", "gone", record.OutcomeError, goneHandler)
	}
}
