// Test helpers shared by the model's *.test.ts: a copy of app.js's
// eventLabel rule, a fake clock with timers, and builders for /api/table,
// /api/flow and hookyard:call payloads.

import type { Entry, Facets, Filters, FlowPath, FlowResponse, PathLike, TableResponse } from "../types.ts";
import { pathBranches } from "./branches.ts";
import { FlowController } from "./controller.ts";
import { emptyFilters } from "./state.ts";

export function eventLabel(rec: PathLike): string {
  if (rec.router === "error") return rec.native_event || "—";
  if (rec.canonical_event) return rec.canonical_event;
  if (rec.native_event) return rec.engine + ":" + rec.native_event;
  return "—";
}

export class FakeClock {
  t: number;
  private seq = 0;
  private readonly timers = new Map<number, { at: number; fn: () => void }>();

  constructor(t = 0) {
    this.t = t;
  }

  now = (): number => this.t;

  setTimeout = (fn: () => void, ms: number): number => {
    const id = ++this.seq;
    this.timers.set(id, { at: this.t + ms, fn });
    return id;
  };

  clearTimeout = (id: number): void => {
    this.timers.delete(id);
  };

  // advance runs every timer due within ms, in time order, at its own time.
  advance(ms: number): void {
    const end = this.t + ms;
    for (;;) {
      let next: [number, { at: number; fn: () => void }] | null = null;
      for (const e of this.timers) if (e[1].at <= end && (!next || e[1].at < next[1].at)) next = e;
      if (!next) break;
      this.timers.delete(next[0]);
      this.t = next[1].at;
      next[1].fn();
    }
    this.t = end;
  }
}

// settle lets every pending promise callback run.
export function settle(): Promise<void> {
  return new Promise((r) => setImmediate(r));
}

export const GUARDS = ["a", "b", "c", "d", "e", "f", "g", "h"].map((s) => "guards." + s);
export const GUARDS_PI = ["a", "b", "c", "d", "e", "f", "g", "h"].map((s) => "guards.pi." + s);
export const AEYE = ["a", "b", "c", "d", "e", "f", "g"].map((s) => "aeye-" + s);
export const HOUSTON = ["houston.state", "houston.state.observe"];

export function table(): TableResponse {
  const ids = [...GUARDS, ...GUARDS_PI, ...AEYE, ...HOUSTON, "lint"];
  return {
    handlers: ids.map((id) => ({ id, engines: ["claude", "codex"], events: ["PreToolUse"] })),
    engines: ["claude", "codex", "cursor", "pi"],
    events: ["PreToolUse", "Stop"],
  };
}

export interface Call { engine?: string; event?: string; native?: string; router?: string; verdict?: string; hops?: [string, string][]; }

export function pathOf(c: Call): PathLike {
  return {
    engine: c.engine ?? "claude",
    canonical_event: c.event ?? "PreToolUse",
    native_event: c.native ?? "",
    router: c.router ?? "ok",
    verdict: c.verdict ?? "allow",
    handlers: (c.hops ?? []).map(([name, outcome]) => ({ name, outcome })),
  };
}

// flow builds an /api/flow response: each [call, count] becomes one path in
// the last bucket; facets count every call/branch (no filter subtleties).
export function flow(paths: [Call, number][], opts: { window?: number; bucketStart?: number } = {}): FlowResponse {
  const window = opts.window ?? 0;
  const facets: Facets = { engine: {}, event: {}, handler: {}, outcome: {} };
  const bump = (m: Record<string, number>, k: string, n: number) => { m[k] = (m[k] ?? 0) + n; };
  const out: FlowPath[] = [];
  let calls = 0;
  let branches = 0;
  for (const [c, n] of paths) {
    const p = pathOf(c);
    const counts = new Array<number>(Math.max(1, window)).fill(0);
    counts[counts.length - 1] = n;
    out.push({ ...p, counts });
    const bs = pathBranches(p, eventLabel);
    calls += n;
    branches += n * bs.length;
    bump(facets.engine, p.engine, n);
    bump(facets.event, eventLabel(p), n);
    for (const b of bs) {
      if (b.nodes.length === 4) bump(facets.handler, b.nodes[2][1], n);
      bump(facets.outcome, b.nodes[b.nodes.length - 1][1], n);
    }
  }
  return {
    day: "2026-09-24", window, bucket_start: opts.bucketStart ?? 0, bucket_ms: window > 0 ? 60_000 : 0,
    calls, branches, next_offset: 1000, paths: out, facets,
  };
}

export function entry(offset: number, c: Call, ts: string, hits?: number[]): Entry {
  return { rec: { ...pathOf(c), ts }, day: DAY, offset, hits };
}

export const DAY = "2026-09-24";
// 12:00:30Z on DAY: a live window of 10 buckets ending at 12:00.
export const NOW = Date.parse(DAY + "T12:00:30Z");
export const BUCKET_START = Date.parse(DAY + "T11:51:00Z");

export interface Harness {
  c: FlowController;
  clock: FakeClock;
  filters: Filters;
  flowURLs: string[];
  setFlow: (resp: FlowResponse) => void;
  setTable: (t: TableResponse) => void;
  failFlow: (err: Error | null) => void;
}

export function harness(): Harness {
  const clock = new FakeClock(NOW);
  const filters = emptyFilters();
  const flowURLs: string[] = [];
  let tableResp = table();
  let flowResp = flow([]);
  let flowErr: Error | null = null;
  const c = new FlowController({
    fetchJSON: async (url) => {
      if (url === "/api/table") return tableResp;
      flowURLs.push(url);
      if (flowErr) throw flowErr;
      return flowResp;
    },
    filterParams: () => {
      const p = new URLSearchParams();
      for (const f of ["engine", "event", "handler", "outcome", "verdict"] as const) {
        for (const v of filters[f]) p.append(f, v);
      }
      return p;
    },
    activeFilters: () => structuredClone(filters),
    eventLabel,
    now: clock.now,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
  });
  return {
    c, clock, filters, flowURLs,
    setFlow: (r) => { flowResp = r; },
    setTable: (t) => { tableResp = t; },
    failFlow: (e) => { flowErr = e; },
  };
}

// loadAndCommit syncs a live (or past) day and lets the fetch land; the
// relayout it asks for commits synchronously.
export async function loadAndCommit(h: Harness, live = true): Promise<void> {
  await h.c.sync({ day: DAY, live });
  await settle();
}
