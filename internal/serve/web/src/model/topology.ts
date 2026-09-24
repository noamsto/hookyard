import { OUTCOMES, ROUTER_ERROR } from "../constants.ts";
import type { Col, TableResponse } from "../types.ts";
import { consecutivePairs, edgeKey, nodeKey } from "./keys.ts";
import type { Branch, RawNode } from "./keys.ts";

export type RawEdge = [RawNode, RawNode];

export interface Topology {
  engineOrder: string[];
  eventOrder: string[];
  handlerOrder: string[];
  handlerSet: Set<string>;
  outcomeOrder: string[];
  skeletonEdges: Map<string, RawEdge>;
  // Observed beyond the skeleton: ghost handlers, engine-scoped events no
  // handler names, unusual outcomes.
  extra: Record<Col, string[]>;
  extraSeen: Record<Col, Set<string>>;
  observedEdges: Map<string, RawEdge>;
}

// buildTopology builds the fixed skeleton from the handler table. It is
// rebuilt on every load, so a day/filter switch carries no stale ghosts.
export function buildTopology(table: TableResponse | null): Topology {
  const handlers = table?.handlers ?? [];
  const engineOrder = (table?.engines ?? []).slice();

  const eventOrder = (table?.events ?? []).slice();
  const eventSeen = new Set(eventOrder);
  for (const h of handlers) {
    for (const e of h.events ?? []) {
      if (e.includes(":") && !eventSeen.has(e)) {
        eventSeen.add(e);
        eventOrder.push(e);
      }
    }
  }

  const handlerOrder = handlers.map((h) => h.id);
  const outcomeOrder = OUTCOMES.concat([ROUTER_ERROR]);

  const skeletonEdges = new Map<string, RawEdge>();
  const addSkeleton = (a: RawNode, b: RawNode) =>
    skeletonEdges.set(edgeKey(nodeKey(a[0], a[1]), nodeKey(b[0], b[1])), [a, b]);
  for (const h of handlers) {
    for (const e of h.events ?? []) {
      addSkeleton(["event", e], ["handler", h.id]);
      const colon = e.indexOf(":");
      for (const g of h.engines ?? []) {
        if (colon === -1 || e.slice(0, colon) === g) addSkeleton(["engine", g], ["event", e]);
      }
    }
  }

  return {
    engineOrder, eventOrder, handlerOrder, outcomeOrder,
    handlerSet: new Set(handlerOrder),
    skeletonEdges,
    extra: { engine: [], event: [], handler: [], outcome: [] },
    extraSeen: {
      engine: new Set(engineOrder),
      event: new Set(eventOrder),
      handler: new Set(handlerOrder),
      outcome: new Set(outcomeOrder),
    },
    observedEdges: new Map(),
  };
}

export function observeNode(topo: Topology, col: Col, name: string): boolean {
  if (topo.extraSeen[col].has(name)) return false;
  topo.extraSeen[col].add(name);
  topo.extra[col].push(name);
  return true;
}

// observePath registers a path's branches into the topology. nodes and edges
// are reported apart: only a node-set change can matter to the layout.
export function observePath(topo: Topology, branches: readonly Branch[]): { nodes: boolean; edges: boolean } {
  let nodes = false;
  let edges = false;
  for (const b of branches) {
    for (const [col, name] of b.nodes) {
      if (observeNode(topo, col, name)) nodes = true;
    }
    for (const [a, c] of consecutivePairs(b.nodes)) {
      const k = edgeKey(nodeKey(a[0], a[1]), nodeKey(c[0], c[1]));
      if (!topo.observedEdges.has(k)) {
        topo.observedEdges.set(k, [a, c]);
        edges = true;
      }
    }
  }
  return { nodes, edges };
}

export function columnNames(topo: Topology, col: Col): string[] {
  const base = col === "engine" ? topo.engineOrder
    : col === "event" ? topo.eventOrder
    : col === "handler" ? topo.handlerOrder
    : topo.outcomeOrder;
  return base.concat(topo.extra[col]);
}
