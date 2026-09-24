import assert from "node:assert/strict";
import { test } from "node:test";
import { bandWidth, BOTTOM, GUTTER, HEAD_H, layoutFlow, pointOn, rankNodes, ribbon, TOP } from "./geometry.ts";
import type { GeoInput, GeoLinkIn, GeoNodeIn, Geometry, RankNode } from "./geometry.ts";

const E = (from: string, to: string, outcome: string, count: number): GeoLinkIn => ({ edge: from + ">" + to, from, to, outcome, count });

function node(key: string, col: number, kind: GeoNodeIn["kind"], lines = 1, head = false): GeoNodeIn {
  return { key, col, kind, lines, head };
}

// A small pipeline: two engines, two events, three handlers, three outcomes.
function input(over: Partial<GeoInput> = {}): GeoInput {
  const nodes = [
    node("e:cc", 0, "engine", 2), node("e:pi", 0, "engine", 2),
    node("v:pre", 1, "event"), node("v:post", 1, "event"),
    node("h:a", 2, "handler", 2), node("h:b", 2, "handler"), node("h:c", 2, "handler"),
    node("o:deny", 3, "outcome"), node("o:allow", 3, "outcome"), node("o:abstain", 3, "outcome"),
  ];
  const links = [
    E("e:cc", "v:pre", "deny", 3), E("e:cc", "v:pre", "abstain", 900), E("e:cc", "v:post", "abstain", 400),
    E("e:pi", "v:pre", "abstain", 50),
    E("v:pre", "h:a", "deny", 3), E("v:pre", "h:a", "abstain", 950), E("v:pre", "h:b", "abstain", 953),
    E("v:post", "h:c", "allow", 400),
    E("h:a", "o:deny", "deny", 3), E("h:a", "o:abstain", "abstain", 950), E("h:b", "o:abstain", "abstain", 953),
    E("h:c", "o:allow", "allow", 400),
  ];
  const rank = new Map(nodes.map((n, i) => [n.key, i]));
  return { width: 1200, minHeight: 600, charW: 7.2, chars: [12, 20, 20, 16], nodes, links, rank, ...over };
}

function assertNoOverlap(g: Geometry, what: string): void {
  for (let c = 0; c < 4; c++) {
    const col = g.nodes.filter((n) => n.col === c).sort((a, b) => a.y0 - b.y0);
    for (let i = 1; i < col.length; i++) {
      assert.ok(col[i].y0 >= col[i - 1].y0 + col[i - 1].h, `${what}: ${col[i - 1].key} overlaps ${col[i].key}`);
    }
    for (const n of col) {
      assert.ok(n.y0 >= TOP - 0.01 && n.y0 + n.h <= g.height - BOTTOM + 0.01, `${what}: ${n.key} outside the graph rows`);
    }
  }
}

test("bandWidth: √count, decisions floored at 2 + log2(n) px, no-ops at 1 px", () => {
  assert.equal(bandWidth(0, "deny", 3), 0);
  assert.equal(bandWidth(1, "deny", 0.5), 2);
  assert.equal(bandWidth(8, "deny", 0.5), 5);
  assert.equal(bandWidth(1, "abstain", 0.5), 1);
  assert.equal(bandWidth(400, "abstain", 2), 40);
});

test("layout: columns left to right inside the width, headers over their columns", () => {
  const g = layoutFlow(input());
  const x = (key: string) => g.byKey.get(key)?.x0 ?? NaN;
  assert.ok(x("e:cc") < x("v:pre") && x("v:pre") < x("h:a") && x("h:a") < x("o:deny"));
  assert.ok(Math.max(...g.nodes.map((n) => n.x1)) <= g.width - GUTTER);
  assert.equal(g.columns[0].x0, GUTTER);
  assert.equal(g.columns[0].x0, g.labelX[0], "engine labels share the header's left edge");
  assert.equal(g.columns[1].x0, x("v:pre"));
  assert.equal(g.columns[2].x0, x("h:a"));
  assert.equal(g.columns[3].x0, x("o:deny"));
  assert.ok(g.labelX[1] > (g.byKey.get("o:deny")?.x1 ?? 0));
});

test("layout: no overlaps, every band stacked inside its plate, widths follow the scale", () => {
  const g = layoutFlow(input());
  assertNoOverlap(g, "default");
  for (const n of g.nodes) {
    const out = g.links.filter((l) => l.from === n.key).sort((a, b) => a.y0 - b.y0);
    const into = g.links.filter((l) => l.to === n.key).sort((a, b) => a.y1 - b.y1);
    for (let i = 1; i < out.length; i++) assert.ok(Math.abs(out[i].y0 - out[i].width / 2 - (out[i - 1].y0 + out[i - 1].width / 2)) < 0.01, n.key + " out stack has a gap");
    for (let i = 1; i < into.length; i++) assert.ok(Math.abs(into[i].y1 - into[i].width / 2 - (into[i - 1].y1 + into[i - 1].width / 2)) < 0.01, n.key + " in stack has a gap");
    if (out.length) assert.ok(out[0].y0 - out[0].width / 2 >= n.y0 - 0.01 && out[out.length - 1].y0 + out[out.length - 1].width / 2 <= n.y0 + n.h + 0.01, n.key + " out stack leaves the plate");
  }
  for (const l of g.links) assert.equal(l.width, bandWidth(l.count, l.outcome, g.ky));
  // In each stack a decision rides above the no-op band sharing its far end.
  const [deny, abstain] = ["deny", "abstain"].map((o) => g.links.find((l) => l.from === "v:pre" && l.to === "h:a" && l.outcome === o));
  assert.ok(deny && abstain && deny.y0 < abstain.y0);
});

test("layout: a small day stays small (the band scale is capped)", () => {
  const tiny = input({ links: [E("e:cc", "v:pre", "abstain", 1), E("v:pre", "h:b", "abstain", 1), E("h:b", "o:abstain", "abstain", 1)] });
  const g = layoutFlow(tiny);
  assert.ok(g.ky <= 3.5);
  assert.equal(g.height, 600);
});

test("layout: plates that cannot fit grow the panel instead of overlapping", () => {
  const nodes = input().nodes.slice();
  const links = input().links.slice();
  for (let i = 0; i < 60; i++) {
    nodes.push(node("h:x" + i, 2, "handler", 2, i === 0));
    links.push(E("v:post", "h:x" + i, "deny", 1), E("h:x" + i, "o:deny", "deny", 1));
  }
  const g = layoutFlow(input({ nodes, links, rank: new Map(nodes.map((n, i) => [n.key, i])), minHeight: 500 }));
  assert.ok(g.height > 500, "grew");
  assertNoOverlap(g, "60 open members");
  const first = g.byKey.get("h:x0");
  const above = g.nodes.filter((n) => n.col === 2 && first && n.y0 < first.y0).sort((a, b) => b.y0 - a.y0)[0];
  assert.ok(first && above && first.y0 - (above.y0 + above.h) >= HEAD_H, "room for the fold header");
});

test("layout: no traffic at all still stacks every node", () => {
  const g = layoutFlow(input({ links: [] }));
  assert.equal(g.nodes.length, 10);
  assert.equal(g.links.length, 0);
  assertNoOverlap(g, "idle");
});

test("rankNodes: engines by volume, idle last; decisions first; groups contiguous; outcomes by severity", () => {
  const n = (key: string, col: number, total: number, outs: [string, number][] = [], group: string | null = null, kind: RankNode["kind"] = "handler", planIdx = 0): RankNode =>
    ({ key, col, kind, name: key.slice(2), group, total, outcomes: new Map(outs), planIdx });
  const rank = rankNodes([
    n("e:idle", 0, 0, [], null, "engine", 0), n("e:pi", 0, 50, [["deny", 5]], null, "engine", 1), n("e:cc", 0, 900, [], null, "engine", 2),
    n("v:post", 1, 400, [["abstain", 400]], null, "event", 0), n("v:pre", 1, 10, [["deny", 1]], null, "event", 1),
    n("h:lint", 2, 5000, [["abstain", 5000]], null, "handler", 0),
    n("h:g.a", 2, 10, [["abstain", 10]], "g", "handler", 1), n("h:g.b", 2, 10, [["deny", 2]], "g", "handler", 2),
    n("h:k*", 2, 100, [["advise", 1]], null, "group", 3), n("P", 2, 7, [["abstain", 7]], null, "pseudo", 99),
    n("o:abstain", 3, 1, [], null, "outcome", 0), n("o:deny", 3, 1, [], null, "outcome", 1), n("o:router-error", 3, 1, [], null, "outcome", 2),
  ]);
  const order = (keys: string[]) => keys.slice().sort((a, b) => (rank.get(a) ?? 0) - (rank.get(b) ?? 0));
  assert.deepEqual(order(["e:idle", "e:pi", "e:cc"]), ["e:cc", "e:pi", "e:idle"]);
  assert.deepEqual(order(["v:post", "v:pre"]), ["v:pre", "v:post"]);
  assert.deepEqual(order(["h:lint", "h:g.a", "h:g.b", "h:k*", "P"]), ["h:g.b", "h:g.a", "h:k*", "h:lint", "P"]);
  assert.deepEqual(order(["o:abstain", "o:deny", "o:router-error"]), ["o:router-error", "o:deny", "o:abstain"]);
});

test("ribbon and pointOn follow the band's centre line", () => {
  const l = { x0: 0, x1: 100, y0: 10, y1: 50 };
  assert.deepEqual(pointOn(l, 0), { x: 0, y: 10 });
  assert.deepEqual(pointOn(l, 1), { x: 100, y: 50 });
  assert.deepEqual(pointOn(l, 0.5, 2), { x: 50, y: 32 });
  assert.equal(ribbon(l, 4), "M0,8C50,8 50,48 100,48V52C50,52 50,12 0,12Z");
});
