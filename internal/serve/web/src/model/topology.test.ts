import assert from "node:assert/strict";
import { test } from "node:test";
import { OUTCOMES, ROUTER_ERROR } from "../constants.ts";
import { pathBranches } from "./branches.ts";
import { edgeKey, nodeKey } from "./keys.ts";
import { eventLabel, pathOf } from "./testkit.ts";
import { buildTopology, columnNames, observeNode, observePath } from "./topology.ts";

test("buildTopology: null lists are empty; outcomes are the known list + router-error", () => {
  const t = buildTopology({ handlers: null, engines: null, events: null });
  assert.deepEqual(columnNames(t, "engine"), []);
  assert.deepEqual(columnNames(t, "handler"), []);
  assert.deepEqual(columnNames(t, "outcome"), [...OUTCOMES, ROUTER_ERROR]);
  assert.equal(buildTopology(null).skeletonEdges.size, 0);
});

test("buildTopology: engine-scoped events appended; skeleton edges scoped by engine", () => {
  const t = buildTopology({
    engines: ["claude", "pi"],
    events: ["Stop"],
    handlers: [{ id: "h", engines: ["claude", "pi"], events: ["Stop", "pi:turn_end"] }, { id: "k", engines: null, events: null }],
  });
  assert.deepEqual(t.eventOrder, ["Stop", "pi:turn_end"]);
  const k = (a: string, b: string) => edgeKey(a, b);
  assert.ok(t.skeletonEdges.has(k(nodeKey("engine", "claude"), nodeKey("event", "Stop"))));
  assert.ok(t.skeletonEdges.has(k(nodeKey("engine", "pi"), nodeKey("event", "pi:turn_end"))));
  assert.ok(!t.skeletonEdges.has(k(nodeKey("engine", "claude"), nodeKey("event", "pi:turn_end"))));
  assert.ok(t.skeletonEdges.has(k(nodeKey("event", "pi:turn_end"), nodeKey("handler", "h"))));
  assert.deepEqual(t.handlerOrder, ["h", "k"]);
});

test("observePath reports node-set changes apart from edge-only changes", () => {
  const t = buildTopology({ engines: ["claude"], events: ["PreToolUse"], handlers: [{ id: "a" }, { id: "b" }] });
  const br = (hops: [string, string][]) => pathBranches(pathOf({ hops }), eventLabel);
  assert.deepEqual(observePath(t, br([["a", "allow"]])), { nodes: false, edges: true });
  assert.deepEqual(observePath(t, br([["a", "allow"]])), { nodes: false, edges: false });
  assert.deepEqual(observePath(t, br([["b", "allow"]])), { nodes: false, edges: true });
  assert.deepEqual(observePath(t, br([["ghost", "allow"]])), { nodes: true, edges: true });
  assert.deepEqual(columnNames(t, "handler"), ["a", "b", "ghost"]);
  assert.equal(observeNode(t, "handler", "ghost"), false);
});
