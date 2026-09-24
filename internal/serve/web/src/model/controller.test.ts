// Sensitivity note: swapping toggleGroup's record for the live
// `this.model.groupState(name)` makes "window 1" below fail — the live state
// already says "collapsed" (5 shown members) while the drawn group is still
// expanded, so the click sets userExpanded = true and the group never
// collapses. Checked once by hand when this suite was written.

import assert from "node:assert/strict";
import { test } from "node:test";
import { PENDING_CAP, RENDER_MS } from "../constants.ts";
import { edgeKey, nodeKey } from "./keys.ts";
import {
  BUCKET_START, DAY, entry, flow, GUARDS, harness, loadAndCommit, settle,
} from "./testkit.ts";
import type { Call, Harness } from "./testkit.ts";

const G = nodeKey("group", "guards");
const TS = DAY + "T12:00:31Z";
const deny = (h: string): Call => ({ hops: [[h, "deny"]], verdict: "deny" });
const live = (paths: [Call, number][]) => flow(paths, { window: 10, bucketStart: BUCKET_START });

async function visible(h: Harness): Promise<void> {
  await h.c.setVisible(true);
}

// Under outcome=deny exactly four guards members have traffic, so the group
// auto-opens; a fifth member's live call flips the live default.
async function fourDenies(): Promise<Harness> {
  const h = harness();
  h.filters.outcome = ["deny"];
  h.setFlow(live(GUARDS.slice(0, 4).map((m): [Call, number] => [deny(m), 1])));
  await visible(h);
  await loadAndCommit(h);
  assert.deepEqual(h.c.snapshot()?.groupState("guards"), { expanded: true, forced: false });
  return h;
}

test("load: live fetch params, commit, meta; a past day is whole-day", async () => {
  const h = harness();
  h.filters.engine = ["claude"];
  h.setFlow({ ...live([[{ hops: [["lint", "allow"]] }, 2]]) });
  await visible(h);
  await h.c.sync({ day: DAY, live: true });
  const params = new URLSearchParams(h.flowURLs[0].split("?")[1]);
  assert.equal(params.get("day"), DAY);
  assert.equal(params.get("window"), "10");
  assert.equal(params.get("engine"), "claude");
  assert.equal(h.c.layoutGen(), 1);
  assert.ok(!h.c.pendingLayout());
  assert.equal(h.c.meta(), "live · last 10 min · 2 calls · 2 branches");
  assert.equal(h.c.frame()?.snap.gen, 1);

  const p = harness();
  p.setFlow({ ...flow([[{ verdict: "allow" }, 3]]), paths: null });
  await visible(p);
  await loadAndCommit(p, false);
  assert.equal(new URLSearchParams(p.flowURLs[0].split("?")[1]).get("window"), null);
  assert.equal(p.c.meta(), "whole day · 0 calls · 0 branches");
});

test("before any load nothing is laid out", async () => {
  const h = harness();
  await visible(h);
  h.c.setShowIdle(true);
  h.c.toggleGroup(G);
  assert.equal(h.c.layoutGen(), 0);
  assert.equal(h.c.frame(), null);
  assert.equal(h.c.meta(), "");
});

test("sync while hidden only marks stale; showing loads", async () => {
  const h = harness();
  await h.c.sync({ day: DAY, live: true });
  assert.equal(h.flowURLs.length, 0);
  await h.c.setVisible(true);
  assert.equal(h.flowURLs.length, 1);
});

test("a failed load reports the error and refetches on the next sync", async () => {
  const h = harness();
  await visible(h);
  h.failFlow(new Error("/api/flow: 500"));
  await h.c.sync({ day: DAY, live: true });
  assert.equal(h.c.error(), "flow: /api/flow: 500");
  h.failFlow(null);
  await loadAndCommit(h);
  assert.equal(h.c.error(), "");
  assert.equal(h.c.layoutGen(), 1);
});

test("live call: offset dedupe, hits pruning, merge into the server's path", async () => {
  const h = await fourDenies();
  const before = h.c.counts().calls;
  assert.deepEqual(h.c.onCall(entry(1000, deny(GUARDS[0]), TS)), []); // at the cursor: already counted
  const rec: Call = { hops: [["lint", "allow"], [GUARDS[0], "deny"]], verdict: "deny" };
  const ready = h.c.onCall(entry(1001, rec, TS, [1]));
  assert.deepEqual(ready.map((b) => b.nodes[2]), [["handler", GUARDS[0]]]);
  assert.equal(h.c.model.paths.size, 4, "merged into the server's pruned path");
  h.clock.advance(RENDER_MS);
  assert.equal(h.c.counts().calls, before + 1);
  assert.deepEqual(h.c.onCall(entry(1001, rec, TS, [1])), []);
});

test("calls during a load are buffered and replayed; overflow marks stale and reloads", async () => {
  const h = harness();
  h.setFlow(live([[deny(GUARDS[0]), 1]]));
  await visible(h);
  await loadAndCommit(h);
  const load = h.c.sync({ day: DAY, live: true });
  assert.deepEqual(h.c.onCall(entry(1001, deny(GUARDS[0]), TS)), []);
  await load;
  assert.equal(h.c.counts().calls, 2);

  const again = h.c.sync({ day: DAY, live: true });
  for (let i = 0; i <= PENDING_CAP; i++) h.c.onCall(entry(2000 + i, deny(GUARDS[0]), TS));
  const urls = h.flowURLs.length;
  await again;
  await settle();
  assert.equal(h.flowURLs.length, urls + 1, "overflow reloads once the load lands");
});

test("count refreshes are throttled to one per RENDER_MS", async () => {
  const h = await fourDenies();
  const v = h.c.getVersion();
  let notified = 0;
  h.c.subscribe(() => notified++);
  for (let i = 0; i < 3; i++) h.c.onCall(entry(1001 + i, deny(GUARDS[0]), TS));
  assert.equal(h.c.getVersion(), v);
  h.clock.advance(RENDER_MS);
  assert.equal(h.c.getVersion(), v + 1);
  assert.equal(notified, 1);
  assert.equal(h.c.counts().calls, 7);
  assert.equal(h.c.frame()?.totals.calls, 7);
});

test("pulses: drawn branches only, only while visible", async () => {
  const h = await fourDenies();
  const got: string[][] = [];
  h.c.onPulse((bs) => { for (const b of bs) got.push(h.c.snapshot()?.displayNodes(b) ?? []); });
  h.c.onCall(entry(1001, deny(GUARDS[1]), TS));
  assert.deepEqual(got, [[nodeKey("engine", "claude"), nodeKey("event", "PreToolUse"), nodeKey("handler", GUARDS[1]), nodeKey("outcome", "deny")]]);
  assert.deepEqual(h.c.onCall(entry(1002, { engine: "codex", ...deny(GUARDS[1]) }, TS)), []);
  assert.ok(h.c.model.isBusy("engine", "codex"), "undrawn node is sticky-shown");
  await h.c.setVisible(false);
  assert.equal(h.c.onCall(entry(1003, deny(GUARDS[1]), TS)).length, 1);
  assert.equal(got.length, 1);
});

test("a new edge between drawn nodes shows at once, without a relayout", async () => {
  const h = harness();
  h.setFlow(live([[{ hops: [["lint", "allow"]] }, 1], [{ verdict: "deny" }, 1]]));
  await visible(h);
  await loadAndCommit(h);
  const e = edgeKey(nodeKey("handler", "lint"), nodeKey("outcome", "deny"));
  assert.ok(!h.c.frame()?.totals.edgeTotals.has(e));
  h.c.onCall(entry(1001, { hops: [["lint", "deny"]] }, TS));
  h.clock.advance(RENDER_MS);
  assert.equal(h.c.frame()?.totals.edgeTotals.get(e), 1);
  assert.deepEqual([...h.c.frame()?.totals.edgeOutcomes.get(e) ?? []], [["deny", 1]]);
  h.clock.advance(10_000);
  assert.equal(h.c.layoutGen(), 1);
});

test("live node additions coalesce: two within 5 s -> one layout", async () => {
  const h = await fourDenies();
  h.clock.advance(1000);
  h.c.onCall(entry(1001, { engine: "codex", ...deny(GUARDS[0]) }, TS));
  h.clock.advance(1000);
  h.c.onCall(entry(1002, { engine: "cursor", ...deny(GUARDS[0]) }, TS));
  assert.equal(h.c.layoutGen(), 1);
  h.clock.advance(2999);
  assert.equal(h.c.layoutGen(), 1);
  h.clock.advance(1);
  assert.equal(h.c.layoutGen(), 2);
  const snap = h.c.snapshot();
  assert.ok(snap?.has(nodeKey("engine", "codex")) && snap.has(nodeKey("engine", "cursor")));
});

test("a live node addition long after the last commit lays out at once", async () => {
  const h = await fourDenies();
  h.clock.advance(6000);
  h.c.onCall(entry(1001, { engine: "codex", ...deny(GUARDS[0]) }, TS));
  assert.equal(h.c.layoutGen(), 2);
});

test("a relayout held by a press: back to what is drawn retires it; the latest wins", async () => {
  const h = await fourDenies();
  h.c.pressBegin();
  h.c.setShowIdle(true);
  assert.ok(h.c.pendingLayout());
  h.c.setShowIdle(false); // back to what is drawn
  assert.ok(!h.c.pendingLayout());
  h.c.pressEnd();
  assert.equal(h.c.layoutGen(), 1);

  h.c.pressBegin();
  h.c.toggleGroup(G);
  h.c.setShowIdle(true); // a different node set: supersedes the toggle's plan
  assert.equal(h.c.layoutGen(), 1);
  h.c.pressEnd();
  assert.equal(h.c.layoutGen(), 4, "gens 2 and 3 were planned, never drawn");
  assert.deepEqual(h.c.snapshot()?.groupState("guards"), { expanded: false, forced: false });
  assert.ok(h.c.snapshot()?.has(nodeKey("engine", "pi")), "the idle nodes are drawn too");
});

test("a press holds the drawn frame; counts move; the release publishes the latest", async () => {
  const h = await fourDenies();
  const drawn = h.c.frame();
  h.c.pressBegin();
  h.c.onCall(entry(1001, deny(GUARDS[0]), TS));
  h.clock.advance(RENDER_MS);
  assert.equal(h.c.counts().calls, 5);
  assert.equal(h.c.frame(), drawn, "the drawn frame is held");
  h.c.pressEnd();
  assert.equal(h.c.frame()?.totals.calls, 5);
});

test("window 1: a click before the coalesced relayout toggles what is drawn", async () => {
  const h = await fourDenies();
  h.clock.advance(1000);
  assert.deepEqual(h.c.onCall(entry(1001, deny(GUARDS[4]), TS, [0])), []);
  assert.equal(h.c.model.groupState("guards").expanded, false, "live default flipped to collapsed");
  assert.equal(h.c.layoutGen(), 1, "relayout still coalescing");
  assert.ok(!h.c.pendingLayout());
  assert.equal(h.c.snapshot()?.groupState("guards")?.expanded, true, "still drawn expanded");

  h.c.pressBegin(G);
  h.c.toggleGroup(G);
  h.c.pressEnd();
  assert.equal(h.c.model.userExpanded.get("guards"), false);
  assert.equal(h.c.layoutGen(), 2);
  assert.deepEqual(h.c.snapshot()?.groupState("guards"), { expanded: false, forced: false });
  h.clock.advance(10_000);
  assert.equal(h.c.layoutGen(), 2, "the coalesced live relayout was folded in");
});

test("window 2: no commit mid-press; the toggle uses the pressed record", async () => {
  const h = await fourDenies();
  h.c.pressBegin(G);
  h.c.onCall(entry(1001, deny(GUARDS[4]), TS, [0]));
  h.clock.advance(6000); // past RELAYOUT_MS: the live relayout runs
  assert.equal(h.c.layoutGen(), 1, "held while pressed");
  assert.ok(h.c.pendingLayout());
  assert.equal(h.c.snapshot()?.groupState("guards")?.expanded, true);
  assert.equal(h.c.counts().calls, 5, "the live call is counted during the press");

  h.c.toggleGroup(G); // drawn (and pressed) expanded -> collapse, same as the held relayout
  assert.equal(h.c.model.userExpanded.get("guards"), false);
  assert.ok(h.c.pendingLayout(), "held relayout already matches");
  h.c.pressEnd();
  assert.equal(h.c.layoutGen(), 2);
  assert.deepEqual(h.c.snapshot()?.groupState("guards"), { expanded: false, forced: false });
});

test("a missed release ends the press after PRESS_HOLD_MAX_MS", async () => {
  const h = await fourDenies();
  h.c.pressBegin();
  h.c.setShowIdle(true);
  assert.equal(h.c.layoutGen(), 1);
  h.clock.advance(10_000);
  assert.equal(h.c.layoutGen(), 2);
});

test("a forced group's toggle is a no-op", async () => {
  const h = harness();
  h.filters.handler = [GUARDS[0]];
  h.setFlow(live([[deny(GUARDS[0]), 1]]));
  await visible(h);
  await loadAndCommit(h);
  assert.deepEqual(h.c.snapshot()?.groupState("guards"), { expanded: true, forced: true });
  h.c.pressBegin(G);
  h.c.toggleGroup(G);
  h.c.pressEnd();
  h.c.toggleGroup(G);
  assert.equal(h.c.model.userExpanded.size, 0);
  assert.equal(h.c.layoutGen(), 1);
});

test("userExpanded resets on a handler/outcome change, not on an engine change", async () => {
  const h = await fourDenies();
  h.c.toggleGroup(G);
  h.filters.engine = ["claude"];
  await loadAndCommit(h);
  assert.equal(h.c.model.userExpanded.get("guards"), false);
  assert.equal(h.c.snapshot()?.groupState("guards")?.expanded, false);
  h.filters.outcome = ["deny", "ask"];
  await loadAndCommit(h);
  assert.equal(h.c.model.userExpanded.size, 0);
  assert.equal(h.c.snapshot()?.groupState("guards")?.expanded, true, "back to the auto-open default");
});

test("the ring tick ages live counts out of the window", async () => {
  const h = await fourDenies();
  assert.equal(h.c.counts().calls, 4);
  h.clock.advance(10 * 60_000);
  assert.equal(h.c.counts().calls, 0);
  h.c.dispose();
});
