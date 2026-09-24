// A non-interactive column header ("engine · calls", …). No handles: no
// edge ever attaches to a header.
import type { Node, NodeProps } from "@xyflow/react";

export interface HeaderData extends Record<string, unknown> {
  title: string;
}

export type HeaderNodeType = Node<HeaderData, "colHeader">;

export function HeaderNode({ data }: NodeProps<HeaderNodeType>) {
  return <div className="flow-col-header">{data.title}</div>;
}
