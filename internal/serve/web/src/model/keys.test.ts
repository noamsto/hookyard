import assert from "node:assert/strict";
import { test } from "node:test";
import { branchKey, consecutivePairs, edgeKey, nodeKey, pathKey, splitEdgeKey, splitKey } from "./keys.ts";
import { pathOf } from "./testkit.ts";

test("node and edge keys round-trip", () => {
  const a = nodeKey("group", "guards.pi");
  assert.deepEqual(splitKey(a), ["group", "guards.pi"]);
  const b = nodeKey("outcome", "deny");
  assert.deepEqual(splitEdgeKey(edgeKey(a, b)), [a, b]);
});

test("pathKey: null and [] handlers merge; a hop's outcome or order splits", () => {
  const base = pathOf({ verdict: "allow" });
  assert.equal(pathKey({ ...base, handlers: null }), pathKey({ ...base, handlers: [] }));
  const one = pathOf({ hops: [["x", "allow"], ["y", "deny"]] });
  assert.notEqual(pathKey(one), pathKey(pathOf({ hops: [["x", "allow"], ["y", "allow"]] })));
  assert.notEqual(pathKey(one), pathKey(pathOf({ hops: [["y", "deny"], ["x", "allow"]] })));
  assert.notEqual(pathKey(one), pathKey({ ...one, native_event: "pre" }));
});

test("branchKey and consecutivePairs", () => {
  assert.equal(branchKey({ nodes: [["engine", "pi"], ["event", "e"]] }), "engine\x00pi\x01event\x00e");
  assert.deepEqual(consecutivePairs([1, 2, 3]), [[1, 2], [2, 3]]);
  assert.deepEqual(consecutivePairs([1]), []);
});
