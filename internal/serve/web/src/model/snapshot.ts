// The rendered snapshot: an immutable view of one committed layout. Display
// mapping, counts, hover, tooltips and pulses all read THIS snapshot's
// expansion, so they stay pinned to what is drawn, never live model state.

import { COL_TITLES, OUTCOMES, ROUTER_ERROR } from "../constants.ts";
import type { Box, Col, DisplayCol, LayoutNode, LayoutResult, PathLike } from "../types.ts";
import { pathBranches } from "./branches.ts";
import type { EventLabel } from "./branches.ts";
import { displayKeyFor, groupLabel } from "./groups.ts";
import { edgeKey, nodeKey, splitEdgeKey, splitKey } from "./keys.ts";
import type { Branch } from "./keys.ts";
import { pathTotal } from "./state.ts";
import type { GroupRecord, LayoutPlan, PathEntry } from "./state.ts";
import type { RawEdge } from "./topology.ts";

export interface Totals {
  calls: number;
  branches: number;
  nodeTotals: Map<string, number>; // display node key -> count (an expanded group: sum of its shown members)
  edgeTotals: Map<string, number>; // display edge key -> count
}

export interface DisplayEdge { key: string; from: string; to: string; n: number; direct: boolean; }

export interface TipLine { text: string; cls: "title" | "" | "dim"; }

export interface SnapGroup {
  label: string; // "guards.*" / "aeye-*"
  members: string[]; // every member id, drawn or not
  shown: string[]; // node keys of the members drawn inside it (expanded only)
  expanded: boolean;
  forced: boolean;
}

export interface SnapNode extends LayoutNode {
  box: Box;
  tipName: string; // "router error", "guards.* (8)", or the name
  dataName: string; // data-name: the group label for a group, else the raw name
  ghost: boolean;
  group?: SnapGroup;
}

export function displayLabel(col: DisplayCol, name: string): string {
  return col === "outcome" && name === ROUTER_ERROR ? "router error" : name;
}

// outcomeClass derives a class name only from the known outcome list.
export function outcomeClass(name: string): string {
  if (name === ROUTER_ERROR) return "v-router-error";
  return OUTCOMES.includes(name) ? "v-" + name : "";
}

export function unitOf(col: DisplayCol): string {
  return col === "engine" || col === "event" ? "calls" : "branches";
}

export const EMPTY_TOTALS: Totals = { calls: 0, branches: 0, nodeTotals: new Map(), edgeTotals: new Map() };

// computeTotals counts paths onto display nodes and edges: engine and event
// nodes and the engine->event edge count calls, everything past the event
// counts branches. Two branches of one call sharing a display edge into a
// collapsed group both count.
export function computeTotals(paths: Iterable<PathEntry>, branchesOf: (p: PathLike) => string[][]): Totals {
  const edgeTotals = new Map<string, number>();
  const nodeTotals = new Map<string, number>();
  const bump = (m: Map<string, number>, k: string, n: number) => m.set(k, (m.get(k) ?? 0) + n);
  let calls = 0;
  let branches = 0;
  for (const entry of paths) {
    const n = pathTotal(entry);
    calls += n;
    if (n === 0) continue;
    const bs = branchesOf(entry.path);
    branches += n * bs.length;
    const [eng, ev] = bs[0];
    bump(nodeTotals, eng, n);
    bump(nodeTotals, ev, n);
    bump(edgeTotals, edgeKey(eng, ev), n);
    for (const keys of bs) {
      for (let i = 2; i < keys.length; i++) {
        bump(nodeTotals, keys[i], n);
        bump(edgeTotals, edgeKey(keys[i - 1], keys[i]), n);
      }
    }
  }
  return { calls, branches, nodeTotals, edgeTotals };
}

export class Snapshot {
  readonly gen: number;
  readonly key: string;
  readonly order: readonly string[]; // node keys in layout-input order (group before its members)
  readonly nodes: readonly SnapNode[];
  readonly columnX: readonly number[];
  private readonly byKey: Map<string, SnapNode>;
  private readonly plan: LayoutPlan;
  private readonly eventLabel: EventLabel;

  constructor(plan: LayoutPlan, result: LayoutResult, eventLabel: EventLabel) {
    this.plan = plan;
    this.eventLabel = eventLabel;
    this.gen = plan.input.gen;
    this.key = plan.input.key;
    this.columnX = result.columnX;
    this.order = plan.input.nodes.map((n) => n.key);

    const shownOf = new Map<string, string[]>();
    for (const n of plan.input.nodes) {
      if (n.parent === undefined) continue;
      const list = shownOf.get(n.parent);
      if (list) list.push(n.key);
      else shownOf.set(n.parent, [n.key]);
    }
    this.nodes = plan.input.nodes.map((n): SnapNode => {
      const box = result.boxes[n.key];
      if (n.col !== "group") {
        const label = displayLabel(n.col, n.name);
        return { ...n, box, tipName: label, dataName: n.name, ghost: n.col === "handler" && plan.ghost.has(n.name) };
      }
      const members = plan.groups.members.get(n.name) ?? [];
      const label = groupLabel(n.name, members);
      return {
        ...n, box, tipName: label + " (" + members.length + ")", dataName: label, ghost: false,
        group: {
          label, members,
          shown: shownOf.get(n.key) ?? [],
          expanded: plan.expanded.has(n.name),
          forced: plan.forced.has(n.name),
        },
      };
    });
    this.byKey = new Map(this.nodes.map((n) => [n.key, n]));
  }

  has(key: string): boolean {
    return this.byKey.has(key);
  }

  node(key: string): SnapNode | undefined {
    return this.byKey.get(key);
  }

  // groupState is the drawn record of a group node (by group key), or
  // undefined when the group is not drawn.
  groupState(group: string): GroupRecord | undefined {
    const g = this.byKey.get(nodeKey("group", group))?.group;
    return g && { expanded: g.expanded, forced: g.forced };
  }

  displayKey(col: Col, name: string): string {
    return displayKeyFor(this.plan.groups.groupOf, this.plan.expanded, col, name);
  }

  displayNodes(b: Branch): string[] {
    return b.nodes.map(([col, name]) => this.displayKey(col, name));
  }

  displayBranches(p: PathLike): string[][] {
    return pathBranches(p, this.eventLabel).map((b) => this.displayNodes(b));
  }

  // drawn: every display node of the branch is in this snapshot.
  drawn(b: Branch): boolean {
    return b.nodes.every(([col, name]) => this.byKey.has(this.displayKey(col, name)));
  }

  totals(paths: Iterable<PathEntry>): Totals {
    const t = computeTotals(paths, (p) => this.displayBranches(p));
    for (const n of this.nodes) {
      if (!n.group?.expanded) continue;
      t.nodeTotals.set(n.key, n.group.shown.reduce((a, k) => a + (t.nodeTotals.get(k) ?? 0), 0));
    }
    return t;
  }

  // displayEdges maps the topology's raw edges onto this snapshot: deduped,
  // both endpoints drawn. A new edge between drawn nodes shows up here without
  // a relayout.
  displayEdges(raw: Iterable<RawEdge>, totals: Totals): DisplayEdge[] {
    const out: DisplayEdge[] = [];
    const seen = new Set<string>();
    for (const [a, b] of raw) {
      const from = this.displayKey(a[0], a[1]);
      const to = this.displayKey(b[0], b[1]);
      const key = edgeKey(from, to);
      if (seen.has(key) || !this.byKey.has(from) || !this.byKey.has(to)) continue;
      seen.add(key);
      const direct = a[0] === "event" && b[0] === "outcome" && b[1] !== ROUTER_ERROR;
      out.push({ key, from, to, n: totals.edgeTotals.get(key) ?? 0, direct });
    }
    return out;
  }

  // highlight: every drawn branch through the node (upstream prefix and
  // downstream suffix); an expanded group header targets its members.
  highlight(key: string, paths: Iterable<PathEntry>): { nodes: Set<string>; edges: Set<string> } {
    const node = this.byKey.get(key);
    const targets = new Set(node?.group?.expanded ? node.group.shown : [key]);
    const nodes = new Set([key]);
    const edges = new Set<string>();
    for (const entry of paths) {
      if (pathTotal(entry) === 0) continue;
      for (const keys of this.displayBranches(entry.path)) {
        if (!keys.some((k) => targets.has(k))) continue;
        for (const k of keys) nodes.add(k);
        for (let i = 1; i < keys.length; i++) edges.add(edgeKey(keys[i - 1], keys[i]));
      }
    }
    return { nodes, edges };
  }

  tooltip(key: string, totals: Totals, paths: Iterable<PathEntry>): TipLine[] {
    const node = this.byKey.get(key);
    if (!node) return [];
    const lines: TipLine[] = [{ text: node.tipName + " — " + COL_TITLES[node.col], cls: "title" }];
    const n = totals.nodeTotals.get(key) ?? 0;
    lines.push({ text: n + " " + unitOf(node.col) + (node.ghost ? " · not in the installed table" : ""), cls: "" });

    const outs: [string, number][] = [];
    const ins: [string, number][] = [];
    for (const [ek, en] of totals.edgeTotals) {
      if (en === 0) continue;
      const [from, to] = splitEdgeKey(ek);
      if (!this.byKey.has(from) || !this.byKey.has(to)) continue;
      if (from === key) outs.push([to, en]);
      else if (to === key) ins.push([from, en]);
    }
    const top = (list: [string, number][]) => list.sort((a, b) => b[1] - a[1]).slice(0, 4);
    const tip = (k: string) => this.byKey.get(k)?.tipName ?? splitKey(k)[1];
    for (const [k, en] of top(outs)) lines.push({ text: "→ " + tip(k) + "  " + en, cls: "" });
    for (const [k, en] of top(ins)) lines.push({ text: "← " + tip(k) + "  " + en, cls: "" });

    if (node.group) {
      for (const [id, mn] of this.memberCounts(node.group.members, paths)) lines.push({ text: "  " + id + "  " + mn, cls: "" });
    }
    const hint = !node.group ? "click: filter · shift-click: add"
      : node.group.forced ? "held open by the handler filter"
      : "click: expand/collapse";
    lines.push({ text: hint, cls: "dim" });
    return lines;
  }

  private memberCounts(members: readonly string[], paths: Iterable<PathEntry>): [string, number][] {
    const counts = new Map(members.map((id) => [id, 0]));
    for (const entry of paths) {
      const n = pathTotal(entry);
      if (n === 0) continue;
      for (const b of pathBranches(entry.path, this.eventLabel)) {
        const h = b.nodes[2];
        if (h && h[0] === "handler") {
          const c = counts.get(h[1]);
          if (c !== undefined) counts.set(h[1], c + n);
        }
      }
    }
    return [...counts].sort((a, b) => b[1] - a[1]);
  }
}
