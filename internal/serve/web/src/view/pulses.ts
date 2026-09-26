// Live pulses: one dot per real call enters its event, then fans out into
// one dot per handler run of that call — the fan-out made literal, never
// invented motion. Real traffic is sparse (a call every few seconds), so each
// pulse lingers: ~3 s engine to outcome, a short fading trail, an ease-out
// into the outcome bar, and a decision slower and larger still. A fixed pool
// of MAX_DOTS dots (a circle and two trail segments each), created once; a
// dot that finds the pool empty is counted as dropped, never drawn.
// rAF-driven, DOM attributes only, and every flight resolves its band
// against the CURRENT geometry each frame, so a refresh mid-flight just moves
// the dot (or ends it if its band is gone).

import { MAX_DOTS } from "../constants.ts";
import { edgeKey, splitKey } from "../model/keys.ts";
import { bySeverity, isLoud, knownOutcome } from "../model/snapshot.ts";
import { svgEl } from "./dom.ts";
import { pointOn } from "./geometry.ts";
import type { GeoLink } from "./geometry.ts";
import { linkId, PSEUDO } from "./scene.ts";

const LEG_MS = 950; // per hop: engine -> event -> handler -> outcome ≈ 3 s
const LOUD_LEG_MS = 1150; // a decision takes its time
const FAN_DELAY_MS = 200; // the dot crosses its event plate, then fans out
const TRAIL = [0.035, 0.09]; // trail segment ends, as leg fractions behind the dot

interface Leg { id: string; ms: number; last: boolean; }
interface Flight { slot: number; legs: Leg[]; leg: number; t0: number; jitter: number; outcome: string; arrive: string; }

interface Dot { circle: SVGCircleElement; near: SVGPathElement; far: SVGPathElement; }

export interface PulseHost {
  link(id: string): GeoLink | undefined;
  flash(nodeKey: string): void;
  dropped(n: number): void;
  active(n: number): void;
}

const legMs = (outcome: string): number => (isLoud(outcome) ? LOUD_LEG_MS : LEG_MS);
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
    const slot = this.busy.indexOf(false);
    if (slot < 0) {
      this.host.dropped(1);
      return;
    }
    this.busy[slot] = true;
    this.busyCount++;
    this.flights.push({ slot, legs, leg: 0, t0, jitter: Math.random() - 0.5, outcome, arrive });
    this.host.active(this.busyCount);
    if (!this.raf) this.raf = requestAnimationFrame(this.frame);
  }

  private hide(d: Dot): void {
    d.circle.setAttribute("r", "0");
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
        f.t0 = now;
        leg = f.legs[f.leg];
        u = 0;
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
      d.circle.setAttribute("r", loud ? "4.2" : "3");
      const seg = (a: number, b: number) => {
        const q0 = at(u - b);
        const q1 = at(u - (a + b) / 2);
        const q2 = at(u - a);
        return `M${q0.x.toFixed(1)},${q0.y.toFixed(1)}L${q1.x.toFixed(1)},${q1.y.toFixed(1)}L${q2.x.toFixed(1)},${q2.y.toFixed(1)}`;
      };
      d.near.setAttribute("d", seg(0, TRAIL[0]));
      d.far.setAttribute("d", seg(TRAIL[0], TRAIL[1]));
      d.near.setAttribute("data-o", o);
      d.far.setAttribute("data-o", o);
      d.near.setAttribute("stroke-width", loud ? "4.4" : "3");
      d.far.setAttribute("stroke-width", loud ? "3.4" : "2.2");
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
