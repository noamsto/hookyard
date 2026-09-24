// ELK graph adapter (plan.md Step 3): builds an ELK input graph from a
// LayoutInput and reads the laid-out result back into a LayoutResult.
//
// Node keys ("col\x00name", or a group's own "group\x00name") can carry
// control characters, so they never become ELK node ids directly. Instead
// each node gets an index-based id (n0, n1, …) and its original key is
// stashed on a custom `origKey` property; elkjs round-trips unknown
// properties through layout() unchanged, so fromElk reads `origKey` straight
// back off the result instead of needing a separate id->key map.
import type { ElkExtendedEdge, ElkNode } from "elkjs/lib/elk-api.js";
import { COL_INDEX } from "../types.ts";
import type { Box, DisplayCol, LayoutInput, LayoutNode, LayoutResult } from "../types.ts";
import { GROUP_PAD_TOP, NODE_H, NODE_W } from "../constants.ts";

const NODE_NODE_BETWEEN_LAYERS = 120;
const NODE_NODE = 16;
const GROUP_SIDE_PAD = 12;
const FALLBACK_PITCH = NODE_W + NODE_NODE_BETWEEN_LAYERS;

interface KeyedElkNode extends ElkNode {
  origKey?: string;
}

export function toElkGraph(input: LayoutInput): ElkNode {
  let nextId = 0;
  const idByKey = new Map<string, string>();
  for (const n of input.nodes) idByKey.set(n.key, `n${nextId++}`);

  const membersOf = new Map<string, LayoutNode[]>();
  const topLevel: LayoutNode[] = [];
  for (const n of input.nodes) {
    if (n.parent === undefined) {
      topLevel.push(n);
      continue;
    }
    const list = membersOf.get(n.parent);
    if (list) list.push(n);
    else membersOf.set(n.parent, [n]);
  }

  function build(n: LayoutNode): KeyedElkNode {
    const partition = String(COL_INDEX[n.col]);
    const members = membersOf.get(n.key);
    if (members && members.length > 0) {
      // Expanded group: a compound node sized by ELK to fit its members.
      return {
        id: idByKey.get(n.key)!,
        origKey: n.key,
        layoutOptions: {
          "elk.partitioning.partition": partition,
          "elk.padding": `[top=${GROUP_PAD_TOP},left=${GROUP_SIDE_PAD},bottom=${GROUP_SIDE_PAD},right=${GROUP_SIDE_PAD}]`,
        },
        children: members.map(build),
      };
    }
    // Plain leaf: a standalone node, or a collapsed group (no members present).
    return {
      id: idByKey.get(n.key)!,
      origKey: n.key,
      width: NODE_W,
      height: NODE_H,
      layoutOptions: { "elk.partitioning.partition": partition },
    };
  }

  const edges: ElkExtendedEdge[] = [];
  for (const [a, b] of input.edges) {
    const src = idByKey.get(a);
    const tgt = idByKey.get(b);
    if (src === undefined || tgt === undefined) continue; // endpoint not in `nodes` — skip
    edges.push({ id: `e${edges.length}`, sources: [src], targets: [tgt] });
  }

  return {
    id: "root",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": "RIGHT",
      "elk.partitioning.activate": "true",
      "elk.hierarchyHandling": "INCLUDE_CHILDREN",
      "elk.layered.considerModelOrder.strategy": "NODES_AND_EDGES",
      "elk.layered.spacing.nodeNodeBetweenLayers": String(NODE_NODE_BETWEEN_LAYERS),
      "elk.spacing.nodeNode": String(NODE_NODE),
      // Without this, ELK's default placement leaves sparse columns (e.g. a
      // couple of engines under a filter) pinned near the top/bottom of a
      // much taller column instead of centred against it. BALANCED spreads
      // each column's nodes around the shared vertical middle.
      "elk.layered.nodePlacement.strategy": "BRANDES_KOEPF",
      "elk.layered.nodePlacement.bk.fixedAlignment": "BALANCED",
    },
    // All edges declared at the root: INCLUDE_CHILDREN lets them cross into
    // (or between) compound group nodes without re-declaring per container.
    children: topLevel.map(build),
    edges,
  };
}

function keyCol(key: string): DisplayCol {
  return key.slice(0, key.indexOf("\x00")) as DisplayCol;
}

export function fromElk(out: ElkNode, gen: number): LayoutResult {
  const boxes: Record<string, Box> = {};
  const colMins: (number | undefined)[] = [undefined, undefined, undefined, undefined];

  function visit(node: ElkNode, topLevel: boolean): void {
    const origKey = (node as KeyedElkNode).origKey;
    if (origKey !== undefined) {
      // Children's x/y are already relative to their parent group (React
      // Flow's parentId semantics); top-level x/y are absolute.
      const box: Box = { x: node.x ?? 0, y: node.y ?? 0, w: node.width ?? NODE_W, h: node.height ?? NODE_H };
      boxes[origKey] = box;
      if (topLevel) {
        const c = COL_INDEX[keyCol(origKey)];
        colMins[c] = colMins[c] === undefined ? box.x : Math.min(colMins[c]!, box.x);
      }
    }
    for (const child of node.children ?? []) visit(child, false);
  }

  for (const child of out.children ?? []) visit(child, true);

  // columnX[c] = min x among top-level nodes in that partition; an empty
  // column falls back to the previous column's x plus a fixed pitch, so
  // headers can still be placed left-to-right in order.
  const columnX: number[] = [0, 0, 0, 0];
  for (let c = 0; c < columnX.length; c++) {
    columnX[c] = colMins[c] !== undefined ? colMins[c]! : (c === 0 ? 0 : columnX[c - 1] + FALLBACK_PITCH);
  }

  return { gen, boxes, columnX };
}
