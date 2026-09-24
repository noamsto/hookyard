import assert from "node:assert/strict";
import { test } from "node:test";
import { edgeKey, nodeKey, pathKey } from "../model/keys.ts";
import { Snapshot } from "../model/snapshot.ts";
import { emptyFilters, FlowModel } from "../model/state.ts";
import { eventLabel, flow, GUARDS, pathOf, table } from "../model/testkit.ts";
import type { Call } from "../model/testkit.ts";
import { buildScene, fanOut, PSEUDO, rankInput, ranksFor, singleOutcome } from "./scene.ts";

function frame(calls: [Call, number][], expand = false) {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow(calls), emptyFilters());
  if (expand) m.userExpanded.set("guards", true);
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  const snap = new Snapshot(plan, eventLabel);
  const paths = [...m.paths.values()];
  return { snap, totals: snap.totals(paths), paths };
}

const EV = nodeKey("event", "PreToolUse");
const G = nodeKey("group", "guards");

test("scene: a direct verdict and a router error route through the no-handler slot, keyed by their display edge", () => {
  const s = buildScene(frame([[{ verdict: "abstain" }, 2], [{ router: "error", event: "", native: "PreToolUse" }, 1], [{ hops: [[GUARDS[0], "deny"]] }, 1]]));
  const direct = s.links.filter((l) => l.edge === edgeKey(EV, nodeKey("outcome", "abstain")));
  assert.deepEqual(direct.map((l) => [l.from, l.to, l.count]), [[EV, PSEUDO, 2], [PSEUDO, nodeKey("outcome", "abstain"), 2]]);
  const pseudo = s.nodes.find((n) => n.key === PSEUDO);
  assert.equal(pseudo?.total, 3);
  assert.ok(s.links.some((l) => l.from === EV && l.to === G && l.outcome === "deny"));
});

test("scene: an open group is a fold header, its members the nodes; members rank as one block", () => {
  const f = frame([[{ hops: [[GUARDS[0], "deny"], [GUARDS[1], "allow"]] }, 1]], true);
  const s = buildScene(f);
  assert.deepEqual(s.groups.map((g) => g.key), [G]);
  assert.ok(!s.nodes.some((n) => n.key === G));
  const members = rankInput(s, f.snap.order).filter((n) => n.col === 2);
  assert.deepEqual(members.map((n) => n.group), [G, G]);
});

test("singleOutcome: only when every band shares one outcome", () => {
  assert.equal(singleOutcome(buildScene(frame([[{ hops: [[GUARDS[0], "deny"]] }, 2]])).links), "deny");
  assert.equal(singleOutcome(buildScene(frame([[{ hops: [[GUARDS[0], "deny"], [GUARDS[1], "allow"]] }, 1]])).links), null);
});

test("ranksFor: a count refresh that brings in the no-handler slot keeps every other node's rank", () => {
  const m = new FlowModel(eventLabel);
  const first: [Call, number][] = [[{ hops: [[GUARDS[0], "deny"]] }, 1], [{ hops: [["lint", "abstain"]] }, 5]];
  const later: [Call, number][] = [...first, [{ hops: [["lint", "deny"]] }, 3], [{ verdict: "abstain" }, 2]];
  m.load(table(), flow(later), emptyFilters());
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  const snap = new Snapshot(plan, eventLabel);
  const keep = new Set(first.map(([c]) => pathKey(pathOf(c))));
  const firstPaths = [...m.paths].filter(([k]) => keep.has(k)).map(([, e]) => e);
  const a = buildScene({ snap, totals: snap.totals(firstPaths), paths: firstPaths });
  assert.ok(!a.nodes.some((n) => n.key === PSEUDO));
  const pinned = ranksFor(a, snap.order);
  const paths = [...m.paths.values()];
  const b = buildScene({ snap, totals: snap.totals(paths), paths });
  assert.ok(b.nodes.some((n) => n.key === PSEUDO));
  const rank = ranksFor(b, snap.order, pinned);
  const byRank = (r: Map<string, number>, col: number) =>
    b.nodes.filter((n) => n.col === col && n.key !== PSEUDO).sort((x, y) => (r.get(x.key) ?? 0) - (r.get(y.key) ?? 0)).map((n) => n.key);
  for (const col of [0, 1, 2, 3]) assert.deepEqual(byRank(rank, col), byRank(pinned, col), "column " + col + " order");
  const col2 = b.nodes.filter((n) => n.col === 2 && n.key !== PSEUDO).map((n) => rank.get(n.key) ?? NaN);
  assert.ok((rank.get(PSEUDO) ?? -1) > Math.max(...col2), "the no-handler slot goes last in its column");
});

function eventOf(paths: [Call, number][], event = "PreToolUse") {
  const f = frame(paths);
  const key = nodeKey("event", event);
  const n = buildScene(f).nodes.find((x) => x.key === key);
  assert.ok(n);
  return { fan: fanOut(n), fact: f.snap.tooltip(key, f.totals, f.totals)?.facts[0] };
}

test("fan-out: only handler runs count, per call that ran a handler; plate and tooltip agree", () => {
  const routerError = eventOf([[{ router: "error", event: "", native: "PreToolUse" }, 3]]);
  assert.deepEqual(routerError, { fan: "", fact: "3 calls → 0 handler runs" });
  const mixed = eventOf([[{ verdict: "abstain" }, 4], [{ hops: [[GUARDS[0], "deny"], ["lint", "allow"]] }, 2]]);
  assert.deepEqual(mixed, { fan: " ×2.0", fact: "6 calls → 4 handler runs on 2 (×2.0)" });
  const oneEach = eventOf([[{ verdict: "abstain" }, 4], [{ hops: [["lint", "allow"]] }, 2]]);
  assert.deepEqual(oneEach, { fan: "", fact: "6 calls → 2 handler runs on 2 (×1.0)" });
  const allFan = eventOf([[{ hops: [[GUARDS[0], "deny"], ["lint", "allow"]] }, 3]]);
  assert.deepEqual(allFan, { fan: " ×2.0", fact: "3 calls → 6 handler runs (×2.0)" });
});
