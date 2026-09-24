import assert from "node:assert/strict";
import { test } from "node:test";
import ELK from "elkjs/lib/elk.bundled.js";
import type { LayoutInput } from "../types.ts";
import { fromElk, toElkGraph } from "./elkGraph.ts";
import { NODE_H, NODE_W } from "../constants.ts";

async function layout(input: LayoutInput) {
  const elk = new ELK();
  const out = await elk.layout(toElkGraph(input));
  return fromElk(out, input.gen);
}

test("four distinct column x's in order, including an idle outcome and a direct event->outcome edge", async () => {
  const input: LayoutInput = {
    gen: 1,
    key: "k1",
    nodes: [
      { key: "engine\x00claude", col: "engine", name: "claude" },
      { key: "event\x00pretool", col: "event", name: "pretool" },
      { key: "handler\x00solo", col: "handler", name: "solo" },
      { key: "outcome\x00allow", col: "outcome", name: "allow" },
      { key: "outcome\x00deny", col: "outcome", name: "deny" }, // idle: no edges reference it
    ],
    edges: [
      ["engine\x00claude", "event\x00pretool"],
      ["event\x00pretool", "handler\x00solo"],
      ["handler\x00solo", "outcome\x00allow"],
      ["event\x00pretool", "outcome\x00allow"], // direct event -> outcome edge
    ],
  };

  const result = await layout(input);

  assert.equal(result.gen, 1);
  assert.equal(result.columnX.length, 4);
  const [engineX, eventX, handlerX, outcomeX] = result.columnX;
  assert.ok(engineX < eventX, "engine column before event column");
  assert.ok(eventX < handlerX, "event column before handler column");
  assert.ok(handlerX < outcomeX, "handler column before outcome column");

  // The idle outcome node is still placed, at the outcome column.
  const idle = result.boxes["outcome\x00deny"];
  assert.ok(idle, "idle outcome node is still laid out");
  assert.equal(idle.x, outcomeX);
});

test("an expanded group's members sit inside its container box", async () => {
  const input: LayoutInput = {
    gen: 2,
    key: "k2",
    nodes: [
      { key: "event\x00pretool", col: "event", name: "pretool" },
      { key: "group\x00guards.*", col: "group", name: "guards.*" },
      { key: "handler\x00guards.a", col: "handler", name: "guards.a", parent: "group\x00guards.*" },
      { key: "handler\x00guards.b", col: "handler", name: "guards.b", parent: "group\x00guards.*" },
      { key: "outcome\x00allow", col: "outcome", name: "allow" },
    ],
    edges: [
      ["event\x00pretool", "handler\x00guards.a"],
      ["event\x00pretool", "handler\x00guards.b"],
      ["handler\x00guards.a", "outcome\x00allow"],
      ["handler\x00guards.b", "outcome\x00allow"],
    ],
  };

  const result = await layout(input);

  const group = result.boxes["group\x00guards.*"];
  const a = result.boxes["handler\x00guards.a"];
  const b = result.boxes["handler\x00guards.b"];
  assert.ok(group && a && b, "group and both members are laid out");

  // Group is sized to contain both members (full container size from ELK).
  assert.ok(group.w > NODE_W, "group is wider than a single member");
  assert.ok(group.h >= 2 * NODE_H, "group is tall enough for two members");

  // Members' x/y are relative to the group, inside its box.
  for (const member of [a, b]) {
    assert.ok(member.x >= 0 && member.x + NODE_W <= group.w, "member x within group width");
    assert.ok(member.y >= 0 && member.y + NODE_H <= group.h, "member y within group height");
  }
  assert.notEqual(a.y, b.y, "members do not overlap vertically");
});

test("a single-member expanded group is still a compound container", async () => {
  const input: LayoutInput = {
    gen: 3,
    key: "k3",
    nodes: [
      { key: "event\x00pretool", col: "event", name: "pretool" },
      { key: "group\x00solo-group", col: "group", name: "solo-group" },
      { key: "handler\x00solo-group.x", col: "handler", name: "solo-group.x", parent: "group\x00solo-group" },
    ],
    edges: [["event\x00pretool", "handler\x00solo-group.x"]],
  };

  const result = await layout(input);

  const group = result.boxes["group\x00solo-group"];
  const member = result.boxes["handler\x00solo-group.x"];
  assert.ok(group && member, "group and its one member are laid out");
  assert.ok(group.w >= NODE_W, "group container fits its one member");
  assert.ok(group.h >= NODE_H, "group container fits its one member");
  assert.ok(member.x >= 0 && member.x + NODE_W <= group.w);
  assert.ok(member.y >= 0 && member.y + NODE_H <= group.h);
});

test("in-column order follows input order when there is no crossing pressure", async () => {
  const names = ["zeta", "alpha", "middle", "beta"];
  const input: LayoutInput = {
    gen: 4,
    key: "k4",
    nodes: [
      { key: "engine\x00e", col: "engine", name: "e" },
      ...names.map((n): LayoutInput["nodes"][number] => ({ key: `handler\x00${n}`, col: "handler", name: n })),
      { key: "outcome\x00allow", col: "outcome", name: "allow" },
    ],
    edges: names.flatMap((n) => [
      ["engine\x00e", `handler\x00${n}`],
      [`handler\x00${n}`, "outcome\x00allow"],
    ] as [string, string][]),
  };

  const result = await layout(input);
  const ordered = names
    .map((n) => ({ n, y: result.boxes[`handler\x00${n}`]!.y }))
    .sort((a, b) => a.y - b.y)
    .map((e) => e.n);
  assert.deepEqual(ordered, names);
});

test("order is preserved across a relayout that adds one node", async () => {
  const names1 = ["zeta", "alpha", "middle", "beta"];
  function build(names: string[]): LayoutInput {
    return {
      gen: names.length,
      key: `k-${names.length}`,
      nodes: [
        { key: "engine\x00e", col: "engine", name: "e" },
        ...names.map((n): LayoutInput["nodes"][number] => ({ key: `handler\x00${n}`, col: "handler", name: n })),
        { key: "outcome\x00allow", col: "outcome", name: "allow" },
      ],
      edges: names.flatMap((n) => [
        ["engine\x00e", `handler\x00${n}`],
        [`handler\x00${n}`, "outcome\x00allow"],
      ] as [string, string][]),
    };
  }

  await layout(build(names1)); // first layout, discarded — only order convention matters
  const names2 = [...names1, "newnode"]; // previous order kept, new node appended (LayoutInput.nodes contract)
  const result2 = await layout(build(names2));

  const ordered2 = names2
    .map((n) => ({ n, y: result2.boxes[`handler\x00${n}`]!.y }))
    .sort((a, b) => a.y - b.y)
    .map((e) => e.n);
  assert.deepEqual(ordered2, names2);
});
