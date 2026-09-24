// Live pulses: one dot per real call enters its event, then fans out into
// one dot per handler run of that call — the fan-out made literal. A fixed
// pool of MAX_DOTS circles, created once; a dot that finds the pool empty is
// counted as dropped, never drawn. rAF-driven, DOM attributes only, and every
// flight resolves its band against the CURRENT geometry each frame, so a
// refresh mid-flight just moves the dot (or ends it if its band is gone).

import { MAX_DOTS } from "../constants.ts";
import { edgeKey, splitKey } from "../model/keys.ts";
import { bySeverity, isLoud, knownOutcome } from "../model/snapshot.ts";
import { svgEl } from "./dom.ts";
import { pointOn } from "./geometry.ts";
import type { GeoLink } from "./geometry.ts";
import { linkId, PSEUDO } from "./scene.ts";

const LEG_MS = 900;
const FAN_DELAY_MS = 180; // the dot crosses its event plate, then fans out

interface Leg { id: string; outcome: string; }
interface Flight { slot: number; legs: Leg[]; leg: number; t0: number; jitter: number; }

export interface PulseHost {
  link(id: string): GeoLink | undefined;
  flash(nodeKey: string): void;
  dropped(n: number): void;
  active(n: number): void;
}

export class Pulses {
  private readonly circles: SVGCircleElement[] = [];
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
      this.circles.push(svgEl("circle", { class: "flow-dot", r: 0, cx: 0, cy: 0 }, layer));
      this.busy.push(false);
    }
  }

  // call animates one call's drawn branches, each given as its display keys
  // [engine, event, handler?, outcome].
  call(branches: string[][]): void {
    if (branches.length === 0) return;
    const [eng, ev] = branches[0];
    const outcomes = branches.map((b) => splitKey(b[b.length - 1])[1]);
    const cls = outcomes.slice().sort(bySeverity)[0];
    if (this.reduced) {
      this.host.flash(ev);
      return;
    }
    const now = performance.now();
    const e1 = edgeKey(eng, ev);
    this.fly([{ id: linkId({ edge: e1, from: eng, to: ev, outcome: cls }), outcome: cls }], now);
    const fanAt = now + LEG_MS;
    const t = window.setTimeout(() => {
      this.timers.delete(t);
      this.host.flash(ev);
    }, LEG_MS);
    this.timers.add(t);
    branches.forEach((keys, i) => {
      const o = outcomes[i];
      const legs: Leg[] = [];
      if (keys.length === 4) {
        const [, , h, out] = keys;
        legs.push({ id: linkId({ edge: edgeKey(ev, h), from: ev, to: h, outcome: o }), outcome: o });
        legs.push({ id: linkId({ edge: edgeKey(h, out), from: h, to: out, outcome: o }), outcome: o });
      } else {
        const out = keys[2];
        const edge = edgeKey(ev, out);
        legs.push({ id: linkId({ edge, from: ev, to: PSEUDO, outcome: o }), outcome: o });
        legs.push({ id: linkId({ edge, from: PSEUDO, to: out, outcome: o }), outcome: o });
      }
      this.fly(legs, fanAt + FAN_DELAY_MS + Math.random() * 120);
    });
  }

  private fly(legs: Leg[], t0: number): void {
    const slot = this.busy.indexOf(false);
    if (slot < 0) {
      this.host.dropped(1);
      return;
    }
    this.busy[slot] = true;
    this.busyCount++;
    this.flights.push({ slot, legs, leg: 0, t0, jitter: Math.random() - 0.5 });
    this.host.active(this.busyCount);
    if (!this.raf) this.raf = requestAnimationFrame(this.frame);
  }

  private release(f: Flight): void {
    const c = this.circles[f.slot];
    c.setAttribute("r", "0");
    this.busy[f.slot] = false;
    this.busyCount--;
  }

  private readonly frame = (now: number): void => {
    this.raf = 0;
    const keep: Flight[] = [];
    for (const f of this.flights) {
      const c = this.circles[f.slot];
      let u = (now - f.t0) / LEG_MS;
      if (u < 0) {
        c.setAttribute("r", "0");
        keep.push(f);
        continue;
      }
      if (u >= 1) {
        f.leg++;
        if (f.leg >= f.legs.length) {
          this.release(f);
          continue;
        }
        f.t0 = now;
        u = 0;
      }
      const leg = f.legs[f.leg];
      const l = this.host.link(leg.id);
      if (!l) {
        this.release(f);
        continue;
      }
      const p = pointOn(l, u, f.jitter * Math.max(l.width - 2, 0));
      c.setAttribute("cx", p.x.toFixed(1));
      c.setAttribute("cy", p.y.toFixed(1));
      c.setAttribute("data-o", knownOutcome(leg.outcome));
      c.setAttribute("r", isLoud(leg.outcome) ? "3.4" : "2.4");
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
