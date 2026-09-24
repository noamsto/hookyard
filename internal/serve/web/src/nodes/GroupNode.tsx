// A collapsible handler group. Click detection on the header is done by the
// caller (FlowCanvas.onNodeClick), which checks the click target against
// .flow-group-header — a click on the container body (between expanded
// members) does nothing.
import { Handle, Position } from "@xyflow/react";
import type { Node, NodeProps } from "@xyflow/react";
import { fmtCount } from "../format.ts";
import { HIDDEN_HANDLE_STYLE } from "./handleStyle.ts";

export interface GroupData extends Record<string, unknown> {
  dataName: string; // data-name: the bare group label — the harness keys on it
  tipName: string; // "guards.* (8)" — label with the member count, for display
  members: string[];
  n: number;
  expanded: boolean;
  forced: boolean;
  on: boolean;
}

// The RF type is "handlerGroup", not "group" — "group" is React Flow's own
// reserved built-in node type and carries default CSS (padding, a fixed
// width, centred text) that would otherwise leak onto this node's wrapper.
export type GroupNodeType = Node<GroupData, "handlerGroup">;

export function GroupNode({ data }: NodeProps<GroupNodeType>) {
  // Not .flow-node: that class is a flex row that vertically centers its
  // content, which would center the header within the whole group's height
  // instead of pinning it to the top.
  const cls = ["flow-group"];
  if (data.n === 0) cls.push("dim");
  if (data.on) cls.push("on");
  if (data.forced) cls.push("forced");

  return (
    <div
      className={cls.join(" ")}
      data-col="group"
      data-name={data.dataName}
      data-n={data.n}
      data-members={JSON.stringify(data.members)}
      data-expanded={data.expanded}
      data-forced={data.forced}
    >
      <Handle type="target" position={Position.Left} style={HIDDEN_HANDLE_STYLE} />
      <div className="flow-group-header">
        <span className="flow-group-arrow" aria-hidden="true">{data.expanded ? "▾" : "▸"}</span>
        <span className="flow-node-label">{data.tipName}</span>
        <span className="flow-node-count">{fmtCount(data.n)}</span>
        {data.forced && <span className="flow-group-pin" aria-hidden="true" />}
      </div>
      <Handle type="source" position={Position.Right} style={HIDDEN_HANDLE_STYLE} />
    </div>
  );
}
