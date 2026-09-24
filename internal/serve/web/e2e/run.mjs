// End-to-end checks for the serve flow view (spec §6): builds hookyard,
// serves a generated fixture state dir, drives headless Chromium over CDP.
//
//   CHROME=/path/to/chromium npm run e2e          all checks
//   node e2e/run.mjs 5 6                          only checks 5 and 6
//   HOOKYARD_BIN=/path/to/hookyard npm run e2e    skip the go build
//
// Prints PASS/FAIL per check and a total; exits non-zero on any failure.
import { execFileSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { Browser, KEYS, MOD, runCleanups, sleep, spawnGroup, tempDir, waitForLine } from "./cdp.mjs";
import { installHelpers, openFlow } from "./helpers.mjs";
import { GUARDS_DENY, GUARDS_FIFTH, TODAY_PATHS, append, record, writeFixture } from "./fixture.mjs";

const WEB = dirname(dirname(fileURLToPath(import.meta.url)));
const REPO = resolve(WEB, "../../..");

// Mirrors src/constants.ts; the #96 checks time themselves against these.
const RELAYOUT_MS = 5000;
const PRESS_HOLD_MAX_MS = 10000;
const MAX_DOTS = 48;
const BURST_CALLS = 500;
const BURST_MS = 5000;
const BURST_ELEMENT_SLACK = 20;

class CheckFailed extends Error {}

function assert(cond, msg) {
  if (!cond) throw new CheckFailed(msg);
}

// ---------- independent derivation (check 1) ----------

// eventLabel mirrors app.js's: the DOM names event nodes with it.
function eventLabel(p) {
  if (p.router === "error") return p.native_event || "—";
  if (p.canonical_event) return p.canonical_event;
  if (p.native_event) return p.engine + ":" + p.native_event;
  return "—";
}

// expectedEdges derives every display edge's count from /api/flow's paths:
// engine -> event counts calls, every later hop counts branches, a handler
// inside a collapsed group (per the DOM's own groups) lands on the group.
function expectedEdges(resp, groups) {
  const collapsed = new Map();
  for (const g of groups) if (!g.expanded) for (const m of g.members) collapsed.set(m, g.name);
  const handlerNode = (name) => (collapsed.has(name) ? ["group", collapsed.get(name)] : ["handler", name]);

  const out = new Map();
  const bump = (a, b, n) => {
    const k = JSON.stringify([a[0], a[1], b[0], b[1]]);
    out.set(k, (out.get(k) ?? 0) + n);
  };
  for (const p of resp.paths ?? []) {
    const n = p.counts.reduce((a, b) => a + b, 0);
    if (n === 0) continue;
    const engine = ["engine", p.engine];
    const event = ["event", eventLabel(p)];
    bump(engine, event, n);
    const hops = p.handlers ?? [];
    let branches;
    if (p.router === "error") branches = [[["outcome", "router-error"]]];
    else if (hops.length === 0) branches = [[["outcome", p.verdict]]];
    else branches = hops.map((h) => [handlerNode(h.name), ["outcome", h.outcome]]);
    for (const b of branches) {
      let prev = event;
      for (const node of b) {
        bump(prev, node, n);
        prev = node;
      }
    }
  }
  return out;
}

function compareEdges(expected, domEdges) {
  const problems = [];
  const dom = new Map();
  for (const [k, n] of domEdges) {
    if (dom.has(k)) problems.push("duplicate DOM edge " + k);
    dom.set(k, n);
  }
  for (const [k, n] of expected) {
    if (!dom.has(k)) problems.push("missing " + k + " (want " + n + ")");
    else if (dom.get(k) !== n) problems.push("edge " + k + ": DOM " + dom.get(k) + ", want " + n);
  }
  for (const [k, n] of dom) {
    if (n > 0 && !expected.has(k)) problems.push("extra " + k + " = " + n);
  }
  return problems;
}

// ---------- environment ----------

async function buildBinary() {
  if (process.env.HOOKYARD_BIN) return process.env.HOOKYARD_BIN;
  const bin = join(tempDir("hookyard-e2e-bin-"), "hookyard");
  execFileSync("go", ["build", "-o", bin, "./cmd/hookyard"], { cwd: REPO, stdio: "inherit" });
  return bin;
}

// startEnv writes a fresh fixture and serves it on a free port.
async function startEnv(bin) {
  const stateDir = tempDir("hookyard-e2e-state-");
  const days = writeFixture(stateDir);
  const { child, kill } = spawnGroup(bin, ["serve", "--state-dir", stateDir, "--port", "0"]);
  const m = await waitForLine(child, child.stdout, /http:\/\/(127\.0\.0\.1:\d+)/, 15000, "hookyard serve");
  child.stdout.resume();
  child.stderr.resume();
  const base = "http://" + m[1];
  const table = await (await fetch(base + "/api/table")).json();
  if (table.error) throw new Error("fixture table.json rejected: " + table.error);
  return { ...days, stateDir, base, kill };
}

async function apiFlow(env, query, day = env.today) {
  const params = new URLSearchParams(query);
  params.set("day", day);
  if (day === env.today) params.set("window", "10");
  const resp = await fetch(env.base + "/api/flow?" + params.toString());
  return resp.json();
}

// totalsMatch retries briefly: DOM counts refresh at most every 500 ms.
async function totalsMatch(page, env, query, day = env.today, timeoutMs = 4000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const resp = await apiFlow(env, query, day);
    const snap = await page.evaluate("({ state: __e2e.state(), groups: __e2e.groups(), edges: __e2e.edges() })");
    const problems = compareEdges(expectedEdges(resp, snap.groups), snap.edges);
    if (snap.state.calls !== resp.calls) problems.push("data-calls " + snap.state.calls + ", /api/flow calls " + resp.calls);
    if (snap.state.branches !== resp.branches) problems.push("data-branches " + snap.state.branches + ", /api/flow branches " + resp.branches);
    const nonZero = snap.edges.filter(([, n]) => n > 0).length;
    if (problems.length === 0) return { calls: resp.calls, branches: resp.branches, edges: snap.edges.length, nonZero };
    if (Date.now() > deadline) throw new CheckFailed(problems.length + " mismatches [" + query + "]:\n      " + problems.join("\n      "));
    await sleep(250);
  }
}

// ---------- checks ----------

async function check1(page, env) {
  const guardsMember = GUARDS_DENY[0];
  const sets = ["", "handler=" + guardsMember, "outcome=deny", "handler=" + guardsMember + "&outcome=deny"];
  const lines = [];
  for (const query of sets) {
    await openFlow(page, env.base, query);
    const t = await totalsMatch(page, env, query);
    assert(t.calls > 0, "fixture produced no calls under [" + query + "]");
    lines.push(`[${query || "no filter"}] 0 mismatches over ${t.nonZero} non-zero edges (${t.edges} drawn); calls ${t.calls}, branches ${t.branches}`);
  }
  return lines;
}

async function check2(page, env) {
  await openFlow(page, env.base);
  const lines = [];
  const e = "__e2e";

  const c = await page.evaluate(e + ".paneCenter()");
  const t0 = await page.evaluate(e + ".transform()");
  await page.wheel(c.x, c.y, -300);
  const t1 = await page.waitFor(`(() => { const t = ${e}.transform(); return t && t.k > ${t0.k} * 1.05 ? t : null; })()`, 3000, "wheel zoom");
  lines.push(`zoom: wheel scale ${t0.k.toFixed(3)} -> ${t1.k.toFixed(3)}`);

  const p = await page.evaluate(e + ".panePoint()");
  assert(p, "no bare pane point to drag from");
  await page.drag(p.x, p.y, p.x + 150, p.y + 90);
  await sleep(100);
  const t2 = await page.evaluate(e + ".transform()");
  const dx = t2.x - t1.x;
  const dy = t2.y - t1.y;
  assert(Math.abs(dx - 150) < 10 && Math.abs(dy - 90) < 10, `pane drag moved the viewport by (${dx}, ${dy}), want ≈ (150, 90)`);
  lines.push(`pan: pane drag moved the viewport (${dx.toFixed(0)}, ${dy.toFixed(0)})`);

  const n = await page.evaluate(`${e}.nodePoint("engine", "claude-code")`);
  assert(n?.ok, "engine claude-code node not hit-testable: " + JSON.stringify(n));
  const search = await page.evaluate("location.search");
  await page.drag(n.x, n.y, n.x + 100, n.y + 60);
  await sleep(300);
  const t3 = await page.evaluate(e + ".transform()");
  const ndx = t3.x - t2.x;
  const ndy = t3.y - t2.y;
  assert(Math.abs(ndx - 100) < 10 && Math.abs(ndy - 60) < 10, `node drag moved the viewport by (${ndx}, ${ndy}), want ≈ (100, 60)`);
  const after = await page.evaluate(`({ search: location.search, chips: ${e}.chips() })`);
  assert(after.search === search && after.chips.length === 0, "a drag starting on a node filtered: " + JSON.stringify(after));
  lines.push(`pan: drag from a node moved the viewport (${ndx.toFixed(0)}, ${ndy.toFixed(0)}) and did not filter`);

  const zoomInFar = async () => {
    for (let i = 0; i < 4; i++) {
      await page.wheel(c.x, c.y, -400);
      await sleep(50);
    }
    return page.waitFor(`${e}.outside().length`, 3000, "zoomed in past the canvas");
  };
  await zoomInFar();
  const fit = await page.evaluate(`${e}.hit(document.querySelector("#flow-body .react-flow__controls-fitview"))`);
  assert(fit.ok, "Controls fit button not hit-testable");
  await page.click(fit.x, fit.y);
  await page.waitFor(`${e}.outside().length === 0`, 3000, "Controls fit brings every node inside").catch(async () => {
    throw new CheckFailed("after the fit button, nodes outside the canvas: " + (await page.evaluate(`${e}.outside()`)).join(", "));
  });
  const nodes = await page.evaluate(`${e}.nodeCount()`);
  lines.push(`fit: Controls fit button -> all ${nodes} nodes inside the canvas`);

  await zoomInFar();
  await page.key(KEYS.zero);
  await page.waitFor(`${e}.outside().length === 0`, 3000, "key 0 brings every node inside").catch(async () => {
    throw new CheckFailed("after key 0, nodes outside the canvas: " + (await page.evaluate(`${e}.outside()`)).join(", "));
  });
  lines.push(`fit: key 0 -> all ${nodes} nodes inside the canvas`);

  const mm = await page.evaluate(`({ present: !!document.querySelector("#flow-body .react-flow__minimap"), mini: ${e}.minimapNodes(), top: ${e}.topLevel() })`);
  assert(mm.present, "no MiniMap");
  assert(mm.mini === mm.top && mm.top > 0, `MiniMap shows ${mm.mini} mini-nodes, want one per top-level node (${mm.top})`);
  lines.push(`minimap: ${mm.mini} mini-nodes == ${mm.top} top-level nodes`);
  return lines;
}

async function clickNode(page, col, name, modifiers = 0) {
  const p = await page.evaluate(`__e2e.nodePoint(${JSON.stringify(col)}, ${JSON.stringify(name)})`);
  assert(p?.ok, `${col} ${name} not hit-testable: ` + JSON.stringify(p));
  if (modifiers & MOD.shift) await page.keyDown(KEYS.shift, MOD.shift);
  await page.click(p.x, p.y, { modifiers });
  if (modifiers & MOD.shift) await page.keyUp(KEYS.shift);
}

const FILTER_STATE = `(async () => ({ url: __e2e.params("engine"), chips: __e2e.chips(), app: await __e2e.appFilter() }))()`;

async function expectFilter(page, want, what) {
  const wantState = JSON.stringify({ url: want, chips: want.map((v) => "engine: " + v), app: want });
  try {
    await page.waitFor(`${FILTER_STATE}.then((s) => JSON.stringify(s) === ${JSON.stringify(wantState)})`, 5000, what);
  } catch {
    const got = await page.evaluate(FILTER_STATE);
    throw new CheckFailed(what + ": want " + wantState + ", got " + JSON.stringify(got));
  }
  await page.waitFor(`__e2e.settled() && ${want.map((v) => `__e2e.selected("engine", ${JSON.stringify(v)})`).concat("true").join(" && ")}`,
    5000, what + " (flow reloaded)");
}

async function check3(page, env) {
  await openFlow(page, env.base);
  const lines = [];
  const loads = await page.evaluate("__e2e.appJsLoads()");
  assert(loads === 1, `app.js fetched ${loads} times, want one instance`);

  await clickNode(page, "engine", "codex");
  await expectFilter(page, ["codex"], "click codex");
  lines.push("click codex -> URL engine=codex, chip 'engine: codex'");

  await clickNode(page, "engine", "cursor", MOD.shift);
  await expectFilter(page, ["codex", "cursor"], "shift-click cursor");
  lines.push("shift-click cursor -> URL engine=codex&engine=cursor, both chips");

  await clickNode(page, "engine", "codex");
  await expectFilter(page, ["cursor"], "click the active codex");
  lines.push("click active codex -> removed; cursor stays");

  const x = await page.evaluate(`__e2e.hit(document.querySelector("#filter-chips .fchip-remove"))`);
  assert(x.ok, "chip × not hit-testable");
  await page.click(x.x, x.y);
  await expectFilter(page, [], "chip ×");
  lines.push("chip × -> removed; URL and chips empty; one app.js instance");
  return lines;
}

async function check4(page, env) {
  await openFlow(page, env.base);
  await sleep(1000);
  const before = await page.evaluate("__e2e.elementCount()");
  const errorsAt = page.errors.length;

  const batches = 50;
  const per = BURST_CALLS / batches;
  let maxDots = 0;
  const started = Date.now();
  for (let b = 0; b < batches; b++) {
    const recs = [];
    for (let i = 0; i < per; i++) {
      const shape = TODAY_PATHS[(b * per + i) % TODAY_PATHS.length];
      recs.push(record({ ...shape, ts: Date.now() }));
    }
    append(env.stateDir, recs);
    const s = await page.evaluate("__e2e.state()");
    maxDots = Math.max(maxDots, s.dots);
    const next = started + ((b + 1) * BURST_MS) / batches;
    if (next > Date.now()) await sleep(next - Date.now());
  }
  const burstMs = Date.now() - started;

  const t = await totalsMatch(page, env, "", env.today, 8000);
  await sleep(1500); // let the last pulses finish
  const after = await page.evaluate("__e2e.elementCount()");
  const circles = await page.evaluate("__e2e.circles()");
  const st = await page.evaluate("__e2e.state()");
  const errors = page.errors.slice(errorsAt);

  const summary = [
    `${BURST_CALLS} records appended over ${burstMs} ms`,
    `flow subtree elements: ${before} before, ${after} after (slack ${BURST_ELEMENT_SLACK})`,
    `<circle> count ${circles}; max active dots sampled ${maxDots}; dropped ${st.dropped}`,
    `totals match: calls ${t.calls}, branches ${t.branches}, ${t.nonZero} non-zero edges`,
  ];
  const problems = [];
  if (errors.length > 0) {
    const distinct = [...new Set(errors)];
    problems.push(`${errors.length} console errors during the burst (${distinct.length} distinct):`, ...distinct.slice(0, 5).map((e) => "  " + e));
  }
  if (circles !== MAX_DOTS) problems.push(`<circle> count ${circles}, want ${MAX_DOTS}`);
  if (Math.abs(after - before) > BURST_ELEMENT_SLACK) problems.push(`flow subtree went from ${before} to ${after} elements`);
  if (maxDots === 0) problems.push("no pulse was animating during the burst");
  assert(problems.length === 0, problems.concat(summary).join("\n     "));
  return summary.concat("console clean");
}

// #96 setup: outcome=deny with the guards group drawn auto-open (4 deny members).
async function openGuardsExpanded(page, env) {
  await openFlow(page, env.base, "outcome=deny");
  const member = JSON.stringify(GUARDS_DENY[0]);
  await page.waitFor(`__e2e.settled() && __e2e.groupOf(${member})?.expanded === true`, 5000, "guards group drawn expanded");
  const committedAt = Date.now();
  const s = await page.evaluate("__e2e.state()");
  return { gen: s.gen, calls: s.calls, committedAt };
}

function fifthDeny() {
  return record({ ts: Date.now(), engine: "claude-code", event: "pre_tool", native: "PreToolUse", tool: "Bash",
    verdict: "deny", hops: [[GUARDS_FIFTH, "deny"]] });
}

const guardsState = `__e2e.groupOf(${JSON.stringify(GUARDS_DENY[0])})`;

async function check5(page, env) {
  const s0 = await openGuardsExpanded(page, env);
  append(env.stateDir, [fifthDeny()]);
  await page.waitFor(`__e2e.state().calls === ${s0.calls + 1}`, RELAYOUT_MS - 1000, "the live call counted");
  const pre = await page.evaluate(`({ state: __e2e.state(), g: ${guardsState}, hdr: __e2e.groupHeaderPoint(${JSON.stringify(GUARDS_DENY[0])}) })`);
  assert(pre.state.gen === s0.gen, `a relayout landed before the click (gen ${s0.gen} -> ${pre.state.gen}); the harness was too slow`);
  assert(pre.g.expanded, "guards group no longer drawn expanded before the click");
  assert(pre.hdr?.ok, "guards group header not hit-testable");
  const clickAt = Date.now() - s0.committedAt;
  await page.click(pre.hdr.x, pre.hdr.y);
  const post = await page.waitFor(`(() => { const s = __e2e.state(); return s.gen > ${s0.gen} && !s.pending ? { state: s, g: ${guardsState} } : null; })()`,
    RELAYOUT_MS + 3000, "the commit after the click");
  assert(!post.g.expanded, `guards group drawn expanded after the click's commit (gen ${s0.gen} -> ${post.state.gen}): the click read the live expansion, not the drawn one`);
  return [
    `guards drawn expanded at gen ${s0.gen}; 5th member (${GUARDS_FIFTH}) deny counted, gen unchanged`,
    `header clicked ${clickAt} ms after the commit (< ${RELAYOUT_MS} ms coalesced relayout), gen still ${pre.state.gen}`,
    `next commit gen ${post.state.gen}: guards collapsed (members ${post.g.members.length}, n ${post.g.n})`,
  ];
}

async function check6(page, env) {
  const s0 = await openGuardsExpanded(page, env);
  const hdr = await page.evaluate(`__e2e.groupHeaderPoint(${JSON.stringify(GUARDS_DENY[0])})`);
  assert(hdr?.ok, "guards group header not hit-testable");
  await page.press(hdr.x, hdr.y);
  const pressedAt = Date.now();
  append(env.stateDir, [fifthDeny()]);

  const holdMs = RELAYOUT_MS + 1500;
  let maxGen = s0.gen;
  let pendingSince = null;
  let counted = false;
  while (Date.now() - pressedAt < holdMs) {
    const s = await page.evaluate("__e2e.state()");
    maxGen = Math.max(maxGen, s.gen);
    if (s.calls === s0.calls + 1) counted = true;
    if (s.pending && pendingSince === null) pendingSince = Date.now() - pressedAt;
    if (!s.pending) pendingSince = null;
    await sleep(100);
  }
  const held = Date.now() - pressedAt;
  const atRelease = await page.evaluate("__e2e.state()");
  maxGen = Math.max(maxGen, atRelease.gen);
  assert(held < PRESS_HOLD_MAX_MS, `held ${held} ms, past the ${PRESS_HOLD_MAX_MS} ms safety release`);
  await page.release(hdr.x, hdr.y);

  assert(counted, "the 5th member's live call was never counted during the press");
  assert(maxGen === s0.gen, `data-layout-gen advanced while pressed: ${s0.gen} -> ${maxGen}`);
  assert(atRelease.pending && pendingSince !== null && held - pendingSince >= 500,
    `no relayout held mid-press (pending at release: ${atRelease.pending}, pending since ${pendingSince} ms)`);

  const post = await page.waitFor(`(() => { const s = __e2e.state(); return s.gen > ${s0.gen} && !s.pending ? { state: s, g: ${guardsState} } : null; })()`,
    5000, "the commit after release");
  assert(!post.g.expanded, `guards group drawn expanded after release (gen ${s0.gen} -> ${post.state.gen}): the toggle was not relative to the drawn state at press`);
  return [
    `pressed the guards header at gen ${s0.gen}; relayout pending from ${pendingSince} ms; held ${held} ms (> ${RELAYOUT_MS} ms)`,
    `data-layout-gen stayed ${s0.gen} while pressed`,
    `after release: gen ${post.state.gen}, guards collapsed (toggled from the drawn expanded state)`,
  ];
}

async function check7(page, env) {
  await openFlow(page, env.base, "day=" + env.past);
  const meta = await page.evaluate("__e2e.meta()");
  assert(meta.includes("whole day"), `meta "${meta}" does not say whole day`);
  const t = await totalsMatch(page, env, "", env.past);

  let maxDots = 0;
  for (let i = 0; i < 20; i++) {
    if (i % 5 === 0) append(env.stateDir, TODAY_PATHS.slice(0, 5).map((p) => record({ ...p, ts: Date.now() })));
    maxDots = Math.max(maxDots, (await page.evaluate("__e2e.state()")).dots);
    await sleep(100);
  }
  assert(maxDots === 0, `data-active-dots reached ${maxDots} on a past day`);
  return [
    `meta "${meta}"`,
    `totals match on ${env.past}: calls ${t.calls}, branches ${t.branches}`,
    "data-active-dots stayed 0 for 2 s while today's file grew",
  ];
}

// Column header nodes exist only so ELK sizes/positions them; they must
// never behave like graph nodes (bridge review: HIGH).
async function check8(page, env) {
  await openFlow(page, env.base);
  const lines = [];
  const errorsAt = page.errors.length;
  const searchBefore = await page.evaluate("location.search");

  for (let i = 0; i < 4; i++) {
    const p = await page.evaluate(`__e2e.headerPoint(${i})`);
    assert(p?.ok, `header ${i} not hit-testable: ` + JSON.stringify(p));
    await page.click(p.x, p.y);
  }
  await sleep(200);

  const errors = page.errors.slice(errorsAt);
  assert(errors.length === 0, `${errors.length} console errors/exceptions after clicking the 4 column headers:\n      ` + errors.join("\n      "));
  const searchAfter = await page.evaluate("location.search");
  assert(searchAfter === searchBefore, `URL changed after header clicks: "${searchBefore}" -> "${searchAfter}"`);
  const chips = await page.evaluate("__e2e.chips()");
  assert(chips.length === 0, "chips appeared after header clicks: " + JSON.stringify(chips));
  lines.push("clicked all 4 column headers: no console errors/exceptions, URL unchanged, no chips");

  const hp = await page.evaluate("__e2e.headerPoint(0)");
  await page.move(hp.x, hp.y);
  await sleep(200);
  const hl = await page.evaluate(`document.getElementById("flow-body").classList.contains("hl")`);
  assert(!hl, "#flow-body has class hl after hovering a header");
  lines.push("hovered the engine header: #flow-body has no hl class");
  return lines;
}

async function switchDay(page, day) {
  await page.evaluate(`(() => {
    const sel = document.getElementById("day-select");
    sel.value = ${JSON.stringify(day)};
    sel.dispatchEvent(new Event("change", { bubbles: true }));
  })()`);
}

async function assertPulsesGone(page, what) {
  try {
    await page.waitFor("__e2e.state().dots === 0 && __e2e.activeCircles() === 0", 1000, what);
  } catch {
    const s = await page.evaluate("__e2e.state()");
    const active = await page.evaluate("__e2e.activeCircles()");
    throw new CheckFailed(`data-active-dots ${s.dots}, ${active} circle(s) with r > 0, 1 s after ${what}`);
  }
}

// In-flight pulses must not freeze mid-edge when animation stops without a
// relayout (bridge review: HIGH).
async function check9(page, env) {
  const lines = [];

  // Phase 1: live day, show idle on -- idle nodes come from the (day-
  // independent) handler table, so the past day's plan key equals today's
  // unchanged and no relayout bumps layoutGen on the switch.
  await openFlow(page, env.base);
  const idle = await page.evaluate(`__e2e.hit(document.querySelector(".flow-idle input"))`);
  assert(idle.ok, "show-idle checkbox not hit-testable");
  await page.click(idle.x, idle.y);
  await page.waitFor("__e2e.settled()", 5000, "relayout after show-idle on");
  const genAfterIdle = (await page.evaluate("__e2e.state()")).gen;

  append(env.stateDir, TODAY_PATHS.slice(0, 6).map((p) => record({ ...p, ts: Date.now() })));
  await page.waitFor("__e2e.state().dots > 0", 3000, "live dots active before the day switch");
  const before = await page.evaluate("__e2e.state()");

  await switchDay(page, env.past);
  await assertPulsesGone(page, `switching #day-select to ${env.past}`);
  const genAfterSwitch = (await page.evaluate("__e2e.state()")).gen;
  assert(genAfterSwitch === genAfterIdle, `layout gen moved ${genAfterIdle} -> ${genAfterSwitch} on the day switch; phase 1 no longer tests the no-relayout path`);
  lines.push(`live day, show idle on: ${before.dots} dot(s) in flight -> switched #day-select to ${env.past} -> 0 dots, 0 active circles within 1 s`);

  // Phase 2: dots in flight, tab away and back before any of them finish.
  await openFlow(page, env.base);
  append(env.stateDir, TODAY_PATHS.slice(0, 6).map((p) => record({ ...p, ts: Date.now() })));
  await page.waitFor("__e2e.state().dots > 0", 3000, "live dots active before the tab switch");

  const feedTab = await page.evaluate(`__e2e.hit(document.querySelector('[data-view="feed"]'))`);
  const flowTab = await page.evaluate(`__e2e.hit(document.querySelector('[data-view="flow"]'))`);
  assert(feedTab.ok && flowTab.ok, "view-toggle tabs not hit-testable");
  await page.click(feedTab.x, feedTab.y);
  await page.click(flowTab.x, flowTab.y);
  await assertPulsesGone(page, "clicking the feed tab then the flow tab");
  lines.push("dots in flight -> click feed tab -> click flow tab -> 0 dots, 0 active circles within 1 s");
  return lines;
}

const CHECKS = [
  { n: 1, title: "edge totals == /api/flow derivation (4 filter sets)", fn: check1 },
  { n: 2, title: "zoom, pan, fit, minimap", fn: check2 },
  { n: 3, title: "node click filters, shift-click adds, re-click and chip × remove", fn: check3 },
  { n: 4, title: "burst of 500 live calls stays bounded", fn: check4 },
  { n: 5, title: "#96 window 1: header click before the coalesced relayout", fn: check5, fresh: true },
  { n: 6, title: "#96 window 2: relayout ready mid-press is held", fn: check6, fresh: true },
  { n: 7, title: "static past day: no pulses, whole-day meta", fn: check7 },
  { n: 8, title: "column headers are not interactive graph nodes", fn: check8 },
  { n: 9, title: "in-flight pulses cancel on a day switch or a view toggle", fn: check9, fresh: true },
];

async function main() {
  const only = process.argv.slice(2).map(Number);
  const selected = CHECKS.filter((c) => only.length === 0 || only.includes(c.n));
  const started = Date.now();

  const bin = await buildBinary();
  const browser = await Browser.launch();
  const page = await browser.newPage();
  await installHelpers(page);

  let shared = null;
  let failed = 0;
  try {
    for (const c of selected) {
      const t0 = Date.now();
      let env;
      try {
        if (c.fresh) env = await startEnv(bin);
        else env = shared ??= await startEnv(bin);
        const lines = await c.fn(page, env);
        console.log(`PASS ${c.n} ${c.title} (${((Date.now() - t0) / 1000).toFixed(1)} s)`);
        for (const l of lines) console.log("     " + l);
      } catch (err) {
        failed++;
        console.log(`FAIL ${c.n} ${c.title} (${((Date.now() - t0) / 1000).toFixed(1)} s)`);
        console.log("     " + (err instanceof CheckFailed ? err.message : err.stack));
      } finally {
        if (c.fresh) env?.kill();
      }
    }
  } finally {
    await browser.close();
    runCleanups();
  }
  const secs = ((Date.now() - started) / 1000).toFixed(1);
  console.log(`${selected.length - failed}/${selected.length} checks passed in ${secs} s`);
  process.exitCode = failed > 0 ? 1 : 0;
}

main().catch((err) => {
  console.error(err.stack ?? err);
  runCleanups();
  process.exit(2);
});
