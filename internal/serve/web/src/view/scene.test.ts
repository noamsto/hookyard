import assert from "node:assert/strict";
import { test } from "node:test";
import { edgeKey, nodeKey } from "../model/keys.ts";
import { Snapshot } from "../model/snapshot.ts";
import { emptyFilters, FlowModel } from "../model/state.ts";
import { eventLabel, flow, GUARDS, table } from "../model/testkit.ts";
import type { Call } from "../model/testkit.ts";
import { buildScene, PSEUDO, rankInput, singleOutcome } from "./scene.ts";

function frame(paths: [Call, number][], expand = false) {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow(paths), emptyFilters());
  if (expand) m.userExpanded.set("guards", true);
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  const snap = new Snapshot(plan, eventLabel);
  return { snap, totals: snap.totals(m.paths.values()) };
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
