package render

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The bridge is the only thing hookyard installs that is executable code rather
// than configuration, and the only other gate this repo runs over it is
// `node --check` (nix/checks/pi_bridge.nix), which proves it parses. Everything
// it actually promises is behaviour: that every router failure resolves to
// allow, that only tool_call can block, that a matcher filters before anything
// is spawned. The live end-to-end arm reaches that too, but needs both pi and a
// model server. These tests splice real data into the embedded template and
// drive the result under node against stubbed routers.

// piBridgeHarness stands in for pi: it collects what the bridge registers, fires
// one event through the matching handler, and prints the return value as JSON.
// `null` is the allow the bridge owes on every router failure; anything else is
// a block. The event arrives in a file rather than in argv because one case
// carries megabytes, which is the whole point of it.
const piBridgeHarness = `import { readFile } from "node:fs/promises";
import bridge from "./bridge.mjs";

const registered = [];
bridge({ on: (event, handler) => registered.push({ event, handler }) });

const [name, eventPath] = process.argv.slice(2);
const entry = registered.find((r) => r.event === name);
if (!entry) throw new Error("the bridge registered nothing for " + name);

const event = JSON.parse(await readFile(eventPath, "utf8"));
const ctx = { cwd: "/probe/cwd", sessionManager: { getSessionId: () => "probe-session" } };
process.stdout.write(JSON.stringify((await entry.handler(event, ctx)) ?? null));
`

type piBridgeRun struct {
	dir  string
	node string
}

// newPiBridgeRun resolves node and lays out one scratch install. node is a test
// dependency and not a build one, so its absence skips rather than fails.
func newPiBridgeRun(t *testing.T) *piBridgeRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so the bridge cannot be driven here")
	}
	dir := t.TempDir()
	// The bridge splits its command on single spaces, which checkShellSafe makes
	// exact in a real install. t.TempDir keeps spaces in a subtest name, so a
	// name like "missing binary" would hand the bridge a mangled argv and fail
	// this for a reason that has nothing to do with the bridge.
	if strings.ContainsAny(dir, " \t") {
		t.Fatalf("temp dir %q contains whitespace; rename the subtest so the router path stays one argv word", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness.mjs"), []byte(piBridgeHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	return &piBridgeRun{dir: dir, node: node}
}

// router writes a stub router. The shebang names the node we already resolved
// rather than a shell, so these tests need nothing on PATH that they have not
// already found.
func (r *piBridgeRun) router(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(r.dir, name+".cjs")
	if err := os.WriteFile(path, []byte("#!"+r.node+"\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// install splices entries into the embedded template exactly as writePiBridge
// does, and lands the result under a name node will load: the template is plain
// ESM JavaScript under a .ts name, and pi reaches it through jiti while node
// here has no loader for that extension. Only the name differs; the bytes are
// the ones that ship.
func (r *piBridgeRun) install(t *testing.T, timeoutMS int, entries ...Entry) {
	t.Helper()
	data := piBridgeData{TimeoutMS: timeoutMS, PiVersion: "0.85.1"}
	for _, e := range entries {
		data.Entries = append(data.Entries, piBridgeEntry(e))
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(piBridgeTemplate, piBridgeSplice, string(encoded), 1)
	if err := os.WriteFile(filepath.Join(r.dir, "bridge.mjs"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fire runs one event through the bridge and returns the handler's verdict as
// JSON. A non-zero exit is a failure of the contract, not of the test harness:
// the handler is documented to resolve whatever the router does, so node coming
// back with a stack trace is the bridge taking pi's process down with it.
func (r *piBridgeRun) fire(t *testing.T, event string, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(r.dir, "event.json")
	if err := os.WriteFile(eventPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.node, "harness.mjs", event, eventPath)
	cmd.Dir = r.dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("driving the bridge failed: %v\nstderr:\n%s", err, stderr.String())
	}
	return string(stdout)
}

func piToolCall(name string, input any) map[string]any {
	return map[string]any{"toolName": name, "input": input, "toolCallId": "call-1"}
}

func assertPiAllows(t *testing.T, verdict string) {
	t.Helper()
	if verdict != "null" {
		t.Errorf("verdict = %s, want null: the bridge owes an allow here", verdict)
	}
}

func TestPiBridgeBlocksOnAWellFormedDeny(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "deny", `process.stdout.write('{"block":true,"reason":"denied by the stub router"}');`)
	run.install(t, EmittedTimeoutSeconds*1000, Entry{Event: "tool_call", Command: router})

	var verdict struct {
		Block  bool   `json:"block"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(run.fire(t, "tool_call", piToolCall("bash", map[string]any{"command": "rm -rf /"}))), &verdict); err != nil {
		t.Fatal(err)
	}
	if !verdict.Block {
		t.Error("a well-formed deny did not block")
	}
	if verdict.Reason != "denied by the stub router" {
		t.Errorf("reason = %q, want the router's own", verdict.Reason)
	}
}

// The router exits before draining a payload far larger than the OS pipe
// buffer, which breaks the pipe mid-write. That EPIPE surfaces as an 'error'
// event on the stdin stream — outside the promise, outside the handler's
// try/catch — so without a listener node treats it as unhandled and kills the
// process, taking every later hook in the session with it. Fail-open has to hold
// here too, and this is the one case where failing to hold it is worse than
// blocking.
func TestPiBridgeSurvivesARouterThatClosesThePipeEarly(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "exit-at-once", `process.exit(0);`)
	run.install(t, EmittedTimeoutSeconds*1000, Entry{Event: "tool_call", Command: router})

	huge := piToolCall("bash", map[string]any{"command": strings.Repeat("a", 5<<20)})
	assertPiAllows(t, run.fire(t, "tool_call", huge))
}

// §5 spends every router failure on allow. Each of these is a different way for
// the router to be useless, and none of them may become a block.
func TestPiBridgeFailsOpenOnEveryRouterFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutMS int
		command   func(t *testing.T, run *piBridgeRun) string
	}{
		{
			name: "missing-binary",
			command: func(_ *testing.T, run *piBridgeRun) string {
				return filepath.Join(run.dir, "router-that-was-never-installed")
			},
		},
		{
			name: "non-executable-file",
			command: func(t *testing.T, run *piBridgeRun) string {
				path := filepath.Join(run.dir, "not-executable")
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			// The deny on stdout is the point: a non-zero exit discards it
			// rather than acting on it.
			name: "non-zero-exit",
			command: func(t *testing.T, run *piBridgeRun) string {
				return run.router(t, "angry", `process.stdout.write('{"block":true}');`+"\n"+`process.exit(3);`)
			},
		},
		{
			name: "unparsable-stdout",
			command: func(t *testing.T, run *piBridgeRun) string {
				return run.router(t, "garbage", `process.stdout.write("not json at all");`)
			},
		},
		{
			// How a router abstains, and the commonest reply of all.
			name: "empty-stdout",
			command: func(t *testing.T, run *piBridgeRun) string {
				return run.router(t, "silent", `process.exitCode = 0;`)
			},
		},
		{
			name: "reply-carrying-no-decision",
			command: func(t *testing.T, run *piBridgeRun) string {
				return run.router(t, "noncommittal", `process.stdout.write('{"reason":"just a note"}');`)
			},
		},
		{
			// Pi imposes no timeout of its own, so the bridge's clock is the
			// only thing between a wedged router and a turn that never ends.
			name:      "timeout",
			timeoutMS: 300,
			command: func(t *testing.T, run *piBridgeRun) string {
				return run.router(t, "wedged", `setInterval(() => {}, 1000);`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			timeout := tc.timeoutMS
			if timeout == 0 {
				timeout = EmittedTimeoutSeconds * 1000
			}
			run.install(t, timeout, Entry{Event: "tool_call", Command: tc.command(t, run)})
			assertPiAllows(t, run.fire(t, "tool_call", piToolCall("bash", map[string]any{"command": "echo hi"})))
		})
	}
}

// Filtering in the bridge, before the spawn, is what keeps a single-tool handler
// free on every other tool call — a property no config file records, because pi
// has no matcher of its own. The sentinel proves the absence: the matching call
// must leave it behind, or the non-matching call's missing sentinel would prove
// nothing.
func TestPiBridgeMatcherFiltersBeforeSpawning(t *testing.T) {
	run := newPiBridgeRun(t)
	sentinel := filepath.Join(run.dir, "router-ran")
	router := run.router(t, "recording",
		`require("node:fs").writeFileSync(`+strconv.Quote(sentinel)+`, "");`+"\n"+
			`process.stdout.write('{"block":true,"reason":"matched"}');`)
	run.install(t, EmittedTimeoutSeconds*1000, Entry{Event: "tool_call", Matcher: "bash", Command: router})

	assertPiAllows(t, run.fire(t, "tool_call", piToolCall("edit", map[string]any{"path": "/etc/hosts"})))
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("the router ran for a tool the matcher excludes (stat err = %v)", err)
	}

	if verdict := run.fire(t, "tool_call", piToolCall("bash", nil)); !strings.Contains(verdict, `"block":true`) {
		t.Errorf("verdict = %s, want a block for the matched tool", verdict)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("the matched call never reached the router: %v", err)
	}
}

// tool_call is the only event pi gives a return channel that can block; a reply
// to any other event is recorded and ignored. A bridge that honoured them all
// would block on a tool_result, which pi turns into a refusal of a call that has
// already run.
func TestPiBridgeBlocksOnlyOnToolCall(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "deny-everything", `process.stdout.write('{"block":true,"reason":"always"}');`)
	run.install(t, EmittedTimeoutSeconds*1000,
		Entry{Event: "tool_call", Command: router},
		Entry{Event: "tool_result", Command: router},
	)

	if verdict := run.fire(t, "tool_call", piToolCall("bash", nil)); !strings.Contains(verdict, `"block":true`) {
		t.Fatalf("verdict = %s, want the same router to block a tool_call", verdict)
	}
	assertPiAllows(t, run.fire(t, "tool_result", map[string]any{
		"toolName": "bash", "input": nil, "toolCallId": "call-1", "content": "out", "isError": false,
	}))
}
