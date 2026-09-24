// A display edge, drawn as a bezier whose width scales with traffic. Its
// <path> registers itself in edgePathRegistry (a ref callback) so the pulse
// layer can sample points along it with getPointAtLength; operator strings
// (dataKey's names) reach the DOM only as a data-* attribute value.
import { useCallback } from "react";
import { getBezierPath } from "@xyflow/react";
import type { Edge, EdgeProps } from "@xyflow/react";
import type { DisplayCol } from "../types.ts";

export interface CountData extends Record<string, unknown> {
  fromCol: DisplayCol;
  fromName: string;
  toCol: DisplayCol;
  toName: string;
  fromTip: string;
  toTip: string;
  toUnit: string;
  n: number;
  maxN: number;
  direct: boolean;
  on: boolean;
}

export type CountEdgeType = Edge<CountData, "count">;

// edgeKey (source\x01target) -> its rendered <path>, read by PulseLayer.
export const edgePathRegistry = new Map<string, SVGPathElement>();

export function CountEdge(props: EdgeProps<CountEdgeType>) {
  const { id, sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition, data } = props;
  const [path] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition });

  const ref = useCallback(
    (el: SVGPathElement | null) => {
      if (el) edgePathRegistry.set(id, el);
      else edgePathRegistry.delete(id);
    },
    [id],
  );

  if (!data) return null;
  const cls = ["react-flow__edge-path", "flow-edge"];
  if (data.n === 0) cls.push("faint");
  if (data.direct) cls.push("direct");
  if (data.on) cls.push("on");
  const strokeWidth = data.n > 0 ? 1 + 7 * Math.sqrt(data.n / Math.max(1, data.maxN)) : undefined;
  const dataKey = JSON.stringify([data.fromCol, data.fromName, data.toCol, data.toName]);

  return (
    <path
      ref={ref}
      id={id}
      className={cls.join(" ")}
      d={path}
      fill="none"
      style={strokeWidth !== undefined ? { strokeWidth } : undefined}
      data-key={dataKey}
      data-n={data.n}
    >
      <title>{`${data.fromTip} → ${data.toTip}: ${data.n} ${data.toUnit}`}</title>
    </path>
  );
}
