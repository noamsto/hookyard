import assert from "node:assert/strict";
import { test } from "node:test";
import type { Filters } from "../types.ts";
import { pathBranches } from "./branches.ts";
import { edgeKey, nodeKey } from "./keys.ts";
import { knownOutcome, Snapshot } from "./snapshot.ts";
import { emptyFilters, FlowModel } from "./state.ts";
import { eventLabel, flow, GUARDS, pathOf, table } from "./testkit.ts";
import type { Call } from "./testkit.ts";

const TWO: Call = { hops: [[GUARDS[0], "deny"], [GUARDS[1], "allow"]], verdict: "deny" };
const LINT: Call = { hops: [["lint", "allow"]] };

function snap(paths: [Call, number][], opts: { expand?: boolean; filters?: Partial<Filters> } = {}) {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow(paths), { ...emptyFilters(), ...opts.filters });
  if (opts.expand !== undefined) m.userExpanded.set("guards", opts.expand);
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  plan.input.gen = 1;
  const s = new Snapshot(plan, eventLabel);
  return { m, s, totals: s.totals(m.paths.values()) };
}

const N = nodeKey;
const E = edgeKey;
const G = N("group", "guards");
const EV = N("event", "PreToolUse");

test("totals, collapsed group: both branches of one call count on the shared edge", () => {
  const { totals: t } = snap([[TWO, 3], [LINT, 1]]);
  assert.equal(t.calls, 4);
  assert.equal(t.branches, 7);
  assert.equal(t.nodeTotals.get(N("engine", "claude")), 4);
  assert.equal(t.nodeTotals.get(EV), 4);
  assert.equal(t.edgeTotals.get(E(N("engine", "claude"), EV)), 4);
  assert.equal(t.nodeTotals.get(G), 6);
  assert.equal(t.edgeTotals.get(E(EV, G)), 6);
  assert.equal(t.edgeTotals.get(E(G, N("outcome", "deny"))), 3);
  assert.equal(t.edgeTotals.get(E(G, N("outcome", "allow"))), 3);
  assert.equal(t.nodeTotals.get(N("handler", GUARDS[0])), undefined);
});

test("totals, expanded group: members count, the header sums its shown members", () => {
  const { totals: t, s } = snap([[TWO, 3], [LINT, 1]], { expand: true });
  assert.equal(t.calls, 4);
  assert.equal(t.branches, 7);
  assert.equal(t.nodeTotals.get(N("handler", GUARDS[0])), 3);
  assert.equal(t.nodeTotals.get(N("handler", GUARDS[1])), 3);
  assert.equal(t.nodeTotals.get(G), 6);
  assert.equal(t.edgeTotals.get(E(EV, N("handler", GUARDS[0]))), 3);
  assert.equal(t.edgeTotals.get(E(EV, G)), undefined);
  const g = s.node(G)?.group;
  assert.deepEqual(g?.shown, [N("handler", GUARDS[0]), N("handler", GUARDS[1])]);
  assert.equal(s.node(N("handler", GUARDS[0]))?.parent, G);
});

test("display mapping is pinned to the snapshot, not the live model", () => {
  const { m, s } = snap([[TWO, 1]], { expand: false });
  m.userExpanded.set("guards", true); // live flips; the snapshot does not
  assert.equal(s.displayKey("handler", GUARDS[0]), G);
  const [b] = pathBranches(pathOf(TWO), eventLabel);
  assert.deepEqual(s.displayNodes(b), [N("engine", "claude"), EV, G, N("outcome", "deny")]);
  assert.ok(s.drawn(b));
  assert.ok(!s.drawn(pathBranches(pathOf({ engine: "codex", hops: [["lint", "allow"]] }), eventLabel)[0]));
  assert.deepEqual(s.groupState("guards"), { expanded: false, forced: false });
  assert.equal(s.groupState("aeye"), undefined);
});

test("outcomes: engine -> event split by call class, later edges by branch outcome", () => {
  const { totals: t } = snap([[TWO, 2], [LINT, 1], [{ verdict: "deny" }, 1], [{ router: "error", event: "", native: "PreToolUse" }, 1]]);
  const ee = E(N("engine", "claude"), EV);
  assert.deepEqual(Object.fromEntries(t.edgeOutcomes.get(ee) ?? []), { deny: 3, allow: 1, "router-error": 1 });
  assert.deepEqual(Object.fromEntries(t.edgeOutcomes.get(E(EV, G)) ?? []), { deny: 2, allow: 2 });
  assert.deepEqual(Object.fromEntries(t.nodeOutcomes.get(EV) ?? []), { deny: 3, allow: 3, "router-error": 1 });
  assert.equal(t.edgeTotals.get(E(EV, N("outcome", "deny"))), 1, "a direct verdict edge");
  assert.equal(t.nodeCalls.get(G), 2, "two branches of one call through the group count one call");
  assert.deepEqual(Object.fromEntries(t.members.get(G)?.get(GUARDS[0]) ?? []), { deny: 2 });
});

test("buckets: per node per ring bucket, only when asked", () => {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow([[TWO, 2]], { window: 3 }), emptyFilters());
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  const s = new Snapshot(plan, eventLabel);
  assert.equal(s.totals(m.paths.values()).buckets.size, 0);
  const b = s.totals(m.paths.values(), true).buckets;
  assert.deepEqual(b.get(EV), [0, 0, 2]);
  assert.deepEqual(b.get(G), [0, 0, 4]);
});

test("subset: every drawn branch through the node; an expanded header targets members", () => {
  const { m, s } = snap([[TWO, 1], [LINT, 1]]);
  const h = s.subset(N("outcome", "deny"), m.paths.values());
  assert.equal(h.calls, 1);
  assert.equal(h.branches, 1);
  assert.equal(h.edgeTotals.get(E(G, N("outcome", "deny"))), 1);
  assert.ok(!h.edgeTotals.has(E(G, N("outcome", "allow"))));
  assert.ok(!h.nodeTotals.has(N("handler", "lint")));
  const e = s.subset(N("engine", "claude"), m.paths.values());
  assert.equal(e.branches, 3, "an engine keeps whole calls");

  const x = snap([[TWO, 1], [LINT, 1]], { expand: true });
  const hx = x.s.subset(G, x.m.paths.values());
  assert.ok(hx.nodeTotals.has(N("handler", GUARDS[0])) && hx.nodeTotals.has(N("outcome", "allow")));
  assert.ok(!hx.nodeTotals.has(N("handler", "lint")));
});

test("tooltip: group outcomes, members with decisions, fed-by, hints; ghost note; router-error label", () => {
  const { m, s, totals } = snap([[TWO, 3], [{ hops: [["ghosty", "allow"]] }, 1], [{ router: "error", native: "x" }, 1]]);
  const tip = s.tooltip(G, totals, s.subset(G, m.paths.values()));
  assert.ok(tip);
  assert.equal(tip.title, "guards.*");
  assert.equal(tip.kind, "handler group · 8");
  assert.deepEqual(tip.facts, ["6 runs on 3 calls"]);
  const [outs, members, fed] = tip.sections;
  assert.deepEqual(outs.rows.map((r) => [r.label, r.n]), [["deny", 3], ["allow", 3]]);
  assert.equal(members.rows.length, 8);
  assert.deepEqual(members.rows[0], { label: "a", n: 3, note: new Map([["deny", 3]]) });
  assert.deepEqual(members.rows[7], { label: "h", n: 0, note: undefined });
  assert.deepEqual(fed.rows, [{ label: "claude · PreToolUse", n: 3 }]);
  assert.equal(tip.hint, "click to expand");

  const ghost = s.tooltip(N("handler", "ghosty"), totals, s.subset(N("handler", "ghosty"), m.paths.values()));
  assert.deepEqual(ghost?.facts, ["1 runs on 1 calls", "not in the installed table"]);
  assert.equal(ghost?.hint, "click to filter · shift-click to add");
  assert.equal(s.node(N("outcome", "router-error"))?.label, "router error");

  const ev = s.tooltip(EV, totals, s.subset(EV, m.paths.values()));
  assert.deepEqual(ev?.facts, ["4 calls → 7 handler runs (×1.8)"]);

  const f = snap([[TWO, 1]], { filters: { handler: [GUARDS[0]] } });
  assert.equal(f.s.tooltip(G, f.totals, f.s.subset(G, f.m.paths.values()))?.hint, "held open by the handler filter");
  assert.equal(f.s.node(G)?.group?.forced, true);
  assert.equal(f.s.memberLabel(N("handler", GUARDS[0])), "a");
});

test("knownOutcome maps anything off the known list to other", () => {
  assert.equal(knownOutcome("deny"), "deny");
  assert.equal(knownOutcome("router-error"), "router-error");
  assert.equal(knownOutcome("x onmouseover"), "other");
});
