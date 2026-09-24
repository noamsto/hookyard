import type { Col } from "../types.ts";
import { nodeKey } from "./keys.ts";

export interface Groups {
  groupOf: Map<string, string>; // handler id -> group key
  members: Map<string, string[]>; // group key -> member ids, in input order
}

export function parentKey(k: string): string | null {
  const dot = k.lastIndexOf(".");
  if (dot !== -1) return k.slice(0, dot);
  const dash = k.indexOf("-");
  return dash > 0 ? k.slice(0, dash) : null;
}

export function keyDepth(k: string): number {
  let d = 0;
  for (let p: string | null = k; p !== null; p = parentKey(p)) d++;
  return d;
}

// computeGroups bubbles singleton keys up to a fixed point, deepest first:
// moving every singleton at once would let houston.state (key houston) and
// houston.state.observe (key houston.state) pass each other and both end
// ungrouped.
export function computeGroups(ids: readonly string[]): Groups {
  const keyOf = new Map<string, string | null>();
  for (const id of ids) keyOf.set(id, parentKey(id));
  for (;;) {
    const byKey = new Map<string, string[]>();
    for (const [id, k] of keyOf) {
      if (k === null) continue;
      const held = byKey.get(k);
      if (held) held.push(id);
      else byKey.set(k, [id]);
    }
    let deepest = 0;
    for (const [k, held] of byKey) if (held.length === 1) deepest = Math.max(deepest, keyDepth(k));
    if (deepest === 0) break;
    for (const [k, held] of byKey) {
      if (held.length === 1 && keyDepth(k) === deepest) keyOf.set(held[0], parentKey(k));
    }
  }
  const groupOf = new Map<string, string>();
  const members = new Map<string, string[]>();
  for (const id of ids) {
    const k = keyOf.get(id);
    if (k === null || k === undefined) continue;
    groupOf.set(id, k);
    const list = members.get(k);
    if (list) list.push(id);
    else members.set(k, [id]);
  }
  return { groupOf, members };
}

export function groupLabel(key: string, members: readonly string[]): string {
  const dash = members.every((id) => id.startsWith(key + "-"));
  return key + (dash ? "-*" : ".*");
}

// displayKey maps a raw branch node to the node that draws it: a handler in a
// group that is not expanded lands on the group.
export function displayKeyFor(groupOf: Map<string, string>, expanded: Set<string>, col: Col, name: string): string {
  if (col === "handler") {
    const key = groupOf.get(name);
    if (key !== undefined && !expanded.has(key)) return nodeKey("group", key);
  }
  return nodeKey(col, name);
}
