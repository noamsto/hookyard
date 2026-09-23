package render

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// a scripted sequence of events through the matching handlers, and prints what
// it registered alongside each return value as JSON. `null` is the allow the
// bridge owes on every router failure; anything else is a block. The script
// arrives in a file rather than in argv because one case carries megabytes,
// which is the whole point of it.
//
// A sequence rather than a single event because some deliveries need two: an
// advisory queued by session_start is flushed by the before_agent_start that
// follows it, and one a tool_call stashes is flushed by that same call's
// tool_result — and both slots live in the module, so both firings of a pair
// have to happen inside one load of the bridge.
const piBridgeHarness = `import { readFile } from "node:fs/promises";
import bridge from "./bridge.mjs";

const [scriptPath, ...rest] = process.argv.slice(2);
const steps = JSON.parse(await readFile(scriptPath, "utf8"));

const registered = [];
const commands = [];
const notifications = [];
bridge({
  on: (event, handler) => registered.push({ event, handler }),
  registerCommand: (name, options) => commands.push({ name, ...options }),
});

const ctx = {
  cwd: "/probe/cwd",
  sessionManager: {
    getSessionId: () => "probe-session",
    // An empty first extra argument is the --no-session run, where pi's own
    // getSessionFile() returns undefined rather than a path.
    getSessionFile: () => rest[0] || undefined,
  },
  ui: { notify: (message, level) => notifications.push({ message, level }) },
};

// pi fires every handler registered for an event, in order, not just the
// first — the bridge itself relies on that for tool_result, where its own
// flush handler has to run ahead of a manifest entry's. tool_call's return is
// a decision, so the first block wins outright and short-circuits the rest;
// every other event carries no decision, only content a handler may patch
// (emitToolResult's own middleware rule: a returned {content} replaces
// event.content for the next handler), so the merge rule below drives all of
// them, and with a single handler — true everywhere but tool_result — it
// reduces to exactly what firing that one handler already meant.
async function runHandlers(eventName, handlers, payload, ctx) {
  if (eventName === "tool_call") {
    let lastTruthy = null;
    for (const { handler } of handlers) {
      const result = await handler(payload, ctx);
      if (result && result.block === true) return result;
      if (result != null) lastTruthy = result;
    }
    return lastTruthy;
  }
  let event = payload;
  let changed = false;
  let lastReturn = null;
  for (const { handler } of handlers) {
    const result = await handler(event, ctx);
    if (result != null) lastReturn = result;
    if (result && result.content !== undefined) {
      event = { ...event, content: result.content };
      changed = true;
    }
  }
  return changed ? { content: event.content } : lastReturn;
}

const returns = [];
for (const step of steps) {
  if (step.command !== undefined) {
    const command = commands.find((c) => c.name === step.command);
    if (!command) throw new Error("the bridge registered no command named " + step.command);
    returns.push((await command.handler(step.args, ctx)) ?? null);
    continue;
  }
  const handlers = registered.filter((r) => r.event === step.event);
  if (handlers.length === 0) throw new Error("the bridge registered nothing for " + step.event);
  returns.push((await runHandlers(step.event, handlers, step.payload, ctx)) ?? null);
}
// A failed spawn reaches its handler after the command handler has already
// resolved — that is the contract — so the notification it produces lands one
// turn of the loop later than this line would otherwise run.
await new Promise((settle) => setTimeout(settle, 0));
process.stdout.write(JSON.stringify({
  registered: registered.map((r) => r.event),
  commands: commands.map((c) => ({ name: c.name, description: c.description })),
  notifications,
  returns,
}));
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
	if err := os.WriteFile(filepath.Join(dir, "harness.mjs"), []byte(piBridgeHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	return &piBridgeRun{dir: dir, node: node}
}

// router writes a stub router — and, where a test needs one, the stub a
// command's exec is, since the bridge spawns both the same way. The shebang
// names the node we already resolved rather than a shell, so these tests need
// nothing on PATH that they have not already found.
func (r *piBridgeRun) router(t *testing.T, name, body string) string {
	t.Helper()
	return r.routerAt(t, filepath.Join(r.dir, name+".cjs"), body)
}

// routerAt writes a stub router at an exact path, which is how a test reaches a
// router path the scratch directory's own name could never produce.
func (r *piBridgeRun) routerAt(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!"+r.node+"\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// capturingRouter records the bytes the bridge piped to its stdin and then
// abstains. It is how a test reaches the thing a verdict never shows: the
// payload hookyard itself authors, which for Pi is the whole contract.
func (r *piBridgeRun) capturingRouter(t *testing.T) (router, capturePath string) {
	t.Helper()
	capturePath = filepath.Join(r.dir, "captured-payload.json")
	body := `require("node:fs").writeFileSync(` + strconv.Quote(capturePath) + `, require("node:fs").readFileSync(0));`
	return r.router(t, "capturing", body), capturePath
}

func piCapturedPayload(t *testing.T, capturePath string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the router recorded no payload: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("the captured payload is not valid JSON: %v\n%s", err, raw)
	}
	return payload
}

// piEntry builds one yard-mode registration field by field. Tests hand the
// bridge a bin and an argv directly rather than a command string, which is what
// lets one of them use a router path no command() rendering could produce.
func piEntry(event, matcher, bin string, args ...string) piBridgeEntry {
	return piBridgeEntry{Event: event, Matcher: matcher, Bin: bin, Args: append([]string{}, args...)}
}

// install lands a yard-mode bridge: DATA.root nil, no commands.
func (r *piBridgeRun) install(t *testing.T, timeoutMS int, entries ...piBridgeEntry) {
	t.Helper()
	r.installData(t, piBridgeData{
		TimeoutMS: timeoutMS,
		PiVersion: "0.85.1",
		Entries:   entries,
		Commands:  []piBridgeCommand{},
	})
}

// installData renders through the same function writePiBridge uses and lands
// the result under a name node will load: the template is plain ESM JavaScript
// under a .ts name, and pi reaches it through jiti while node here has no
// loader for that extension. Only the name differs; the bytes are the ones that
// ship.
func (r *piBridgeRun) installData(t *testing.T, data piBridgeData) {
	t.Helper()
	source, err := piBridgeSource(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "bridge.mjs"), source, 0o600); err != nil {
		t.Fatal(err)
	}
}

// piBridgeStep is one event of a drive: the name the bridge registered under
// and the event object pi would hand the handler. Command names a registered
// command to invoke instead, with Args the single argument string pi hands its
// handler — "" when the user typed the command alone.
type piBridgeStep struct {
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload"`
	Command string         `json:"command,omitempty"`
	Args    string         `json:"args"`
}

// piBridgeRegistration is one pi.registerCommand call as the harness saw it.
// The handler is left out: what the registration itself promises is the name a
// user types and the description pi lists beside it.
type piBridgeRegistration struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// piBridgeNotification is one ctx.ui.notify call. It is the whole of what a
// command handler may do about a failure, since it neither throws nor waits for
// an exit status.
type piBridgeNotification struct {
	Message string `json:"message"`
	Level   string `json:"level"`
}

// piBridgeDrive is what one load of the bridge did: the events and commands it
// registered, in order, each fired handler's return value as JSON text, and
// what it asked pi to show the user.
type piBridgeDrive struct {
	Registered    []string               `json:"registered"`
	Commands      []piBridgeRegistration `json:"commands"`
	Notifications []piBridgeNotification `json:"notifications"`
	Returns       []json.RawMessage      `json:"returns"`
}

func (d piBridgeDrive) ret(i int) string { return string(d.Returns[i]) }

// drive runs a sequence of events through one load of the bridge. A non-zero
// exit is a failure of the contract, not of the test harness: the handler is
// documented to resolve whatever the router does, so node coming back with a
// stack trace is the bridge taking pi's process down with it.
//
// argv is appended to the harness's own command line, which is the only way to
// reach process.argv.slice(2) — the bridge reads it directly, the way it does
// under pi. Its first element is what the ctx stub's getSessionFile() answers,
// "" standing in for --no-session; the rest is command line for the bridge to
// sanitise.
func (r *piBridgeRun) drive(t *testing.T, steps []piBridgeStep, argv ...string) piBridgeDrive {
	t.Helper()
	if steps == nil {
		// A nil slice marshals to null and the harness iterates what it parses.
		// A drive with no steps is how a test reads the registrations alone.
		steps = []piBridgeStep{}
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(r.dir, "script.json")
	if err := os.WriteFile(scriptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.node, append([]string{"harness.mjs", scriptPath}, argv...)...)
	cmd.Dir = r.dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("driving the bridge failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var drive piBridgeDrive
	if err := json.Unmarshal(stdout, &drive); err != nil {
		t.Fatalf("the harness printed %s, which is not a drive: %v", stdout, err)
	}
	return drive
}

// fire runs one event through the bridge and returns the handler's verdict as
// JSON.
func (r *piBridgeRun) fire(t *testing.T, event string, payload map[string]any, argv ...string) string {
	t.Helper()
	return r.drive(t, []piBridgeStep{{Event: event, Payload: payload}}, argv...).ret(0)
}

// "call-1" is the toolCallId every single-call test can share; piToolCallID
// exists only for a drive that plays two calls against each other and needs
// them to carry different ids.
func piToolCall(name string, input any) map[string]any {
	return piToolCallID("call-1", name, input)
}

func piToolCallID(toolCallID, name string, input any) map[string]any {
	return map[string]any{"toolName": name, "input": input, "toolCallId": toolCallID}
}

// content is `any` because what the bridge does with a content that is not the
// array pi documents is itself part of the contract.
func piToolResult(content any) map[string]any {
	return piToolResultID("call-1", content, false)
}

func piToolResultID(toolCallID string, content any, isError bool) map[string]any {
	return map[string]any{"toolName": "bash", "input": nil, "toolCallId": toolCallID, "content": content, "isError": isError}
}

func piTextBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
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
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

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
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

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
			run.install(t, timeout, piEntry("tool_call", "", tc.command(t, run)))
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
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "bash", router))

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
		piEntry("tool_call", "", router),
		piEntry("tool_result", "", router),
	)

	if verdict := run.fire(t, "tool_call", piToolCall("bash", nil)); !strings.Contains(verdict, `"block":true`) {
		t.Fatalf("verdict = %s, want the same router to block a tool_call", verdict)
	}
	assertPiAllows(t, run.fire(t, "tool_result", map[string]any{
		"toolName": "bash", "input": nil, "toolCallId": "call-1", "content": "out", "isError": false,
	}))
}

// The advisory path owes the same fail-open decision() owes, on every
// delivery vehicle. None of these could block — only tool_call can — but a
// bridge that read an advisory out of a reply it does not understand would put
// the router's own failure in front of the model as hookyard's advice: on
// tool_result in place of the tool's output, or stashed for a tool_call's own
// tool_result to carry later (covered on its own below).
func TestPiBridgeDeliversNothingOnEveryUnreadableAdvisory(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "unparsable-stdout", body: `process.stdout.write("not json at all");`},
		{name: "empty-stdout", body: `process.exitCode = 0;`},
		{name: "reply-carrying-no-advisory", body: `process.stdout.write('{"reason":"just a note"}');`},
		{name: "non-string-advisory", body: `process.stdout.write('{"advisory":{"text":"structured"}}');`},
		{name: "empty-advisory", body: `process.stdout.write('{"advisory":""}');`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			router := run.router(t, "advising", tc.body)
			run.install(t, EmittedTimeoutSeconds*1000,
				piEntry("session_start", "", router),
				piEntry("tool_result", "", router),
			)

			drive := run.drive(t, []piBridgeStep{
				{Event: "session_start", Payload: map[string]any{"reason": "startup"}},
				{Event: "before_agent_start", Payload: map[string]any{"prompt": "do the thing"}},
				// The content is an array, so the only thing standing between
				// this reply and an appended block is the reply itself.
				{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("the tool's own output")})},
			})

			for i, fired := range []string{"session_start", "the before_agent_start flush", "tool_result"} {
				if got := drive.ret(i); got != "null" {
					t.Errorf("%s returned %s, want null: an unreadable reply delivers no advisory", fired, got)
				}
			}
		})
	}
}

// piResultTexts decodes a tool_result handler's returned content patch into
// its text blocks, in order — the shape every advisory test on this path
// needs to assert against, whether the advisory came from tool_result's own
// router or was flushed out of a tool_call's stash.
func piResultTexts(t *testing.T, verdict string) []string {
	t.Helper()
	var patch struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(verdict), &patch); err != nil {
		t.Fatalf("tool_result returned %s, which is not a content patch: %v", verdict, err)
	}
	texts := make([]string, len(patch.Content))
	for i, block := range patch.Content {
		texts[i] = block.Text
	}
	return texts
}

// tool_call's return channel is spent whole on the allow/block decision, so a
// non-blocking advisory stashes in pendingToolAdvice and rides the SAME call's
// own tool_result instead, keyed by toolCallId: a different call's tool_result
// must never see it, and once flushed the stash is gone, so a second
// tool_result for the same call sees nothing either.
func TestPiBridgeAttachesAToolCallAdvisoryOnlyToTheSameCallsToolResult(t *testing.T) {
	run := newPiBridgeRun(t)
	const advice = "the yard has notes about this command"
	router := run.router(t, "advising", `process.stdout.write('{"advisory":`+strconv.Quote(advice)+`}');`)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	original := []any{piTextBlock("the tool's own output")}
	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCall("bash", nil)},
		{Event: "tool_result", Payload: piToolResultID("call-2", original, false)},
		{Event: "tool_result", Payload: piToolResultID("call-1", original, false)},
		{Event: "tool_result", Payload: piToolResultID("call-1", original, false)},
	})

	assertPiAllows(t, drive.ret(0))
	if got := drive.ret(1); got != "null" {
		t.Errorf("a different call's tool_result returned %s, want null: the advice belongs to call-1 alone", got)
	}
	texts := piResultTexts(t, drive.ret(2))
	if len(texts) != 2 || texts[0] != "the tool's own output" || !strings.Contains(texts[1], advice) {
		t.Fatalf("content = %v, want the original block plus the attributed advisory", texts)
	}
	if got := drive.ret(3); got != "null" {
		t.Errorf("a second tool_result for the same call returned %s, want null: delivered exactly once", got)
	}
}

// A well-formed deny already folds the router's advice into decision()'s
// reason (§11.1), so the tool_call branch returns before ever reaching
// pendingToolAdvice — a blocked call has nothing for the following
// tool_result to find, which would otherwise deliver the same advice twice.
func TestPiBridgeNeverStashesAToolCallAdvisoryOnABlock(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "block-alone", body: `process.stdout.write('{"block":true,"reason":"denied"}');`},
		{
			name: "block-with-advisory-already-folded-into-the-reason",
			body: `process.stdout.write('{"block":true,"reason":"denied","advisory":"never delivered"}');`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", run.router(t, "deny", tc.body)))

			drive := run.drive(t, []piBridgeStep{
				{Event: "tool_call", Payload: piToolCall("bash", nil)},
				{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("never produced")})},
			})

			if !strings.Contains(drive.ret(0), `"block":true`) {
				t.Fatalf("verdict = %s, want a block", drive.ret(0))
			}
			if got := drive.ret(1); got != "null" {
				t.Errorf("tool_result returned %s, want null: a blocked call never stashes advice", got)
			}
		})
	}
}

// A call aborted after tool_call but before it ever executes fires no
// tool_result, so nothing would otherwise delete its stash — turn_end sweeps
// it. Firing turn_end and then a tool_result for the same id proves the sweep
// actually ran, not merely that this id was never stashed.
func TestPiBridgeClearsStashedAdviceOnTurnEnd(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "advising", `process.stdout.write('{"advisory":"never claimed"}');`)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCall("bash", nil)},
		{Event: "turn_end", Payload: map[string]any{"turnIndex": 0}},
		{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("the tool's own output")})},
	})

	assertPiAllows(t, drive.ret(0))
	if got := drive.ret(2); got != "null" {
		t.Errorf("tool_result after turn_end returned %s, want null: turn_end must have cleared the stash", got)
	}
}

// Two tool_calls stash under their own toolCallId, and a router that answers
// per-call proves the keys never cross — even when the flushes arrive in the
// opposite order the calls were made.
func TestPiBridgeKeepsParallelCallsAdviceSeparate(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "per-call",
		`const payload = JSON.parse(require("node:fs").readFileSync(0, "utf8"));`+"\n"+
			`const advice = payload.tool_use_id === "call-A" ? "advice for A" : "advice for B";`+"\n"+
			`process.stdout.write(JSON.stringify({ advisory: advice }));`)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	original := []any{piTextBlock("output")}
	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCallID("call-A", "bash", nil)},
		{Event: "tool_call", Payload: piToolCallID("call-B", "bash", nil)},
		{Event: "tool_result", Payload: piToolResultID("call-B", original, false)},
		{Event: "tool_result", Payload: piToolResultID("call-A", original, false)},
	})

	assertPiAllows(t, drive.ret(0))
	assertPiAllows(t, drive.ret(1))
	bTexts := piResultTexts(t, drive.ret(2))
	if len(bTexts) != 2 || !strings.Contains(bTexts[1], "advice for B") {
		t.Fatalf("call-B's tool_result content = %v, want its own advice, not call-A's", bTexts)
	}
	aTexts := piResultTexts(t, drive.ret(3))
	if len(aTexts) != 2 || !strings.Contains(aTexts[1], "advice for A") {
		t.Fatalf("call-A's tool_result content = %v, want its own advice, not call-B's", aTexts)
	}
}

// The bridge-owned flush is registered after the per-entry loop specifically
// so a post_tool manifest entry's own tool_result handler sees the tool's
// original content, undisturbed by the pre_tool advisory — and the pre_tool
// advisory lands in tool_result's content only after whatever the post_tool
// advisory already added. Model-visible order: tool output, then post, then
// pre.
func TestPiBridgeOrdersItsOwnPostAndPreToolAdvisoriesOnOneCall(t *testing.T) {
	run := newPiBridgeRun(t)
	preRouter := run.router(t, "pre", `process.stdout.write('{"advisory":"pre"}');`)
	postRouter := run.router(t, "post", `process.stdout.write('{"advisory":"post"}');`)
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("tool_call", "", preRouter),
		piEntry("tool_result", "", postRouter),
	)

	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCall("bash", nil)},
		{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("the tool's own output")})},
	})

	assertPiAllows(t, drive.ret(0))
	texts := piResultTexts(t, drive.ret(1))
	if len(texts) != 3 {
		t.Fatalf("content = %v, want the original block plus exactly one post and one pre advisory", texts)
	}
	if texts[0] != "the tool's own output" {
		t.Errorf("content[0] = %q, want the tool's own output first", texts[0])
	}
	if texts[1] != "[hookyard advisory] post" || texts[2] != "[hookyard advisory] pre" {
		t.Errorf("content = %v, want the post-tool advisory immediately before the pre-tool one", texts)
	}
}

// A post_tool manifest entry's tool_result handler builds its router payload
// from event.content before the bridge-owned flush ever runs, so it must see
// the tool's own output only — never the pre_tool advisory the flush appends
// afterward.
func TestPiBridgePostToolRouterPayloadExcludesThePreToolAdvisory(t *testing.T) {
	run := newPiBridgeRun(t)
	preRouter := run.router(t, "pre", `process.stdout.write('{"advisory":"pre"}');`)
	postRouter, capturePath := run.capturingRouter(t)
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("tool_call", "", preRouter),
		piEntry("tool_result", "", postRouter),
	)

	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCall("bash", nil)},
		{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("the tool's own output")})},
	})

	assertPiAllows(t, drive.ret(0))
	texts := piResultTexts(t, drive.ret(1))
	if len(texts) != 2 || texts[0] != "the tool's own output" || texts[1] != "[hookyard advisory] pre" {
		t.Fatalf("content = %v, want the tool's own output plus the flushed pre advisory", texts)
	}

	payload := piCapturedPayload(t, capturePath)
	var response struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload["tool_response"], &response); err != nil {
		t.Fatalf("unmarshal tool_response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Text != "the tool's own output" {
		t.Fatalf("captured tool_response.content = %+v, want only the tool's own output, no hookyard advisory text", response.Content)
	}
	for _, block := range response.Content {
		if strings.Contains(block.Text, "[hookyard advisory]") {
			t.Fatalf("captured tool_response.content = %+v, must not contain the pre_tool advisory", response.Content)
		}
	}
}

// afterToolCall fires tool_result for a call that failed too — the error just
// becomes an isError result — so the advisory owed to it must reach the model
// the same way a successful call's does.
func TestPiBridgeAttachesAdviceToAnErroredToolResult(t *testing.T) {
	run := newPiBridgeRun(t)
	router := run.router(t, "advising", `process.stdout.write('{"advisory":"notes on the failure"}');`)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	drive := run.drive(t, []piBridgeStep{
		{Event: "tool_call", Payload: piToolCall("bash", nil)},
		{Event: "tool_result", Payload: piToolResultID("call-1", []any{piTextBlock("command exited 1")}, true)},
	})

	assertPiAllows(t, drive.ret(0))
	texts := piResultTexts(t, drive.ret(1))
	if len(texts) != 2 || !strings.Contains(texts[1], "notes on the failure") {
		t.Fatalf("content = %v, want the advisory appended to the errored result too", texts)
	}
}

// The same fail-open decision() owes covers the stash: an unreadable reply on
// tool_call must leave nothing for the following tool_result to find.
func TestPiBridgeStashesNothingFromAnUnreadableToolCallAdvisory(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "unparsable-stdout", body: `process.stdout.write("not json at all");`},
		{name: "empty-stdout", body: `process.exitCode = 0;`},
		{name: "empty-advisory", body: `process.stdout.write('{"advisory":""}');`},
		{name: "non-string-advisory", body: `process.stdout.write('{"advisory":5}');`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			router := run.router(t, "advising", tc.body)
			run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

			drive := run.drive(t, []piBridgeStep{
				{Event: "tool_call", Payload: piToolCall("bash", nil)},
				{Event: "tool_result", Payload: piToolResult([]any{piTextBlock("the tool's own output")})},
			})

			assertPiAllows(t, drive.ret(0))
			if got := drive.ret(1); got != "null" {
				t.Errorf("tool_result returned %s, want null: an unreadable tool_call reply stashes nothing", got)
			}
		})
	}
}

// The flush and its turn_end sweep exist only to serve a tool_call entry, so
// an install with none registers neither — the same pattern
// TestPiBridgeRegistersBeforeAgentStartOnlyForSessionStart proves for
// before_agent_start and session_start.
func TestPiBridgeRegistersItsAdvisoryFlushOnlyForToolCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry piBridgeEntry
		want  int
	}{
		{name: "with-a-tool-call-entry", entry: piEntry("tool_call", "", "router-never-spawned"), want: 1},
		{name: "without-one", entry: piEntry("session_start", "", "router-never-spawned"), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			run.install(t, EmittedTimeoutSeconds*1000, tc.entry)

			registered := run.drive(t, nil).Registered
			for _, event := range []string{"tool_result", "turn_end"} {
				got := 0
				for _, e := range registered {
					if e == event {
						got++
					}
				}
				if got != tc.want {
					t.Errorf("registered = %q, want %s registered %d time(s)", registered, event, tc.want)
				}
			}
		})
	}
}

// The advisory is appended to the tool's own output and names
// where it came from. Pi takes the returned content as the whole block list, so
// a bridge that returned its own block alone would delete the tool result the
// model is waiting on — and the probe model, shown an unattributed appended
// block, called it "exactly the shape of an injection attempt" and disregarded
// it, which costs exactly as much as never delivering the advice.
func TestPiBridgeAppendsAnAttributedAdvisoryAndKeepsTheOriginalContent(t *testing.T) {
	run := newPiBridgeRun(t)
	const advice = "the yard has notes about this command"
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("tool_result", "", run.router(t, "advising", `process.stdout.write('{"advisory":`+strconv.Quote(advice)+`}');`)))

	verdict := run.fire(t, "tool_result", piToolResult([]any{
		piTextBlock("the tool's own first block"),
		piTextBlock("and its second"),
	}))

	var patch struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(verdict), &patch); err != nil {
		t.Fatalf("tool_result returned %s, which is not a content patch: %v", verdict, err)
	}
	if len(patch.Content) != 3 {
		t.Fatalf("content = %+v, want the two original blocks plus one appended", patch.Content)
	}
	if patch.Content[0].Text != "the tool's own first block" || patch.Content[1].Text != "and its second" {
		t.Errorf("content = %+v, want the original blocks first and unchanged", patch.Content)
	}
	appended := patch.Content[2]
	if appended.Type != "text" || !strings.Contains(appended.Text, advice) {
		t.Fatalf("appended block = %+v, want a text block carrying the router's advisory", appended)
	}
	if !strings.Contains(appended.Text, "hookyard") {
		t.Errorf("appended block = %q names no source, which is the shape the probe model read as an attack", appended.Text)
	}
}

// tool_result's content is pi's shape rather than hookyard's, and a
// content that is not an array is one this bridge does not understand. It
// declines instead of returning a block list of its own, which pi would take as
// the whole patch — dropping the tool output to make room for the advice.
func TestPiBridgeAppendsNothingToAToolResultContentItCannotRead(t *testing.T) {
	run := newPiBridgeRun(t)
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("tool_result", "", run.router(t, "advising", `process.stdout.write('{"advisory":"the yard has notes"}');`)))

	if verdict := run.fire(t, "tool_result", piToolResult("a plain string, not a block list")); verdict != "null" {
		t.Errorf("verdict = %s, want null: the bridge rewrote a content shape it cannot read", verdict)
	}
}

// session_start's own return value reaches nothing, so the advisory it collects
// rides the before_agent_start that follows — the channel the probe proved
// reaches the model. That fires once per prompt rather than once per session,
// so the slot has to clear on the flush: otherwise every later prompt of the
// session would carry the same advisory again.
func TestPiBridgeDeliversASessionStartAdvisoryOnceThroughBeforeAgentStart(t *testing.T) {
	run := newPiBridgeRun(t)
	const advice = "the yard has notes"
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("session_start", "", run.router(t, "advising", `process.stdout.write('{"advisory":`+strconv.Quote(advice)+`}');`)))

	drive := run.drive(t, []piBridgeStep{
		{Event: "session_start", Payload: map[string]any{"reason": "startup"}},
		{Event: "before_agent_start", Payload: map[string]any{"prompt": "first prompt"}},
		{Event: "before_agent_start", Payload: map[string]any{"prompt": "second prompt"}},
	})

	if got := drive.ret(0); got != "null" {
		t.Errorf("session_start returned %s, want null: its advisory rides before_agent_start", got)
	}
	var injected struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(drive.ret(1)), &injected); err != nil {
		t.Fatalf("before_agent_start returned %s, which is not an injected message: %v", drive.ret(1), err)
	}
	// Compared as raw JSON because an absent key and a false one are different
	// answers here: display:false is what keeps the advisory out of the UI while
	// pi still stores it and sends it to the model.
	for key, want := range map[string]string{
		"customType": `"hookyard"`,
		// Attributed in the text, exactly as the tool_result path attributes
		// it: customType is metadata pi may never put in front of the model,
		// and an unattributed block is the shape the probe model read as an
		// injection attempt.
		"content": strconv.Quote("[hookyard advisory] " + advice),
		"display": "false",
	} {
		if got := string(injected.Message[key]); got != want {
			t.Errorf("message.%s = %s, want %s", key, got, want)
		}
	}
	if got := drive.ret(2); got != "null" {
		t.Errorf("the second prompt's before_agent_start returned %s, want null: the slot is cleared on flush", got)
	}
}

// A prompt that follows no queued advisory injects nothing — the same
// fail-open every other path here owes, reached by the commonest route of all
// (a router that abstained, or a session_start that never ran).
func TestPiBridgeInjectsNothingWhenNoAdvisoryIsQueued(t *testing.T) {
	run := newPiBridgeRun(t)
	run.install(t, EmittedTimeoutSeconds*1000,
		piEntry("session_start", "", run.router(t, "silent", `process.exitCode = 0;`)))

	drive := run.drive(t, []piBridgeStep{{Event: "before_agent_start", Payload: map[string]any{"prompt": "first prompt"}}})
	if got := drive.ret(0); got != "null" {
		t.Errorf("before_agent_start returned %s, want null: nothing was ever queued", got)
	}
}

// before_agent_start exists only as session_start's delivery vehicle, so
// an install with no session_start entry registers none. Registering it either
// way would put a handler in front of every prompt of every session that
// answers a protocol no entry routes.
func TestPiBridgeRegistersBeforeAgentStartOnlyForSessionStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry piBridgeEntry
		want  int
	}{
		{name: "with-a-session-start-entry", entry: piEntry("session_start", "", "router-never-spawned"), want: 1},
		{name: "without-one", entry: piEntry("tool_call", "", "router-never-spawned"), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			run.install(t, EmittedTimeoutSeconds*1000, tc.entry)

			registered := run.drive(t, nil).Registered
			got := 0
			for _, event := range registered {
				if event == "before_agent_start" {
					got++
				}
			}
			if got != tc.want {
				t.Errorf("registered = %q, want before_agent_start registered %d time(s)", registered, tc.want)
			}
		})
	}
}

// session_file is hookyard's own synthesis — pi hands a handler no such field
// — and an ephemeral --no-session run is a state, not a failure: the key is
// emitted empty rather than dropped, so a handler can tell "this run keeps no
// transcript" from "this bridge predates the key".
func TestPiBridgeEmitsTheSessionFileEvenWhenThereIsNone(t *testing.T) {
	for _, tc := range []struct{ name, sessionFile, want string }{
		{name: "with-a-session", sessionFile: "/probe/sessions/probe.jsonl", want: `"/probe/sessions/probe.jsonl"`},
		{name: "no-session", sessionFile: "", want: `""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newPiBridgeRun(t)
			router, capturePath := run.capturingRouter(t)
			run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

			assertPiAllows(t, run.fire(t, "tool_call", piToolCall("bash", nil), tc.sessionFile))

			payload := piCapturedPayload(t, capturePath)
			got, ok := payload["session_file"]
			if !ok {
				t.Fatalf("the payload carries no session_file key at all: %v", payload)
			}
			if string(got) != tc.want {
				t.Errorf("session_file = %s, want %s", got, tc.want)
			}
		})
	}
}

// pi takes --api-key on its own command line, so argv is the one payload field
// that can carry a live credential — and the payload travels to a router that
// records what it is given. Both spellings have to go, and the bare flag's
// value with it. Single-dash spellings too: pi has no short alias for --api-key
// today, but the word match exists to fail closed on a flag a later version
// adds, and that promise is only kept if the dash count does not decide it.
func TestPiBridgeStripsSecretBearingFlagsFromArgv(t *testing.T) {
	run := newPiBridgeRun(t)
	router, capturePath := run.capturingRouter(t)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	const secret = "sk-probe-must-not-travel"
	assertPiAllows(t, run.fire(t, "tool_call", piToolCall("bash", nil),
		"/probe/sessions/probe.jsonl",
		"--api-key", secret, "--api-key="+secret,
		"-apikey", secret, "-apikey="+secret,
		// pi's real argv is full of single-dash flags that carry nothing
		// secret, and a guard that matched on the dash alone would eat them.
		"-p", "probe-prompt", "-e", "--model", "probe-model"))

	payload := piCapturedPayload(t, capturePath)
	var argv []string
	if err := json.Unmarshal(payload["argv"], &argv); err != nil {
		t.Fatalf("argv is not an array of strings: %v", err)
	}
	for _, arg := range argv {
		if strings.Contains(arg, secret) || strings.Contains(arg, "api-key") || strings.Contains(arg, "apikey") {
			t.Fatalf("argv = %q still carries the secret-bearing flag", argv)
		}
	}
	for _, kept := range []string{"-p", "probe-prompt", "-e", "--model", "probe-model"} {
		if !slices.Contains(argv, kept) {
			t.Errorf("argv = %q dropped %q, which carries no secret", argv, kept)
		}
	}
}

// session_shutdown is the one pi event with no canonical counterpart, so
// nothing else in the plan would notice it going missing: no engine renders it
// from a canonical name, and it has no decision slot whose absence would show.
func TestPiBridgeFiresOnSessionShutdownAndCarriesTheReason(t *testing.T) {
	run := newPiBridgeRun(t)
	router, capturePath := run.capturingRouter(t)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("session_shutdown", "", router))

	assertPiAllows(t, run.fire(t, "session_shutdown", map[string]any{"reason": "quit"}, "/probe/sessions/probe.jsonl"))

	payload := piCapturedPayload(t, capturePath)
	if got := string(payload["hook_event_name"]); got != `"session_shutdown"` {
		t.Errorf("hook_event_name = %s, want \"session_shutdown\"", got)
	}
	if got := string(payload["reason"]); got != `"quit"` {
		t.Errorf("reason = %s, want \"quit\"", got)
	}
}

// agent_settled is pi's terminal idle signal and the other pi event with no
// canonical counterpart. It carries no event-specific fields, so its payload
// must be exactly the base envelope — the bridge must still spawn the router
// (that is what records the event) and must not invent a field pi never sent.
func TestPiBridgeFiresOnAgentSettledWithTheBasePayload(t *testing.T) {
	run := newPiBridgeRun(t)
	router, capturePath := run.capturingRouter(t)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("agent_settled", "", router))

	assertPiAllows(t, run.fire(t, "agent_settled", map[string]any{}, "/probe/sessions/probe.jsonl"))

	payload := piCapturedPayload(t, capturePath)
	if got := string(payload["hook_event_name"]); got != `"agent_settled"` {
		t.Errorf("hook_event_name = %s, want \"agent_settled\"", got)
	}
	if got := string(payload["session_file"]); got != `"/probe/sessions/probe.jsonl"` {
		t.Errorf("session_file = %s, want the session file", got)
	}
	// No extras row exists for agent_settled, so no event field rides the
	// payload; a key added here that pi does not send is a fabricated contract.
	for _, unexpected := range []string{"reason", "turn_index", "tool_name"} {
		if _, ok := payload[unexpected]; ok {
			t.Errorf("payload carries %q, which agent_settled never sends", unexpected)
		}
	}
}

// The bridge no longer splits a command string on spaces, so a router path
// containing one has to spawn. The stub denies rather than abstains because
// only a block proves the router ran at all: the old split would have handed
// execFile a truncated path, and a failed spawn is an allow — the exact shape
// of a fail-open nothing else here would notice.
func TestPiBridgeSpawnsARouterWhosePathContainsASpace(t *testing.T) {
	run := newPiBridgeRun(t)
	dir := filepath.Join(run.dir, "state dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	router := run.routerAt(t, filepath.Join(dir, "deny.cjs"),
		`process.stdout.write('{"block":true,"reason":"spawned anyway"}');`)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	if verdict := run.fire(t, "tool_call", piToolCall("bash", nil)); !strings.Contains(verdict, `"block":true`) {
		t.Errorf("verdict = %s, want a block: the router under %q never ran", verdict, router)
	}
}

// A built package ships a root relative to this file and no absolute path at
// all, so the bridge is the only thing that knows where the package landed: it
// resolves the launcher against that root and appends --plugin-root itself.
// Nothing else in the repo drives that path until build mode exists.
func TestPiBridgeResolvesAgainstItsRootAndAppendsPluginRoot(t *testing.T) {
	run := newPiBridgeRun(t)
	capturePath := filepath.Join(run.dir, "captured-argv.json")
	run.router(t, "recording",
		`require("node:fs").writeFileSync(`+strconv.Quote(capturePath)+`, JSON.stringify(process.argv.slice(2)));`)

	// "." because the harness loads the bridge from the scratch directory
	// itself; a real package ships ".." from <root>/extensions/.
	root := "."
	run.installData(t, piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		Root:      &root,
		Entries: []piBridgeEntry{{
			Event: "tool_call",
			Bin:   "recording.cjs",
			Args:  []string{"route", "--registered-for", "pi", "--event", "pre_tool"},
		}},
		Commands: []piBridgeCommand{},
	})

	assertPiAllows(t, run.fire(t, "tool_call", piToolCall("bash", nil)))

	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the router never ran, so the relative bin did not resolve against the root: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatal(err)
	}
	want := []string{"route", "--registered-for", "pi", "--event", "pre_tool", "--plugin-root", run.dir}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// piCommandCapture is what a command's exec records about the way the bridge
// spawned it.
type piCommandCapture struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	// InheritsEnv is a boolean rather than a value because the property under
	// test is that the spawn added to process.env instead of replacing it.
	InheritsEnv bool `json:"inherits_env"`
	GroupLeader bool `json:"group_leader"`
}

// piCommandExec is a stub exec that records how it was spawned. It waits for a
// gate file first: the handler does not wait for the child, so holding the
// child back until the drive has already returned is what proves it. The record
// is renamed into place so the polling read never sees half a file.
func piCommandExec(gate, capturePath string) string {
	return `const fs = require("node:fs");
(async () => {
  const deadline = Date.now() + 30000;
  while (!fs.existsSync(` + strconv.Quote(gate) + `) && Date.now() < deadline) {
    await new Promise((settle) => setTimeout(settle, 10));
  }
  // A detached child leads its own process group, so a signal aimed at the
  // group named by its own pid lands; an attached one shares its parent's
  // group and no group by that name exists at all.
  let groupLeader = true;
  try { process.kill(-process.pid, 0); } catch { groupLeader = false; }
  const record = {
    argv: process.argv.slice(2),
    env: {
      HOOKYARD_SESSION_ID: process.env.HOOKYARD_SESSION_ID ?? "",
      HOOKYARD_SESSION_FILE: process.env.HOOKYARD_SESSION_FILE ?? "",
      HOOKYARD_CWD: process.env.HOOKYARD_CWD ?? "",
    },
    inherits_env: process.env.PATH !== undefined,
    group_leader: groupLeader,
  };
  fs.writeFileSync(` + strconv.Quote(capturePath+".part") + `, JSON.stringify(record));
  fs.renameSync(` + strconv.Quote(capturePath+".part") + `, ` + strconv.Quote(capturePath) + `);
})();`
}

// piAwaitCommandCapture waits for the exec's record to land. Nothing in the
// handler waits for the child, so the record appearing is the only evidence the
// spawn happened at all.
func piAwaitCommandCapture(t *testing.T, capturePath string) piCommandCapture {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(capturePath)
		if err == nil {
			var captured piCommandCapture
			if err := json.Unmarshal(raw, &captured); err != nil {
				t.Fatalf("the exec recorded %s, which is not a capture: %v", raw, err)
			}
			return captured
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command's exec recorded nothing at %s: %v", capturePath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The environment is the whole reason the commands surface exists: a subprocess
// cannot set a variable inside pi's own process, so these three keys are the
// only way pi's session identity reaches the tool a user invokes. The child is
// also detached and never awaited — an exec is typically a viewer the user
// leaves open, and a handler that waited for one would hold pi's command open
// for as long as the viewer lives, which is a hang rather than a slow command.
// The gate proves exactly that: the child cannot finish until the test opens
// it, and the test opens it only after the drive has already returned.
func TestPiBridgeSpawnsACommandDetachedWithTheSessionEnvironment(t *testing.T) {
	run := newPiBridgeRun(t)
	gate := filepath.Join(run.dir, "gate")
	capturePath := filepath.Join(run.dir, "captured-command.json")
	run.router(t, "viewer", piCommandExec(gate, capturePath))

	root := "."
	run.installData(t, piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		Root:      &root,
		Entries:   []piBridgeEntry{},
		Commands: []piBridgeCommand{{
			Name:        "aeye",
			Description: "Open the aeye image carousel for this session",
			Bin:         "viewer.cjs",
			Args:        []string{},
		}},
	})

	drive := run.drive(t, []piBridgeStep{{Command: "aeye", Args: "one two"}}, "/probe/session.jsonl")

	want := []piBridgeRegistration{{Name: "aeye", Description: "Open the aeye image carousel for this session"}}
	if !slices.Equal(drive.Commands, want) {
		t.Errorf("registered commands = %+v, want %+v: both strings are passed through verbatim", drive.Commands, want)
	}
	if _, err := os.Stat(capturePath); err == nil {
		t.Fatal("the exec ran to completion before the gate opened, so the handler awaited it")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	captured := piAwaitCommandCapture(t, capturePath)
	if wantArgv := []string{"one two"}; !slices.Equal(captured.Argv, wantArgv) {
		t.Errorf("argv = %q, want %q: the argument string is one element, spaces and all", captured.Argv, wantArgv)
	}
	wantEnv := map[string]string{
		"HOOKYARD_SESSION_ID":   "probe-session",
		"HOOKYARD_SESSION_FILE": "/probe/session.jsonl",
		"HOOKYARD_CWD":          "/probe/cwd",
	}
	for key, value := range wantEnv {
		if captured.Env[key] != value {
			t.Errorf("%s = %q, want %q", key, captured.Env[key], value)
		}
	}
	if !captured.InheritsEnv {
		t.Error("the exec inherited none of the environment: the three keys are added to process.env, not a replacement for it")
	}
	if !captured.GroupLeader {
		t.Error("the exec shares pi's process group, so it was not spawned detached")
	}
}

// piCommandExecEnvProbe is piCommandExec's sibling for env sanitization: it
// records a caller-chosen set of env var names instead of the three fixed
// HOOKYARD_* keys, so a test can assert exactly which of the parent's env
// vars reached the spawned child.
func piCommandExecEnvProbe(gate, capturePath string, probeNames []string) string {
	namesJSON, err := json.Marshal(probeNames)
	if err != nil {
		panic(err)
	}
	return `const fs = require("node:fs");
(async () => {
  const deadline = Date.now() + 30000;
  while (!fs.existsSync(` + strconv.Quote(gate) + `) && Date.now() < deadline) {
    await new Promise((settle) => setTimeout(settle, 10));
  }
  const names = ` + string(namesJSON) + `;
  const env = {};
  for (const name of names) env[name] = process.env[name] ?? null;
  fs.writeFileSync(` + strconv.Quote(capturePath+".part") + `, JSON.stringify({ env }));
  fs.renameSync(` + strconv.Quote(capturePath+".part") + `, ` + strconv.Quote(capturePath) + `);
})();`
}

// commands[] hands its exec pi's own process.env, and pi's process env can
// carry a live LLM API key — a secret-bearing name must not reach the child,
// while a legitimately-needed one still must (sanitizeEnv in pi_bridge.ts,
// reusing sanitizeArgv's SECRET_FLAG_WORDS against env key names rather than
// flag names).
func TestPiBridgeStripsSecretBearingEnvVarsFromCommandSpawn(t *testing.T) {
	run := newPiBridgeRun(t)
	gate := filepath.Join(run.dir, "gate")
	capturePath := filepath.Join(run.dir, "captured-env.json")
	probeNames := []string{"HOOKYARD_TEST_PROBE_API_KEY", "HOOKYARD_TEST_PROBE_TOKEN", "HOOKYARD_TEST_PROBE_SAFE_VAR"}
	run.router(t, "envprobe", piCommandExecEnvProbe(gate, capturePath, probeNames))

	t.Setenv("HOOKYARD_TEST_PROBE_API_KEY", "sk-probe-must-not-travel")
	t.Setenv("HOOKYARD_TEST_PROBE_TOKEN", "probe-token-must-not-travel")
	t.Setenv("HOOKYARD_TEST_PROBE_SAFE_VAR", "probe-safe-value")

	root := "."
	run.installData(t, piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		Root:      &root,
		Entries:   []piBridgeEntry{},
		Commands: []piBridgeCommand{{
			Name:        "aeye",
			Description: "Open the aeye image carousel for this session",
			Bin:         "envprobe.cjs",
			Args:        []string{},
		}},
	})

	run.drive(t, []piBridgeStep{{Command: "aeye", Args: ""}}, "/probe/session.jsonl")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var captured struct {
		Env map[string]*string `json:"env"`
	}
	for {
		raw, err := os.ReadFile(capturePath)
		if err == nil {
			if jsonErr := json.Unmarshal(raw, &captured); jsonErr != nil {
				t.Fatalf("the exec recorded %s, which is not a capture: %v", raw, jsonErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command's exec recorded nothing at %s: %v", capturePath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := captured.Env["HOOKYARD_TEST_PROBE_API_KEY"]; got != nil {
		t.Errorf("HOOKYARD_TEST_PROBE_API_KEY = %q, want it stripped from the child env", *got)
	}
	if got := captured.Env["HOOKYARD_TEST_PROBE_TOKEN"]; got != nil {
		t.Errorf("HOOKYARD_TEST_PROBE_TOKEN = %q, want it stripped from the child env", *got)
	}
	if got := captured.Env["HOOKYARD_TEST_PROBE_SAFE_VAR"]; got == nil || *got != "probe-safe-value" {
		t.Errorf("HOOKYARD_TEST_PROBE_SAFE_VAR = %v, want \"probe-safe-value\": a legitimately-needed var must still reach the command", got)
	}
}

// Nothing escapes a command handler either. A failed spawn is the one failure a
// handler that waits for nothing can still see — there is no exit status to
// read — and it is shown to the user rather than thrown, because a user who
// typed a command is owed the news that it never started.
func TestPiBridgeReportsACommandItCannotSpawn(t *testing.T) {
	run := newPiBridgeRun(t)
	root := "."
	run.installData(t, piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		Root:      &root,
		Entries:   []piBridgeEntry{},
		Commands: []piBridgeCommand{{
			Name:        "aeye",
			Description: "Open the aeye image carousel for this session",
			Bin:         "absent.cjs",
			Args:        []string{},
		}},
	})

	drive := run.drive(t, []piBridgeStep{{Command: "aeye", Args: ""}})

	if got := drive.ret(0); got != "null" {
		t.Errorf("handler returned %s, want null: a command handler answers pi with nothing", got)
	}
	if len(drive.Notifications) != 1 {
		t.Fatalf("notifications = %+v, want exactly one", drive.Notifications)
	}
	if got := drive.Notifications[0]; got.Level != "error" || !strings.Contains(got.Message, "absent.cjs") {
		t.Errorf("notification = %+v, want an error naming the exec that could not start", got)
	}
}

// Commands are a build-mode surface and a yard install renders an empty list,
// so nothing is registered there — which is also what keeps the loop from
// resolving a bin against the null root a yard bridge carries.
func TestPiBridgeRegistersNoCommandsInYardMode(t *testing.T) {
	run := newPiBridgeRun(t)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", "/nonexistent/router"))

	if drive := run.drive(t, nil); len(drive.Commands) != 0 {
		t.Errorf("registered commands = %+v, want none in yard mode", drive.Commands)
	}
}
