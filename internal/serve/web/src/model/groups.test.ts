import assert from "node:assert/strict";
import { test } from "node:test";
import { computeGroups, displayKeyFor, groupLabel, keyDepth, parentKey } from "./groups.ts";
import { nodeKey } from "./keys.ts";
import { AEYE, GUARDS, GUARDS_PI, HOUSTON } from "./testkit.ts";

test("parentKey / keyDepth", () => {
  assert.equal(parentKey("guards.pi.a"), "guards.pi");
  assert.equal(parentKey("aeye-a"), "aeye");
  assert.equal(parentKey("-x"), null);
  assert.equal(parentKey("lint"), null);
  assert.equal(keyDepth("guards.pi"), 2);
});

test("computeGroups: the real prefixes", () => {
  const g = computeGroups([...GUARDS, ...GUARDS_PI, ...AEYE, "lint"]);
  assert.deepEqual(g.members.get("guards"), GUARDS);
  assert.deepEqual(g.members.get("guards.pi"), GUARDS_PI);
  assert.deepEqual(g.members.get("aeye"), AEYE);
  assert.equal(g.groupOf.get("lint"), undefined);
  assert.equal(groupLabel("guards", GUARDS), "guards.*");
  assert.equal(groupLabel("guards.pi", GUARDS_PI), "guards.pi.*");
  assert.equal(groupLabel("aeye", AEYE), "aeye-*");
});

test("computeGroups: houston.state / houston.state.observe meet at the fixed point", () => {
  const g = computeGroups(HOUSTON);
  assert.deepEqual(g.members.get("houston"), HOUSTON);
  assert.equal(g.members.size, 1);
  assert.equal(groupLabel("houston", HOUSTON), "houston.*");
});

test("computeGroups: a lone prefixed id stays ungrouped", () => {
  const g = computeGroups(["guards.a", "aeye-a", "lint"]);
  assert.equal(g.groupOf.size, 0);
});

test("displayKeyFor: a member of a group not expanded lands on the group", () => {
  const g = computeGroups(GUARDS);
  assert.equal(displayKeyFor(g.groupOf, new Set(), "handler", "guards.a"), nodeKey("group", "guards"));
  assert.equal(displayKeyFor(g.groupOf, new Set(["guards"]), "handler", "guards.a"), nodeKey("handler", "guards.a"));
  assert.equal(displayKeyFor(g.groupOf, new Set(), "outcome", "deny"), nodeKey("outcome", "deny"));
});
