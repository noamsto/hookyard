// Shapes shared across the flow view's model and view layers.

export type Col = "engine" | "event" | "handler" | "outcome";
export type DisplayCol = Col | "group";
export const COLUMNS: Col[] = ["engine", "event", "handler", "outcome"];
export const COL_INDEX: Record<DisplayCol, number> = { engine: 0, event: 1, handler: 2, group: 2, outcome: 3 };

export interface Hop { name: string; outcome: string; }
export interface PathLike {           // a FlowPath or a live rec — same field names on the wire
  engine: string; canonical_event: string; native_event: string;
  router: string; verdict: string; handlers: Hop[] | null;   // wire sends null or []
}
export interface FlowPath extends PathLike { counts: number[]; }
export interface Facets { engine: Record<string, number>; event: Record<string, number>;
  handler: Record<string, number>; outcome: Record<string, number>; }
export interface FlowResponse { day: string; window: number; bucket_start: number; bucket_ms: number;
  calls: number; branches: number; next_offset: number; paths: FlowPath[] | null; facets: Facets; }
export interface LiveRec extends PathLike { ts: string; }
export interface Entry { rec: LiveRec; day: string; offset: number; hits?: number[] | null; }
export interface TableHandler { id: string; engines?: string[] | null; events?: string[] | null; }
export interface TableResponse { handlers: TableHandler[] | null; engines: string[] | null;
  events: string[] | null; error?: string; }
export interface Filters { engine: string[]; event: string[]; handler: string[];
  outcome: string[]; verdict: string[]; session: string; }

// Node keys: col + "\x00" + name (display col "group" for a group); edge keys: a + "\x01" + b.
export interface PlanNode { key: string; col: DisplayCol; name: string;
  parent?: string;     // group node key, when this handler is a member of an EXPANDED group
}
