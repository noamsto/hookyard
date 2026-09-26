// The flow panel: a decision Sankey drawn as plain SVG. It redraws only when
// the controller publishes a new frame (≤ 1 per RENDER_MS, held while a press
// is in progress) or the panel's size changes; hover, tooltips, clicks and
// pulses all read the frame that is drawn.

import { toggleFilter } from "../bridge.ts";
import type { Frame, FlowController } from "../model/controller.ts";
import { splitEdgeKey, splitKey } from "../model/keys.ts";
import { bySeverity, isLoud, knownOutcome } from "../model/snapshot.ts";
import type { Snapshot, SnapNode, Tip, Totals } from "../model/snapshot.ts";
import type { Col } from "../types.ts";
import { fit, fmt, htmlEl, partsLen, svgEl, svgText } from "./dom.ts";
import type { Part } from "./dom.ts";
import { bandWidth, GUTTER, HEAD_H, layoutFlow, LH, ribbon } from "./geometry.ts";
import type { GeoLink, GeoNode, Geometry } from "./geometry.ts";
import { Pulses } from "./pulses.ts";
import { buildScene, fanOut, linkId, loudOutcomes, PSEUDO, ranksFor, singleOutcome } from "./scene.ts";
import type { Scene, SceneNode } from "./scene.ts";
import { clampView, fitView, panBy, sameView, wheelFactor, zoomAt } from "./viewport.ts";
import type { View } from "./viewport.ts";

const HINT = "hover for exact counts · click to filter · shift-click to add · drag to pan · wheel to zoom · 0 to fit";
const DRAG_PX = 5; // a press that moves less than this is still a click
const MIN_H = 420;
const INSET = 8; // plate text inset
const SPARK = { w: 3, gap: 1, h: 10 };
const HEADERS: [string, string][] = [
  ["engine", "calls"], ["event", "calls ×runs per handled call"], ["handler", "branches"], ["outcome", "branches"],
];
const HEADER_CHARS = HEADERS.map(([name, unit]) => name.length + 3 + unit.length) as [number, number, number, number];

type LayerName = "heads" | "bands" | "over" | "nodes" | "labels" | "dots" | "legend";

interface Drawn {
  frame: Frame;
  scene: Scene;
  geo: Geometry;
  nodes: Map<string, SceneNode>;
  links: Map<string, GeoLink>;
  nodeEls: Map<string, SVGGElement>;
  labelEls: Map<string, SVGGElement>;
  mono: boolean;
}

export class FlowView {
  private readonly c: FlowController;
  private readonly body: HTMLDivElement;
  private readonly svg: SVGSVGElement;
  private readonly scene: SVGGElement;
  private readonly fitBtn: HTMLButtonElement;
  private readonly layers: Record<LayerName, SVGGElement>;
  private readonly tip: HTMLDivElement;
  private readonly empty: HTMLParagraphElement;
  private readonly metaEl: HTMLSpanElement;
  private readonly decisionsEl: HTMLSpanElement;
  private readonly errorEl: HTMLSpanElement;
  private readonly idleInput: HTMLInputElement;
  private readonly idleText: HTMLSpanElement;
  private readonly pulses: Pulses;
  private readonly keyOf = new WeakMap<Element, string>();
  private readonly flashTimers = new WeakMap<Element, number>();
  private drawn: Drawn | null = null;
  private rankMemo: { snap: Snapshot; rank: Map<string, number> } | null = null;
  private hover: string | null = null; // the hovered node key
  private hoverEdge: string | null = null; // or the hovered band's edge key
  private charW = 0;
  private width = 0;
  private activeDots = -1;
  private resizeRaf = 0;
  // The user's view of the scene. While `fitted` it is recomputed on every
  // layout; once they zoom or pan it is kept (and only clamped).
  private view: View = { k: 1, tx: 0, ty: 0 };
  private fitted = true;
  private fitK = 1;
  private sceneW = 0;
  private sceneH = 0;
  private readonly pointers = new Map<number, { x: number; y: number }>();
  private pinch = 0; // the pinch's last finger distance
  private press: { x: number; y: number } | null = null; // where the primary press began, until it counts as a drag
  private dragged = false;

  constructor(panel: HTMLElement, c: FlowController) {
    this.c = c;
    const header = htmlEl("div", "panel-header", "", panel);
    htmlEl("h2", "", "flow", header);
    this.metaEl = htmlEl("span", "dim flow-meta", "", header);
    this.decisionsEl = htmlEl("span", "flow-decisions", "", header);
    this.errorEl = htmlEl("span", "flow-error", "", header);
    const idle = htmlEl("label", "flow-idle", "", header);
    this.idleInput = htmlEl("input", "", "", idle);
    this.idleInput.type = "checkbox";
    this.idleText = htmlEl("span", "", "", idle);
    this.idleInput.addEventListener("change", () => c.setShowIdle(this.idleInput.checked));
    this.fitBtn = htmlEl("button", "flow-fit", "fit", header);
    this.fitBtn.type = "button";
    this.fitBtn.title = "fit the graph to the panel (0)";
    this.fitBtn.addEventListener("click", () => this.fit());
    htmlEl("p", "hint flow-hint", HINT, panel);

    this.body = htmlEl("div", "flow-body", "", panel);
    this.body.id = "flow-body";
    this.svg = svgEl("svg", { class: "flow-svg" }, this.body);
    this.scene = svgEl("g", { class: "flow-scene" }, this.svg);
    const layers = {} as Record<LayerName, SVGGElement>;
    for (const k of ["heads", "bands", "over", "nodes", "labels", "dots", "legend"] as const) {
      layers[k] = svgEl("g", { class: "l-" + k }, this.scene);
    }
    this.layers = layers;
    this.tip = htmlEl("div", "flow-tip", "", this.body);
    this.tip.hidden = true;
    this.empty = htmlEl("p", "flow-empty dim", "", this.body);
    this.empty.hidden = true;

    const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
    this.pulses = new Pulses(layers.dots, {
      link: (id) => this.drawn?.links.get(id),
      flash: (key) => this.flash(key),
      dropped: (n) => c.addDropped(n),
      active: (n) => {
        if (n === this.activeDots) return;
        this.activeDots = n;
        this.body.dataset.activeDots = String(n);
      },
    }, reduced);
    this.body.dataset.activeDots = "0";

    c.subscribe(this.update);
    c.onPulse((branches) => {
      const d = this.drawn;
      if (!d || !c.isLive() || document.hidden) return;
      this.pulses.call(branches.map((b) => d.frame.snap.displayNodes(b)));
    });

    this.body.addEventListener("pointerdown", (ev) => {
      c.pressBegin(this.keyAt(ev.target));
      this.pointerDown(ev);
    }, true);
    this.body.addEventListener("pointermove", (ev) => this.pointerMove(ev));
    for (const t of ["pointerup", "pointercancel"]) this.body.addEventListener(t, (ev) => this.pointerUp(ev as PointerEvent));
    // a drag that panned is not a click on whatever lay under the pointer
    this.body.addEventListener("click", (ev) => {
      if (!this.dragged) return;
      this.dragged = false;
      ev.stopPropagation();
    }, true);
    this.body.addEventListener("wheel", (ev) => this.wheel(ev), { passive: false });
    document.addEventListener("keydown", (ev) => this.key(ev));
    const release = () => window.setTimeout(() => c.pressEnd(), 0);
    for (const t of ["pointerup", "pointercancel", "lostpointercapture", "blur"]) window.addEventListener(t, release);
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) this.pulses.cancel();
    });
    const relayout = () => {
      if (this.resizeRaf) return;
      this.resizeRaf = requestAnimationFrame(() => {
        this.resizeRaf = 0;
        if (this.c.isVisible() && this.body.clientWidth !== this.width) this.render();
      });
    };
    new ResizeObserver(relayout).observe(this.body);
    window.addEventListener("resize", () => {
      this.width = -1; // the panel's height follows the window's
      relayout();
    });
  }

  // ---------- controller -> view ----------

  private readonly update = (): void => {
    const c = this.c;
    this.metaEl.textContent = c.meta();
    this.decisionsEl.replaceChildren();
    for (const [o, n] of c.decisions()) {
      const s = htmlEl("span", "dec", "", this.decisionsEl);
      s.dataset.o = knownOutcome(o);
      htmlEl("b", "", fmt(n), s);
      s.append(" " + o);
    }
    this.errorEl.textContent = c.error();
    this.idleInput.checked = c.showIdle();
    this.idleText.textContent = "show idle (" + c.idleCount() + ")";

    const d = this.body.dataset;
    const counts = c.counts();
    d.calls = String(counts.calls);
    d.branches = String(counts.branches);
    d.dropped = String(c.dropped());
    d.layoutGen = String(c.layoutGen());
    d.pendingLayout = c.pendingLayout() ? "1" : "0";

    if (!c.isVisible() || !c.isLive()) this.pulses.cancel();
    if (!c.isVisible()) return;
    if (c.frame() !== this.drawn?.frame || this.body.clientWidth !== this.width) this.render();
  };

  // ---------- render ----------

  private measureChar(): number {
    if (this.charW > 0) return this.charW;
    const t = svgText(this.layers.heads, 0, -100, [["0000000000"]], { class: "flow-measure" });
    const w = t.getComputedTextLength() / 10;
    t.remove();
    if (w > 0) this.charW = w;
    return w > 0 ? w : 7.2;
  }

  private rankFor(frame: Frame, scene: Scene): Map<string, number> {
    // Pinned per committed snapshot: a count refresh never reorders.
    const memo = this.rankMemo;
    const rank = ranksFor(scene, frame.snap.order, memo?.snap === frame.snap ? memo.rank : undefined);
    this.rankMemo = { snap: frame.snap, rank };
    return rank;
  }

  private render(): void {
    const frame = this.c.frame();
    const width = this.body.clientWidth;
    this.width = width;
    if (!frame || width === 0) return;
    const sameSnap = this.drawn?.frame.snap === frame.snap;
    const focused = this.keyAt(document.activeElement);
    const hover = sameSnap ? this.hover : null;
    const hoverEdge = sameSnap ? this.hoverEdge : null;
    this.clearHover();

    const scene = buildScene(frame);
    const rank = this.rankFor(frame, scene);
    const first = new Map<string, string>();
    for (const n of scene.nodes) {
      const p = n.snap?.parent;
      if (p === undefined) continue;
      const f = first.get(p);
      if (f === undefined || (rank.get(n.key) ?? 0) < (rank.get(f) ?? 0)) first.set(p, n.key);
    }
    const heads = new Set(first.values());
    const live = frame.totals.buckets.size > 0;
    const charW = this.measureChar();
    const chars: [number, number, number, number] = [0, 0, 0, 0];
    for (const n of scene.nodes) chars[n.col] = Math.max(chars[n.col], this.labelChars(n, frame, live));

    const top = this.body.getBoundingClientRect().top + window.scrollY;
    const geo = layoutFlow({
      width, minHeight: Math.max(MIN_H, Math.floor(window.innerHeight - top - 14)), charW, chars, headerChars: HEADER_CHARS,
      nodes: scene.nodes.map((n) => ({ key: n.key, col: n.col, kind: n.kind, lines: this.lines(n), head: heads.has(n.key) })),
      links: scene.links, rank,
    });

    const d: Drawn = {
      frame, scene, geo,
      nodes: new Map(scene.nodes.map((n) => [n.key, n])),
      links: new Map(geo.links.map((l) => [linkId(l), l])),
      nodeEls: new Map(), labelEls: new Map(),
      mono: singleOutcome(scene.links) !== null && scene.links.length > 0,
    };
    this.drawn = d;

    // Laid out wider than the panel: the scene pans and zooms inside it.
    this.sceneW = geo.width;
    this.sceneH = geo.height;
    this.fitK = fitView(this.sceneSize(), { w: width, h: 0 }).k;
    this.body.style.height = Math.ceil(geo.height * this.fitK) + "px";
    this.svg.setAttribute("width", String(width));
    this.svg.setAttribute("height", String(this.body.clientHeight));
    this.view = this.fitted ? this.fitView() : clampView(this.view, this.sceneSize(), this.panel(), this.fitK);
    this.applyView();
    this.svg.classList.toggle("mono", d.mono);
    for (const k of ["heads", "bands", "over", "nodes", "labels", "legend"] as const) this.layers[k].replaceChildren();

    this.empty.hidden = geo.nodes.length > 0;
    this.empty.textContent = this.c.isLive() ? "no calls in the last 10 min" : "no calls this day";
    if (geo.nodes.length > 0) {
      this.drawHeaders(geo);
      this.drawBands(d);
      for (const g of geo.nodes) this.drawNode(d, g);
      for (const grp of scene.groups) this.drawOpenGroup(d, grp);
      this.drawLegend(d);
    }

    if (hover !== null && d.nodeEls.has(hover)) this.hoverNode(hover);
    if (hoverEdge !== null && d.geo.links.some((l) => l.edge === hoverEdge)) this.hoverBand(hoverEdge);
    if (focused !== undefined) {
      for (const el of this.svg.querySelectorAll("[tabindex]")) {
        if (this.keyOf.get(el) === focused) (el as SVGElement).focus();
      }
    }
  }

  // ---------- pan and zoom ----------

  private sceneSize() {
    return { w: this.sceneW, h: this.sceneH };
  }

  private panel() {
    return { w: this.body.clientWidth, h: this.body.clientHeight };
  }

  private fitView(): View {
    return fitView(this.sceneSize(), this.panel());
  }

  private applyView(): void {
    const v = this.view;
    this.scene.setAttribute("transform", `translate(${v.tx.toFixed(2)} ${v.ty.toFixed(2)}) scale(${v.k.toFixed(5)})`);
    this.body.classList.toggle("zoomed", !this.fitted);
    const d = this.body.dataset;
    d.viewK = v.k.toFixed(4);
    d.viewTx = v.tx.toFixed(2);
    d.viewTy = v.ty.toFixed(2);
    d.fitted = this.fitted ? "1" : "0";
  }

  private setView(v: View): void {
    if (sameView(v, this.view)) return;
    this.view = v;
    this.fitted = false;
    this.applyView();
    this.rehover();
  }

  private fit(): void {
    this.fitted = true;
    this.view = this.fitView();
    this.applyView();
    this.rehover();
  }

  // The hover overlay rides the scene transform; only the tooltip needs to
  // move. A drag in progress has no hover at all.
  private rehover(): void {
    const [node, edge] = [this.hover, this.hoverEdge];
    if (this.dragged) this.clearHover();
    else if (node !== null) this.hoverNode(node);
    else if (edge !== null) this.hoverBand(edge);
  }

  private zoomBy(factor: number, px: number, py: number): void {
    this.setView(zoomAt(this.view, factor, px, py, this.sceneSize(), this.panel(), this.fitK));
  }

  private local(ev: { clientX: number; clientY: number }): { x: number; y: number } {
    const r = this.body.getBoundingClientRect();
    return { x: ev.clientX - r.left, y: ev.clientY - r.top };
  }

  private wheel(ev: WheelEvent): void {
    if (!this.drawn || this.drawn.geo.nodes.length === 0) return;
    ev.preventDefault();
    const p = this.local(ev);
    this.zoomBy(wheelFactor(ev.deltaY, ev.deltaMode, ev.ctrlKey, this.body.clientHeight), p.x, p.y);
  }

  private key(ev: KeyboardEvent): void {
    if (ev.ctrlKey || ev.metaKey || ev.altKey || !this.c.isVisible()) return;
    const t = ev.target;
    if (t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
    const [w, h] = [this.body.clientWidth, this.body.clientHeight];
    if (ev.key === "+" || ev.key === "=") this.zoomBy(1.25, w / 2, h / 2);
    else if (ev.key === "-") this.zoomBy(0.8, w / 2, h / 2);
    else if (ev.key === "0") this.fit();
    else return;
    ev.preventDefault();
  }

  private pointerDown(ev: PointerEvent): void {
    if (ev.pointerType === "mouse" && ev.button !== 0) return;
    this.pointers.set(ev.pointerId, this.local(ev));
    this.dragged = false;
    if (this.pointers.size === 1) this.press = this.local(ev);
    else {
      this.press = null;
      this.pinch = this.pinchDistance();
      this.dragged = true;
    }
  }

  private pinchDistance(): number {
    const [a, b] = [...this.pointers.values()];
    return a && b ? Math.hypot(a.x - b.x, a.y - b.y) : 0;
  }

  private pointerMove(ev: PointerEvent): void {
    const prev = this.pointers.get(ev.pointerId);
    if (!prev) return;
    const p = this.local(ev);
    this.pointers.set(ev.pointerId, p);
    if (this.pointers.size >= 2) {
      const dist = this.pinchDistance();
      const [a, b] = [...this.pointers.values()];
      if (this.pinch > 0 && dist > 0) this.zoomBy(dist / this.pinch, (a.x + b.x) / 2, (a.y + b.y) / 2);
      this.pinch = dist;
      return;
    }
    let from = prev;
    if (this.press) {
      if (Math.hypot(p.x - this.press.x, p.y - this.press.y) < DRAG_PX) return;
      from = this.press; // the threshold's travel counts: the graph does not lag the finger
      this.press = null;
      this.dragged = true;
      this.body.setPointerCapture(ev.pointerId);
      this.body.classList.add("panning");
      this.clearHover();
    }
    if (this.dragged) this.setView(panBy(this.view, p.x - from.x, p.y - from.y, this.sceneSize(), this.panel(), this.fitK));
  }

  private pointerUp(ev: PointerEvent): void {
    this.pointers.delete(ev.pointerId);
    this.pinch = 0;
    if (this.pointers.size === 0) {
      this.press = null;
      this.body.classList.remove("panning");
    }
  }

  private drawHeaders(geo: Geometry): void {
    geo.columns.forEach((col, i) => {
      const g = svgEl("g", { class: "flow-colhead" }, this.layers.heads);
      svgEl("rect", { class: "colhead-bg", x: col.x0, y: 2, width: col.x1 - col.x0, height: 22 }, g);
      const [name, unit] = HEADERS[i];
      svgText(g, col.x0, 16, [[name, "h-name"], [" · " + unit, "h-what"]]);
      svgEl("line", { class: "colrule", x1: col.x0, x2: col.x1, y1: 24.5, y2: 24.5 }, g);
    });
  }

  private drawBands(d: Drawn): void {
    const byEdge = new Map<string, SVGGElement>();
    const maxOf = new Map<number, number>();
    for (const l of d.geo.links) {
      const col = d.geo.byKey.get(l.from)?.col ?? 0;
      maxOf.set(col, Math.max(maxOf.get(col) ?? 0, l.count));
    }
    for (const l of d.geo.links) {
      let g = byEdge.get(l.edge);
      if (!g) {
        g = svgEl("g", { class: "flow-edge" }, this.layers.bands);
        const [a, b] = splitEdgeKey(l.edge);
        const na = d.frame.snap.node(a);
        const nb = d.frame.snap.node(b);
        g.dataset.key = JSON.stringify([na?.col, na?.dataName, nb?.col, nb?.dataName]);
        g.dataset.n = String(d.frame.totals.edgeTotals.get(l.edge) ?? 0);
        byEdge.set(l.edge, g);
      }
      const o = knownOutcome(l.outcome);
      const p = svgEl("path", { class: "band" + (isLoud(o) ? " loud" : "") + (l.width < 4 ? " thin" : ""), d: ribbon(l, l.width), "data-o": o }, g);
      if (d.mono) {
        // One outcome everywhere: emphasis follows each band's share of its
        // column's biggest band, so the main contributors still stand out.
        const share = l.count / (maxOf.get(d.geo.byKey.get(l.from)?.col ?? 0) ?? 1);
        p.style.setProperty("--share", share.toFixed(3));
      }
      p.addEventListener("mouseenter", () => this.hoverBand(l.edge));
      p.addEventListener("mouseleave", () => this.clearHover());
    }
  }

  private drawNode(d: Drawn, g: GeoNode): void {
    const n = d.nodes.get(g.key);
    if (!n) return;
    const el = svgEl("g", { class: "flow-node k-" + n.kind }, this.layers.nodes);
    const sn = n.snap;
    if (sn) this.tagNode(el, sn, n.total);
    if (n.total === 0) el.classList.add("idle");
    d.nodeEls.set(g.key, el);

    if (n.kind === "engine" || n.kind === "outcome") {
      const o = n.kind === "outcome" ? knownOutcome(sn?.name ?? "") : loudOutcomes(n.outcomes).length ? "loud" : "quiet";
      svgEl("rect", { class: "bar", x: g.x0, y: g.y0, width: g.x1 - g.x0, height: Math.max(g.h, 1), "data-o": o }, el);
    } else {
      svgEl("line", { class: "rule", x1: g.x0, x2: g.x1, y1: g.y0 + 0.5, y2: g.y0 + 0.5 }, el);
      if (fanOut(n) && g.outH > g.inH + 0.5) {
        // The fan-out gate: calls in on the left, handler runs out on the right.
        const yl = g.y0 + (g.h - g.inH) / 2;
        const yr = g.y0 + (g.h - g.outH) / 2;
        svgEl("path", { class: "gate", d: `M${g.x0},${yl}L${g.x1},${yr}V${yr + g.outH}L${g.x0},${yl + g.inH}Z` }, el);
      }
      if (g.inH > 0) svgEl("rect", { class: "port", x: g.x0 - 1, y: g.y0 + (g.h - g.inH) / 2, width: 2, height: g.inH }, el);
      if (g.outH > 0) svgEl("rect", { class: "port", x: g.x1 - 1, y: g.y0 + (g.h - g.outH) / 2, width: 2, height: g.outH }, el);
    }

    let x0 = g.x0;
    let x1 = g.x1;
    let y0 = g.y0 - 2;
    let h = Math.max(g.h, LH) + 4;
    if (n.kind === "engine") {
      x0 = GUTTER;
      const room = Math.max(g.h, 2 * LH);
      y0 = g.y0 + g.h / 2 - room / 2 - 2;
      h = room + 4;
    } else if (n.kind === "outcome") {
      x1 = Math.min(d.geo.width - GUTTER, d.geo.labelX[1] + this.labelChars(n, d.frame, false) * this.charW);
      y0 = g.y0 + g.h / 2 - Math.max(g.h, LH) / 2 - 2;
    }
    const hit = svgEl("rect", { class: "hit", x: x0, y: y0, width: x1 - x0, height: h }, el);
    if (n.kind === "pseudo") {
      this.drawLabel(d, g.key);
      return;
    }
    this.keyOf.set(hit, g.key);
    hit.addEventListener("mouseenter", () => this.hoverNode(g.key));
    hit.addEventListener("mouseleave", () => this.clearHover());
    hit.addEventListener("click", (ev) => this.click(g.key, ev));
    if (sn?.group) {
      hit.classList.add("flow-group-header");
      this.focusable(hit, g.key, !sn.group.forced);
    }
    this.drawLabel(d, g.key);
  }

  private tagNode(el: SVGGElement, sn: SnapNode, n: number): void {
    el.dataset.col = sn.col;
    el.dataset.name = sn.dataName;
    el.dataset.n = String(n);
    if (sn.ghost) el.classList.add("ghost");
    if (sn.col !== "group" && this.c.isSelected(sn.col as Col, sn.name)) el.classList.add("selected");
    if (!sn.group) return;
    el.dataset.members = JSON.stringify(sn.group.members);
    el.dataset.expanded = String(sn.group.expanded);
    el.dataset.forced = String(sn.group.forced);
  }

  private focusable(el: SVGElement, key: string, active: boolean): void {
    if (!active) return;
    el.setAttribute("tabindex", "0");
    el.setAttribute("role", "button");
    el.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      ev.preventDefault();
      this.c.toggleGroup(key);
    });
  }

  private drawOpenGroup(d: Drawn, grp: SnapNode): void {
    const members = (grp.group?.shown ?? []).map((k) => d.geo.byKey.get(k)).filter((g): g is GeoNode => g !== undefined);
    if (members.length === 0 || !grp.group) return;
    const top = Math.min(...members.map((m) => m.y0));
    const bottom = Math.max(...members.map((m) => m.y0 + m.h));
    const x0 = members[0].x0;
    const x1 = members[0].x1;
    const total = d.frame.totals.nodeTotals.get(grp.key) ?? 0;
    const room = Math.floor((x1 - x0 - 2) / (this.charW || 7.2));
    const el = svgEl("g", { class: "flow-node flow-group-open" }, this.layers.nodes);
    this.tagNode(el, grp, total);
    d.nodeEls.set(grp.key, el);
    svgEl("path", { class: "bracket", d: `M${x0 - 3},${top - HEAD_H + 10}H${x0 - 7}V${bottom}H${x0 - 3}` }, el);
    const hit = svgEl("rect", { class: "hit flow-group-header", x: x0, y: top - HEAD_H, width: x1 - x0, height: HEAD_H - 2 }, el);
    const y = top - 7;
    const left: Part[] = [["▾ ", "caret"], [grp.group.label, "nm"], [" (" + grp.group.members.length + ")", "cnt"]];
    const right: Part[] = [[fmt(total) + "  ", "cnt"], grp.group.forced ? ["pinned", "cnt"] : ["fold", "fold"]];
    // Narrow plates drop the count first (the tooltip has it), then shorten the name.
    if (partsLen(left) + partsLen(right) + 1 > room) right.shift();
    const over = partsLen(left) + partsLen(right) + 1 - room;
    if (over > 0) left[1] = [fit(grp.group.label, grp.group.label.length - over), "nm"];
    svgText(el, x0 + 2, y, left, { class: "grouphead" });
    svgText(el, x1, y, right, { class: "grouphead", "text-anchor": "end" });
    this.keyOf.set(hit, grp.key);
    hit.addEventListener("mouseenter", () => this.hoverNode(grp.key));
    hit.addEventListener("mouseleave", () => this.clearHover());
    hit.addEventListener("click", (ev) => this.click(grp.key, ev));
    this.focusable(hit, grp.key, !grp.group.forced);
  }

  private drawLegend(d: Drawn): void {
    const present = [...new Set(d.geo.links.map((l) => knownOutcome(l.outcome)))].sort(bySeverity);
    const y = d.geo.height - 10;
    let x = GUTTER;
    const cw = this.charW || 7.2;
    for (const o of present) {
      svgEl("rect", { class: "swatch", x, y: y - 5, width: 12, height: 4, "data-o": o }, this.layers.legend);
      svgText(this.layers.legend, x + 16, y, [[o, isLoud(o) ? "lg-loud" : "lg", o]]);
      x += 16 + o.length * cw + 14;
    }
    svgText(this.layers.legend, x + 6, y, [["width ∝ √count · decisions floored", "lg"]]);
  }

  // ---------- labels ----------

  private lines(n: SceneNode): number {
    if (n.kind === "engine") return 2;
    if ((n.kind === "handler" || n.kind === "group") && loudOutcomes(n.outcomes).length > 0) return 2;
    return 1;
  }

  private nameOf(n: SceneNode, frame: Frame): string {
    if (!n.snap) return "∅ no handler";
    if (n.snap.group) return n.snap.group.label + " (" + n.snap.group.members.length + ")";
    return frame.snap.memberLabel(n.key);
  }

  // labelChars: the widest the node's label gets (a hover shows sub / total).
  private labelChars(n: SceneNode, frame: Frame, live: boolean): number {
    const count = fmt(n.total).length;
    const hovered = count * 2 + 3;
    const name = this.nameOf(n, frame).length + (n.kind === "group" ? 2 : 0);
    switch (n.kind) {
      case "engine": return Math.max(name, hovered);
      case "outcome": return name + 2 + hovered;
      case "event": return name + 2 + count + fanOut(n).length + (live ? 7 : 0);
      default: return name + 2 + count + (live ? 7 : 0);
    }
  }

  private countParts(total: number, sub: number | undefined): Part[] {
    if (sub === undefined || sub === total) return [[fmt(total), "cnt"]];
    return [[fmt(sub), "cnt hl"], [" / " + fmt(total), "cnt"]];
  }

  private drawLabel(d: Drawn, key: string, sub?: Totals): void {
    d.labelEls.get(key)?.remove();
    const g = d.geo.byKey.get(key);
    const n = d.nodes.get(key);
    if (!g || !n) return;
    const el = svgEl("g", { class: "label k-" + n.kind }, this.layers.labels);
    d.labelEls.set(key, el);
    if (n.snap) el.dataset.of = JSON.stringify([n.snap.col, n.snap.dataName]);
    const nodeEl = d.nodeEls.get(key);
    for (const c of ["selected", "on"]) if (nodeEl?.classList.contains(c)) el.classList.add(c);
    if (n.total === 0) el.classList.add("idle");
    const subN = sub ? (key === PSEUDO ? this.pseudoSub(d, sub) : sub.nodeTotals.get(key) ?? 0) : undefined;
    const cw = this.charW || 7.2;
    const name = this.nameOf(n, d.frame);

    if (n.kind === "engine") {
      const y = g.y0 + g.h / 2 - LH / 2 + 4;
      svgText(el, d.geo.labelX[0], y, [[fit(name, Math.floor((g.x0 - d.geo.labelX[0] - 6) / cw)), "nm"]]);
      svgText(el, d.geo.labelX[0], y + LH, this.countParts(n.total, subN));
      return;
    }
    if (n.kind === "outcome") {
      const o = knownOutcome(n.snap?.name ?? "");
      const loud = isLoud(o);
      svgText(el, d.geo.labelX[1], g.y0 + g.h / 2 + 4, [[name, loud ? "nm loud" : "nm quiet", o], ["  "], ...this.countParts(n.total, subN)]);
      return;
    }

    const lines = this.lines(n);
    const width = g.x1 - g.x0 - 2 * INSET;
    const y = g.y0 + g.h / 2 - ((lines - 1) * LH) / 2 + 4;
    const right: Part[] = [...this.countParts(n.total, subN)];
    const fan = fanOut(n);
    if (fan) right.push([fan, "fan"]);
    const rightChars = partsLen(right);
    const caret = n.snap?.group ? 2 : 0;
    const nameRoom = Math.floor((width - rightChars * cw) / cw) - 1 - caret;
    // The sparkline only takes room the name does not need.
    const buckets = d.frame.totals.buckets.get(key);
    const sparkW = buckets ? buckets.length * (SPARK.w + SPARK.gap) : 0;
    const live = sparkW > 0 && (name.length + caret + 2 + rightChars) * cw + sparkW + 8 <= width ? buckets : undefined;
    const left: Part[] = [[fit(name, Math.max(nameRoom, 4)), n.snap ? "nm" : "nm quiet"]];
    if (caret) left.unshift(["▸ ", "caret"]);
    svgText(el, g.x0 + INSET, y, left);
    svgText(el, g.x1 - INSET, y, right, { "text-anchor": "end" });
    if (live && sparkW) this.sparkline(el, live, g.x1 - INSET - rightChars * cw - 8 - sparkW, y);
    if (lines > 1) svgText(el, g.x0 + INSET, y + LH, this.decisionLine(d, n, sub, Math.floor(width / cw)));
  }

  private pseudoSub(d: Drawn, sub: Totals): number {
    let s = 0;
    for (const l of d.scene.links) if (l.to === PSEUDO) s += sub.edgeOutcomes.get(l.edge)?.get(l.outcome) ?? 0;
    return s;
  }

  // decisionLine: the plate's decisions, most consequential first; a
  // collapsed group names the member behind its top one.
  private decisionLine(d: Drawn, n: SceneNode, sub: Totals | undefined, room: number): Part[] {
    const outs = loudOutcomes(sub ? sub.nodeOutcomes.get(n.key) ?? new Map() : n.outcomes);
    const parts: Part[] = [];
    outs.forEach(([o, c], i) => {
      const ko = knownOutcome(o);
      if (i > 0) parts.push(["  "]);
      parts.push([o + " " + fmt(c), "dec-o", ko]);
      if (i !== 0 || !n.snap?.group) return;
      const who = [...d.frame.totals.members.get(n.key) ?? []]
        .filter(([, m]) => (m.get(o) ?? 0) > 0).sort((a, b) => (b[1].get(o) ?? 0) - (a[1].get(o) ?? 0));
      if (who.length === 0) return;
      const prefix = n.snap.group.label.slice(0, -1);
      const top = who[0][0].startsWith(prefix) ? who[0][0].slice(prefix.length) : who[0][0];
      parts.push([" " + top + (who.length > 1 ? " +" + (who.length - 1) : ""), "dec-who", ko]);
    });
    while (parts.length > 1 && partsLen(parts) > room) parts.pop();
    if (partsLen(parts) > room) parts[0] = [fit(parts[0][0], room), parts[0][1], parts[0][2]];
    return parts;
  }

  private sparkline(parent: Element, b: readonly number[], x: number, baseline: number): void {
    const max = Math.max(...b, 1);
    b.forEach((c, i) => {
      const h = c ? Math.max(1.5, (SPARK.h * c) / max) : 0.75;
      svgEl("rect", { class: "spark" + (c ? "" : " zero"), x: x + i * (SPARK.w + SPARK.gap), y: baseline - h, width: SPARK.w, height: h }, parent);
    });
  }

  // ---------- hover ----------

  private clearHover(): void {
    const d = this.drawn;
    this.hover = null;
    this.hoverEdge = null;
    this.layers.over.replaceChildren();
    this.tip.hidden = true;
    this.body.classList.remove("hl");
    if (!d) return;
    for (const [key, el] of d.nodeEls) {
      if (!el.classList.contains("on")) continue;
      el.classList.remove("on");
      if (d.labelEls.has(key)) this.drawLabel(d, key);
    }
  }

  private hoverNode(key: string): void {
    const d = this.drawn;
    if (!d) return;
    this.clearHover();
    this.hover = key;
    const sub = this.c.subset(key);
    this.body.classList.add("hl");
    for (const l of d.geo.links) {
      const c = sub.edgeOutcomes.get(l.edge)?.get(l.outcome) ?? 0;
      if (c === 0) continue;
      const w = Math.min(bandWidth(c, l.outcome, d.geo.ky), l.width);
      const o = knownOutcome(l.outcome);
      svgEl("path", { class: "band" + (isLoud(o) ? " loud" : ""), d: ribbon(l, w, -(l.width - w) / 2), "data-o": o }, this.layers.over);
    }
    for (const [k, el] of d.nodeEls) {
      const on = k === key || (sub.nodeTotals.get(k) ?? 0) > 0 || (k === PSEUDO && this.pseudoSub(d, sub) > 0);
      if (!on) continue;
      el.classList.add("on");
      if (d.labelEls.has(k)) this.drawLabel(d, k, sub);
    }
    const tip = this.c.tooltip(key, sub);
    if (tip) this.showTip(tip, d, key);
  }

  private hoverBand(edge: string): void {
    const d = this.drawn;
    if (!d) return;
    this.clearHover();
    this.hoverEdge = edge;
    this.body.classList.add("hl");
    const links = d.geo.links.filter((l) => l.edge === edge);
    for (const l of links) {
      const o = knownOutcome(l.outcome);
      svgEl("path", { class: "band" + (isLoud(o) ? " loud" : ""), d: ribbon(l, l.width), "data-o": o }, this.layers.over);
    }
    const [a, b] = splitEdgeKey(edge);
    const na = d.frame.snap.node(a);
    const nb = d.frame.snap.node(b);
    const calls = na?.col === "engine";
    const outs = d.frame.totals.edgeOutcomes.get(edge) ?? new Map<string, number>();
    const tip: Tip = {
      title: (na?.group?.label ?? na?.label ?? "") + " → " + (nb?.group?.label ?? nb?.label ?? ""),
      kind: calls ? "calls" : "branches",
      facts: [fmt(d.frame.totals.edgeTotals.get(edge) ?? 0) + (calls ? " calls, by their loudest outcome" : " branches")],
      sections: [{ head: "", rows: [...outs].sort((x, y) => bySeverity(x[0], y[0])).map(([o, n]) => ({ label: o, n, o })) }],
      hint: "",
    };
    const l0 = links[0];
    if (l0) this.showTip(tip, d, null, { x: (l0.x0 + l0.x1) / 2 + 12, y: (l0.y0 + l0.y1) / 2 + 12 });
  }

  private showTip(tip: Tip, d: Drawn, key: string | null, at?: { x: number; y: number }): void {
    const t = this.tip;
    t.replaceChildren();
    const h = htmlEl("div", "tt-h", "", t);
    htmlEl("span", "tt-title", tip.title, h);
    htmlEl("span", "dim", " · " + tip.kind, h);
    for (const f of tip.facts) htmlEl("div", "tt-fact", f, t);
    for (const s of tip.sections) {
      if (s.rows.length === 0) continue;
      if (s.head) htmlEl("div", "tt-sec", s.head, t);
      for (const r of s.rows) {
        const row = htmlEl("div", "tt-row", "", t);
        const sw = htmlEl("span", "sw", "", row);
        if (r.o !== undefined) {
          sw.dataset.o = knownOutcome(r.o);
          row.classList.toggle("loud", isLoud(r.o));
        }
        const label = htmlEl("span", "tt-l", r.label, row);
        if (r.o !== undefined) label.dataset.o = knownOutcome(r.o);
        const note = htmlEl("span", "tt-note", "", row);
        for (const [o, c] of [...r.note ?? []].sort((x, y) => bySeverity(x[0], y[0]))) {
          const s2 = htmlEl("span", "", o + " " + fmt(c) + " ", note);
          s2.dataset.o = knownOutcome(o);
        }
        htmlEl("b", "", fmt(r.n), row);
      }
    }
    if (tip.hint) htmlEl("div", "tt-hint", tip.hint, t);
    t.hidden = false;

    const W = this.body.clientWidth;
    const H = this.body.clientHeight;
    const v = this.view;
    const w = t.offsetWidth;
    const th = t.offsetHeight;
    let x = 0;
    let y = 0;
    // Scene coordinates to panel ones, so the tip lands beside its node at any zoom.
    const px = (sx: number) => sx * v.k + v.tx;
    const py = (sy: number) => sy * v.k + v.ty;
    if (at) [x, y] = [px(at.x), py(at.y)];
    const g = key !== null ? d.geo.byKey.get(key) : undefined;
    if (g) {
      const [x0, x1, y0, y1] = [px(g.x0), px(g.x1), py(g.y0), py(g.y0 + g.h)];
      if (g.kind === "outcome") [x, y] = [x0 - w - 14, y0];
      else if (g.kind === "engine") [x, y] = [x1 + 14, y0];
      else [x, y] = [x0, y1 + 8 + th < H ? y1 + 8 : y0 - th - 8];
    } else if (key !== null) {
      const el = d.nodeEls.get(key)?.querySelector("rect.hit");
      const r = el?.getBoundingClientRect();
      const b = this.body.getBoundingClientRect();
      if (r) [x, y] = [r.left - b.left, r.bottom - b.top + 6];
    }
    t.style.left = Math.max(4, Math.min(x, W - w - 8)) + "px";
    t.style.top = Math.max(4, Math.min(y, H - th - 8)) + "px";
  }

  // ---------- input ----------

  private keyAt(target: EventTarget | null): string | undefined {
    for (let el = target instanceof Element ? target : null; el && el !== this.body; el = el.parentElement) {
      const k = this.keyOf.get(el);
      if (k !== undefined) return k;
    }
    return undefined;
  }

  private click(key: string, ev: MouseEvent): void {
    const [col, name] = splitKey(key);
    if (col === "group") {
      this.c.toggleGroup(key);
      return;
    }
    toggleFilter(col, name, ev.shiftKey || ev.ctrlKey || ev.metaKey);
  }

  // flash restarts the node's arrival animation (1.5 s decay, in CSS).
  private flash(key: string): void {
    const el = this.drawn?.nodeEls.get(key);
    if (!el) return;
    el.classList.remove("flash");
    void el.getBoundingClientRect();
    el.classList.add("flash");
    window.clearTimeout(this.flashTimers.get(el));
    this.flashTimers.set(el, window.setTimeout(() => el.classList.remove("flash"), 1500));
  }
}
