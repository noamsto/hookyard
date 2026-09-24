// The flow panel: header (meta/error/idle toggle), hint line, and the React
// Flow canvas. useReactFlow (for keyboard shortcuts and the initial fitView)
// only works inside a ReactFlowProvider subtree, so FlowCanvas — not
// FlowApp — is the provider's child.
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { MouseEvent as ReactMouseEvent, PointerEvent as ReactPointerEvent } from "react";
import { Controls, MiniMap, ReactFlow, ReactFlowProvider, useOnViewportChange, useReactFlow } from "@xyflow/react";
import type { Node as RFNode } from "@xyflow/react";
import { toggleFilter } from "./bridge.ts";
import type { FlowController } from "./model/controller.ts";
import { splitKey } from "./model/keys.ts";
import type { TipLine } from "./model/snapshot.ts";
import { CardNode } from "./nodes/CardNode.tsx";
import { GroupNode } from "./nodes/GroupNode.tsx";
import { HeaderNode } from "./nodes/HeaderNode.tsx";
import { CountEdge } from "./edges/CountEdge.tsx";
import { PulseLayer } from "./PulseLayer.tsx";
import { buildEdges, buildNodes } from "./viewModel.ts";
import type { Highlight } from "./viewModel.ts";

const NODE_TYPES = { card: CardNode, handlerGroup: GroupNode, colHeader: HeaderNode };
const EDGE_TYPES = { count: CountEdge };
const FIT_OPTIONS = { padding: 0.15, duration: 200 };

function isEditable(el: Element | null): boolean {
  const tag = el?.tagName ?? "";
  return tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA";
}

function positionTip(bodyEl: HTMLDivElement | null, tipEl: HTMLDivElement | null, ev: { clientX: number; clientY: number }): void {
  if (!bodyEl || !tipEl) return;
  const r = bodyEl.getBoundingClientRect();
  const x = ev.clientX - r.left;
  const y = ev.clientY - r.top;
  const w = tipEl.offsetWidth;
  const h = tipEl.offsetHeight;
  tipEl.style.left = (x + 14 + w > r.width ? Math.max(0, x - 14 - w) : x + 14) + "px";
  tipEl.style.top = (y + 14 + h > r.height ? Math.max(0, y - 14 - h) : y + 14) + "px";
}

function Tooltip({ lines }: { lines: TipLine[] }) {
  return (
    <>
      {lines.map((line, i) => (
        <div key={i} className={line.cls || undefined}>{line.text}</div>
      ))}
    </>
  );
}

function FlowCanvas({ controller }: { controller: FlowController }) {
  const rf = useReactFlow();
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const tipRef = useRef<HTMLDivElement | null>(null);
  const fittedRef = useRef(false);
  const panningRef = useRef(false);
  const lastPointerRef = useRef<{ clientX: number; clientY: number } | null>(null);
  const [hoveredKey, setHoveredKey] = useState<string | undefined>(undefined);

  const snap = controller.snapshot();
  const totals = controller.totals();
  const rawEdges = controller.edges();

  const highlight: Highlight | null = useMemo(
    () => (hoveredKey !== undefined ? controller.highlight(hoveredKey) : null),
    [hoveredKey, snap, totals, controller],
  );
  const nodes = useMemo(() => buildNodes(snap, totals, controller, highlight), [snap, totals, controller, highlight]);
  const edges = useMemo(() => buildEdges(snap, rawEdges, highlight), [snap, rawEdges, highlight]);

  useEffect(() => {
    if (!fittedRef.current && snap && controller.isVisible()) {
      fittedRef.current = true;
      rf.fitView(FIT_OPTIONS);
    }
  });

  useEffect(() => {
    function onKey(ev: KeyboardEvent): void {
      if (!controller.isVisible() || ev.ctrlKey || ev.metaKey || ev.altKey) return;
      if (isEditable(document.activeElement)) return;
      if (ev.key === "+" || ev.key === "=") rf.zoomIn();
      else if (ev.key === "-") rf.zoomOut();
      else if (ev.key === "0") rf.fitView(FIT_OPTIONS);
      else return;
      ev.preventDefault();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [controller, rf]);

  useEffect(() => {
    const release = () => setTimeout(() => controller.pressEnd(), 0);
    window.addEventListener("pointerup", release);
    window.addEventListener("pointercancel", release);
    window.addEventListener("lostpointercapture", release);
    window.addEventListener("blur", release);
    return () => {
      window.removeEventListener("pointerup", release);
      window.removeEventListener("pointercancel", release);
      window.removeEventListener("lostpointercapture", release);
      window.removeEventListener("blur", release);
    };
  }, [controller]);

  useOnViewportChange({
    onChange: (vp) => {
      bodyRef.current?.style.setProperty("--flow-label-font", Math.max(12, 9 / vp.zoom) + "px");
    },
  });

  const onPointerDownCapture = useCallback(
    (ev: ReactPointerEvent) => {
      const el = (ev.target as HTMLElement).closest(".react-flow__node");
      controller.pressBegin(el?.getAttribute("data-id") ?? undefined);
    },
    [controller],
  );

  const onNodeClick = useCallback(
    (ev: ReactMouseEvent, node: RFNode) => {
      if (!controller.snapshot()?.has(node.id)) return; // headers etc. — not a graph node
      const [col, name] = splitKey(node.id);
      if (col === "group") {
        if (!(ev.target as HTMLElement).closest(".flow-group-header")) return;
        controller.toggleGroup(node.id);
        return;
      }
      toggleFilter(col, name, ev.shiftKey || ev.ctrlKey || ev.metaKey);
    },
    [controller],
  );

  const onNodeMouseEnter = useCallback(
    (ev: ReactMouseEvent, node: RFNode) => {
      if (panningRef.current) return;
      if (!controller.snapshot()?.has(node.id)) return; // headers etc. — not a graph node
      lastPointerRef.current = { clientX: ev.clientX, clientY: ev.clientY };
      setHoveredKey(node.id);
    },
    [controller],
  );
  const onNodeMouseMove = useCallback((ev: ReactMouseEvent) => {
    lastPointerRef.current = { clientX: ev.clientX, clientY: ev.clientY };
    positionTip(bodyRef.current, tipRef.current, ev);
  }, []);
  const onNodeMouseLeave = useCallback(() => setHoveredKey(undefined), []);
  const onMoveStart = useCallback((ev: MouseEvent | TouchEvent | null) => {
    if (ev?.type === "wheel") return; // wheel zoom, not a drag pan — keep the hover
    panningRef.current = true;
    setHoveredKey(undefined);
  }, []);
  const onMoveEnd = useCallback(() => {
    panningRef.current = false;
  }, []);

  const tipLines = hoveredKey !== undefined ? controller.tooltip(hoveredKey) : null;

  // The tooltip div only mounts once hoveredKey is set, so it can't be
  // positioned from onNodeMouseEnter (tipRef.current is still null there).
  // Position it here, after render, from the last known pointer position;
  // mousemove keeps that position fresh and repositions directly.
  useLayoutEffect(() => {
    if (hoveredKey !== undefined && lastPointerRef.current) {
      positionTip(bodyRef.current, tipRef.current, lastPointerRef.current);
    }
  }, [hoveredKey, tipLines]);

  return (
    <div
      id="flow-body"
      className={"flow-body" + (hoveredKey !== undefined ? " hl" : "")}
      ref={bodyRef}
      data-calls={totals.calls}
      data-branches={totals.branches}
      data-dropped={controller.dropped()}
      data-layout-gen={controller.layoutGen()}
      data-pending-layout={controller.pendingLayout() ? 1 : 0}
      onPointerDownCapture={onPointerDownCapture}
    >
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={NODE_TYPES}
        edgeTypes={EDGE_TYPES}
        nodesDraggable={false}
        nodesConnectable={false}
        nodesFocusable={false}
        edgesFocusable={false}
        elementsSelectable={false}
        minZoom={0.2}
        maxZoom={4}
        proOptions={{ hideAttribution: true }}
        onNodeClick={onNodeClick}
        onNodeMouseEnter={onNodeMouseEnter}
        onNodeMouseMove={onNodeMouseMove}
        onNodeMouseLeave={onNodeMouseLeave}
        onMoveStart={onMoveStart}
        onMoveEnd={onMoveEnd}
      >
        <Controls showInteractive={false} />
        <MiniMap pannable zoomable nodeClassName={(n) => (n.type === "colHeader" || n.parentId ? "flow-minimap-hidden" : "")} />
        <PulseLayer controller={controller} bodyRef={bodyRef} />
      </ReactFlow>
      {tipLines && tipLines.length > 0 && (
        <div ref={tipRef} className="flow-tip">
          <Tooltip lines={tipLines} />
        </div>
      )}
    </div>
  );
}

export function FlowApp({ controller }: { controller: FlowController }) {
  useSyncExternalStore(controller.subscribe, controller.getVersion);
  const error = controller.error();
  const idle = controller.idleCount();

  return (
    <>
      <div className="panel-header">
        <h2>flow</h2>
        <span className="dim">{controller.meta()}</span>
        {error && <span className="flow-error">{error}</span>}
        <label className="flow-idle">
          <input
            type="checkbox"
            checked={controller.showIdle()}
            onChange={(ev) => controller.setShowIdle(ev.target.checked)}
          />
          <span>show idle ({idle})</span>
        </label>
      </div>
      <p className="hint flow-hint">
        engine and event count calls; handler and outcome count branches (one handler run). Click a node to filter,
        shift-click to add, drag to pan, wheel to zoom, 0 to fit
      </p>
      <ReactFlowProvider>
        <FlowCanvas controller={controller} />
      </ReactFlowProvider>
    </>
  );
}
