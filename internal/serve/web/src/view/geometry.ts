// Sankey geometry for one drawn frame: pure (no DOM), synchronous, run at
// most once per refresh. Band widths are data (√count, floored so every
// decision stays a visible band), so the geometry follows the counts; the
// node ORDER does not — ranks are computed once per committed snapshot
// (rankNodes) and passed in, so a live refresh never reshuffles the columns.
//
// d3-sankey places the nodes (relaxation along the bands, within the pinned
// order); everything it cannot know — plate minimum heights for their text,
// room above an open group's first member for its fold header, the panel
// growing when the plates alone overflow it — is folded in around it: node
// heights are given to d3-sankey as fixedValue in px with the extent sized so
// its scale is exactly 1, then a collision pass settles the final positions.

import { sankey } from "d3-sankey";
import type { SankeyGraph, SankeyNode } from "d3-sankey";
import { bySeverity, isLoud, severityRank } from "../model/snapshot.ts";

export type NodeKind = "engine" | "event" | "handler" | "group" | "outcome" | "pseudo";

export interface GeoNodeIn {
  key: string;
  col: number; // 0 engine, 1 event, 2 handler/group/pseudo, 3 outcome
  kind: NodeKind;
  lines: number; // text lines the node's label needs (plates and engines)
  head: boolean; // first member of an open group: room for its fold header above
}

export interface GeoLinkIn {
  edge: string; // display edge key this band belongs to
  from: string;
  to: string;
  outcome: string;
  count: number;
}

export interface GeoInput {
  width: number;
  minHeight: number;
  charW: number; // px per monospace character
  chars: [number, number, number, number]; // widest label per column, in characters
  headerChars: [number, number, number, number]; // column header text length ("name · unit"), in characters
  nodes: GeoNodeIn[];
  links: GeoLinkIn[];
  rank: Map<string, number>; // node key -> order within its column
}

export interface GeoNode {
  key: string;
  col: number;
  kind: NodeKind;
  x0: number;
  x1: number;
  y0: number; // top of the node's box (plate or bar)
  h: number;
  inH: number; // stacked band height entering / leaving
  outH: number;
}

export interface GeoLink {
  edge: string;
  from: string;
  to: string;
  outcome: string;
  count: number;
  width: number;
  x0: number; // source's right edge
  x1: number; // target's left edge
  y0: number; // band centre at the source / target
  y1: number;
}

interface GeoColumn { x0: number; x1: number; }

export interface Geometry {
  width: number;
  height: number;
  ky: number;
  top: number;
  bottom: number;
  columns: GeoColumn[]; // header extents: where each column's header and rule sit
  labelX: [number, number]; // engine labels' left edge, outcome labels' left edge
  nodes: GeoNode[];
  links: GeoLink[];
  byKey: Map<string, GeoNode>;
}

export const GUTTER = 12; // matches the panel header's side padding
export const TOP = 40; // header row
export const BOTTOM = 30; // legend row
const BAR_W = 8;
export const LH = 14;
const GAP = 5;
export const HEAD_H = 20;
const PAD = 14; // d3-sankey's node padding while it relaxes
const PLATE_PAD = 6;
const KY_MAX = 3.5; // px per √count: a small (filtered) day stays visibly small
const KY_MIN = 0.3; // below this the big bands stop reading as big; the panel grows instead
const MIN_GAP = 48;
const PLATE_MAX = 280;

// floorPx: a decision band is at least 2 + log2(n) px, so a single deny is
// visible and decisions still rank against each other; a no-op band 1 px.
function floorPx(count: number, outcome: string): number {
  return isLoud(outcome) ? 2 + Math.log2(count) : 1;
}

export function bandWidth(count: number, outcome: string, ky: number): number {
  if (count <= 0) return 0;
  return Math.max(Math.sqrt(count) * ky, floorPx(count, outcome));
}

// ---------- ranking ----------

export interface RankNode {
  key: string;
  col: number;
  kind: NodeKind;
  name: string;
  group: string | null; // an open group's member: the group it stays contiguous with
  total: number; // calls for engine/event, branches otherwise
  outcomes: Map<string, number>;
  planIdx: number;
}

function loudTotal(outcomes: Map<string, number>): number {
  let s = 0;
  for (const [o, c] of outcomes) if (isLoud(o)) s += c;
  return s;
}

// topDecision: the node's most consequential decision and its count
// (severity 99 when it carries none).
function topDecision(outcomes: Map<string, number>): [number, number] {
  let best: [number, number] = [99, 0];
  for (const [o, c] of outcomes) {
    if (isLoud(o) && c > 0 && severityRank(o) < best[0]) best = [severityRank(o), c];
  }
  return best;
}

function compareKeys(a: number[], b: number[]): number {
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const d = (a[i] ?? 0) - (b[i] ?? 0);
    if (d !== 0) return d;
  }
  return 0;
}

// rankNodes orders each column. Engines by volume (idle ones last, in plan
// order, so an idle toggle never moves a busy engine). Events and handlers:
// the ones carrying decisions first — most consequential decision, then how
// many — then volume; an open group's members stay contiguous and rank as a
// block by their aggregate. Outcomes by severity. The no-handler slot last.
export function rankNodes(nodes: readonly RankNode[]): Map<string, number> {
  const block = new Map<string, { loud: number; vol: number; anchor: number }>();
  const blockOf = (n: RankNode) => n.group ?? n.key;
  for (const n of nodes) {
    if (n.col !== 2) continue;
    const b = block.get(blockOf(n)) ?? { loud: 0, vol: 0, anchor: n.planIdx };
    b.loud += loudTotal(n.outcomes);
    b.vol += n.total;
    b.anchor = Math.min(b.anchor, n.planIdx);
    block.set(blockOf(n), b);
  }
  const key = (n: RankNode): number[] => {
    const [sev, loud] = topDecision(n.outcomes);
    switch (n.col) {
      case 0: return [n.total > 0 ? 0 : 1, -n.total, n.planIdx];
      case 1: return [sev, -loud, -n.total, n.planIdx];
      case 3: return [severityRank(n.name), n.planIdx];
    }
    if (n.kind === "pseudo") return [3];
    const b = block.get(blockOf(n)) ?? { loud: 0, vol: 0, anchor: 0 };
    return [b.loud > 0 ? 0 : b.vol > 0 ? 1 : 2, -b.loud, -b.vol, b.anchor, sev, -loud, -n.total, n.planIdx];
  };
  const keyed = nodes.map((n) => ({ n, k: key(n) }));
  keyed.sort((a, b) => a.n.col - b.n.col || compareKeys(a.k, b.k));
  return new Map(keyed.map(({ n }, i) => [n.key, i]));
}

// ---------- layout ----------

interface N extends GeoNodeIn { room: number; inH: number; outH: number; plateH: number; }
interface L extends GeoLinkIn { width: number; }

// columnsFor places the columns across width; a panel too narrow for them
// at their minimum lays out wider (the flow body then scrolls sideways).
function columnsFor(width: number, charW: number, chars: GeoInput["chars"], headerChars: GeoInput["headerChars"]): { x: number[]; w: number[]; labelX: [number, number]; width: number } {
  // Every gap the layout produces is at least MIN_GAP (see below), so a
  // column's header text fits without touching the next header as long as
  // the column is at least headerW - MIN_GAP wide.
  const headerW = headerChars.map((c) => Math.ceil(c * charW) + 6);
  const engineLabel = Math.max(Math.ceil(chars[0] * charW) + 8, headerW[0] - BAR_W - MIN_GAP);
  const outcomeLabel = Math.ceil(chars[3] * charW) + 8;
  const plate = (c: number) => Math.ceil(c * charW) + 2 * 8;
  const plateMin = Math.min(PLATE_MAX, Math.max(170, width * 0.16));
  let eventW = Math.min(Math.max(plate(chars[1]), plateMin), PLATE_MAX);
  let handlerW = Math.min(Math.max(plate(chars[2]), plateMin), PLATE_MAX);
  const fixed = GUTTER * 2 + engineLabel + BAR_W * 2 + outcomeLabel;
  let free = width - fixed - eventW - handlerW;
  if (free < MIN_GAP * 3) {
    // Narrow panel: the plates give way first (their labels ellipsize).
    const cut = Math.min(MIN_GAP * 3 - free, eventW + handlerW - 2 * 130);
    eventW -= cut / 2;
    handlerW -= cut / 2;
  }
  // ...but never past their own header text.
  eventW = Math.max(eventW, headerW[1] - MIN_GAP);
  handlerW = Math.max(handlerW, headerW[2] - MIN_GAP);
  free = width - fixed - eventW - handlerW;
  // Gaps share the free width 1 : 1.6 : 1.6; the engine gap, the smallest,
  // takes at least MIN_GAP from the other two.
  let gaps = [1, 1.6, 1.6].map((w) => (free * w) / 4.2);
  if (gaps[0] < MIN_GAP) {
    const rest = Math.max(MIN_GAP, (free - MIN_GAP) / 2);
    gaps = [MIN_GAP, rest, rest];
  }
  const x0 = GUTTER + engineLabel;
  const x1 = x0 + BAR_W + gaps[0];
  const x2 = x1 + eventW + gaps[1];
  const x3 = x2 + handlerW + gaps[2];
  const need = x3 + BAR_W + outcomeLabel + GUTTER;
  return { x: [x0, x1, x2, x3], w: [BAR_W, eventW, handlerW, BAR_W], labelX: [GUTTER, x3 + BAR_W + 6], width: need > width + 0.5 ? Math.ceil(need) : width };
}

// Links into and out of each node, in the band order: every stack sorted by
// the far end's rank, then by severity (decisions ride on top of a shared
// neighbour's bands) — the order that keeps crossings down.
function linkOrder(rank: Map<string, number>) {
  return (a: L, b: L): number =>
    (rank.get(a.from) ?? 0) - (rank.get(b.from) ?? 0)
    || (rank.get(a.to) ?? 0) - (rank.get(b.to) ?? 0)
    || bySeverity(a.outcome, b.outcome);
}

function sizeNodes(nodes: N[], links: L[], ky: number): void {
  for (const n of nodes) {
    n.inH = 0;
    n.outH = 0;
  }
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  for (const l of links) {
    l.width = bandWidth(l.count, l.outcome, ky);
    const s = byKey.get(l.from);
    const t = byKey.get(l.to);
    if (s) s.outH += l.width;
    if (t) t.inH += l.width;
  }
  for (const n of nodes) {
    const stack = Math.max(n.inH, n.outH);
    const text = n.lines * LH + PLATE_PAD;
    if (n.kind === "engine") {
      n.plateH = Math.max(stack, 2);
      n.room = Math.max(n.plateH, n.lines * LH);
    } else if (n.kind === "outcome") {
      n.plateH = Math.max(stack, 2);
      n.room = Math.max(n.plateH, LH);
    } else {
      n.plateH = Math.max(stack, text);
      n.room = n.plateH;
    }
    if (n.head) n.room += HEAD_H;
  }
}

function columnNeed(nodes: N[]): number {
  const need = [0, 0, 0, 0];
  const count = [0, 0, 0, 0];
  for (const n of nodes) {
    need[n.col] += n.room;
    count[n.col]++;
  }
  return Math.max(...need.map((h, c) => h + Math.max(0, count[c] - 1) * GAP));
}

// fitKy: the largest band scale (≤ KY_MAX, ≥ KY_MIN) whose tallest column
// fits avail. Column need grows monotonically with ky.
function fitKy(nodes: N[], links: L[], avail: number): number {
  sizeNodes(nodes, links, KY_MAX);
  if (columnNeed(nodes) <= avail) return KY_MAX;
  let lo = KY_MIN;
  let hi = KY_MAX;
  for (let i = 0; i < 24; i++) {
    const mid = (lo + hi) / 2;
    sizeNodes(nodes, links, mid);
    if (columnNeed(nodes) <= avail) lo = mid;
    else hi = mid;
  }
  return lo;
}

type DN = SankeyNode<N, L>;

// relax runs d3-sankey over px-valued nodes and links so it keeps each
// node's height and nudges the column positions along the bands.
function relax(nodes: N[], links: L[], rank: Map<string, number>): Map<string, number> {
  const cols = [0, 1, 2, 3].map((c) => nodes.filter((n) => n.col === c));
  const extent = Math.max(...cols.map((c) => c.reduce((a, n) => a + n.room, 0) + Math.max(0, c.length - 1) * PAD));
  const order = linkOrder(rank);
  const graph: SankeyGraph<N, L> = {
    nodes: nodes.map((n) => ({ ...n, fixedValue: n.room })),
    links: links.filter((l) => l.width > 0).map((l) => ({ ...l, source: l.from, target: l.to, value: l.width })),
  };
  const layout = sankey<N, L>()
    .nodeId((d) => d.key)
    .nodeAlign((d) => d.col)
    .nodeWidth(1)
    .nodePadding(PAD)
    .nodeSort((a, b) => (rank.get(a.key) ?? 0) - (rank.get(b.key) ?? 0))
    .linkSort(order)
    .iterations(6)
    .extent([[0, 0], [3, extent]]);
  const out = layout(graph);
  return new Map((out.nodes as DN[]).map((n) => [n.key, n.y0 ?? 0]));
}

export function layoutFlow(input: GeoInput): Geometry {
  const { x, w, labelX, width } = columnsFor(input.width, input.charW, input.chars, input.headerChars);
  const nodes: N[] = input.nodes.map((n) => ({ ...n, room: 0, inH: 0, outH: 0, plateH: 0 }));
  const keys = new Set(nodes.map((n) => n.key));
  const links: L[] = input.links.filter((l) => l.count > 0 && keys.has(l.from) && keys.has(l.to)).map((l) => ({ ...l, width: 0 }));
  const avail = input.minHeight - TOP - BOTTOM;
  const ky = fitKy(nodes, links, avail);
  sizeNodes(nodes, links, ky);
  const need = columnNeed(nodes);
  const height = Math.max(input.minHeight, Math.ceil(need + TOP + BOTTOM));
  const bottom = height - BOTTOM;

  // d3-sankey requires every column to be reachable along the links; with
  // no traffic at all the columns simply stack.
  const hasFlow = links.some((l) => l.width > 0) && [0, 1, 2, 3].every((c) => nodes.some((n) => n.col === c));
  const relaxed = hasFlow ? relax(nodes, links, input.rank) : new Map<string, number>();

  const byKey = new Map<string, GeoNode>();
  const out: GeoNode[] = [];
  for (let c = 0; c < 4; c++) {
    const col = nodes.filter((n) => n.col === c).sort((a, b) => (input.rank.get(a.key) ?? 0) - (input.rank.get(b.key) ?? 0));
    const ys = col.map((n) => TOP + (relaxed.get(n.key) ?? 0));
    // Push overlaps down, then pull the column back up inside the bottom edge.
    let y = TOP;
    for (let i = 0; i < col.length; i++) {
      ys[i] = Math.max(ys[i], y);
      y = ys[i] + col[i].room + GAP;
    }
    y = bottom;
    for (let i = col.length - 1; i >= 0; i--) {
      ys[i] = Math.max(TOP, Math.min(ys[i], y - col[i].room));
      y = ys[i] - GAP;
    }
    for (let i = 0; i < col.length; i++) {
      const n = col[i];
      const head = n.head ? HEAD_H : 0;
      const pad = (n.room - head - n.plateH) / 2; // an engine's bar sits centred beside its two label lines
      const g: GeoNode = {
        key: n.key, col: c, kind: n.kind, x0: x[c], x1: x[c] + w[c],
        y0: ys[i] + head + pad, h: n.plateH, inH: n.inH, outH: n.outH,
      };
      out.push(g);
      byKey.set(g.key, g);
    }
  }

  // A small graph sits a little below the header row, not glued to it.
  const used = Math.max(TOP, ...out.map((n) => n.y0 + n.h)) - TOP;
  const slack = (bottom - TOP - used) / 5;
  if (slack > 0) for (const n of out) n.y0 += slack;

  // Stack each node's bands, centred on its plate.
  const order = linkOrder(input.rank);
  const drawn = links.filter((l) => l.width > 0).sort(order);
  const outY = new Map<string, number>();
  const inY = new Map<string, number>();
  for (const n of out) {
    outY.set(n.key, n.y0 + (n.h - n.outH) / 2);
    inY.set(n.key, n.y0 + (n.h - n.inH) / 2);
  }
  const geoLinks: GeoLink[] = [];
  for (const l of drawn) {
    const s = byKey.get(l.from);
    const t = byKey.get(l.to);
    if (!s || !t) continue;
    const y0 = outY.get(s.key) ?? 0;
    const y1 = inY.get(t.key) ?? 0;
    outY.set(s.key, y0 + l.width);
    inY.set(t.key, y1 + l.width);
    geoLinks.push({
      edge: l.edge, from: l.from, to: l.to, outcome: l.outcome, count: l.count, width: l.width,
      x0: s.x1, x1: t.x0, y0: y0 + l.width / 2, y1: y1 + l.width / 2,
    });
  }

  const columns: GeoColumn[] = [
    { x0: GUTTER, x1: x[0] + BAR_W },
    { x0: x[1], x1: x[1] + w[1] },
    { x0: x[2], x1: x[2] + w[2] },
    { x0: x[3], x1: width - GUTTER },
  ];
  return { width, height, ky, top: TOP, bottom, columns, labelX, nodes: out, links: geoLinks, byKey };
}

// ribbon: the closed outline of a band of width w whose centre runs from
// (x0, y0) to (x1, y1); dy shifts it (a hover subset rides its band's top).
export function ribbon(l: { x0: number; x1: number; y0: number; y1: number }, w: number, dy = 0): string {
  const xm = (l.x0 + l.x1) / 2;
  const a = l.y0 + dy - w / 2;
  const b = l.y1 + dy - w / 2;
  const f = (n: number) => Math.round(n * 100) / 100;
  return `M${f(l.x0)},${f(a)}C${f(xm)},${f(a)} ${f(xm)},${f(b)} ${f(l.x1)},${f(b)}`
    + `V${f(b + w)}C${f(xm)},${f(b + w)} ${f(xm)},${f(a + w)} ${f(l.x0)},${f(a + w)}Z`;
}

// pointOn: the band centre line at t in [0, 1], shifted by dy.
export function pointOn(l: { x0: number; x1: number; y0: number; y1: number }, t: number, dy = 0): { x: number; y: number } {
  const u = 1 - t;
  const xm = (l.x0 + l.x1) / 2;
  const x = u * u * u * l.x0 + 3 * u * u * t * xm + 3 * u * t * t * xm + t * t * t * l.x1;
  const y = (u * u * u + 3 * u * u * t) * l.y0 + (3 * u * t * t + t * t * t) * l.y1;
  return { x, y: y + dy };
}
