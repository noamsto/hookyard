import assert from "node:assert/strict";
import { test } from "node:test";
import type { Filters } from "../types.ts";
import { nodeKey } from "./keys.ts";
import { emptyFilters, FlowModel } from "./state.ts";
import { eventLabel, flow, GUARDS, pathOf, table } from "./testkit.ts";
import type { Call } from "./testkit.ts";

function model(paths: [Call, number][], filters: Partial<Filters> = {}): FlowModel {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow(paths), { ...emptyFilters(), ...filters });
  return m;
}

const deny = (h: string): Call => ({ hops: [[h, "deny"]], verdict: "deny" });

test("visibility: facet, active value, sticky, raw drawn totals, show idle", () => {
  const m = model([[{ hops: [["lint", "allow"]] }, 2]], { engine: ["cursor"] });
  assert.ok(m.isBusy("handler", "lint")); // facet
  assert.ok(m.isBusy("engine", "cursor")); // active filter value, no traffic
  assert.ok(!m.isBusy("engine", "codex"));
  m.stickyShow("engine", "codex");
  assert.ok(m.isBusy("engine", "codex"));
  assert.ok(!m.isShown("handler", "guards.a"));
  m.showIdle = true;
  assert.ok(m.isShown("handler", "guards.a"));
  assert.ok(!m.isBusy("handler", "guards.a"));
});

test("visibility: a drawn node whose facet key disagrees stays shown via raw totals", () => {
  const resp = flow([[{ router: "error", event: "", native: "" }, 1]]);
  delete resp.facets.event["—"]; // Go's event facet skips the "" label
  const m = new FlowModel(eventLabel);
  m.load(table(), resp, emptyFilters());
  assert.ok(!m.isBusy("event", "—"));
  m.refreshRawTotals();
  assert.ok(m.isBusy("event", "—"));
  assert.ok(m.isBusy("outcome", "router-error"));
});

test("groupState: forced > user > auto (<= 4 shown under a branch filter)", () => {
  const four = GUARDS.slice(0, 4).map((h): [Call, number] => [deny(h), 1]);
  const m = model(four, { outcome: ["deny"] });
  assert.deepEqual(m.groupState("guards"), { expanded: true, forced: false });
  m.stickyShow("handler", GUARDS[4]);
  assert.deepEqual(m.groupState("guards"), { expanded: false, forced: false });
  m.userExpanded.set("guards", true);
  assert.deepEqual(m.groupState("guards"), { expanded: true, forced: false });

  const f = model(four, { handler: [GUARDS[0]] });
  f.userExpanded.set("guards", false);
  assert.deepEqual(f.groupState("guards"), { expanded: true, forced: true });

  const plain = model(four);
  assert.deepEqual(plain.groupState("guards"), { expanded: false, forced: false });
});

test("layoutPlan: groups, members with parent, ungrouped handlers, group before members", () => {
  const m = model([[deny(GUARDS[0]), 3], [deny(GUARDS[1]), 1], [{ hops: [["lint", "allow"]] }, 1]], { outcome: ["deny"] });
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  const keys = plan.input.nodes.map((n) => n.key);
  const g = nodeKey("group", "guards");
  const gi = keys.indexOf(g);
  assert.ok(gi >= 0);
  assert.deepEqual(keys.slice(gi, gi + 3), [g, nodeKey("handler", GUARDS[0]), nodeKey("handler", GUARDS[1])]);
  assert.equal(plan.input.nodes[gi + 1].parent, g);
  assert.ok(keys.includes(nodeKey("handler", "lint")));
  assert.ok(!keys.includes(nodeKey("group", "aeye"))); // no shown members
  assert.ok(plan.expanded.has("guards"));
  // A kept member ahead of its group in the previous order still follows it.
  const again = m.layoutPlan([nodeKey("handler", GUARDS[1]), nodeKey("outcome", "deny"), g]);
  assert.ok(again);
  const k2 = again.input.nodes.map((n) => n.key);
  assert.deepEqual(k2.slice(0, 4), [nodeKey("outcome", "deny"), g, nodeKey("handler", GUARDS[1]), nodeKey("handler", GUARDS[0])]);
});

test("layoutPlan key: unchanged by counts and new edges, changed by a node", () => {
  const m = model([[{ hops: [["lint", "allow"]] }, 1], [{ verdict: "deny" }, 1]]);
  const k0 = m.layoutPlan([])?.input;
  assert.ok(k0);

  m.recordCall(pathOf({ hops: [["lint", "allow"]] }), "");
  const k1 = m.layoutPlan([])?.input;
  assert.equal(k1?.key, k0.key);

  m.recordCall(pathOf({ hops: [["lint", "deny"]] }), ""); // lint -> deny: a new edge between shown nodes
  assert.equal(m.layoutPlan([])?.input.key, k0.key);

  m.stickyShow("engine", "codex");
  assert.notEqual(m.layoutPlan([])?.input.key, k0.key);
});

test("layoutPlan key: a group's expansion and forced flag change it", () => {
  const m = model([[deny(GUARDS[0]), 1]]);
  const collapsed = m.layoutPlan([])?.input.key;
  m.userExpanded.set("guards", true);
  const expanded = m.layoutPlan([])?.input;
  assert.notEqual(expanded?.key, collapsed);

  // Same nodes and parents, but the group is now held open by the filter.
  const f = model([[deny(GUARDS[0]), 1]], { handler: [GUARDS[0]] });
  const forced = f.layoutPlan([])?.input;
  assert.deepEqual(forced?.nodes, expanded?.nodes);
  assert.notEqual(forced?.key, expanded?.key);
});

test("load resets userExpanded when handler/outcome filters change, keeps it otherwise", () => {
  const m = new FlowModel(eventLabel);
  const resp = flow([[deny(GUARDS[0]), 1]]);
  m.load(table(), resp, { ...emptyFilters(), outcome: ["deny"] });
  m.userExpanded.set("guards", false);
  m.load(table(), resp, { ...emptyFilters(), outcome: ["deny"], engine: ["claude"], session: "s" });
  assert.equal(m.userExpanded.get("guards"), false, "engine/session change keeps toggles");
  m.load(table(), resp, { ...emptyFilters(), outcome: ["deny", "ask"] });
  assert.equal(m.userExpanded.size, 0, "outcome change resets");
  m.userExpanded.set("guards", true);
  m.load(table(), resp, { ...emptyFilters(), outcome: ["ask", "deny"] });
  assert.equal(m.userExpanded.get("guards"), true, "same values in another order keep toggles");
  m.load(table(), resp, { ...emptyFilters(), outcome: ["ask", "deny"], handler: [GUARDS[1]] });
  assert.equal(m.userExpanded.size, 0, "handler change resets");
});

test("idleCount counts every column's not-busy names", () => {
  const m = model([[{ hops: [["lint", "allow"]] }, 1]]);
  // busy: claude, PreToolUse, lint, allow
  const t = table();
  const total = (t.engines?.length ?? 0) + (t.events?.length ?? 0) + (t.handlers?.length ?? 0) + 10;
  assert.equal(m.idleCount(), total - 4);
});
