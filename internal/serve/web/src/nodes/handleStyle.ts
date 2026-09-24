import type { CSSProperties } from "react";

// Handles are never used for connecting (nodesConnectable={false}); they
// exist only so React Flow can compute edge attachment points on the node's
// left/right sides.
export const HIDDEN_HANDLE_STYLE: CSSProperties = { visibility: "hidden" };
