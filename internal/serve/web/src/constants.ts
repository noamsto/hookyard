import type { DisplayCol } from "./types.ts";

export const WINDOW_MIN = 10;
export const MAX_DOTS = 96;
export const RENDER_MS = 500;
export const PENDING_CAP = 5000;
export const RELAYOUT_MS = 5000;
export const PRESS_HOLD_MAX_MS = 10000;

export const OUTCOMES = [
  "allow", "deny", "ask", "advise", "abstain",
  "dispatched", "suppressed", "timeout", "error",
];
export const ROUTER_ERROR = "router-error"; // Go's outcomeRouterError

// Most consequential first. The first seven are decisions (drawn loud); the
// rest are no-op traffic (drawn quiet).
export const SEVERITY = [
  ROUTER_ERROR, "error", "timeout", "deny", "ask", "advise", "allow",
  "suppressed", "dispatched", "abstain",
];
export const LOUD = new Set(SEVERITY.slice(0, 7));

export const COL_TITLES: Record<DisplayCol, string> = {
  engine: "engine", event: "event", handler: "handler", group: "handler group", outcome: "outcome",
};
