// A leaf display node (engine/event/handler/outcome). Operator-controlled
// strings (dataName, tipName) reach the DOM only as React text or data-*
// attribute values; the outcome class comes from Snapshot's outcomeClass
// (the known OUTCOMES list), never from the raw string directly.
import { Handle, Position } from "@xyflow/react";
import type { Node, NodeProps } from "@xyflow/react";
import { fmtCount } from "../format.ts";
import { HIDDEN_HANDLE_STYLE } from "./handleStyle.ts";

export interface CardData extends Record<string, unknown> {
  col: string;
  dataName: string;
  tipName: string;
  n: number;
  ghost: boolean;
  selected: boolean;
  outcomeCls: string;
  on: boolean;
}

export type CardNodeType = Node<CardData, "card">;

export function CardNode({ data }: NodeProps<CardNodeType>) {
  const cls = ["flow-node"];
  if (data.ghost) cls.push("ghost");
  if (data.selected) cls.push("selected");
  if (data.outcomeCls) cls.push(data.outcomeCls);
  if (data.n === 0) cls.push("dim");
  if (data.on) cls.push("on");

  return (
    <div className={cls.join(" ")} data-col={data.col} data-name={data.dataName} data-n={data.n}>
      <Handle type="target" position={Position.Left} style={HIDDEN_HANDLE_STYLE} />
      <span className="flow-node-label">{data.tipName}</span>
      <span className="flow-node-count">{fmtCount(data.n)}</span>
      <Handle type="source" position={Position.Right} style={HIDDEN_HANDLE_STYLE} />
    </div>
  );
}
