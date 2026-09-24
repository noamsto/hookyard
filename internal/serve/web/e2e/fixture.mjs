// Writes a hookyard state dir the flow view can be driven against:
// table.json (handler groups guards.* ×8, guards.pi.* ×8, aeye-* ×7,
// houston.* ×2, plus ungrouped handlers) and two stream day files — today
// (every record inside the live 10-minute window) and yesterday. Record
// fields follow internal/record's wire shape.
import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export const GUARDS = ["guards.rm", "guards.git-push", "guards.secrets", "guards.curl",
  "guards.sudo", "guards.chmod", "guards.dd", "guards.fork"];
// Exactly these four guards members carry a deny today: under outcome=deny
// the guards group auto-opens (≤ 4 shown members) — the #96 setup.
export const GUARDS_DENY = GUARDS.slice(0, 4);
// A guards member with traffic today but no deny: a live deny through it
// sticky-shows a 5th member.
export const GUARDS_FIFTH = "guards.sudo";
export const GUARDS_PI = ["a", "b", "c", "d", "e", "f", "g", "h"].map((s) => "guards.pi." + s);
export const AEYE = ["aeye-lint", "aeye-fmt", "aeye-spell", "aeye-ctx", "aeye-mem", "aeye-tone", "aeye-safety"];
export const HOUSTON = ["houston.state", "houston.state.observe"];
export const UNGROUPED = ["notify", "audit", "lint-go"];

const ENGINES = ["claude-code", "codex", "cursor", "pi"];

function handler(id, events, engines = ENGINES) {
  return { id, exec: "/bin/true", events, engines, match: null, timeout_ms: 0, lane: "verdict" };
}

function table() {
  return [
    ...GUARDS.map((id) => handler(id, ["pre_tool"])),
    ...GUARDS_PI.map((id) => handler(id, ["pre_tool"], ["pi", "codex"])),
    ...AEYE.map((id) => handler(id, ["post_tool", "prompt_submit"])),
    ...HOUSTON.map((id) => handler(id, ["session_start"])),
    handler("notify", ["pre_tool"]),
    handler("audit", ["post_tool"]),
    handler("lint-go", ["pre_tool"], ["pi"]),
  ];
}

export function utcDay(ms) {
  return new Date(ms).toISOString().slice(0, 10);
}

function isoMicro(ms) {
  return new Date(ms).toISOString().replace("Z", "000Z");
}

let seq = 0;

// record builds one stream record. hops: [[handler, outcome], ...].
export function record({ ts, engine, event = "", native, hops = [], verdict, router = "ok", tool = "" }) {
  seq++;
  return {
    v: 1,
    ts: isoMicro(ts),
    key: engine + "/e2e-" + (seq % 5),
    engine,
    session_id: "e2e-" + (seq % 5),
    canonical_event: event,
    native_event: native,
    cwd: "/tmp/e2e",
    tool_name: tool,
    verdict,
    enforced: verdict === "deny",
    router,
    router_ms: 3,
    handlers: hops.map(([name, outcome], i) => ({ name, outcome, ms: 2 + i })),
  };
}

// TODAY_PATHS are the live day's call shapes; `n` copies of each are written.
// The burst reuses them so every burst call runs through drawn nodes.
export const TODAY_PATHS = [
  { n: 3, engine: "claude-code", event: "pre_tool", native: "PreToolUse", tool: "Bash", verdict: "deny",
    hops: [["guards.rm", "deny"], ["guards.git-push", "allow"], ["guards.sudo", "allow"], ["aeye-lint", "advise"], ["notify", "dispatched"]] },
  { n: 2, engine: "claude-code", event: "pre_tool", native: "PreToolUse", tool: "Bash", verdict: "deny",
    hops: [["guards.rm", "allow"], ["guards.git-push", "deny"], ["guards.chmod", "abstain"], ["notify", "dispatched"]] },
  { n: 2, engine: "codex", event: "pre_tool", native: "PreToolUse", tool: "exec_command", verdict: "deny",
    hops: [["guards.secrets", "deny"], ["guards.pi.a", "allow"], ["guards.pi.b", "deny"]] },
  { n: 1, engine: "cursor", event: "pre_tool", native: "beforeShellExecution", tool: "Shell", verdict: "deny",
    hops: [["guards.curl", "deny"], ["guards.dd", "allow"], ["guards.fork", "allow"]] },
  { n: 4, engine: "claude-code", event: "post_tool", native: "PostToolUse", tool: "Edit", verdict: "allow",
    hops: [["aeye-fmt", "allow"], ["aeye-lint", "allow"], ["aeye-spell", "abstain"], ["audit", "allow"]] },
  { n: 2, engine: "pi", event: "pre_tool", native: "tool_call", tool: "bash", verdict: "allow",
    hops: [["guards.pi.c", "allow"], ["guards.pi.d", "error"], ["guards.pi.e", "timeout"], ["lint-go", "allow"]] },
  { n: 2, engine: "claude-code", event: "session_start", native: "SessionStart", verdict: "allow",
    hops: [["houston.state", "allow"], ["houston.state.observe", "dispatched"]] },
  { n: 2, engine: "codex", event: "prompt_submit", native: "UserPromptSubmit", verdict: "allow",
    hops: [["aeye-ctx", "advise"], ["aeye-mem", "allow"], ["aeye-tone", "abstain"], ["aeye-safety", "allow"]] },
  // A router error: addressed by its native event, one router-error branch.
  { n: 1, engine: "cursor", event: "", native: "beforeReadFile", verdict: "allow", router: "error" },
  // pi's handler-less engine-scoped turn_end: a direct event -> outcome branch.
  { n: 3, engine: "pi", event: "", native: "turn_end", verdict: "abstain" },
  // A cross-registration drop: suppressed as the call verdict, no handlers.
  { n: 1, engine: "claude-code", event: "pre_tool", native: "PreToolUse", tool: "Read", verdict: "suppressed" },
];

const PAST_PATHS = [
  { n: 3, engine: "claude-code", event: "pre_tool", native: "PreToolUse", tool: "Bash", verdict: "deny",
    hops: [["guards.rm", "deny"], ["notify", "dispatched"]] },
  { n: 2, engine: "codex", event: "post_tool", native: "PostToolUse", tool: "apply_patch", verdict: "allow",
    hops: [["aeye-fmt", "allow"], ["audit", "allow"]] },
  { n: 1, engine: "pi", event: "", native: "turn_end", verdict: "abstain" },
  // Mirrors TODAY_PATHS' router-error entry so the two days observe the same
  // "extra" (non-skeleton) topology nodes under show-idle: a day switch with
  // idle on then keeps the layout's node set — and its plan key — unchanged.
  { n: 1, engine: "cursor", event: "", native: "beforeReadFile", verdict: "allow", router: "error" },
];

function linesOf(paths, tsOf) {
  const out = [];
  let i = 0;
  for (const p of paths) {
    for (let k = 0; k < p.n; k++) out.push(JSON.stringify(record({ ...p, ts: tsOf(i++) })) + "\n");
  }
  return out.join("");
}

export function dayFile(stateDir, day) {
  return join(stateDir, "stream", day + ".jsonl");
}

// writeFixture fills stateDir and returns { today, past }. Today's records
// sit 20–110 s before now (clamped to the day), well inside the 10-minute
// window for the length of a run.
export function writeFixture(stateDir, now = Date.now()) {
  mkdirSync(join(stateDir, "stream"), { recursive: true });
  writeFileSync(join(stateDir, "table.json"), JSON.stringify({ handlers: table() }), { mode: 0o600 });

  const today = utcDay(now);
  const dayStart = Date.parse(today + "T00:00:00Z");
  writeFileSync(dayFile(stateDir, today), linesOf(TODAY_PATHS, (i) => Math.max(dayStart + 1000, now - 20000 - i * 4000)));

  const past = utcDay(dayStart - 1);
  const pastNoon = Date.parse(past + "T12:00:00Z");
  writeFileSync(dayFile(stateDir, past), linesOf(PAST_PATHS, (i) => pastNoon + i * 60000));
  return { today, past };
}

// append writes records (built with record()) to today's file in one write,
// so the server's tailer sees whole lines.
export function append(stateDir, recs) {
  const day = utcDay(Date.parse(recs[0].ts));
  appendFileSync(dayFile(stateDir, day), recs.map((r) => JSON.stringify(r) + "\n").join(""));
}
