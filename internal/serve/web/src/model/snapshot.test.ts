import assert from "node:assert/strict";
import { test } from "node:test";
import type { Filters } from "../types.ts";
import { pathBranches } from "./branches.ts";
import { edgeKey, nodeKey } from "./keys.ts";
import { outcomeClass, Snapshot } from "./snapshot.ts";
import { emptyFilters, FlowModel } from "./state.ts";
import { eventLabel, fakeResult, flow, GUARDS, pathOf, table } from "./testkit.ts";
import type { Call } from "./testkit.ts";
import { rawEdges } from "./topology.ts";

const TWO: Call = { hops: [[GUARDS[0], "deny"], [GUARDS[1], "allow"]], verdict: "deny" };
const LINT: Call = { hops: [["lint", "allow"]] };

function snap(paths: [Call, number][], opts: { expand?: boolean; filters?: Partial<Filters> } = {}) {
  const m = new FlowModel(eventLabel);
  m.load(table(), flow(paths), { ...emptyFilters(), ...opts.filters });
  if (opts.expand !== undefined) m.userExpanded.set("guards", opts.expand);
  const plan = m.layoutPlan([]);
  assert.ok(plan);
  plan.input.gen = 1;
  const s = new Snapshot(plan, fakeResult(plan.input), eventLabel);
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

test("displayEdges: deduped onto the group, both ends drawn, direct flag", () => {
  const { m, s, totals } = snap([[TWO, 2], [{ verdict: "deny" }, 1], [{ router: "error", event: "", native: "PreToolUse" }, 1]]);
  assert.ok(m.topo);
  const edges = s.displayEdges(rawEdges(m.topo), totals);
  const keys = edges.map((e) => e.key);
  assert.equal(new Set(keys).size, keys.length);
  const intoGroup = edges.find((e) => e.key === E(EV, G));
  assert.equal(intoGroup?.n, 4);
  assert.equal(edges.find((e) => e.key === E(EV, N("outcome", "deny")))?.direct, true);
  assert.equal(edges.find((e) => e.key === E(EV, N("outcome", "router-error")))?.direct, false);
  for (const e of edges) assert.ok(s.has(e.from) && s.has(e.to));
});

test("highlight: every drawn branch through the node; an expanded header targets members", () => {
  const { m, s } = snap([[TWO, 1], [LINT, 1]]);
  const h = s.highlight(N("outcome", "deny"), m.paths.values());
  assert.deepEqual([...h.nodes].sort(), [N("engine", "claude"), EV, G, N("outcome", "deny")].sort());
  assert.ok(h.edges.has(E(G, N("outcome", "deny"))));
  assert.ok(!h.nodes.has(N("handler", "lint")));

  const x = snap([[TWO, 1], [LINT, 1]], { expand: true });
  const hx = x.s.highlight(G, x.m.paths.values());
  assert.ok(hx.nodes.has(N("handler", GUARDS[0])) && hx.nodes.has(N("outcome", "allow")));
  assert.ok(!hx.nodes.has(N("handler", "lint")));
});

test("tooltip: group lines, per-member counts, hints; ghost note; router-error label", () => {
  const { m, s, totals } = snap([[TWO, 3], [{ hops: [["ghosty", "allow"]] }, 1], [{ router: "error", native: "x" }, 1]]);
  const tip = s.tooltip(G, totals, m.paths.values()).map((l) => l.text);
  assert.equal(tip[0], "guards.* (8) — handler group");
  assert.equal(tip[1], "6 branches");
  assert.ok(tip.includes("→ deny  3") && tip.includes("→ allow  3"));
  assert.ok(tip.includes("← PreToolUse  6"));
  assert.ok(tip.includes("  " + GUARDS[0] + "  3"));
  assert.ok(tip.includes("  " + GUARDS[7] + "  0"));
  assert.equal(tip[tip.length - 1], "click: expand/collapse");

  const ghost = s.tooltip(N("handler", "ghosty"), totals, m.paths.values()).map((l) => l.text);
  assert.equal(ghost[1], "1 branches · not in the installed table");
  assert.equal(ghost[ghost.length - 1], "click: filter · shift-click: add");
  assert.equal(s.node(N("outcome", "router-error"))?.tipName, "router error");

  const f = snap([[TWO, 1]], { filters: { handler: [GUARDS[0]] } });
  const ft = f.s.tooltip(G, f.totals, f.m.paths.values());
  assert.deepEqual(ft[ft.length - 1], { text: "held open by the handler filter", cls: "dim" });
  assert.equal(f.s.node(G)?.group?.forced, true);
});

test("outcomeClass only from the known outcome list", () => {
  assert.equal(outcomeClass("deny"), "v-deny");
  assert.equal(outcomeClass("router-error"), "v-router-error");
  assert.equal(outcomeClass("x onmouseover"), "");
});
