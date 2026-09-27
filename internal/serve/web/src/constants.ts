import type { DisplayCol } from "./types.ts";

export const WINDOW_MIN = 10;
export const MAX_DOTS = 128;
export const RENDER_MS = 500;
export const PENDING_CAP = 5000;
export const RELAYOUT_MS = 5000;
export const PRESS_HOLD_MAX_MS = 10000;

export const HOP_MS = 1280; // per hop: engine -> event -> handler -> outcome
export const LOUD_HOP_MS = 1550; // a decision takes its time — see the derivation below
// LOUD_HOP_MS is tuned so a single-branch, loud-outcome pulse (the common
// case — most decision outcomes are loud, see LOUD below) takes about 5s
// end to end: 3*LOUD_HOP_MS + FAN_DELAY_MS + BRIDGE_MS + jitter(0-160) ≈
// 4990-5150ms. Quiet (no-op) traffic stays proportionally faster via HOP_MS
// (same ~950:1150 ratio the two constants had before this retune).
export const FAN_DELAY_MS = 200; // the dot crosses its event plate, then fans out
export const TRAIL: [number, number] = [0.035, 0.09]; // trail segment ends, as leg fractions behind the dot
export const BRIDGE_MS = 140; // the plate-crossing ease: how long a leg-boundary transition holds the new leg's clock while blending across the node's own width

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
