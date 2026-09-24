// FlowModel holds the flow view's data state (spec §4.4): paths + counts,
// the live ring, facets, active filters, sticky-shown nodes, topology,
// showIdle and the user's group toggles. It knows nothing about what is
// drawn — the rendered snapshot (snapshot.ts) does — so nothing here may be
// read to interpret a click on a drawn node.

import { COLUMNS } from "../types.ts";
import type { Col, Facets, FlowResponse, Filters, PathLike, PlanNode, TableResponse } from "../types.ts";
import { pathBranches } from "./branches.ts";
import type { EventLabel } from "./branches.ts";
import { computeGroups } from "./groups.ts";
import type { Groups } from "./groups.ts";
import { nodeKey, pathKey } from "./keys.ts";
import type { Branch } from "./keys.ts";
import { advanceRing, bucketIndex } from "./ring.ts";
import type { Ring } from "./ring.ts";
import { buildTopology, columnNames, observeNode, observePath } from "./topology.ts";
import type { Topology } from "./topology.ts";

export interface PathEntry { path: PathLike; counts: number[]; }

export interface GroupRecord { expanded: boolean; forced: boolean; }

export interface PlanInput {
  gen: number;
  key: string;         // stable signature of nodes (+ parent) — equal key ⇒ the drawn snapshot is still exact
  nodes: PlanNode[];   // ordered: previous snapshot order for kept nodes, then new ones in topology order
}

// LayoutPlan is the node set to draw plus the display decisions it was
// derived from; a snapshot is built from a plan, so every display question is
// answered as of the plan, never live.
export interface LayoutPlan {
  input: PlanInput;
  groups: Groups;
  expanded: Set<string>; // group keys expanded as of this plan
  forced: Set<string>; // group keys holding an active handler filter value
  ghost: Set<string>; // handler ids not in the installed table
}

export function emptyFilters(): Filters {
  return { engine: [], event: [], handler: [], outcome: [], verdict: [], session: "" };
}

function emptyColumnSets(): Record<Col, Set<string>> {
  return { engine: new Set(), event: new Set(), handler: new Set(), outcome: new Set() };
}

function sameValues(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  const sa = a.slice().sort();
  const sb = b.slice().sort();
  return sa.every((v, i) => v === sb[i]);
}

export function pathTotal(entry: PathEntry): number {
  return entry.counts.reduce((a, b) => a + b, 0);
}

export class FlowModel {
  readonly eventLabel: EventLabel;
  paths = new Map<string, PathEntry>();
  ring: Ring | null = null;
  facets: Facets | null = null;
  active: Filters = emptyFilters();
  sticky: Record<Col, Set<string>> = emptyColumnSets();
  topo: Topology | null = null;
  showIdle = false;
  userExpanded = new Map<string, boolean>();
  // Per-node drawn counts keyed by the node's own (col, name), refreshed per
  // layout plan: isBusy reads it so a node carrying traffic is never hidden
  // by a facet-key mismatch (a router-error record with an empty
  // native_event: Go's facet skips "", eventLabel draws "—").
  private rawTotals = new Map<string, number>();
  private groupsCache: { topo: Topology; n: number; groups: Groups } | null = null;

  constructor(eventLabel: EventLabel) {
    this.eventLabel = eventLabel;
  }

  // load replaces the data state with one /api/flow response.
  load(table: TableResponse | null, resp: FlowResponse, filters: Filters): void {
    // A group toggle overrides the default for the question the branch
    // filters ask; a new question starts from its own defaults (spec §4.4).
    if (!sameValues(this.active.handler, filters.handler) || !sameValues(this.active.outcome, filters.outcome)) {
      this.userExpanded.clear();
    }
    const topo = buildTopology(table);
    this.topo = topo;
    this.active = filters;
    this.facets = resp.facets;
    this.sticky = emptyColumnSets();
    for (const col of COLUMNS) {
      for (const [name, n] of Object.entries(resp.facets[col])) if (n > 0) observeNode(topo, col, name);
      for (const name of filters[col]) observeNode(topo, col, name);
    }
    this.paths = new Map();
    for (const p of resp.paths ?? []) {
      this.paths.set(pathKey(p), { path: p, counts: p.counts.slice() });
      observePath(topo, pathBranches(p, this.eventLabel));
    }
    this.ring = resp.window > 0 ? { start: resp.bucket_start, bucketMs: resp.bucket_ms, n: resp.window } : null;
  }

  // clear drops the paths after a failed load; the topology stays so the
  // last drawing keeps its labels until the next load.
  clear(): void {
    this.paths = new Map();
    this.ring = null;
  }

  advance(nowMs: number): boolean {
    if (!this.ring) return false;
    return advanceRing(this.ring, Array.from(this.paths.values(), (e) => e.counts), nowMs);
  }

  // recordCall counts one (already pruned) call into its path and observes
  // its nodes/edges; returns the path's raw branches.
  recordCall(path: PathLike, ts: string): Branch[] {
    const key = pathKey(path);
    let entry = this.paths.get(key);
    if (!entry) {
      entry = { path, counts: new Array<number>(this.ring ? this.ring.n : 1).fill(0) };
      this.paths.set(key, entry);
    }
    if (this.ring) {
      const idx = bucketIndex(this.ring, ts);
      if (idx !== null) entry.counts[idx]++;
    } else {
      entry.counts[0]++;
    }
    const branches = pathBranches(entry.path, this.eventLabel);
    if (this.topo) observePath(this.topo, branches);
    return branches;
  }

  stickyShow(col: Col, name: string): void {
    this.sticky[col].add(name);
  }

  groups(): Groups {
    const topo = this.topo;
    if (!topo) return { groupOf: new Map(), members: new Map() };
    const ids = columnNames(topo, "handler");
    const c = this.groupsCache;
    if (c && c.topo === topo && c.n === ids.length) return c.groups;
    const groups = computeGroups(ids);
    this.groupsCache = { topo, n: ids.length, groups };
    return groups;
  }

  facet(col: Col, name: string): number {
    const f = this.facets?.[col];
    return f && Object.hasOwn(f, name) ? f[name] : 0;
  }

  // isBusy: shown without the idle toggle.
  isBusy(col: Col, name: string): boolean {
    return this.facet(col, name) > 0 || this.active[col].includes(name) || this.sticky[col].has(name)
      || (this.rawTotals.get(nodeKey(col, name)) ?? 0) > 0;
  }

  isShown(col: Col, name: string): boolean {
    return this.showIdle || this.isBusy(col, name);
  }

  refreshRawTotals(): void {
    const totals = new Map<string, number>();
    for (const entry of this.paths.values()) {
      const n = pathTotal(entry);
      if (n === 0) continue;
      for (const b of pathBranches(entry.path, this.eventLabel)) {
        for (const [col, name] of b.nodes) {
          const k = nodeKey(col, name);
          totals.set(k, (totals.get(k) ?? 0) + n);
        }
      }
    }
    this.rawTotals = totals;
  }

  // groupState is the LIVE expansion: forced (an active handler filter value
  // in the group) wins, then an explicit user toggle, then auto-open under a
  // branch filter (handler/outcome) when at most 4 members are shown. Only
  // layoutPlan reads it; a click reads the rendered snapshot instead.
  groupState(key: string): GroupRecord {
    const groups = this.groups();
    if (this.active.handler.some((id) => groups.groupOf.get(id) === key)) return { expanded: true, forced: true };
    const user = this.userExpanded.get(key);
    if (user !== undefined) return { expanded: user, forced: false };
    if (!this.active.handler.length && !this.active.outcome.length) return { expanded: false, forced: false };
    const shown = (groups.members.get(key) ?? []).filter((m) => this.isShown("handler", m));
    return { expanded: shown.length <= 4, forced: false };
  }

  idleCount(): number {
    const topo = this.topo;
    if (!topo) return 0;
    let idle = 0;
    for (const col of COLUMNS) for (const name of columnNames(topo, col)) if (!this.isBusy(col, name)) idle++;
    return idle;
  }

  // layoutPlan derives the next node set to draw from the data state. Node order:
  // prevOrder's order for kept nodes, then new ones in topology order; each
  // group node is followed directly by its members. key is a signature of
  // what a snapshot bakes in per node — identity, parent, and for a group its
  // member list and forced flag, for a handler its ghost flag — never counts
  // or edges, so an equal key means the drawn snapshot is still exact.
  layoutPlan(prevOrder: readonly string[]): LayoutPlan | null {
    const topo = this.topo;
    if (!topo) return null;
    this.refreshRawTotals();
    const groups = this.groups();

    const forced = new Set<string>();
    const expanded = new Set<string>();
    for (const key of groups.members.keys()) {
      const st = this.groupState(key);
      if (st.forced) forced.add(key);
      if (st.expanded) expanded.add(key);
    }

    const nodes: PlanNode[] = [];
    const plain = (col: Col) => {
      for (const name of columnNames(topo, col)) {
        if (this.isShown(col, name)) nodes.push({ key: nodeKey(col, name), col, name });
      }
    };
    plain("engine");
    plain("event");
    const seenGroups = new Set<string>();
    for (const id of columnNames(topo, "handler")) {
      const g = groups.groupOf.get(id);
      if (g === undefined) {
        if (this.isShown("handler", id)) nodes.push({ key: nodeKey("handler", id), col: "handler", name: id });
        continue;
      }
      if (seenGroups.has(g)) continue;
      seenGroups.add(g);
      const shown = (groups.members.get(g) ?? []).filter((m) => this.isShown("handler", m));
      if (shown.length === 0) continue;
      const gk = nodeKey("group", g);
      nodes.push({ key: gk, col: "group", name: g });
      if (!expanded.has(g)) continue;
      for (const m of shown) nodes.push({ key: nodeKey("handler", m), col: "handler", name: m, parent: gk });
    }
    plain("outcome");

    const ghost = new Set<string>();
    for (const n of nodes) if (n.col === "handler" && !topo.handlerSet.has(n.name)) ghost.add(n.name);

    const prevIdx = new Map(prevOrder.map((k, i) => [k, i]));
    const kept = nodes.filter((n) => prevIdx.has(n.key))
      .sort((a, b) => (prevIdx.get(a.key) ?? 0) - (prevIdx.get(b.key) ?? 0));
    const ordered = kept.concat(nodes.filter((n) => !prevIdx.has(n.key)));
    const children = new Map<string, PlanNode[]>();
    for (const n of ordered) {
      if (n.parent === undefined) continue;
      const list = children.get(n.parent);
      if (list) list.push(n);
      else children.set(n.parent, [n]);
    }
    const out: PlanNode[] = [];
    for (const n of ordered) {
      if (n.parent !== undefined) continue;
      out.push(n);
      out.push(...children.get(n.key) ?? []);
    }

    const sig = out.map((n) => {
      let s = n.key + "\x02" + (n.parent ?? "");
      if (n.col === "group") s += "\x02" + (forced.has(n.name) ? "F" : "") + (groups.members.get(n.name) ?? []).join("\x03");
      else if (ghost.has(n.name) && n.col === "handler") s += "\x02G";
      return s;
    });
    const key = sig.sort().join("\n");

    return { input: { gen: 0, key, nodes: out }, groups, expanded, forced, ghost };
  }
}
