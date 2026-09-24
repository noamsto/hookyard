// scene maps one drawn frame (snapshot + totals) onto Sankey nodes and
// bands. Pure, so the mapping — which node a band leaves and enters, how a
// direct verdict routes through the no-handler slot — is unit-tested apart
// from the DOM.

import { COL_INDEX } from "../types.ts";
import type { Frame } from "../model/controller.ts";
import { splitEdgeKey, splitKey } from "../model/keys.ts";
import { bySeverity, isLoud } from "../model/snapshot.ts";
import type { SnapNode } from "../model/snapshot.ts";
import type { GeoLinkIn, NodeKind, RankNode } from "./geometry.ts";

// The no-handler slot in the handler column: a call with no handler (a
// direct verdict) or a router error passes through it on its way from the
// event to the outcome, so no band crosses the handler plates.
export const PSEUDO = "pseudo\x00";

export interface SceneNode {
  key: string;
  col: number;
  kind: NodeKind;
  snap: SnapNode | null; // null for the no-handler slot
  total: number;
  outcomes: Map<string, number>;
}

export interface Scene {
  nodes: SceneNode[];
  links: GeoLinkIn[];
  groups: SnapNode[]; // open groups: drawn as a fold header over their members
}

function kindOf(n: SnapNode): NodeKind {
  if (n.col === "group") return "group";
  return n.col;
}

export function linkId(l: { edge: string; from: string; to: string; outcome: string }): string {
  return l.edge + "\x03" + l.from + "\x03" + l.to + "\x03" + l.outcome;
}

function isDirect(from: string, to: string): boolean {
  return splitKey(from)[0] === "event" && splitKey(to)[0] === "outcome";
}

export function buildScene(frame: Frame): Scene {
  const { snap, totals } = frame;
  const nodes: SceneNode[] = [];
  const groups: SnapNode[] = [];
  for (const n of snap.nodes) {
    if (n.group?.expanded) {
      groups.push(n);
      continue;
    }
    nodes.push({
      key: n.key, col: COL_INDEX[n.col], kind: kindOf(n), snap: n,
      total: totals.nodeTotals.get(n.key) ?? 0, outcomes: totals.nodeOutcomes.get(n.key) ?? new Map(),
    });
  }
  const links: GeoLinkIn[] = [];
  const pseudo: SceneNode = { key: PSEUDO, col: 2, kind: "pseudo", snap: null, total: 0, outcomes: new Map() };
  for (const [edge, outs] of totals.edgeOutcomes) {
    const [from, to] = splitEdgeKey(edge);
    if (!snap.has(from) || !snap.has(to)) continue;
    for (const [outcome, count] of outs) {
      if (count <= 0) continue;
      if (!isDirect(from, to)) {
        links.push({ edge, from, to, outcome, count });
        continue;
      }
      links.push({ edge, from, to: PSEUDO, outcome, count }, { edge, from: PSEUDO, to, outcome, count });
      pseudo.total += count;
      pseudo.outcomes.set(outcome, (pseudo.outcomes.get(outcome) ?? 0) + count);
    }
  }
  if (pseudo.total > 0) nodes.push(pseudo);
  return { nodes, links, groups };
}

// rankInput: what rankNodes needs of each scene node; an open group's
// members rank as one block.
export function rankInput(scene: Scene, order: readonly string[]): RankNode[] {
  const idx = new Map(order.map((k, i) => [k, i]));
  return scene.nodes.map((n): RankNode => ({
    key: n.key, col: n.col, kind: n.kind, name: n.snap?.name ?? "",
    group: n.snap?.parent ?? null,
    total: n.total, outcomes: n.outcomes, planIdx: idx.get(n.key) ?? order.length,
  }));
}

// loudOutcomes: a node's decisions, most consequential first.
export function loudOutcomes(outcomes: Map<string, number>): [string, number][] {
  return [...outcomes].filter(([o, c]) => isLoud(o) && c > 0).sort((a, b) => bySeverity(a[0], b[0]));
}

// singleOutcome: the one outcome every band carries, or null when the
// bands differ (a deny-only filter draws everything in one colour).
export function singleOutcome(links: readonly GeoLinkIn[]): string | null {
  let only: string | null = null;
  for (const l of links) {
    if (only === null) only = l.outcome;
    else if (l.outcome !== only) return null;
  }
  return only;
}
