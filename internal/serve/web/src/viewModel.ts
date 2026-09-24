// Builds React Flow nodes/edges from a controller's rendered snapshot +
// throttled totals + hover state. Pure (given a Snapshot and Totals object)
// so it never reads live model state (#96 R1's invariant extends to the view).
import { COL_HEADERS } from "./constants.ts";
import type { CardData, CardNodeType } from "./nodes/CardNode.tsx";
import type { GroupData, GroupNodeType } from "./nodes/GroupNode.tsx";
import type { HeaderData, HeaderNodeType } from "./nodes/HeaderNode.tsx";
import type { CountData, CountEdgeType } from "./edges/CountEdge.tsx";
import type { Col } from "./types.ts";
import { COL_INDEX, COLUMNS } from "./types.ts";
import type { FlowController } from "./model/controller.ts";
import { outcomeClass, unitOf } from "./model/snapshot.ts";
import type { DisplayEdge, Snapshot, Totals } from "./model/snapshot.ts";

const HEADER_GAP = 40;

export type FlowNode = CardNodeType | GroupNodeType | HeaderNodeType;
export interface Highlight { nodes: Set<string>; edges: Set<string>; }

// All four headers sit on one common row, above the highest top-level node
// in any column, so they never stagger to their own column's height.
function topRowY(snap: Snapshot): number {
  let minY: number | undefined;
  for (const n of snap.nodes) {
    if (n.parent !== undefined) continue;
    if (minY === undefined || n.box.y < minY) minY = n.box.y;
  }
  return (minY ?? 0) - HEADER_GAP;
}

export function buildNodes(snap: Snapshot | null, totals: Totals, controller: FlowController, hl: Highlight | null): FlowNode[] {
  if (!snap) return [];
  const nodes: FlowNode[] = [];

  const headerY = topRowY(snap);
  for (const col of COLUMNS) {
    const x = snap.columnX[COL_INDEX[col]] ?? 0;
    const data: HeaderData = { title: COL_HEADERS[col] };
    nodes.push({
      id: "header\x00" + col, type: "colHeader", position: { x, y: headerY }, data,
      draggable: false, selectable: false, focusable: false,
    });
  }

  for (const n of snap.nodes) {
    const count = totals.nodeTotals.get(n.key) ?? 0;
    const on = hl?.nodes.has(n.key) ?? false;
    // Both style (actual rendered size) and width/height (so MiniMap's
    // nodeHasDimensions sees a size before React Flow's own async
    // measurement would otherwise land).
    const style = { width: n.box.w, height: n.box.h };
    if (n.group) {
      const data: GroupData = {
        dataName: n.group.label, tipName: n.tipName, members: n.group.members, n: count,
        expanded: n.group.expanded, forced: n.group.forced, on,
      };
      const node: GroupNodeType = {
        id: n.key, type: "handlerGroup", position: { x: n.box.x, y: n.box.y },
        width: n.box.w, height: n.box.h, style, data,
        parentId: n.parent, extent: n.parent ? "parent" : undefined,
        draggable: false, connectable: false, selectable: false, focusable: false,
      };
      nodes.push(node);
      continue;
    }
    const data: CardData = {
      col: n.col, dataName: n.dataName, tipName: n.tipName, n: count, ghost: n.ghost,
      selected: controller.isSelected(n.col as Col, n.name),
      outcomeCls: n.col === "outcome" ? outcomeClass(n.name) : "",
      on,
    };
    const node: CardNodeType = {
      id: n.key, type: "card", position: { x: n.box.x, y: n.box.y },
      width: n.box.w, height: n.box.h, style, data,
      parentId: n.parent, extent: n.parent ? "parent" : undefined,
      draggable: false, connectable: false, selectable: false, focusable: false,
    };
    nodes.push(node);
  }
  return nodes;
}

export function buildEdges(snap: Snapshot | null, edges: readonly DisplayEdge[], hl: Highlight | null): CountEdgeType[] {
  if (!snap) return [];
  let maxN = 1;
  for (const e of edges) if (e.n > maxN) maxN = e.n;

  const out: CountEdgeType[] = [];
  for (const e of edges) {
    const from = snap.node(e.from);
    const to = snap.node(e.to);
    if (!from || !to) continue;
    const data: CountData = {
      fromCol: from.col, fromName: from.dataName, toCol: to.col, toName: to.dataName,
      fromTip: from.tipName, toTip: to.tipName, toUnit: unitOf(to.col),
      n: e.n, maxN, direct: e.direct, on: hl?.edges.has(e.key) ?? false,
    };
    out.push({
      id: e.key, type: "count", source: e.from, target: e.to, data,
      selectable: false, focusable: false, deletable: false,
    });
  }
  return out;
}
