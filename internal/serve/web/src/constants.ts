// Ported from #93's flow.js. NODE_W/NODE_H/GROUP_PAD_TOP are ELK layout
// sizes, not the old SVG's hand-tuned pixel geometry.
import type { Col, DisplayCol } from "./types.ts";

export const WINDOW_MIN = 10;
export const MAX_DOTS = 48;
export const RENDER_MS = 500;
export const DOT_MS = 900;
export const PENDING_CAP = 5000;
export const RELAYOUT_MS = 5000;
export const PRESS_HOLD_MAX_MS = 10000;

export const NODE_W = 220;
export const NODE_H = 34;
export const GROUP_PAD_TOP = 34;

export const OUTCOMES = [
  "allow", "deny", "ask", "advise", "abstain",
  "dispatched", "suppressed", "timeout", "error",
];
export const ROUTER_ERROR = "router-error"; // Go's outcomeRouterError

export const COL_HEADERS: Record<Col, string> = {
  engine: "engine · calls",
  event: "event · calls",
  handler: "handler · branches",
  outcome: "handler outcome · branches",
};
export const COL_TITLES: Record<DisplayCol, string> = {
  engine: "engine", event: "event", handler: "handler", group: "handler group", outcome: "handler outcome",
};
