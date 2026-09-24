// Live pulse animation (spec §4.6): a fixed pool of MAX_DOTS circles created
// once; the rAF loop writes cx/cy straight to the DOM from refs, never
// through React state. Branches resolve to display edges through the
// CURRENT rendered snapshot, so a grouped handler's pulse lands on the
// collapsed group (same invariant as hover/tooltip).
import { useEffect, useRef } from "react";
import type { RefObject } from "react";
import { ViewportPortal } from "@xyflow/react";
import { DOT_MS, MAX_DOTS } from "./constants.ts";
import type { FlowController } from "./model/controller.ts";
import { branchKey, consecutivePairs, edgeKey } from "./model/keys.ts";
import type { Branch } from "./model/keys.ts";
import { edgePathRegistry } from "./edges/CountEdge.tsx";

const SVG_NS = "http://www.w3.org/2000/svg";

interface Dot { el: SVGCircleElement; busy: boolean; }
interface ActiveDot { dot: Dot; edges: SVGPathElement[]; start: number; perEdgeMs: number; }
interface QueueItem { branch: Branch; n: number; }

export function PulseLayer({ controller, bodyRef }: { controller: FlowController; bodyRef: RefObject<HTMLDivElement | null> }) {
  const svgRef = useRef<SVGSVGElement | null>(null);
  const dotsRef = useRef<Dot[]>([]);

  // The fixed pool: created once, never resized.
  useEffect(() => {
    const svg = svgRef.current;
    if (!svg) return;
    const dots: Dot[] = [];
    for (let i = 0; i < MAX_DOTS; i++) {
      const el = document.createElementNS(SVG_NS, "circle");
      el.setAttribute("class", "flow-dot");
      el.setAttribute("r", "0");
      svg.appendChild(el);
      dots.push({ el, busy: false });
    }
    dotsRef.current = dots;
    return () => {
      for (const d of dots) d.el.remove();
      dotsRef.current = [];
    };
  }, []);

  useEffect(() => {
    const queue = new Map<string, QueueItem>();
    const active: ActiveDot[] = [];
    let rafId: number | null = null;
    let lastGen = controller.layoutGen();

    const canAnimate = () => controller.isLive() && controller.isVisible() && !document.hidden;

    function claimDot(): Dot | null {
      for (const d of dotsRef.current) if (!d.busy) return d;
      return null;
    }
    function freeDot(d: Dot): void {
      d.busy = false;
      d.el.setAttribute("r", "0");
    }
    function requestTick(): void {
      if (rafId !== null || !canAnimate()) return;
      rafId = requestAnimationFrame(tick);
    }
    function flushQueue(): void {
      if (queue.size === 0) return;
      const items = Array.from(queue.values());
      queue.clear();
      const snap = controller.snapshot();
      let dropped = 0;
      for (let i = 0; i < items.length; i++) {
        const item = items[i];
        const dot = claimDot();
        if (!dot) {
          for (let j = i; j < items.length; j++) dropped += items[j].n;
          break;
        }
        const nodes = snap ? snap.displayNodes(item.branch) : [];
        const edges: SVGPathElement[] = [];
        for (const [a, b] of consecutivePairs(nodes)) {
          const el = edgePathRegistry.get(edgeKey(a, b));
          if (el) edges.push(el);
        }
        if (edges.length === 0) {
          freeDot(dot); // the topology hasn't drawn this branch yet — skip this frame
          continue;
        }
        dot.busy = true;
        const radius = 3 + Math.min(4, Math.log2(Math.max(1, item.n)));
        dot.el.setAttribute("r", String(radius));
        active.push({ dot, edges, start: performance.now(), perEdgeMs: DOT_MS / edges.length });
      }
      if (dropped > 0) controller.addDropped(dropped);
    }
    function tick(now: number): void {
      rafId = null;
      flushQueue();
      for (let i = active.length - 1; i >= 0; i--) {
        const st = active[i];
        const elapsed = now - st.start;
        if (elapsed >= DOT_MS) {
          freeDot(st.dot);
          active.splice(i, 1);
          continue;
        }
        const edgeIdx = Math.min(st.edges.length - 1, Math.floor(elapsed / st.perEdgeMs));
        const edgeElapsed = elapsed - edgeIdx * st.perEdgeMs;
        const t = Math.min(1, edgeElapsed / st.perEdgeMs);
        const edge = st.edges[edgeIdx];
        const pt = edge.getPointAtLength(t * edge.getTotalLength());
        st.dot.el.setAttribute("cx", String(pt.x));
        st.dot.el.setAttribute("cy", String(pt.y));
      }
      bodyRef.current?.setAttribute("data-active-dots", String(active.length));
      if (canAnimate() && (active.length > 0 || queue.size > 0)) rafId = requestAnimationFrame(tick);
    }
    function cancelAll(): void {
      if (rafId !== null) {
        cancelAnimationFrame(rafId);
        rafId = null;
      }
      for (const st of active) freeDot(st.dot);
      active.length = 0;
      queue.clear();
      bodyRef.current?.setAttribute("data-active-dots", "0");
    }

    const unsubscribe = controller.onPulse((branches) => {
      if (!canAnimate()) return;
      for (const b of branches) {
        const key = branchKey(b);
        const item = queue.get(key);
        if (item) item.n++;
        else queue.set(key, { branch: b, n: 1 });
      }
      requestTick();
    });

    // A relayout commit may unmount the edge <path>s an active dot is
    // sampling; cancel rather than animate onto a stale, detached element.
    const unsubscribeVersion = controller.subscribe(() => {
      const gen = controller.layoutGen();
      if (gen !== lastGen) {
        lastGen = gen;
        cancelAll();
      }
    });

    function onVisibility(): void {
      if (document.hidden) cancelAll();
    }
    document.addEventListener("visibilitychange", onVisibility);

    return () => {
      unsubscribe();
      unsubscribeVersion();
      document.removeEventListener("visibilitychange", onVisibility);
      cancelAll();
    };
  }, [controller, bodyRef]);

  return (
    <ViewportPortal>
      <svg ref={svgRef} className="flow-pulse-layer" style={{ overflow: "visible", position: "absolute", width: 0, height: 0 }} />
    </ViewportPortal>
  );
}
