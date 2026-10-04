// Live pulses: one dot per real call enters its event, then fans out into
// one dot per handler run of that call — the fan-out made literal, never
// invented motion. Real traffic is sparse (a call every few seconds), so each
// pulse lingers: ~5 s engine to outcome, a short fading trail, an ease-out
// into the outcome bar, and a decision slower and larger still. A fixed pool
// of MAX_DOTS dots (a circle and two trail segments each), created once; a
// dot that finds the pool full evicts the oldest flight so it can be drawn.
// rAF-driven, DOM attributes only, and every flight resolves its band
// against the CURRENT geometry each frame, so a refresh mid-flight just moves
// the dot (or ends it if its band is gone).

import { BRIDGE_MS, FAN_DELAY_MS, HOP_MS, LOUD_HOP_MS, MAX_DOTS, TRAIL } from "../constants.ts";
import { edgeKey, splitKey } from "../model/keys.ts";
import { bySeverity, isLoud, knownOutcome } from "../model/snapshot.ts";
import { svgEl } from "./dom.ts";
import { pointOn } from "./geometry.ts";
import type { GeoLink } from "./geometry.ts";
import { linkId, PSEUDO } from "./scene.ts";

interface Leg { id: string; ms: number; last: boolean; }
interface Flight {
  slot: number; legs: Leg[]; leg: number; t0: number; jitter: number; outcome: string; arrive: string;
  lastX?: number; lastY?: number; bridgeFrom: { x: number; y: number } | null; bridgeStart: number;
  tail?: { near: number[]; far: number[] };
}

interface Dot { circle: SVGCircleElement; near: SVGPathElement; far: SVGPathElement; }

export interface PulseHost {
  link(id: string): GeoLink | undefined;
  flash(nodeKey: string): void;
  dropped(n: number): void;
  active(n: number): void;
}

const legMs = (outcome: string): number => (isLoud(outcome) ? LOUD_HOP_MS : HOP_MS);
const easeOut = (u: number): number => 1 - (1 - u) ** 3;

export class Pulses {
  private readonly dots: Dot[] = [];
  private readonly busy: boolean[] = [];
  private flights: Flight[] = [];
  private timers = new Set<number>();
  private raf = 0;
  private busyCount = 0;
  private readonly host: PulseHost;
  private readonly reduced: boolean;

  constructor(layer: SVGGElement, host: PulseHost, reduced: boolean) {
    this.host = host;
    this.reduced = reduced;
    for (let i = 0; i < MAX_DOTS; i++) {
      const far = svgEl("path", { class: "flow-trail far", d: "" }, layer);
      const near = svgEl("path", { class: "flow-trail near", d: "" }, layer);
      const circle = svgEl("circle", { class: "flow-dot", r: 0, cx: 0, cy: 0 }, layer);
      this.dots.push({ circle, near, far });
      this.busy.push(false);
    }
  }

  // call animates one call's drawn branches, each given as its display keys
  // [engine, event, handler?, outcome].
  call(branches: string[][]): void {
    if (branches.length === 0) return;
    const [eng, ev] = branches[0];
    const outcomes = branches.map((b) => splitKey(b[b.length - 1])[1]);
    if (this.reduced) {
      this.host.flash(ev);
      for (const b of branches) this.host.flash(b[b.length - 1]);
      return;
    }
    const cls = outcomes.slice().sort(bySeverity)[0];
    const now = performance.now();
    const first = legMs(cls);
    this.fly([{ id: linkId({ edge: edgeKey(eng, ev), from: eng, to: ev, outcome: cls }), ms: first, last: false }], now, cls, "");
    this.later(first, () => this.host.flash(ev));
    branches.forEach((keys, i) => {
      const o = outcomes[i];
      const out = keys[keys.length - 1];
      const ms = legMs(o);
      const [a, b] = keys.length === 4
        ? [{ edge: edgeKey(ev, keys[2]), from: ev, to: keys[2] }, { edge: edgeKey(keys[2], out), from: keys[2], to: out }]
        : [{ edge: edgeKey(ev, out), from: ev, to: PSEUDO }, { edge: edgeKey(ev, out), from: PSEUDO, to: out }];
      this.fly([
        { id: linkId({ ...a, outcome: o }), ms, last: false },
        { id: linkId({ ...b, outcome: o }), ms, last: true },
      ], now + first + FAN_DELAY_MS + Math.random() * 160, o, out);
    });
  }

  private later(ms: number, fn: () => void): void {
    const t = window.setTimeout(() => {
      this.timers.delete(t);
      fn();
    }, ms);
    this.timers.add(t);
  }

  private fly(legs: Leg[], t0: number, outcome: string, arrive: string): void {
    let slot = this.busy.indexOf(false);
    if (slot < 0) {
      const old = this.flights.shift(); // oldest active flight — arrival order, never reordered
      if (old) {
        this.release(old);
        this.host.dropped(1);
      }
      slot = this.busy.indexOf(false);
    }
    if (slot < 0) return; // MAX_DOTS === 0 is not a real configuration; nothing to draw into
    this.busy[slot] = true;
    this.busyCount++;
    this.flights.push({ slot, legs, leg: 0, t0, jitter: Math.random() - 0.5, outcome, arrive, bridgeFrom: null, bridgeStart: 0 });
    this.host.active(this.busyCount);
    if (!this.raf) this.raf = requestAnimationFrame(this.frame);
  }

  private hide(d: Dot): void {
    d.circle.setAttribute("r", "0");
    d.circle.removeAttribute("data-bridging");
    d.near.setAttribute("d", "");
    d.far.setAttribute("d", "");
  }

  private release(f: Flight): void {
    this.hide(this.dots[f.slot]);
    this.busy[f.slot] = false;
    this.busyCount--;
  }

  private readonly frame = (now: number): void => {
    this.raf = 0;
    const keep: Flight[] = [];
    for (const f of this.flights) {
      const d = this.dots[f.slot];
      let leg = f.legs[f.leg];
      let u = (now - f.t0) / leg.ms;

      if (u < 0) {
        if (f.bridgeFrom) {
          const l = this.host.link(leg.id);
          if (!l) {
            this.release(f);
            continue;
          }
          const bt = Math.min(1, (now - f.bridgeStart) / BRIDGE_MS);
          const dy = f.jitter * Math.max(l.width - 2, 0);
          const target = pointOn(l, 0, dy);
          const from = f.bridgeFrom;
          const x = from.x + (target.x - from.x) * bt;
          const y = from.y + (target.y - from.y) * bt;
          const bridgeAt = (t: number) => {
            const tt = Math.max(bt - t, 0);
            return { x: from.x + (target.x - from.x) * tt, y: from.y + (target.y - from.y) * tt };
          };
          const loud = isLoud(f.outcome);
          const o = knownOutcome(f.outcome);
          d.circle.setAttribute("cx", x.toFixed(1));
          d.circle.setAttribute("cy", y.toFixed(1));
          d.circle.setAttribute("data-o", o);
          d.circle.setAttribute("data-bridging", "1"); // e2e: on-path across the node's own plate, not stray
          d.circle.setAttribute("r", loud ? "4.2" : "3");
          const segB = (a: number, b: number) => {
            const q0 = bridgeAt(b);
            const q1 = bridgeAt((a + b) / 2);
            const q2 = bridgeAt(a);
            return `M${q0.x.toFixed(1)},${q0.y.toFixed(1)}L${q1.x.toFixed(1)},${q1.y.toFixed(1)}L${q2.x.toFixed(1)},${q2.y.toFixed(1)}`;
          };
          // Carry the last leg trail rigidly so its pixel length stays constant.
          const carry = (o: number[]) =>
            `M${(x + o[0]).toFixed(1)},${(y + o[1]).toFixed(1)}L${(x + o[2]).toFixed(1)},${(y + o[3]).toFixed(1)}L${(x + o[4]).toFixed(1)},${(y + o[5]).toFixed(1)}`;
          d.near.setAttribute("d", f.tail ? carry(f.tail.near) : segB(0, TRAIL[0]));
          d.far.setAttribute("d", f.tail ? carry(f.tail.far) : segB(TRAIL[0], TRAIL[1]));
          d.near.setAttribute("data-o", o);
          d.far.setAttribute("data-o", o);
          d.near.setAttribute("stroke-width", loud ? "4.4" : "3");
          d.far.setAttribute("stroke-width", loud ? "3.4" : "2.2");
          f.lastX = x;
          f.lastY = y;
          if (bt >= 1) f.bridgeFrom = null;
          keep.push(f);
          continue;
        }
        this.hide(d);
        keep.push(f);
        continue;
      }
      if (u >= 1) {
        if (leg.last && f.arrive) this.host.flash(f.arrive);
        f.leg++;
        if (f.leg >= f.legs.length) {
          this.release(f);
          continue;
        }
        if (f.lastX !== undefined) {
          f.bridgeFrom = { x: f.lastX, y: f.lastY! };
          f.bridgeStart = now;
          f.t0 = now + BRIDGE_MS;
        } else {
          f.t0 = now;
        }
        keep.push(f);
        continue;
      }
      const l = this.host.link(leg.id);
      if (!l) {
        this.release(f);
        continue;
      }
      const ease = leg.last ? easeOut : (x: number) => x;
      const dy = f.jitter * Math.max(l.width - 2, 0);
      const at = (x: number) => pointOn(l, ease(Math.max(x, 0)), dy);
      const p = at(u);
      const loud = isLoud(f.outcome);
      const o = knownOutcome(f.outcome);
      d.circle.setAttribute("cx", p.x.toFixed(1));
      d.circle.setAttribute("cy", p.y.toFixed(1));
      d.circle.setAttribute("data-o", o);
      d.circle.removeAttribute("data-bridging");
      d.circle.setAttribute("r", loud ? "4.2" : "3");
      const pts = (a: number, b: number) => [at(u - b), at(u - (a + b) / 2), at(u - a)];
      const path = (q: { x: number; y: number }[]) =>
        `M${q[0].x.toFixed(1)},${q[0].y.toFixed(1)}L${q[1].x.toFixed(1)},${q[1].y.toFixed(1)}L${q[2].x.toFixed(1)},${q[2].y.toFixed(1)}`;
      const nearPts = pts(0, TRAIL[0]);
      const farPts = pts(TRAIL[0], TRAIL[1]);
      d.near.setAttribute("d", path(nearPts));
      d.far.setAttribute("d", path(farPts));
      const rel = (q: { x: number; y: number }[]) => q.flatMap((v) => [v.x - p.x, v.y - p.y]);
      f.tail = { near: rel(nearPts), far: rel(farPts) };
      d.near.setAttribute("data-o", o);
      d.far.setAttribute("data-o", o);
      d.near.setAttribute("stroke-width", loud ? "4.4" : "3");
      d.far.setAttribute("stroke-width", loud ? "3.4" : "2.2");
      f.lastX = p.x;
      f.lastY = p.y;
      keep.push(f);
    }
    this.flights = keep;
    this.host.active(this.busyCount);
    if (this.flights.length > 0) this.raf = requestAnimationFrame(this.frame);
  };

  // cancel ends every flight at once: the view hid, the day stopped being
  // live, or the tab went to the background.
  cancel(): void {
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    for (const t of this.timers) window.clearTimeout(t);
    this.timers.clear();
    for (const f of this.flights) this.release(f);
    this.flights = [];
    this.host.active(0);
  }
}
