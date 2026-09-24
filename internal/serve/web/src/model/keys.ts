import type { Col, DisplayCol, PathLike } from "../types.ts";

// A raw branch node: the column and name a record names, before the display
// mapping folds a grouped handler onto its collapsed group.
export type RawNode = [Col, string];
export interface Branch { nodes: RawNode[]; }

export function nodeKey(col: DisplayCol, name: string): string {
  return col + "\x00" + name;
}

export function splitKey(key: string): [DisplayCol, string] {
  const i = key.indexOf("\x00");
  return [key.slice(0, i) as DisplayCol, key.slice(i + 1)];
}

export function edgeKey(aKey: string, bKey: string): string {
  return aKey + "\x01" + bKey;
}

export function splitEdgeKey(key: string): [string, string] {
  const i = key.indexOf("\x01");
  return [key.slice(0, i), key.slice(i + 1)];
}

// pathKey groups a path or live record by the same tuple Go's flowKey groups
// by, so a live call merges into the /api/flow aggregate it belongs to.
export function pathKey(p: PathLike): string {
  const hh = (p.handlers ?? []).map((h) => h.name + "\x02" + h.outcome).join("\x03");
  return [p.engine, p.canonical_event, p.native_event, p.router, p.verdict, hh].join("\x01");
}

export function branchKey(b: Branch): string {
  return b.nodes.map(([c, n]) => nodeKey(c, n)).join("\x01");
}

export function consecutivePairs<T>(items: readonly T[]): [T, T][] {
  const pairs: [T, T][] = [];
  for (let i = 0; i < items.length - 1; i++) pairs.push([items[i], items[i + 1]]);
  return pairs;
}
