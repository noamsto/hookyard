// Entry point: mounts the flow view into #flow-panel, wires the controller to
// app.js's hookyard:sync / hookyard:call events, and owns the feed|flow
// toggle (view=flow in the URL).
import "./flow.css";
import { activeFilters, eventLabel, filterParams, syncState } from "./bridge.ts";
import { FlowController } from "./model/controller.ts";
import type { SyncDetail } from "./model/controller.ts";
import type { Entry } from "./types.ts";
import { FlowView } from "./view/FlowView.ts";

async function fetchJSON(url: string): Promise<unknown> {
  const resp = await fetch(url, { credentials: "same-origin" });
  if (!resp.ok) throw new Error(url + ": " + resp.status);
  return resp.json();
}

const controller = new FlowController({
  fetchJSON,
  filterParams,
  activeFilters,
  eventLabel,
  now: () => Date.now(),
  setTimeout: (fn, ms) => window.setTimeout(fn, ms),
  clearTimeout: (id) => window.clearTimeout(id),
});

const flowPanel = document.getElementById("flow-panel");
if (flowPanel) new FlowView(flowPanel, controller);

const feedPanel = document.querySelector<HTMLElement>(".feed-panel");
const viewButtons = document.querySelectorAll<HTMLButtonElement>(".view-toggle [role=tab]");

function setView(v: string): void {
  const flow = v === "flow";
  if (feedPanel) feedPanel.hidden = flow;
  if (flowPanel) flowPanel.hidden = !flow;
  for (const btn of viewButtons) btn.setAttribute("aria-selected", String(btn.dataset.view === v));

  const params = new URLSearchParams(location.search);
  if (flow) params.set("view", "flow");
  else params.delete("view");
  const qs = params.toString();
  history.replaceState(null, "", qs ? "?" + qs : location.pathname);

  void controller.setVisible(flow);
}

for (const btn of viewButtons) btn.addEventListener("click", () => setView(btn.dataset.view ?? "feed"));

document.addEventListener("hookyard:sync", (ev) => {
  void controller.sync((ev as CustomEvent<SyncDetail>).detail);
});
document.addEventListener("hookyard:call", (ev) => {
  controller.onCall((ev as CustomEvent<Entry>).detail);
});

// app.js can finish its init chain and emit hookyard:sync before this bundle
// has evaluated far enough to have added the listener above — reliably so on
// a past day, whose init has no SSE round trip to wait on. flow.js always
// imports app.js, so by the time we get here app.js has already run and
// remembers its last sync; catch up on it directly.
const missedSync = syncState();
if (missedSync) void controller.sync(missedSync);

setView(new URLSearchParams(location.search).get("view") === "flow" ? "flow" : "feed");
