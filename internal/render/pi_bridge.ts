// hookyard's Pi bridge. Pi exposes no subprocess hook protocol at all: its hook
// surface is the extension API, so the hook *is* this file — JavaScript pi
// loads into its own process and awaits.
//
// One fixed template plus exactly one spliced value. Every install-specific
// string — router path, state directory, event names, matchers — arrives inside
// DATA as json.Marshal output, so a hostile handler string lands as JSON data
// and can never become code. Nothing else is ever concatenated into this
// source (§8).
//
// Plain ESM JavaScript under a .ts name, and both halves are load-bearing:
// `node --check` can parse plain JS, which is the only gate in this repo that
// looks at this file at all, and pi loads it through jiti regardless of the
// extension. No type annotations, no type-only imports.
//
// Node builtins only. The bridge sits in pi's config directory with no
// node_modules beside it, so a value import from the pi package would throw at
// load — and an extension that throws at load takes down every extension pi
// would have loaded after it.

import { execFile } from "node:child_process";

const DATA = __HOOKYARD_DATA__;

// Pi imposes no timeout of its own — it awaits the handler's promise — so a
// wedged router would stall the turn forever. On the other three engines the
// engine owns the timeout, which makes this the one place hookyard can leak an
// orphaned router per tool call; the child is killed, not merely abandoned.
//
// Resolves, never rejects. A missing or non-executable binary, a non-zero exit
// and the timeout kill all arrive as `error`, and §5 spends every one of them
// on allow.
function askRouter(command, payload) {
  return new Promise((resolve) => {
    // The command stays one string, split here on single spaces, rather than
    // travelling as a pre-split array: doctor recovers the state directory from
    // this file's raw bytes with a flag-then-whitespace regex that a JSON array
    // would break. checkShellSafe already guarantees the router path and the
    // state directory contain no whitespace and no quotes, so the split is an
    // exact argv — and execFile takes argv directly, so no shell ever sees it.
    const argv = command.split(" ");
    const child = execFile(argv[0], argv.slice(1), { timeout: DATA.timeout_ms }, (error, stdout) =>
      resolve(error ? "" : stdout),
    );
    // A router that exits before draining its stdin breaks the pipe under us,
    // and a payload larger than the OS pipe buffer — a tool_result carrying
    // file contents clears 64KB easily — is still being written when that
    // happens. The resulting EPIPE arrives as an 'error' event on the stdin
    // stream, outside the promise and outside the handler's try/catch, so with
    // no listener node treats it as unhandled and kills pi's whole process,
    // taking every later hook with it — the exact inversion of the fail-open
    // guarantee above. Swallowing it here leaves the execFile callback to
    // resolve the call the way it resolves every other router failure. The
    // child itself needs no such listener: execFile registers its own 'error'
    // handler and routes a failed spawn into that callback.
    child.stdin.on("error", () => {});
    child.stdin.end(payload);
  });
}

// Blocks only on a well-formed deny. Empty stdout is how the router abstains,
// and unparsable stdout or a reply carrying no decision are the router's own
// failures — all three land here as allow.
function decision(stdout) {
  let reply;
  try {
    reply = JSON.parse(stdout);
  } catch {
    return undefined;
  }
  if (!reply || reply.block !== true) return undefined;
  return { block: true, reason: typeof reply.reason === "string" ? reply.reason : "Blocked by hookyard" };
}

// For Pi, and only for Pi, hookyard authors the inbound payload — pi sends none
// — so its shape is pinned by the committed pi-*.json fixtures rather than by
// the engine. A key added here that no re-captured fixture carries does not
// exist.
const extras = {
  session_start: (event) => ({ reason: event.reason }),
  input: (event) => ({ prompt: event.text, source: event.source }),
  tool_call: (event) => ({
    tool_name: event.toolName,
    tool_input: event.input,
    tool_use_id: event.toolCallId,
  }),
  tool_result: (event) => ({
    tool_name: event.toolName,
    tool_input: event.input,
    tool_use_id: event.toolCallId,
    tool_response: { content: event.content, is_error: event.isError },
  }),
  turn_end: (event) => ({ turn_index: event.turnIndex }),
};

function payload(name, event, ctx) {
  const extra = extras[name];
  return JSON.stringify({
    hook_event_name: name,
    // The discriminator Detect keys on, and pi exposes no version to an
    // extension, so it is resolved at install time and spliced in. An absent or
    // empty value would be dropped by JSON.stringify, Detect would fall through
    // every rule, and every Pi hook would become a silent no-op.
    pi_version: DATA.pi_version || "unknown",
    session_id: ctx.sessionManager.getSessionId(),
    // Emitted even when empty: the envelope deliberately invents no cwd
    // fallback for Pi, so a dropped key would leave the record with no working
    // directory at all.
    cwd: ctx.cwd ?? "",
    ...(extra ? extra(event) : {}),
  });
}

export default function (pi) {
  for (const entry of DATA.entries) {
    pi.on(entry.event, async (event, ctx) => {
      try {
        // An empty matcher means every tool. Filtering here, before anything is
        // spawned, is what keeps a single-tool handler free on every other
        // call.
        if (entry.matcher && !entry.matcher.split("|").includes(event.toolName)) return undefined;

        const stdout = await askRouter(entry.command, payload(entry.event, event, ctx));

        // Only tool_call carries a return channel that can block; every other
        // event ignores the reply. The spawn still happens, because recording
        // the event is the router's job either way.
        return entry.event === "tool_call" ? decision(stdout) : undefined;
      } catch {
        // Nothing may escape a handler: pi turns a throw out of a tool_call
        // handler into a block, so an escaping exception would invert the one
        // guarantee this file owns.
        return undefined;
      }
    });
  }
}
