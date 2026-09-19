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
// A sequence rather than a single event because one delivery needs two: an
// advisory queued by session_start is flushed by the before_agent_start that
// follows it, and the slot holding it lives in the module — so both firings have
// to happen inside one load of the bridge.
const piBridgeHarness = `import { readFile } from "node:fs/promises";
import bridge from "./bridge.mjs";

const registered = [];
const commands = [];
const notifications = [];
bridge({
  on: (event, handler) => registered.push({ event, handler }),
  registerCommand: (name, options) => commands.push({ name, ...options }),
});

const [scriptPath, ...rest] = process.argv.slice(2);
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

const returns = [];
for (const step of JSON.parse(await readFile(scriptPath, "utf8"))) {
  if (step.command !== undefined) {
    const command = commands.find((c) => c.name === step.command);
    if (!command) throw new Error("the bridge registered no command named " + step.command);
    returns.push((await command.handler(step.args, ctx)) ?? null);
    continue;
  }
  const entry = registered.find((r) => r.event === step.event);
  if (!entry) throw new Error("the bridge registered nothing for " + step.event);
  returns.push((await entry.handler(step.payload, ctx)) ?? null);
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

func piToolCall(name string, input any) map[string]any {
	return map[string]any{"toolName": name, "input": input, "toolCallId": "call-1"}
}

// content is `any` because what the bridge does with a content that is not the
// array pi documents is itself part of the contract.
func piToolResult(content any) map[string]any {
	return map[string]any{"toolName": "bash", "input": nil, "toolCallId": "call-1", "content": content, "isError": false}
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

// The advisory path owes the same fail-open decision() owes, on both delivery
// vehicles. None of these could block — only tool_call can — but a bridge that
// read an advisory out of a reply it does not understand would put the router's
// own failure in front of the model as hookyard's advice, and on tool_result it
// would put it there in place of the tool's output.
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

// R4.1 and R4.4: the advisory is appended to the tool's own output and names
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

// R4.2: tool_result's content is pi's shape rather than hookyard's, and a
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
		"content":    strconv.Quote(advice),
		"display":    "false",
	} {
		if got := string(injected.Message[key]); got != want {
			t.Errorf("message.%s = %s, want %s", key, got, want)
		}
	}
	if got := drive.ret(2); got != "null" {
		t.Errorf("the second prompt's before_agent_start returned %s, want null: the slot is cleared on flush", got)
	}
}

// R3.4: a prompt that follows no queued advisory injects nothing — the same
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

// R3.1: before_agent_start exists only as session_start's delivery vehicle, so
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
// value with it.
func TestPiBridgeStripsSecretBearingFlagsFromArgv(t *testing.T) {
	run := newPiBridgeRun(t)
	router, capturePath := run.capturingRouter(t)
	run.install(t, EmittedTimeoutSeconds*1000, piEntry("tool_call", "", router))

	const secret = "sk-probe-must-not-travel"
	assertPiAllows(t, run.fire(t, "tool_call", piToolCall("bash", nil),
		"/probe/sessions/probe.jsonl", "--api-key", secret, "--api-key="+secret, "--model", "probe-model"))

	payload := piCapturedPayload(t, capturePath)
	var argv []string
	if err := json.Unmarshal(payload["argv"], &argv); err != nil {
		t.Fatalf("argv is not an array of strings: %v", err)
	}
	for _, arg := range argv {
		if strings.Contains(arg, secret) || strings.Contains(arg, "api-key") {
			t.Fatalf("argv = %q still carries the secret-bearing flag", argv)
		}
	}
	if !slices.Contains(argv, "--model") || !slices.Contains(argv, "probe-model") {
		t.Errorf("argv = %q dropped a flag that carries no secret", argv)
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
