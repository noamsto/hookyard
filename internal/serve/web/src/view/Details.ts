// The details panel beside the flow graph: what the current drill level
// holds. It opens once the operator has drilled in (any active filter value)
// and describes the level they are on — the outcome mix of its branches, the
// handlers (or, drilled to one handler, the engines) that carry it, and the
// latest decisions on it with the handler's own words. Every count comes
// from the frame the graph is drawing, so the two never disagree; only the
// latest-decisions list asks the server, through /api/events with the same
// filters.

import { backTo, filterParams, filterTrail, syncState, toggleFilter } from "../bridge.ts";
import type { FlowController } from "../model/controller.ts";
import { bySeverity, isLoud, knownOutcome } from "../model/snapshot.ts";
import type { Counts } from "../model/snapshot.ts";
import type { Entry } from "../types.ts";
import { fmt, htmlEl } from "./dom.ts";

const FIELD_LABELS: Record<string, string> = {
  engine: "engine", event: "event", handler: "handler", outcome: "handler outcome", verdict: "call verdict",
};
const DECISIONS = ["router-error", "error", "timeout", "deny", "ask", "advise", "allow"];
const RECENT = 5;

interface HandlerRow { id: string; n: number; loud: [string, number][]; }

interface RecentRun {
  ts: string; engine: string; event: string; handler: string; outcome: string; text: string;
}

interface Hop { name: string; outcome: string; ms?: number; advice?: string; message?: string; }

function loudOf(c: Counts | undefined): [string, number][] {
  const out: [string, number][] = [];
  for (const [o, n] of c ?? []) if (isLoud(o) && n > 0) out.push([o, n]);
  return out.sort((a, b) => bySeverity(a[0], b[0]));
}

function fmtTime(ts: string): string {
  const d = new Date(ts);
  return isNaN(d.getTime()) ? ts : d.toLocaleTimeString(undefined, { hour12: false });
}

export class Details {
  private readonly c: FlowController;
  private readonly panel: HTMLElement;
  private readonly toggleBtn: HTMLButtonElement;
  private dismissedAt = ""; // the trail signature the operator closed the panel on
  private view = "feed";
  private recent: RecentRun[] | null = null;
  private recentFor = "";
  private recentSeq = 0;
  private inflight = "";
  private refetchTimer = 0;

  constructor(panel: HTMLElement, flowHeader: HTMLElement | null, c: FlowController) {
    this.c = c;
    this.panel = panel;
    this.toggleBtn = htmlEl("button", "flow-details-toggle", "details", flowHeader ?? undefined);
    this.toggleBtn.type = "button";
    this.toggleBtn.title = "show or hide the details for this drill level";
    this.toggleBtn.addEventListener("click", () => {
      this.dismissedAt = this.isOpen() ? this.sig() : "";
      this.render();
    });
    c.subscribe(() => this.render());
    document.addEventListener("hookyard:sync", () => this.fetchRecent(true));
    document.addEventListener("hookyard:call", (ev) => {
      const rec = (ev as CustomEvent<Entry>).detail.rec;
      if ((rec.handlers ?? []).some((h) => isLoud(h.outcome))) this.scheduleRefetch();
    });
  }

  setView(v: string): void {
    this.view = v;
    this.render();
    this.fetchRecent(false);
  }

  private sig(): string {
    return JSON.stringify(filterTrail());
  }

  private isOpen(): boolean {
    return this.view === "flow" && filterTrail().length > 0 && this.dismissedAt !== this.sig();
  }

  private scheduleRefetch(): void {
    window.clearTimeout(this.refetchTimer);
    this.refetchTimer = window.setTimeout(() => this.fetchRecent(true), 1500);
  }

  // fetchRecent asks /api/events for the newest calls on this level that
  // carry a decision. Without an outcome step of its own, the level's filters
  // gain every decision outcome, so a quiet level still finds its few loud
  // runs; with one, the operator's outcome stands.
  private async fetchRecent(force: boolean): Promise<void> {
    const sync = syncState();
    if (!sync || this.view !== "flow" || filterTrail().length === 0) return;
    const params = filterParams();
    if (params.getAll("outcome").length === 0) for (const o of DECISIONS) params.append("outcome", o);
    params.set("day", sync.day);
    params.set("limit", "40");
    const url = "/api/events?" + params.toString();
    if (!force && (url === this.inflight || (url === this.recentFor && this.recent))) return;
    if (url !== this.recentFor) this.recent = null;
    this.inflight = url;
    const seq = ++this.recentSeq;
    try {
      const resp = await fetch(url, { credentials: "same-origin" });
      if (!resp.ok) throw new Error(String(resp.status));
      const body = await resp.json() as { records: Entry[] | null };
      if (seq !== this.recentSeq) return;
      const runs: RecentRun[] = [];
      for (const e of body.records ?? []) {
        const hops = (e.rec.handlers ?? []) as Hop[];
        const idx = e.hits ?? hops.map((_, i) => i);
        for (const i of idx) {
          const h = hops[i];
          if (!h || !isLoud(h.outcome)) continue;
          runs.push({
            ts: e.rec.ts, engine: e.rec.engine, event: e.rec.canonical_event || e.rec.native_event || "—",
            handler: h.name, outcome: h.outcome, text: h.message || h.advice || "",
          });
        }
      }
      // newest first by the call's own time, not its place in the file
      runs.sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0));
      this.recent = runs.slice(0, RECENT);
      this.recentFor = url;
    } catch {
      if (seq !== this.recentSeq) return;
      this.recent = [];
      this.recentFor = url;
    } finally {
      if (seq === this.recentSeq) this.inflight = "";
    }
    this.render();
  }

  private render(): void {
    const open = this.isOpen();
    this.toggleBtn.hidden = this.view !== "flow" || filterTrail().length === 0;
    this.toggleBtn.setAttribute("aria-pressed", String(open));
    const wasHidden = this.panel.hidden;
    this.panel.hidden = !open;
    document.body.classList.toggle("details-open", open);
    if (wasHidden !== this.panel.hidden) window.dispatchEvent(new Event("resize"));
    if (!open) return;
    void this.fetchRecent(false);

    const trail = filterTrail();
    const [field, value] = trail[trail.length - 1];
    const frame = this.c.frame();
    const counts = this.c.counts();
    const p = this.panel;
    p.replaceChildren();

    const head = htmlEl("div", "dt-head", "", p);
    const titles = htmlEl("div", "dt-titles", "", head);
    htmlEl("span", "dt-kind", "level " + trail.length + " · " + FIELD_LABELS[field], titles);
    const title = htmlEl("span", "dt-title", value, titles);
    if (field === "outcome" || field === "verdict") title.dataset.o = knownOutcome(value);
    htmlEl("span", "dt-sub dim", fmt(counts.calls) + " calls · " + fmt(counts.branches) + " branches", titles);
    const close = htmlEl("button", "dt-close", "×", head);
    close.type = "button";
    close.setAttribute("aria-label", "close details");
    close.addEventListener("click", () => {
      this.dismissedAt = this.sig();
      this.render();
    });

    if (!frame) {
      htmlEl("p", "dt-empty dim", "loading…", p);
      return;
    }
    const { snap, totals } = frame;

    // outcome mix
    const outs: [string, number][] = [];
    for (const n of snap.nodes) {
      if (n.col !== "outcome") continue;
      const v = totals.nodeTotals.get(n.key) ?? 0;
      if (v > 0) outs.push([n.name, v]);
    }
    outs.sort((a, b) => bySeverity(a[0], b[0]));
    const sec1 = this.section(p, "outcomes · branches");
    sec1.title = "click one to drill into it";
    if (outs.length === 0) htmlEl("p", "dt-empty dim", "no traffic on this level in the window", sec1);
    const max = Math.max(1, ...outs.map(([, n]) => n));
    for (const [o, n] of outs) {
      const row = htmlEl("button", "dt-out", "", sec1);
      row.type = "button";
      row.dataset.o = knownOutcome(o);
      row.title = "drill into " + o;
      htmlEl("span", "dt-out-name" + (isLoud(o) ? " loud" : ""), o, row);
      const bar = htmlEl("span", "dt-bar", "", row);
      const fill = htmlEl("span", "dt-bar-fill", "", bar);
      fill.style.width = Math.max(3, Math.round(Math.sqrt(n / max) * 100)) + "%";
      htmlEl("span", "dt-n", fmt(n), row);
      row.addEventListener("click", () => toggleFilter("outcome", o, false));
    }

    // who carries this level: handlers, or engines once drilled to a handler
    const byEngine = field === "handler";
    const rows: HandlerRow[] = [];
    for (const n of snap.nodes) {
      if (byEngine) {
        if (n.col !== "engine") continue;
        rows.push({ id: n.name, n: totals.nodeTotals.get(n.key) ?? 0, loud: loudOf(totals.nodeOutcomes.get(n.key)) });
      } else if (n.col === "handler") {
        rows.push({ id: n.name, n: totals.nodeTotals.get(n.key) ?? 0, loud: loudOf(totals.nodeOutcomes.get(n.key)) });
      } else if (n.col === "group") {
        for (const [id, c] of totals.members.get(n.key) ?? []) {
          let sum = 0;
          for (const v of c.values()) sum += v;
          rows.push({ id, n: sum, loud: loudOf(c) });
        }
      }
    }
    const shown = rows.filter((r) => r.n > 0)
      .sort((a, b) => (b.loud.length ? 1 : 0) - (a.loud.length ? 1 : 0) || b.n - a.n || (a.id < b.id ? -1 : 1))
      .slice(0, 8);
    const sec2 = this.section(p, (byEngine ? "engines" : "handlers") + " on this level");
    sec2.title = "click one to drill into it";
    if (shown.length === 0) htmlEl("p", "dt-empty dim", "none in the window", sec2);
    for (const r of shown) {
      const row = htmlEl("button", "dt-row", "", sec2);
      row.type = "button";
      row.title = "drill into " + r.id;
      htmlEl("span", "dt-row-name", r.id, row);
      const loud = htmlEl("span", "dt-row-loud", "", row);
      for (const [o, n] of r.loud.slice(0, 2)) {
        const s = htmlEl("span", "", fmt(n) + " " + o, loud);
        s.dataset.o = knownOutcome(o);
      }
      htmlEl("span", "dt-n", fmt(r.n), row);
      row.addEventListener("click", () => toggleFilter(byEngine ? "engine" : "handler", r.id, false));
    }

    // latest decisions
    const sec3 = this.section(p, "latest decisions");
    sec3.classList.add("dt-recent");
    if (this.recent === null) htmlEl("p", "dt-empty dim", "loading…", sec3);
    else if (this.recent.length === 0) htmlEl("p", "dt-empty dim", "only quiet traffic on this level today", sec3);
    for (const r of this.recent ?? []) {
      const card = htmlEl("div", "dt-card", "", sec3);
      const top = htmlEl("div", "dt-card-top", "", card);
      htmlEl("span", "dim", fmtTime(r.ts), top);
      htmlEl("span", "", r.engine + " · " + r.event, top);
      const o = htmlEl("span", "dt-card-o", r.outcome, top);
      o.dataset.o = knownOutcome(r.outcome);
      htmlEl("span", "dt-card-h", r.handler, card);
      if (r.text) htmlEl("span", "dt-card-msg", r.text, card);
    }

    const foot = htmlEl("div", "dt-foot", "", p);
    const feed = htmlEl("button", "dt-feed", "open these calls in the feed", foot);
    feed.type = "button";
    feed.addEventListener("click", () => document.querySelector<HTMLButtonElement>('.view-toggle [data-view="feed"]')?.click());
    if (trail.length > 1) {
      const up = htmlEl("button", "dt-up", "← up a level", foot);
      up.type = "button";
      up.addEventListener("click", () => backTo(trail.length - 1));
    }
  }

  private section(parent: HTMLElement, title: string): HTMLElement {
    const s = htmlEl("section", "dt-sec", "", parent);
    htmlEl("h3", "", title, s);
    return s;
  }
}
