// The rendered snapshot: an immutable view of one committed node set.
// Display mapping, counts, hover, tooltips and pulses all read THIS
// snapshot's expansion, so they stay pinned to what is drawn, never live
// model state.

import { COL_TITLES, LOUD, OUTCOMES, ROUTER_ERROR, SEVERITY } from "../constants.ts";
import type { Col, DisplayCol, PathLike, PlanNode } from "../types.ts";
import { pathBranches } from "./branches.ts";
import type { EventLabel } from "./branches.ts";
import { displayKeyFor, groupLabel } from "./groups.ts";
import { edgeKey, nodeKey, splitEdgeKey, splitKey } from "./keys.ts";
import type { Branch } from "./keys.ts";
import { pathTotal } from "./state.ts";
import type { GroupRecord, LayoutPlan, PathEntry } from "./state.ts";

export type Counts = Map<string, number>; // outcome -> count

// Handled: calls that ran at least one handler, and their handler runs. A
// direct verdict or a router error is a branch but runs no handler.
export interface Handled { calls: number; runs: number; }

export interface Totals {
  calls: number;
  branches: number;
  nodeTotals: Map<string, number>; // display node key -> count (calls for engine/event, else branches)
  nodeCalls: Map<string, number>; // display node key -> calls with >= 1 branch through it
  nodeOutcomes: Map<string, Counts>; // display node key -> branches by outcome
  handled: Map<string, Handled>; // engine/event display key -> its calls that ran a handler
  edgeTotals: Map<string, number>; // display edge key -> count
  edgeOutcomes: Map<string, Counts>; // display edge key -> count by outcome (engine -> event: by call class)
  buckets: Map<string, number[]>; // display node key -> per-bucket count (live ring only)
  members: Map<string, Map<string, Counts>>; // collapsed group node key -> member id -> branches by outcome
}

export interface SnapGroup {
  label: string; // "guards.*" / "aeye-*"
  members: string[]; // every member id, drawn or not
  shown: string[]; // node keys of the members drawn inside it (expanded only)
  expanded: boolean;
  forced: boolean;
}

export interface SnapNode extends PlanNode {
  label: string; // "router error", "guards.*", or the name
  dataName: string; // data-name: the group label for a group, else the raw name
  ghost: boolean;
  group?: SnapGroup;
}

export interface TipRow { label: string; n: number; o?: string; note?: Counts; }
export interface TipSection { head: string; rows: TipRow[]; }
export interface Tip {
  title: string;
  kind: string; // "engine", "handler group · 8", …
  facts: string[]; // one line each, e.g. "11,429 calls → 67,610 handler runs (×5.9)"
  sections: TipSection[];
  hint: string;
}

export function displayLabel(col: DisplayCol, name: string): string {
  return col === "outcome" && name === ROUTER_ERROR ? "router error" : name;
}

// knownOutcome maps a wire outcome onto the known list (for data-o and
// classes); anything else is "other".
export function knownOutcome(name: string): string {
  return name === ROUTER_ERROR || OUTCOMES.includes(name) ? name : "other";
}

export function severityRank(o: string): number {
  const i = SEVERITY.indexOf(o);
  return i < 0 ? SEVERITY.length : i;
}

export function bySeverity(a: string, b: string): number {
  return severityRank(a) - severityRank(b);
}

export function isLoud(o: string): boolean {
  return LOUD.has(o);
}

export function unitOf(col: DisplayCol): string {
  return col === "engine" || col === "event" ? "calls" : "branches";
}

export function emptyTotals(): Totals {
  return {
    calls: 0, branches: 0, nodeTotals: new Map(), nodeCalls: new Map(), nodeOutcomes: new Map(), handled: new Map(),
    edgeTotals: new Map(), edgeOutcomes: new Map(), buckets: new Map(), members: new Map(),
  };
}

function bump(m: Map<string, number>, k: string, n: number): void {
  m.set(k, (m.get(k) ?? 0) + n);
}

function bumpIn<K>(m: Map<K, Counts>, k: K, o: string, n: number): void {
  let c = m.get(k);
  if (!c) m.set(k, c = new Map());
  bump(c, o, n);
}

function addBuckets(m: Map<string, number[]>, k: string, counts: readonly number[], times: number): void {
  let b = m.get(k);
  if (!b) m.set(k, b = new Array<number>(counts.length).fill(0));
  for (let i = 0; i < counts.length; i++) b[i] += counts[i] * times;
}

const outcomeOf = (keys: readonly string[]): string => splitKey(keys[keys.length - 1])[1];

// computeTotals counts paths onto display nodes and edges: engine and event
// nodes and the engine->event edge count calls, everything past the event
// counts branches. Two branches of one call sharing a display edge into a
// collapsed group both count. keep restricts to a subset of branches (a
// hover): a call counts when at least one of its branches is kept. The
// engine->event edge is split by the call's class: its most consequential
// kept branch outcome.
export function computeTotals(
  paths: Iterable<PathEntry>,
  branchesOf: (p: PathLike) => string[][],
  opts: { keep?: (keys: readonly string[]) => boolean; buckets?: boolean } = {},
): Totals {
  const t = emptyTotals();
  for (const entry of paths) {
    const n = pathTotal(entry);
    if (n === 0) continue;
    const all = branchesOf(entry.path);
    const bs = opts.keep ? all.filter(opts.keep) : all;
    if (bs.length === 0) continue;
    t.calls += n;
    t.branches += n * bs.length;
    const [eng, ev] = bs[0];
    const ee = edgeKey(eng, ev);
    const cls = bs.map(outcomeOf).sort(bySeverity)[0];
    bump(t.nodeTotals, eng, n);
    bump(t.nodeTotals, ev, n);
    bump(t.edgeTotals, ee, n);
    bumpIn(t.edgeOutcomes, ee, cls, n);
    const runs = bs.filter((keys) => keys.length === 4).length;
    if (runs > 0) {
      for (const k of [eng, ev]) {
        const h = t.handled.get(k) ?? { calls: 0, runs: 0 };
        h.calls += n;
        h.runs += n * runs;
        t.handled.set(k, h);
      }
    }
    const touched = new Map<string, number>(); // node -> branches through it
    for (const keys of bs) {
      const o = outcomeOf(keys);
      for (const k of keys) {
        bumpIn(t.nodeOutcomes, k, o, n);
        touched.set(k, (touched.get(k) ?? 0) + 1);
      }
      for (let i = 2; i < keys.length; i++) {
        const e = edgeKey(keys[i - 1], keys[i]);
        bump(t.nodeTotals, keys[i], n);
        bump(t.edgeTotals, e, n);
        bumpIn(t.edgeOutcomes, e, o, n);
      }
    }
    for (const [k, times] of touched) {
      bump(t.nodeCalls, k, n);
      if (opts.buckets) addBuckets(t.buckets, k, entry.counts, k === eng || k === ev ? 1 : times);
    }
  }
  return t;
}

function sum(c: Counts | undefined): number {
  let s = 0;
  for (const v of c?.values() ?? []) s += v;
  return s;
}

function outcomeRows(c: Counts | undefined): TipRow[] {
  return [...c ?? []].sort((a, b) => bySeverity(a[0], b[0])).map(([o, n]) => ({ label: o, n, o }));
}

const fmt = (n: number): string => n.toLocaleString("en-US");

// runsFact: "N calls → R handler runs (×F)", F per call that ran a handler.
function runsFact(calls: number, h: Handled | undefined): string {
  const fact = fmt(calls) + " calls → " + fmt(h?.runs ?? 0) + " handler runs";
  if (!h) return fact;
  return fact + (h.calls < calls ? " on " + fmt(h.calls) : "") + " (×" + (h.runs / h.calls).toFixed(1) + ")";
}

export class Snapshot {
  readonly gen: number;
  readonly key: string;
  readonly order: readonly string[]; // node keys in plan order (group before its members)
  readonly nodes: readonly SnapNode[];
  private readonly byKey: Map<string, SnapNode>;
  private readonly plan: LayoutPlan;
  private readonly eventLabel: EventLabel;

  constructor(plan: LayoutPlan, eventLabel: EventLabel) {
    this.plan = plan;
    this.eventLabel = eventLabel;
    this.gen = plan.input.gen;
    this.key = plan.input.key;
    this.order = plan.input.nodes.map((n) => n.key);

    const shownOf = new Map<string, string[]>();
    for (const n of plan.input.nodes) {
      if (n.parent === undefined) continue;
      const list = shownOf.get(n.parent);
      if (list) list.push(n.key);
      else shownOf.set(n.parent, [n.key]);
    }
    this.nodes = plan.input.nodes.map((n): SnapNode => {
      if (n.col !== "group") {
        const label = displayLabel(n.col, n.name);
        return { ...n, label, dataName: n.name, ghost: n.col === "handler" && plan.ghost.has(n.name) };
      }
      const members = plan.groups.members.get(n.name) ?? [];
      const label = groupLabel(n.name, members);
      return {
        ...n, label, dataName: label, ghost: false,
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

  // memberLabel: a member drawn inside an expanded group, without the
  // group's prefix ("read-skeleton" under guards.*).
  memberLabel(key: string): string {
    const n = this.byKey.get(key);
    if (!n) return splitKey(key)[1];
    const parent = n.parent !== undefined ? this.byKey.get(n.parent)?.group : undefined;
    if (!parent) return n.label;
    const prefix = parent.label.slice(0, -1); // "guards." / "aeye-"
    return n.name.startsWith(prefix) ? n.name.slice(prefix.length) : n.name;
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

  totals(paths: Iterable<PathEntry>, buckets = false): Totals {
    const list = [...paths];
    const t = computeTotals(list, (p) => this.displayBranches(p), { buckets });
    for (const n of this.nodes) {
      if (!n.group?.expanded) continue;
      t.nodeTotals.set(n.key, n.group.shown.reduce((a, k) => a + (t.nodeTotals.get(k) ?? 0), 0));
    }
    for (const entry of list) {
      const n = pathTotal(entry);
      if (n === 0) continue;
      for (const b of pathBranches(entry.path, this.eventLabel)) {
        const h = b.nodes[2];
        if (h?.[0] !== "handler") continue;
        const dk = this.displayKey("handler", h[1]);
        if (!this.byKey.get(dk)?.group) continue;
        let m = t.members.get(dk);
        if (!m) t.members.set(dk, m = new Map());
        bumpIn(m, h[1], b.nodes[3][1], n);
      }
    }
    return t;
  }

  // subset: the totals of every drawn branch through the node (upstream
  // prefix and downstream suffix); an expanded group header targets its
  // members.
  subset(key: string, paths: Iterable<PathEntry>): Totals {
    const node = this.byKey.get(key);
    const targets = new Set(node?.group?.expanded ? node.group.shown : [key]);
    return computeTotals(paths, (p) => this.displayBranches(p), { keep: (keys) => keys.some((k) => targets.has(k)) });
  }

  tooltip(key: string, totals: Totals, sub: Totals): Tip | null {
    const node = this.byKey.get(key);
    if (!node) return null;
    const title = node.group ? node.group.label : node.label;
    const n = totals.nodeTotals.get(key) ?? 0;
    const sections: TipSection[] = [];
    const facts: string[] = [];
    let kind: string = COL_TITLES[node.col];

    if (node.col === "engine" || node.col === "event") {
      facts.push(runsFact(n, totals.handled.get(key)));
      sections.push({ head: "outcomes · branches", rows: outcomeRows(totals.nodeOutcomes.get(key)) });
    } else if (node.col === "outcome") {
      facts.push(fmt(n) + " branches on " + fmt(totals.nodeCalls.get(key) ?? 0) + " calls");
      const from: TipRow[] = [];
      for (const [ek, en] of totals.edgeTotals) {
        const [a, b] = splitEdgeKey(ek);
        if (b !== key || en === 0) continue;
        const an = this.byKey.get(a);
        from.push({ label: (an?.group?.label ?? an?.label ?? splitKey(a)[1]) + (an?.col === "event" ? " (no handler)" : ""), n: en });
      }
      sections.push({ head: "from · branches", rows: from.sort((x, y) => y.n - x.n).slice(0, 8) });
    } else {
      if (node.group) kind = COL_TITLES.group + " · " + node.group.members.length;
      facts.push(fmt(n) + " runs on " + fmt(totals.nodeCalls.get(key) ?? sub.calls) + " calls");
      if (node.ghost) facts.push("not in the installed table");
      sections.push({ head: "outcomes · branches", rows: outcomeRows(this.outcomesOf(key, totals)) });
      if (node.group) {
        const rows = this.memberRows(key, node.group, totals);
        sections.push({ head: "member · runs", rows });
      }
      const fed = new Map<string, number>();
      for (const [ek, en] of sub.edgeTotals) {
        const [a, b] = splitEdgeKey(ek);
        if (splitKey(a)[0] !== "engine") continue;
        bump(fed, splitKey(a)[1] + " · " + (this.byKey.get(b)?.label ?? splitKey(b)[1]), en);
      }
      sections.push({ head: "fed by · calls", rows: [...fed].sort((x, y) => y[1] - x[1]).slice(0, 6).map(([label, c]) => ({ label, n: c })) });
    }
    const hint = !node.group ? "click to filter · shift-click to add"
      : node.group.forced ? "held open by the handler filter"
      : node.group.expanded ? "click to fold"
      : "click to expand";
    return { title, kind, facts, sections, hint };
  }

  // outcomesOf a handler-column node: an expanded group sums its members.
  private outcomesOf(key: string, totals: Totals): Counts | undefined {
    const g = this.byKey.get(key)?.group;
    if (!g?.expanded) return totals.nodeOutcomes.get(key);
    const out: Counts = new Map();
    for (const k of g.shown) for (const [o, c] of totals.nodeOutcomes.get(k) ?? []) bump(out, o, c);
    return out;
  }

  private memberRows(key: string, g: SnapGroup, totals: Totals): TipRow[] {
    const byMember = new Map<string, Counts>();
    if (g.expanded) {
      for (const k of g.shown) byMember.set(splitKey(k)[1], totals.nodeOutcomes.get(k) ?? new Map());
    } else {
      for (const [id, c] of totals.members.get(key) ?? []) byMember.set(id, c);
    }
    const prefix = g.label.slice(0, -1);
    return g.members.map((id): TipRow => {
      const c = byMember.get(id);
      const loud = new Map([...c ?? []].filter(([o]) => isLoud(o)));
      return { label: id.startsWith(prefix) ? id.slice(prefix.length) : id, n: sum(c), note: loud.size ? loud : undefined };
    }).sort((a, b) => b.n - a.n);
  }
}
