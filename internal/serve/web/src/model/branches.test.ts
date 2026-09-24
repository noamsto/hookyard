import assert from "node:assert/strict";
import { test } from "node:test";
import { pathBranches } from "./branches.ts";
import { eventLabel, pathOf } from "./testkit.ts";

test("router error: one branch to router-error, event by native_event", () => {
  const p = pathOf({ router: "error", event: "", native: "PreToolUse", hops: [["x", "allow"]] });
  assert.deepEqual(pathBranches(p, eventLabel), [
    { nodes: [["engine", "claude"], ["event", "PreToolUse"], ["outcome", "router-error"]] },
  ]);
  const blank = pathOf({ router: "error", event: "", native: "" });
  assert.deepEqual(pathBranches(blank, eventLabel)[0].nodes[1], ["event", "—"]);
});

test("no handlers (null or []): one direct branch to the verdict", () => {
  const p = pathOf({ engine: "pi", event: "", native: "turn_end", verdict: "abstain" });
  const want = [{ nodes: [["engine", "pi"], ["event", "pi:turn_end"], ["outcome", "abstain"]] }];
  assert.deepEqual(pathBranches(p, eventLabel), want);
  assert.deepEqual(pathBranches({ ...p, handlers: null }, eventLabel), want);
});

test("multi-handler: one branch per hop, in record order", () => {
  const p = pathOf({ hops: [["guards.a", "deny"], ["lint", "allow"]], verdict: "deny" });
  assert.deepEqual(pathBranches(p, eventLabel), [
    { nodes: [["engine", "claude"], ["event", "PreToolUse"], ["handler", "guards.a"], ["outcome", "deny"]] },
    { nodes: [["engine", "claude"], ["event", "PreToolUse"], ["handler", "lint"], ["outcome", "allow"]] },
  ]);
});
