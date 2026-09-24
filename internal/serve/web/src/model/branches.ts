import { ROUTER_ERROR } from "../constants.ts";
import type { PathLike } from "../types.ts";
import type { Branch } from "./keys.ts";

// app.js's eventLabel in the browser; injected so the model never imports it.
export type EventLabel = (p: PathLike) => string;

// pathBranches is the one place the engine -> event -> handler -> outcome
// edges are derived, shared by /api/flow's aggregate paths and live records
// (the wire uses the same field names for both).
export function pathBranches(p: PathLike, eventLabel: EventLabel): Branch[] {
  const engine = p.engine;
  const ev = eventLabel(p);
  if (p.router === "error") {
    return [{ nodes: [["engine", engine], ["event", ev], ["outcome", ROUTER_ERROR]] }];
  }
  const handlers = p.handlers ?? [];
  if (handlers.length === 0) {
    return [{ nodes: [["engine", engine], ["event", ev], ["outcome", p.verdict]] }];
  }
  return handlers.map((h) => ({
    nodes: [["engine", engine], ["event", ev], ["handler", h.name], ["outcome", h.outcome]],
  }));
}
