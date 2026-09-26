// FlowController owns the flow view's data state (FlowModel), the rendered
// snapshot and the pending relayout. Pure: every browser dependency is
// injected (FlowDeps), so it runs under node --test.
//
// View API:
//   subscribe(fn) / getVersion()  the version bumps on every refresh.
//                     Count-only refreshes are throttled to <= 1 per
//                     RENDER_MS; commits and user actions refresh at once.
//   frame()           what to draw: the committed Snapshot + its Totals, or
//                     null before the first commit. Held while a press is in
//                     progress: no new snapshot or counts are drawn under
//                     the pointer; the release publishes the latest.
//   counts()          calls/branches as of the latest refresh, held or not
//   subset(key) / tooltip(key)   hover data for a drawn node key
//   meta() decisions() idleCount() error() dropped() layoutGen() pendingLayout()
//   isLive() isVisible() showIdle() isSelected(col, name)
// Inputs:
//   sync(detail)      hookyard:sync; loads when visible, else marks stale
//   onCall(entry)     hookyard:call; returns the call's branches that can pulse
//                     now, as RAW branches ([col, name] nodes). Map one to drawn
//                     edges with frame().snap.displayNodes(branch) at animation
//                     time. The same branches go to onPulse listeners, only
//                     while visible.
//   onPulse(fn)       subscribe the pulse layer; returns an unsubscribe
//   addDropped(n)     the pulse pool's overflow count (reset per load)
//   setVisible(v) setShowIdle(v)
//   pressBegin(nodeKey?) / pressEnd()   any press in the flow; pressEnd must
//                     run after the click handler (one macrotask after release)
//   toggleGroup(nodeKey)   a group click, by the group's display node key
//                     ("group\0<key>"); toggles relative to what was drawn at
//                     press; a forced group is inert
//   dispose()         clears every timer and listener

import { PENDING_CAP, PRESS_HOLD_MAX_MS, RELAYOUT_MS, RENDER_MS, WINDOW_MIN } from "../constants.ts";
import type { Col, Entry, Filters, FlowResponse, PathLike, TableResponse } from "../types.ts";
import type { EventLabel } from "./branches.ts";
import { splitKey } from "./keys.ts";
import type { Branch } from "./keys.ts";
import { bySeverity, emptyTotals, isLoud, Snapshot } from "./snapshot.ts";
import type { Tip, Totals } from "./snapshot.ts";
import { FlowModel } from "./state.ts";
import type { GroupRecord, LayoutPlan, PathEntry } from "./state.ts";

export interface FlowDeps {
  // Resolves the parsed JSON; rejects on a non-OK response ("<url>: <status>").
  fetchJSON: (url: string) => Promise<unknown>;
  filterParams: () => URLSearchParams;
  activeFilters: () => Filters;
  eventLabel: EventLabel;
  now: () => number; // epoch ms: the live ring buckets by wall-clock minute
  setTimeout: (fn: () => void, ms: number) => number;
  clearTimeout: (id: number) => void;
}

export interface SyncDetail { day: string; live: boolean; }

export type LayoutReason = "load" | "toggle" | "idle" | "live";

const RING_TICK_MS = 1000;

interface PendingLayout { plan: LayoutPlan; gen: number; }

// A frame's paths are copied at refresh: live calls count into the model's
// paths in place, and hover counts must add up against the drawn totals.
export interface Frame { snap: Snapshot; totals: Totals; paths: readonly PathEntry[]; }

interface Refresh { frame: Frame | null; calls: number; branches: number; idle: number; }

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export class FlowController {
  readonly model: FlowModel;
  private readonly deps: FlowDeps;

  private rendered: Snapshot | null = null;
  private pending: PendingLayout | null = null;
  private layoutGenSeq = 0;
  private lastCommitAt = -Infinity;
  private liveTimer: number | null = null;

  private pressed = false;
  private pressRecord: { key: string; rec: GroupRecord } | null = null;
  private pressTimer: number | null = null;

  private dayValue: string | null = null;
  private live = false;
  private visible = false;
  private stale = false;
  private cursor = -1; // a call at offset <= cursor is already counted
  private inflight = false;
  private buffered: Entry[] = [];
  private loadGen = 0;
  private fetchError = "";
  private tableError = "";
  private droppedCount = 0;

  private version = 0;
  private readonly listeners = new Set<() => void>();
  private readonly pulseListeners = new Set<(branches: Branch[]) => void>();
  private refreshTimer: number | null = null;
  private lastRefreshAt = -Infinity;
  private ringTimer: number | null = null;
  // latest: the newest refresh; drawn: what the view draws — the same
  // except while a press holds it.
  private latest: Refresh = { frame: null, calls: 0, branches: 0, idle: 0 };
  private drawn: Refresh = this.latest;

  constructor(deps: FlowDeps) {
    this.deps = deps;
    this.model = new FlowModel(deps.eventLabel);
  }

  // ---------- store ----------

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getVersion = (): number => this.version;

  onPulse(fn: (branches: Branch[]) => void): () => void {
    this.pulseListeners.add(fn);
    return () => this.pulseListeners.delete(fn);
  }

  snapshot(): Snapshot | null { return this.rendered; }
  frame(): Frame | null { return this.drawn.frame; }
  counts(): { calls: number; branches: number } { return { calls: this.latest.calls, branches: this.latest.branches }; }
  idleCount(): number { return this.latest.idle; }
  error(): string { return this.fetchError || this.tableError; }
  dropped(): number { return this.droppedCount; }
  layoutGen(): number { return this.rendered?.gen ?? 0; }
  pendingLayout(): boolean { return this.pending !== null; }
  isLive(): boolean { return this.live; }
  isVisible(): boolean { return this.visible; }
  showIdle(): boolean { return this.model.showIdle; }
  isSelected(col: Col, name: string): boolean { return this.model.active[col].includes(name); }

  meta(): string {
    if (!this.model.topo) return "";
    const fmt = (n: number) => n.toLocaleString("en-US");
    return (this.live ? "live · last " + WINDOW_MIN + " min · " : "whole day · ")
      + fmt(this.latest.calls) + " calls · " + fmt(this.latest.branches) + " branches";
  }

  // decisions: every drawn consequential outcome's branch count, most
  // consequential first.
  decisions(): [string, number][] {
    const f = this.drawn.frame;
    if (!f) return [];
    const out: [string, number][] = [];
    for (const n of f.snap.nodes) {
      const c = f.totals.nodeTotals.get(n.key) ?? 0;
      if (n.col === "outcome" && isLoud(n.name) && c > 0) out.push([n.name, c]);
    }
    return out.sort((a, b) => bySeverity(a[0], b[0]));
  }

  subset(key: string): Totals {
    const f = this.drawn.frame;
    return f ? f.snap.subset(key, f.paths) : emptyTotals();
  }

  tooltip(key: string, sub: Totals): Tip | null {
    const f = this.drawn.frame;
    return f ? f.snap.tooltip(key, f.totals, sub) : null;
  }

  // ---------- inputs ----------

  sync(detail: SyncDetail): Promise<void> {
    this.dayValue = detail.day;
    this.live = detail.live;
    this.emit(); // a pulse layer stops at once when the day stops being live
    if (!this.visible) {
      this.stale = true;
      return Promise.resolve();
    }
    return this.load();
  }

  setVisible(v: boolean): Promise<void> {
    this.visible = v;
    if (!v) {
      // No relayout follows hiding the panel; emit anyway so the pulse
      // layer sees the visibility change and cancels in-flight dots rather
      // than wait for a queued rAF that may run after the tab has flipped
      // back to visible.
      this.emit();
      return Promise.resolve();
    }
    if (this.stale) {
      this.stale = false;
      return this.load();
    }
    this.refreshNow();
    return Promise.resolve();
  }

  onCall(entry: Entry): Branch[] {
    if (this.stale) return []; // wait for the next load
    if (!this.live || entry.day !== this.dayValue) return [];
    if (this.inflight) {
      if (this.buffered.length < PENDING_CAP) this.buffered.push(entry);
      else this.stale = true; // drop and refetch on the next show/sync
      return [];
    }
    return this.addCall(entry);
  }

  addDropped(n: number): void {
    this.droppedCount += n;
    this.scheduleRefresh();
  }

  setShowIdle(v: boolean): void {
    this.model.showIdle = v;
    this.requestLayout("idle");
    this.refreshNow();
  }

  pressBegin(key?: string): void {
    this.pressed = true;
    this.pressRecord = null;
    if (key !== undefined && this.rendered) {
      const [col, name] = splitKey(key);
      const rec = col === "group" ? this.rendered.groupState(name) : undefined;
      if (rec) this.pressRecord = { key, rec };
    }
    if (this.pressTimer !== null) this.deps.clearTimeout(this.pressTimer);
    // A missed release must never freeze the view.
    this.pressTimer = this.deps.setTimeout(() => {
      this.pressTimer = null;
      this.pressEnd();
    }, PRESS_HOLD_MAX_MS);
  }

  pressEnd(): void {
    if (!this.pressed) return;
    this.pressed = false;
    this.pressRecord = null;
    if (this.pressTimer !== null) {
      this.deps.clearTimeout(this.pressTimer);
      this.pressTimer = null;
    }
    if (this.pending) this.commit();
    else if (this.drawn !== this.latest) this.refreshNow();
  }

  // toggleGroup reads the record captured at press, else the rendered
  // snapshot — never FlowModel.groupState, which may already have moved on
  // (a live sticky-show flipping an auto-open group) before the relayout.
  toggleGroup(key: string): void {
    const [col, name] = splitKey(key);
    if (col !== "group") return;
    const rec = this.pressRecord?.key === key ? this.pressRecord.rec : this.rendered?.groupState(name);
    if (!rec || rec.forced) return;
    this.model.userExpanded.set(name, !rec.expanded);
    this.requestLayout("toggle");
  }

  dispose(): void {
    for (const t of [this.liveTimer, this.pressTimer, this.refreshTimer, this.ringTimer]) {
      if (t !== null) this.deps.clearTimeout(t);
    }
    this.liveTimer = this.pressTimer = this.refreshTimer = this.ringTimer = null;
    this.listeners.clear();
    this.pulseListeners.clear();
    this.loadGen++;
  }

  // ---------- load ----------

  private async fetchTable(): Promise<TableResponse> {
    try {
      return (await this.deps.fetchJSON("/api/table")) as TableResponse;
    } catch (err) {
      return { handlers: [], engines: [], events: [], error: errText(err) };
    }
  }

  private async load(): Promise<void> {
    const g = ++this.loadGen;
    this.inflight = true;
    this.buffered = []; // every call buffered before this fetch is already inside its scan

    const params = this.deps.filterParams();
    const filters = this.deps.activeFilters();
    params.set("day", this.dayValue ?? "");
    if (this.live) params.set("window", String(WINDOW_MIN));

    let table: TableResponse;
    let resp: FlowResponse;
    try {
      [table, resp] = await Promise.all([
        this.fetchTable(),
        this.deps.fetchJSON("/api/flow?" + params.toString()) as Promise<FlowResponse>,
      ]);
    } catch (err) {
      if (g !== this.loadGen) return;
      this.fetchError = "flow: " + errText(err);
      this.inflight = false;
      this.model.clear();
      this.buffered = [];
      this.stale = true; // the next show/sync refetches
      this.refreshNow();
      return;
    }
    if (g !== this.loadGen) return; // superseded by a newer load

    this.tableError = table.error ?? "";
    this.fetchError = "";
    this.model.load(table, resp, filters);
    this.cursor = resp.next_offset;
    this.droppedCount = 0;
    this.inflight = false;

    const buffered = this.buffered;
    this.buffered = [];
    for (const entry of buffered) {
      if (entry.day === this.dayValue && this.live) this.addCall(entry);
    }

    if (this.stale && this.visible) {
      this.stale = false;
      return this.load();
    }
    this.startRingTick();
    this.requestLayout("load");
    this.refreshNow();
  }

  // ---------- live calls ----------

  private addCall(entry: Entry): Branch[] {
    this.model.advance(this.deps.now());
    if (entry.offset <= this.cursor) return [];
    this.cursor = entry.offset;

    // Under a handler/outcome filter only the hit branches are drawn — the
    // pruning /api/flow applies — so the path merges into the server's.
    const rec = entry.rec;
    const all = rec.handlers ?? [];
    const path: PathLike = {
      engine: rec.engine, canonical_event: rec.canonical_event, native_event: rec.native_event,
      router: rec.router, verdict: rec.verdict,
      handlers: entry.hits ? entry.hits.map((i) => all[i]) : all,
    };
    const branches = this.model.recordCall(path, rec.ts);

    // A node that is not drawn is shown until the next load; its branch
    // pulses only once a layout draws it.
    const snap = this.rendered;
    const ready: Branch[] = [];
    for (const b of branches) {
      if (snap?.drawn(b)) {
        ready.push(b);
        continue;
      }
      for (const [col, name] of b.nodes) {
        if (!snap?.has(snap.displayKey(col, name))) this.model.stickyShow(col, name);
      }
      this.requestLayout("live");
    }
    if (this.visible && ready.length > 0) for (const fn of this.pulseListeners) fn(ready);
    this.scheduleRefresh();
    return ready;
  }

  private startRingTick(): void {
    if (this.ringTimer !== null || !this.model.ring) return;
    this.ringTimer = this.deps.setTimeout(() => {
      this.ringTimer = null;
      if (!this.model.ring) return;
      if (this.model.advance(this.deps.now())) this.scheduleRefresh();
      this.startRingTick();
    }, RING_TICK_MS);
  }

  // ---------- relayout (spec §4.5) ----------

  // requestLayout: user reasons run now; live node-set growth coalesces to at
  // most one relayout per RELAYOUT_MS since the last commit.
  private requestLayout(reason: LayoutReason): void {
    if (!this.model.topo) return; // nothing to draw before the first load
    if (reason === "live") {
      if (this.liveTimer !== null) return;
      const wait = this.lastCommitAt + RELAYOUT_MS - this.deps.now();
      if (wait > 0) {
        this.liveTimer = this.deps.setTimeout(() => {
          this.liveTimer = null;
          this.runLayout();
        }, wait);
        return;
      }
    } else if (this.liveTimer !== null) {
      this.deps.clearTimeout(this.liveTimer);
      this.liveTimer = null;
    }
    this.runLayout();
  }

  // runLayout commits the next node set at once, or holds it while a press
  // is in progress (the latest held plan wins).
  private runLayout(): void {
    const plan = this.model.layoutPlan(this.rendered?.order ?? []);
    if (!plan) return;
    const key = plan.input.key;
    if (key === (this.pending?.plan.input.key ?? this.rendered?.key)) return;
    if (this.pending && key === this.rendered?.key) {
      // Back to what is drawn: retire the held relayout.
      this.pending = null;
      this.emit();
      return;
    }
    const gen = ++this.layoutGenSeq;
    plan.input.gen = gen;
    this.pending = { plan, gen };
    if (this.pressed) {
      this.emit();
      return;
    }
    this.commit();
  }

  private commit(): void {
    const p = this.pending;
    if (!p) return;
    this.rendered = new Snapshot(p.plan, this.deps.eventLabel);
    this.pending = null;
    this.lastCommitAt = this.deps.now();
    this.refreshNow();
  }

  // ---------- view refresh ----------

  private scheduleRefresh(): void {
    if (!this.visible || this.refreshTimer !== null) return;
    const wait = Math.max(0, this.lastRefreshAt + RENDER_MS - this.deps.now());
    this.refreshTimer = this.deps.setTimeout(() => {
      this.refreshTimer = null;
      this.refreshNow();
    }, wait);
  }

  private refreshNow(): void {
    if (this.refreshTimer !== null) {
      this.deps.clearTimeout(this.refreshTimer);
      this.refreshTimer = null;
    }
    const snap = this.rendered;
    let frame: Frame | null = null;
    let calls = 0;
    let branches = 0;
    if (snap) {
      const paths = Array.from(this.model.paths.values(), (e) => ({ path: e.path, counts: e.counts.slice() }));
      const totals = snap.totals(paths, this.model.ring !== null);
      frame = { snap, totals, paths };
      calls = totals.calls;
      branches = totals.branches;
    }
    this.latest = { frame, calls, branches, idle: this.model.idleCount() };
    if (!this.pressed) this.drawn = this.latest;
    this.lastRefreshAt = this.deps.now();
    this.emit();
  }

  private emit(): void {
    this.version++;
    for (const fn of this.listeners) fn();
  }
}
