// hookyard serve flow view. It imports app.js's one filter serialization,
// event-label rule and filter mutator, and listens for app.js's
// hookyard:sync/hookyard:call events. Same safety rule as app.js: engine,
// event, handler and outcome names are operator-controlled and reach the DOM
// only through textContent or a data-* attribute, and a class name derived
// from an outcome comes only from the known outcome list.

import { filterParams, eventLabel, toggleFilter, activeFilters } from "./app.js";

const SVG_NS = "http://www.w3.org/2000/svg";

const WINDOW_MIN = 10;
const MAX_DOTS = 48;
const RENDER_MS = 500;
const DOT_MS = 900;
const PENDING_CAP = 5000;
const RELAYOUT_MS = 5000;

const COL_PITCH = 300;
const NODE_W = 210;
const NODE_H = 26;
const ROW_GAP = 14;
const HEADER_H = 36;
const MEMBER_INDENT = 14;
const LABEL_PAD = 8;
const CHAR_EM = 0.6; // advance width of the monospace font, in em

const K_MIN = 0.2;
const K_MAX = 4;
const FIT_MAX = 1.5;
const FIT_PAD = 24;
const DRAG_PX = 4;
const KEY_ZOOM = 1.25;

const COLUMNS = ["engine", "event", "handler", "outcome"];
const COL_INDEX = { engine: 0, event: 1, handler: 2, group: 2, outcome: 3 };
const COL_HEADERS = {
  engine: "engine · calls",
  event: "event · calls",
  handler: "handler · branches",
  outcome: "handler outcome · branches",
};
const COL_TITLES = {
  engine: "engine", event: "event", handler: "handler", group: "handler group", outcome: "handler outcome",
};

const OUTCOMES = [
  "allow", "deny", "ask", "advise", "abstain",
  "dispatched", "suppressed", "timeout", "error",
];
const ROUTER_ERROR = "router-error"; // Go's outcomeRouterError

// ---------- DOM ----------

const feedPanel = document.querySelector(".feed-panel");
const flowPanel = document.getElementById("flow-panel");
const flowBodyEl = document.getElementById("flow-body");
const flowMetaEl = document.getElementById("flow-meta");
const flowErrorEl = document.getElementById("flow-error");
const flowIdleEl = document.getElementById("flow-idle");
const flowIdleCountEl = document.getElementById("flow-idle-count");
const flowFitEl = document.getElementById("flow-fit");
const viewButtons = document.querySelectorAll(".view-toggle [role=tab]");

const svg = document.createElementNS(SVG_NS, "svg");
svg.setAttribute("role", "img");
svg.setAttribute("aria-label", "flow graph");
const viewportG = document.createElementNS(SVG_NS, "g");
viewportG.setAttribute("class", "viewport");
svg.appendChild(viewportG);
const tipEl = document.createElement("div");
tipEl.className = "flow-tip";
tipEl.hidden = true;
flowBodyEl.append(svg, tipEl);

// ---------- state ----------

let tableCache = null;
let tableError = "";
let fetchError = "";
let topo = null; // built by buildTopology()
let observedEdges = new Map(); // raw edge key -> [[col, name], [col, name]]

// paths: pathKey -> { path: {engine,canonical_event,native_event,router,verdict,handlers}, counts: number[] }
let paths = new Map();
let ring = null; // { start, bucketMs, n } for the live 10-minute window; null for a static whole-day day

let day = null;
let live = false;
let visible = false;
let stale = false;

let cursor = -1; // dedupe cursor: a call at offset <= cursor was already counted
let inflight = false;
let pending = [];
let gen = 0;
let dropped = 0;

// Per load: the filters its fetch used, the column facets, and the nodes
// shown because they got live traffic since.
let active = { engine: [], event: [], handler: [], outcome: [], verdict: [], session: "" };
let facets = null;
let stickyShown = emptyColumnSets();
let showIdle = false;
// rawTotals: per-node drawn counts from paths, keyed by the node's own (col,
// name) rather than the display node — refreshed once per layout(), read by
// isBusy so a node carrying traffic is never hidden by a facet-key mismatch.
let rawTotals = new Map();

let groups = { groupOf: new Map(), members: new Map() };
const userExpanded = new Map(); // group key -> explicit user toggle, overrides auto-expand
let forced = new Set(); // group keys holding an active handler filter value
// expandedAtLayout: which groups were expanded as of the last layout() call.
// isExpanded() is live (tracks isShown/stickyShown), but the SVG only
// changes on layout(); display mapping must stay pinned to what's drawn.
let expandedAtLayout = new Set();
let order = { engine: [], event: [], handler: [], outcome: [] }; // handler: unit keys
let memberOrder = new Map(); // expanded group node key -> member node keys
let orderDirty = true;
let bbox = { x: 0, y: 0, w: 0, h: 0 };

const view = { k: 1, tx: 0, ty: 0 };
let fontSize = 0;
let fitted = false;

let layoutDirty = true;
let relayoutTimer = null;
let lastLayoutAt = -Infinity;
let nodeEls = new Map(); // display node key -> node record (see makeNode)
let edgeEls = new Map(); // display edge key -> { el, titleEl, length, from, to, n }
let headerEls = [];
const keyOfEl = new WeakMap(); // node g -> display node key
let hoveredKey;
let dots = []; // fixed pool: { el, busy }
const activeDots = []; // { dot, edges, start, perEdgeMs }
const queue = new Map(); // branchKey -> { branch, n } — coalesced per animation frame

let rafId = null;
let renderTimer = null;
let lastRenderAt = 0;

const pointers = new Map(); // pointerId -> { x, y } while pressed
let drag = null; // { x, y, tx, ty, moved, downNode }
let pinch = null; // { dist, k, tx, ty, mx, my }

function emptyColumnSets() {
  return { engine: new Set(), event: new Set(), handler: new Set(), outcome: new Set() };
}

// ---------- key helpers ----------

function nodeKey(col, name) {
  return col + "\x00" + name;
}

function splitKey(key) {
  const i = key.indexOf("\x00");
  return [key.slice(0, i), key.slice(i + 1)];
}

function edgeKey(aKey, bKey) {
  return aKey + "\x01" + bKey;
}

function consecutivePairs(nodes) {
  const pairs = [];
  for (let i = 0; i < nodes.length - 1; i++) pairs.push([nodes[i], nodes[i + 1]]);
  return pairs;
}

// pathKey groups a path or live record by the same tuple /api/flow groups by,
// so live calls merge into the aggregate they belong to.
function pathKey(p) {
  const hh = (p.handlers || []).map((h) => h.name + "\x02" + h.outcome).join("\x03");
  return [p.engine, p.canonical_event, p.native_event, p.router, p.verdict, hh].join("\x01");
}

// ---------- the one edge rule ----------

// pathBranches is the one place the engine -> event -> handler -> outcome
// edges are derived, shared by the aggregate FlowPaths from /api/flow and
// live records from hookyard:call. p is path-shaped: {engine, canonical_event,
// native_event, router, verdict, handlers}, true for both a FlowPath (JSON)
// and a live rec — the wire uses the same field names as the record.
function pathBranches(p) {
  const engine = p.engine;
  const ev = eventLabel(p);
  if (p.router === "error") {
    return [{ nodes: [["engine", engine], ["event", ev], ["outcome", ROUTER_ERROR]] }];
  }
  const handlers = p.handlers || [];
  if (handlers.length === 0) {
    return [{ nodes: [["engine", engine], ["event", ev], ["outcome", p.verdict]] }];
  }
  return handlers.map((h) => ({
    nodes: [["engine", engine], ["event", ev], ["handler", h.name], ["outcome", h.outcome]],
  }));
}

// ---------- groups (D8.1) ----------

function parentKey(k) {
  const dot = k.lastIndexOf(".");
  if (dot !== -1) return k.slice(0, dot);
  const dash = k.indexOf("-");
  return dash > 0 ? k.slice(0, dash) : null;
}

function keyDepth(k) {
  let d = 0;
  for (let p = k; p !== null; p = parentKey(p)) d++;
  return d;
}

// computeGroups bubbles singleton keys up to a fixed point, deepest first:
// moving every singleton at once would let houston.state (key houston) and
// houston.state.observe (key houston.state) pass each other and both end
// ungrouped.
function computeGroups(ids) {
  const keyOf = new Map();
  for (const id of ids) keyOf.set(id, parentKey(id));
  for (;;) {
    const byKey = new Map();
    for (const [id, k] of keyOf) {
      if (k === null) continue;
      if (!byKey.has(k)) byKey.set(k, []);
      byKey.get(k).push(id);
    }
    let deepest = 0;
    for (const [k, held] of byKey) if (held.length === 1) deepest = Math.max(deepest, keyDepth(k));
    if (deepest === 0) break;
    for (const [k, held] of byKey) {
      if (held.length === 1 && keyDepth(k) === deepest) keyOf.set(held[0], parentKey(k));
    }
  }
  const groupOf = new Map();
  const members = new Map();
  for (const id of ids) {
    const k = keyOf.get(id);
    if (k === null) continue;
    groupOf.set(id, k);
    if (!members.has(k)) members.set(k, []);
    members.get(k).push(id);
  }
  return { groupOf, members };
}

function groupLabel(key) {
  const dash = groups.members.get(key).every((id) => id.startsWith(key + "-"));
  return key + (dash ? "-*" : ".*");
}

// isExpanded: forced (active handler filter in the group) wins outright; an
// explicit user click wins next; otherwise auto-expand under a branch filter
// (handler/outcome) when the group is small enough to not be noise.
function isExpanded(key) {
  if (forced.has(key)) return true;
  if (userExpanded.has(key)) return userExpanded.get(key);
  if (!active.handler.length && !active.outcome.length) return false;
  const shown = (groups.members.get(key) || []).filter((m) => isShown("handler", m));
  return shown.length <= 4;
}

// displayNode maps a raw branch node to the node that draws it: a handler
// inside a collapsed group lands on the group. Reads expandedAtLayout (not
// the live isExpanded) so counting/pulses/hover stay pinned to the drawn SVG.
function displayNode(col, name) {
  if (col === "handler") {
    const key = groups.groupOf.get(name);
    if (key !== undefined && !expandedAtLayout.has(key)) return ["group", key];
  }
  return [col, name];
}

function displayKey(col, name) {
  const [c, n] = displayNode(col, name);
  return nodeKey(c, n);
}

function displayBranches(p) {
  return pathBranches(p).map((b) => b.nodes.map(([col, name]) => displayKey(col, name)));
}

// ---------- topology ----------

async function fetchTable() {
  try {
    return await fetchJSON("/api/table");
  } catch (err) {
    return { handlers: [], engines: [], events: [], error: String(err && err.message || err) };
  }
}

// buildTopology rebuilds the fixed skeleton from the handler table and
// resets the "observed beyond skeleton" extras — ghost handlers, engine-
// scoped events not named by any handler, unusual outcomes — so a day/filter
// switch doesn't carry stale ghosts from the previous load() cycle forward.
function buildTopology() {
  const table = tableCache || {};
  const handlers = table.handlers || [];

  const engineOrder = (table.engines || []).slice();

  const eventOrder = (table.events || []).slice();
  const eventSeen = new Set(eventOrder);
  for (const h of handlers) {
    for (const e of h.events || []) {
      if (e.indexOf(":") !== -1 && !eventSeen.has(e)) {
        eventSeen.add(e);
        eventOrder.push(e);
      }
    }
  }

  const handlerOrder = handlers.map((h) => h.id);
  const handlerSet = new Set(handlerOrder);

  const outcomeOrder = OUTCOMES.concat([ROUTER_ERROR]);

  const skeletonEdges = new Map();
  const addSkeleton = (a, b) => skeletonEdges.set(edgeKey(nodeKey(a[0], a[1]), nodeKey(b[0], b[1])), [a, b]);
  for (const h of handlers) {
    for (const e of h.events || []) {
      addSkeleton(["event", e], ["handler", h.id]);
      const colon = e.indexOf(":");
      for (const g of h.engines || []) {
        if (colon === -1 || e.slice(0, colon) === g) addSkeleton(["engine", g], ["event", e]);
      }
    }
  }

  topo = {
    engineOrder, eventOrder, handlerOrder, handlerSet, outcomeOrder,
    skeletonEdges,
    extra: { engine: [], event: [], handler: [], outcome: [] },
    extraSeen: {
      engine: new Set(engineOrder),
      event: new Set(eventOrder),
      handler: new Set(handlerOrder),
      outcome: new Set(outcomeOrder),
    },
  };
  observedEdges = new Map();
  layoutDirty = true;
}

function observeNode(col, name) {
  if (topo.extraSeen[col].has(name)) return false;
  topo.extraSeen[col].add(name);
  topo.extra[col].push(name);
  return true;
}

// observePath registers a path's nodes and edges into the topology (so
// layout() has something to draw) and returns its branches so callers don't
// have to recompute them.
function observePath(path) {
  const branches = pathBranches(path);
  let changed = false;
  for (const b of branches) {
    for (const [col, name] of b.nodes) {
      if (observeNode(col, name)) changed = true;
    }
    for (const [a, c] of consecutivePairs(b.nodes)) {
      const k = edgeKey(nodeKey(a[0], a[1]), nodeKey(c[0], c[1]));
      if (!observedEdges.has(k)) {
        observedEdges.set(k, [a, c]);
        changed = true;
      }
    }
  }
  if (changed) requestRelayout();
  return branches;
}

function columnNames(col) {
  const base = col === "engine" ? topo.engineOrder
    : col === "event" ? topo.eventOrder
    : col === "handler" ? topo.handlerOrder
    : topo.outcomeOrder;
  return base.concat(topo.extra[col]);
}

function updateErrorBanner() {
  flowErrorEl.textContent = fetchError || tableError || "";
}

async function fetchJSON(url) {
  const resp = await fetch(url, { credentials: "same-origin" });
  if (!resp.ok) throw new Error(url + ": " + resp.status);
  return resp.json();
}

// ---------- visibility (D8.2) ----------

// isBusy: shown without the idle toggle. rawTotals catches nodes whose facet
// key disagrees with the drawn label (e.g. a router-error record with empty
// native_event: Go's event facet skips the "" label, app.js draws it as "—").
function isBusy(col, name) {
  return (facets[col][name] || 0) > 0 || active[col].includes(name) || stickyShown[col].has(name)
    || (rawTotals.get(nodeKey(col, name)) || 0) > 0;
}

function isShown(col, name) {
  return showIdle || isBusy(col, name);
}

function facetOf(key) {
  const [col, name] = splitKey(key);
  if (col !== "group") return facets[col][name] || 0;
  let n = 0;
  for (const id of groups.members.get(name)) n += facets.handler[id] || 0;
  return n;
}

// ---------- live ring + calls ----------

// advanceRing shifts every path's bucket array left by the number of whole
// minutes that have elapsed since the ring's last bucket, zero-filling the
// new buckets at the end. Returns whether it moved.
function advanceRing(nowMs) {
  if (!ring) return false;
  const curBucketStart = Math.floor(nowMs / ring.bucketMs) * ring.bucketMs;
  const lastBucketStart = ring.start + (ring.n - 1) * ring.bucketMs;
  let shift = Math.round((curBucketStart - lastBucketStart) / ring.bucketMs);
  if (shift <= 0) return false;
  if (shift > ring.n) shift = ring.n;
  for (const entry of paths.values()) {
    entry.counts.splice(0, shift);
    while (entry.counts.length < ring.n) entry.counts.push(0);
  }
  ring.start += shift * ring.bucketMs;
  return true;
}

// showUnlaidOut marks each node of the call's branches that isn't laid out
// as shown until the next load (D10), and returns the branches that can
// pulse now.
function showUnlaidOut(branches) {
  const ready = [];
  for (const b of branches) {
    let laidOut = true;
    for (const [col, name] of b.nodes) {
      if (nodeEls.has(displayKey(col, name))) continue;
      laidOut = false;
      stickyShown[col].add(name);
      requestRelayout();
    }
    if (laidOut) ready.push(b);
  }
  return ready;
}

function addCall(entry) {
  if (ring) advanceRing(Date.now());
  if (entry.offset <= cursor) return;
  cursor = entry.offset;

  // Under a handler/outcome filter only the hit branches are drawn, the same
  // pruning /api/flow applies, so the path merges into the server's.
  const rec = entry.rec;
  const hops = entry.hits ? entry.hits.map((i) => rec.handlers[i]) : (rec.handlers || []);
  const path = {
    engine: rec.engine, canonical_event: rec.canonical_event, native_event: rec.native_event,
    router: rec.router, verdict: rec.verdict, handlers: hops,
  };
  const key = pathKey(path);
  let node = paths.get(key);
  if (!node) {
    node = { path, counts: new Array(ring ? ring.n : 1).fill(0) };
    paths.set(key, node);
  }

  if (ring) {
    const ts = Date.parse(rec.ts);
    if (!isNaN(ts)) {
      let idx = Math.floor((ts - ring.start) / ring.bucketMs);
      if (idx >= ring.n) idx = ring.n - 1;
      if (idx >= 0) node.counts[idx]++;
    }
  } else {
    node.counts[0]++;
  }

  const branches = showUnlaidOut(observePath(node.path));
  if (visible) queueAnimation(branches);
  render();
}

function onCall(entry) {
  if (stale) return; // a sync arrived while hidden or the buffer overflowed — wait for the next load()
  if (!live || entry.day !== day) return;
  if (inflight) {
    if (pending.length < PENDING_CAP) {
      pending.push(entry);
    } else {
      stale = true; // drop and mark stale so the next show/sync refetches
    }
    return;
  }
  addCall(entry);
}

// ---------- sync / load ----------

function sync(detail) {
  day = detail.day;
  live = detail.live;
  if (!visible) {
    stale = true;
    return;
  }
  load();
}

async function load() {
  gen += 1;
  const g = gen;
  inflight = true;
  pending = []; // every call buffered before this load's fetch is already inside its scan

  const params = filterParams();
  const filters = activeFilters();
  params.set("day", day);
  if (live) params.set("window", String(WINDOW_MIN));

  let table, resp;
  try {
    [table, resp] = await Promise.all([fetchTable(), fetchJSON("/api/flow?" + params.toString())]);
  } catch (err) {
    if (g === gen) {
      fetchError = "flow: " + (err && err.message ? err.message : String(err));
      updateErrorBanner();
      inflight = false;
      paths = new Map();
      ring = null;
      pending = [];
      stale = true; // next show/sync refetches; don't replay into stale state
      render();
    }
    return;
  }
  if (g !== gen) return; // a newer load() superseded this one — discard

  tableCache = table;
  tableError = table.error || "";
  buildTopology();

  fetchError = "";
  updateErrorBanner();

  active = filters;
  facets = resp.facets;
  stickyShown = emptyColumnSets();
  for (const col of COLUMNS) {
    for (const [name, n] of Object.entries(facets[col])) if (n > 0) observeNode(col, name);
    for (const name of active[col]) observeNode(col, name);
  }

  paths = new Map();
  for (const p of resp.paths) {
    paths.set(pathKey(p), { path: p, counts: p.counts.slice() });
    observePath(p);
  }
  cursor = resp.next_offset;
  ring = resp.window > 0 ? { start: resp.bucket_start, bucketMs: resp.bucket_ms, n: resp.window } : null;
  dropped = 0;
  inflight = false;

  const buffered = pending;
  pending = [];
  for (const entry of buffered) {
    if (entry.day === day && live) addCall(entry);
  }

  if (stale && visible) {
    stale = false;
    load();
    return;
  }

  layoutDirty = true;
  orderDirty = true;
  layout();
  paint();
}

// ---------- view toggle ----------

function setView(v) {
  const flow = v === "flow";
  feedPanel.hidden = flow;
  flowPanel.hidden = !flow;
  for (const btn of viewButtons) btn.setAttribute("aria-selected", String(btn.dataset.view === v));

  const params = new URLSearchParams(location.search);
  if (flow) params.set("view", "flow");
  else params.delete("view");
  const qs = params.toString();
  history.replaceState(null, "", qs ? "?" + qs : location.pathname);

  visible = flow;
  if (!flow) {
    cancelAllDots();
    clearHover();
    return;
  }
  if (!fitted) fit(); // the first layout may have run while the panel was hidden
  if (stale) {
    stale = false;
    load();
  } else {
    render();
  }
}

// ---------- layout (rebuilds the SVG; only when the node/edge set changes) ----------

// requestRelayout coalesces live-driven re-layouts to at most one per
// RELAYOUT_MS; they keep the existing order (D8.4).
function requestRelayout() {
  if (relayoutTimer !== null) return;
  const wait = Math.max(0, RELAYOUT_MS - (performance.now() - lastLayoutAt));
  relayoutTimer = setTimeout(() => {
    relayoutTimer = null;
    layoutDirty = true;
    render();
  }, wait);
}

function displayLabel(col, name) {
  return col === "outcome" && name === ROUTER_ERROR ? "router error" : name;
}

function outcomeClass(name) {
  if (name === ROUTER_ERROR) return "v-router-error";
  return OUTCOMES.includes(name) ? "v-" + name : "";
}

function unitOf(col) {
  return col === "engine" || col === "event" ? "calls" : "branches";
}

// arrange orders one column's keys: on a load or toggle by the traffic-
// weighted barycenter of their predecessors (unweighted ones after, by facet),
// otherwise the previous order with new keys appended.
function arrange(keys, prev, incomingOf, yOf) {
  if (!orderDirty) {
    const present = new Set(keys);
    const kept = prev.filter((k) => present.has(k));
    const keptSet = new Set(kept);
    return kept.concat(keys.filter((k) => !keptSet.has(k)));
  }
  const idx = new Map(keys.map((k, i) => [k, i]));
  const bary = new Map();
  for (const k of keys) {
    let w = 0;
    let wy = 0;
    for (const [from, n] of incomingOf(k)) {
      const y = yOf.get(from);
      if (y === undefined) continue;
      w += n;
      wy += n * y;
    }
    if (w > 0) bary.set(k, wy / w);
  }
  return keys.slice().sort((a, b) => {
    const baryA = bary.get(a);
    const baryB = bary.get(b);
    if (baryA !== undefined && baryB !== undefined) return baryA - baryB || idx.get(a) - idx.get(b);
    if (baryA !== undefined) return -1;
    if (baryB !== undefined) return 1;
    return facetOf(b) - facetOf(a) || idx.get(a) - idx.get(b);
  });
}

function layout() {
  if (topo === null) return;
  // In-flight dots hold references to the old edge paths.
  cancelAllDots();
  clearHover();
  clearTimeout(relayoutTimer);
  relayoutTimer = null;
  rawTotals = computeRawTotals();

  const handlerIds = columnNames("handler");
  groups = computeGroups(handlerIds);
  forced = new Set();
  for (const id of active.handler) {
    const key = groups.groupOf.get(id);
    if (key !== undefined) forced.add(key);
  }
  expandedAtLayout = new Set();
  for (const key of groups.members.keys()) {
    if (isExpanded(key)) expandedAtLayout.add(key);
  }

  const units = { engine: [], event: [], handler: [], outcome: [] };
  for (const col of ["engine", "event", "outcome"]) {
    for (const name of columnNames(col)) if (isShown(col, name)) units[col].push(nodeKey(col, name));
  }
  const blocks = new Map(); // expanded group node key -> shown member node keys
  for (const id of handlerIds) {
    const key = groups.groupOf.get(id);
    if (key === undefined) {
      if (isShown("handler", id)) units.handler.push(nodeKey("handler", id));
      continue;
    }
    const gk = nodeKey("group", key);
    if (units.handler.includes(gk)) continue;
    const shownMembers = groups.members.get(key).filter((m) => isShown("handler", m));
    if (shownMembers.length === 0) continue;
    units.handler.push(gk);
    if (expandedAtLayout.has(key)) blocks.set(gk, shownMembers.map((m) => nodeKey("handler", m)));
  }

  const totals = computeTotals();
  const incoming = new Map();
  for (const [key, n] of totals.edgeTotals) {
    const sep = key.indexOf("\x01");
    const to = key.slice(sep + 1);
    if (!incoming.has(to)) incoming.set(to, []);
    incoming.get(to).push([key.slice(0, sep), n]);
  }
  const incomingOf = (k) => {
    if (!blocks.has(k)) return incoming.get(k) || [];
    return blocks.get(k).flatMap((m) => incoming.get(m) || []);
  };

  let handlerRows = units.handler.length;
  for (const members of blocks.values()) handlerRows += members.length;
  const rows = [units.engine.length, units.event.length, handlerRows, units.outcome.length];
  const colHeight = (r) => Math.max(0, r * (NODE_H + ROW_GAP) - ROW_GAP);
  const maxH = colHeight(Math.max(1, ...rows));

  const yOf = new Map(); // node key -> centre y, for the next column's barycenters
  const placed = { engine: [], event: [], handler: [], outcome: [] };
  const nextMemberOrder = new Map();
  for (const col of COLUMNS) {
    let keys;
    if (col === "engine") {
      keys = units.engine; // engines keep table order
    } else if (col === "handler") {
      keys = [];
      for (const u of arrange(units.handler, order.handler, incomingOf, yOf)) {
        keys.push(u);
        if (!blocks.has(u)) continue;
        const members = arrange(blocks.get(u), memberOrder.get(u) || [], incomingOf, yOf);
        nextMemberOrder.set(u, members);
        keys.push(...members);
      }
    } else {
      keys = arrange(units[col], order[col], incomingOf, yOf);
    }
    const top = HEADER_H + (maxH - colHeight(keys.length)) / 2;
    keys.forEach((k, i) => yOf.set(k, top + i * (NODE_H + ROW_GAP) + NODE_H / 2));
    placed[col] = keys;
  }
  order = {
    engine: placed.engine,
    event: placed.event,
    handler: placed.handler.filter((k) => units.handler.includes(k)),
    outcome: placed.outcome,
  };
  memberOrder = nextMemberOrder;

  const headersG = document.createElementNS(SVG_NS, "g");
  headersG.setAttribute("class", "col-headers");
  const edgesGroup = document.createElementNS(SVG_NS, "g");
  edgesGroup.setAttribute("class", "edges");
  const nodesGroup = document.createElementNS(SVG_NS, "g");
  nodesGroup.setAttribute("class", "nodes");
  const dotsGroup = document.createElementNS(SVG_NS, "g");
  dotsGroup.setAttribute("class", "dots");

  headerEls = [];
  for (const col of COLUMNS) {
    const text = document.createElementNS(SVG_NS, "text");
    text.setAttribute("class", "col-header");
    text.setAttribute("x", String(COL_INDEX[col] * COL_PITCH + LABEL_PAD));
    text.setAttribute("y", String(HEADER_H / 2));
    headersG.appendChild(text);
    headerEls.push({ labelEl: text, label: COL_HEADERS[col], w: COL_PITCH, countLen: 0 });
  }

  const memberKeys = new Set();
  for (const members of blocks.values()) for (const m of members) memberKeys.add(m);
  nodeEls = new Map();
  for (const col of COLUMNS) {
    for (const key of placed[col]) {
      const member = memberKeys.has(key);
      const x = COL_INDEX[col] * COL_PITCH + (member ? MEMBER_INDENT : 0);
      const node = makeNode(key, x, yOf.get(key) - NODE_H / 2, NODE_W - (member ? MEMBER_INDENT : 0), blocks.get(key));
      if (member) node.g.classList.add("member");
      nodesGroup.appendChild(node.g);
      nodeEls.set(key, node);
    }
  }

  edgeEls = new Map();
  for (const [a, b] of [...topo.skeletonEdges.values(), ...observedEdges.values()]) {
    const aKey = displayKey(a[0], a[1]);
    const bKey = displayKey(b[0], b[1]);
    const key = edgeKey(aKey, bKey);
    if (edgeEls.has(key)) continue;
    const from = nodeEls.get(aKey);
    const to = nodeEls.get(bKey);
    if (!from || !to) continue;

    const x1 = from.x + from.w, y1 = from.y + from.h / 2;
    const x2 = to.x, y2 = to.y + to.h / 2;
    const dx = Math.max(30, (x2 - x1) / 2);
    const d = "M " + x1 + " " + y1 + " C " + (x1 + dx) + " " + y1 + ", " + (x2 - dx) + " " + y2 + ", " + x2 + " " + y2;

    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    let cls = "edge";
    if (from.col === "event" && to.col === "outcome" && to.name !== ROUTER_ERROR) cls += " direct";
    path.setAttribute("class", cls);
    path.setAttribute("data-key", JSON.stringify([from.col, from.dataName, to.col, to.dataName]));
    path.setAttribute("data-n", "0");

    const title = document.createElementNS(SVG_NS, "title");
    path.appendChild(title);
    edgesGroup.appendChild(path);
    edgeEls.set(key, { el: path, titleEl: title, length: path.getTotalLength(), from: aKey, to: bKey, n: 0 });
  }

  dots = [];
  for (let i = 0; i < MAX_DOTS; i++) {
    const c = document.createElementNS(SVG_NS, "circle");
    c.setAttribute("class", "dot");
    c.setAttribute("r", "0");
    dotsGroup.appendChild(c);
    dots.push({ el: c, busy: false });
  }

  viewportG.replaceChildren(headersG, edgesGroup, nodesGroup, dotsGroup);

  let idle = 0;
  for (const col of COLUMNS) for (const name of columnNames(col)) if (!isBusy(col, name)) idle++;
  flowIdleCountEl.textContent = String(idle);

  bbox = { x: 0, y: 0, w: (COLUMNS.length - 1) * COL_PITCH + NODE_W, h: HEADER_H + maxH };
  orderDirty = false;
  layoutDirty = false;
  lastLayoutAt = performance.now();

  fontSize = 0; // the new labels are untruncated
  if (!fitted) fit();
  applyTransform();
}

// makeNode builds one node g. memberKeys is set for an expanded group's
// header, whose count is the sum over its shown members.
function makeNode(key, x, y, w, memberKeys) {
  const [col, name] = splitKey(key);
  const g = document.createElementNS(SVG_NS, "g");
  const cls = ["node", "node-" + (col === "group" ? "handler" : col)];
  let label, tipName, dataName, members;
  if (col === "group") {
    members = groups.members.get(name);
    dataName = groupLabel(name);
    tipName = dataName + " (" + members.length + ")";
    label = (expandedAtLayout.has(name) ? "▾ " : "▸ ") + tipName;
    cls.push("group");
    g.setAttribute("data-members", String(members.length));
  } else {
    dataName = name;
    tipName = label = displayLabel(col, name);
    if (active[col].includes(name)) cls.push("selected");
    if (col === "outcome") {
      const oc = outcomeClass(name);
      if (oc) cls.push(oc);
    }
  }
  const ghost = col === "handler" && !topo.handlerSet.has(name);
  if (ghost) cls.push("ghost");
  g.setAttribute("class", cls.join(" "));
  g.setAttribute("data-col", col);
  g.setAttribute("data-name", dataName);
  g.setAttribute("data-n", "0");

  const rect = document.createElementNS(SVG_NS, "rect");
  rect.setAttribute("x", String(x));
  rect.setAttribute("y", String(y));
  rect.setAttribute("width", String(w));
  rect.setAttribute("height", String(NODE_H));
  rect.setAttribute("rx", "4");
  g.appendChild(rect);

  const labelEl = document.createElementNS(SVG_NS, "text");
  labelEl.setAttribute("class", "label");
  labelEl.setAttribute("x", String(x + LABEL_PAD));
  labelEl.setAttribute("y", String(y + NODE_H / 2));
  g.appendChild(labelEl);

  const countEl = document.createElementNS(SVG_NS, "text");
  countEl.setAttribute("class", "count");
  countEl.setAttribute("x", String(x + w - LABEL_PAD));
  countEl.setAttribute("y", String(y + NODE_H / 2));
  g.appendChild(countEl);

  keyOfEl.set(g, key);
  return { g, labelEl, countEl, col, name, label, tipName, dataName, ghost, members, memberKeys, x, y, w, h: NODE_H, n: 0, countLen: 0 };
}

// ---------- render (throttled to <=2/s; skipped while hidden) ----------

function render() {
  if (!visible || renderTimer !== null) return;
  const now = performance.now();
  const wait = Math.max(0, RENDER_MS - (now - lastRenderAt));
  renderTimer = setTimeout(() => {
    renderTimer = null;
    if (!visible) return;
    if (layoutDirty) layout();
    paint();
  }, wait);
}

function pathTotal(entry) {
  return entry.counts.reduce((a, b) => a + b, 0);
}

// computeRawTotals counts drawn paths onto their own (col, name), not the
// display node — feeds isBusy, so it can't depend on expandedAtLayout (which
// itself depends on isShown/isBusy for group members) without a cycle.
function computeRawTotals() {
  const totals = new Map();
  for (const entry of paths.values()) {
    const n = pathTotal(entry);
    if (n === 0) continue;
    for (const b of pathBranches(entry.path)) {
      for (const [col, name] of b.nodes) {
        const k = nodeKey(col, name);
        totals.set(k, (totals.get(k) || 0) + n);
      }
    }
  }
  return totals;
}

// computeTotals counts the drawn paths onto display nodes and edges (D4):
// engine and event nodes and the engine->event edge count calls, everything
// past the event counts branches. Two branches of one call can share a
// display edge into a collapsed group; both count.
function computeTotals() {
  const edgeTotals = new Map();
  const nodeTotals = new Map();
  const bump = (m, k, n) => m.set(k, (m.get(k) || 0) + n);
  let calls = 0;
  let branches = 0;
  for (const entry of paths.values()) {
    const n = pathTotal(entry);
    calls += n;
    if (n === 0) continue;
    const bs = displayBranches(entry.path);
    branches += n * bs.length;
    const [eng, ev] = bs[0];
    bump(nodeTotals, eng, n);
    bump(nodeTotals, ev, n);
    bump(edgeTotals, edgeKey(eng, ev), n);
    for (const keys of bs) {
      for (let i = 2; i < keys.length; i++) {
        bump(nodeTotals, keys[i], n);
        bump(edgeTotals, edgeKey(keys[i - 1], keys[i]), n);
      }
    }
  }
  return { edgeTotals, nodeTotals, calls, branches };
}

function fmtCount(n) {
  if (n < 1000) return String(n);
  if (n < 1e5) return (n / 1e3).toFixed(1) + "k";
  if (n < 1e6) return Math.round(n / 1e3) + "k";
  return (n / 1e6).toFixed(1) + "M";
}

function paint() {
  if (topo === null) return;
  const t = computeTotals();

  let maxN = 1;
  for (const n of t.edgeTotals.values()) if (n > maxN) maxN = n;

  for (const [key, edge] of edgeEls) {
    const n = t.edgeTotals.get(key) || 0;
    edge.n = n;
    edge.el.classList.toggle("faint", n === 0);
    edge.el.style.strokeWidth = n > 0 ? (1 + 7 * Math.sqrt(n / maxN)).toFixed(2) : "";
    edge.el.setAttribute("data-n", String(n));
    const to = nodeEls.get(edge.to);
    edge.titleEl.textContent = nodeEls.get(edge.from).tipName + " → " + to.tipName + ": " + n + " " + unitOf(to.col);
  }
  for (const [key, node] of nodeEls) {
    const n = node.memberKeys
      ? node.memberKeys.reduce((a, k) => a + (t.nodeTotals.get(k) || 0), 0)
      : t.nodeTotals.get(key) || 0;
    node.n = n;
    node.g.setAttribute("data-n", String(n));
    node.g.classList.toggle("dim", n === 0);
    const text = fmtCount(n);
    node.countEl.textContent = text;
    if (text.length !== node.countLen) {
      node.countLen = text.length;
      fitLabel(node);
    }
  }

  flowMetaEl.textContent = (live
    ? "live · last " + WINDOW_MIN + " min · "
    : (day || "") + " · whole day · ") + t.calls + " calls · " + t.branches + " branches";

  flowBodyEl.dataset.activeDots = String(activeDots.length);
  flowBodyEl.dataset.dropped = String(dropped);
  flowBodyEl.dataset.calls = String(t.calls);
  flowBodyEl.dataset.branches = String(t.branches);

  lastRenderAt = performance.now();
}

// ---------- zoom / pan (D9) ----------

function clamp(v, lo, hi) {
  return Math.min(hi, Math.max(lo, v));
}

function fitLabel(node) {
  const ch = fontSize * CHAR_EM;
  const avail = node.w - 2 * LABEL_PAD - (node.countLen ? (node.countLen + 1) * ch : 0);
  const max = Math.max(1, Math.floor(avail / ch));
  node.labelEl.textContent = node.label.length > max ? node.label.slice(0, max - 1) + "…" : node.label;
}

// applyTransform keeps text at >= 9px on screen: the world font size grows
// as k shrinks, and labels re-truncate to the node width at that size.
function applyTransform() {
  viewportG.setAttribute("transform", "translate(" + view.tx + " " + view.ty + ") scale(" + view.k + ")");
  flowBodyEl.dataset.view = view.k + "," + view.tx + "," + view.ty;
  const fs = Math.round(Math.max(12, 9 / view.k) * 2) / 2;
  if (fs === fontSize) return;
  fontSize = fs;
  viewportG.style.fontSize = fs + "px";
  for (const node of nodeEls.values()) fitLabel(node);
  for (const h of headerEls) fitLabel(h);
}

function zoomAt(px, py, factor) {
  const k = clamp(view.k * factor, K_MIN, K_MAX);
  view.tx = px - (px - view.tx) * (k / view.k);
  view.ty = py - (py - view.ty) * (k / view.k);
  view.k = k;
  applyTransform();
}

function fit() {
  const r = svg.getBoundingClientRect();
  if (r.width === 0 || r.height === 0 || bbox.w === 0) return;
  const k = clamp(Math.min((r.width - 2 * FIT_PAD) / bbox.w, (r.height - 2 * FIT_PAD) / bbox.h), K_MIN, FIT_MAX);
  view.k = k;
  view.tx = (r.width - bbox.w * k) / 2 - bbox.x * k;
  // Top-align, not vertically centred: when content is taller than the
  // viewport the height-bound scale already makes it fit exactly, so this
  // coincides with centring; when shorter, it pins content to the top.
  view.ty = FIT_PAD - bbox.y * k;
  fitted = true;
  applyTransform();
}

function startPinch() {
  const [a, b] = pointers.values();
  const r = svg.getBoundingClientRect();
  pinch = {
    dist: Math.max(1, Math.hypot(a.x - b.x, a.y - b.y)),
    k: view.k, tx: view.tx, ty: view.ty,
    mx: (a.x + b.x) / 2 - r.left, my: (a.y + b.y) / 2 - r.top,
  };
  if (drag) drag.moved = true;
}

function onPointerDown(ev) {
  if (ev.pointerType === "mouse" && ev.button !== 0) return;
  svg.setPointerCapture(ev.pointerId);
  pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
  if (pointers.size === 1) {
    // Under pointer capture pointerup targets the svg, so the node is taken here.
    drag = { x: ev.clientX, y: ev.clientY, tx: view.tx, ty: view.ty, moved: false, downNode: ev.target.closest(".node") };
  } else if (pointers.size === 2) {
    startPinch();
  }
}

function onPointerMove(ev) {
  if (!pointers.has(ev.pointerId)) {
    if (hoveredKey !== undefined) moveTip(ev);
    return;
  }
  pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
  if (pinch && pointers.size >= 2) {
    const [a, b] = pointers.values();
    const r = svg.getBoundingClientRect();
    const k = clamp(pinch.k * Math.hypot(a.x - b.x, a.y - b.y) / pinch.dist, K_MIN, K_MAX);
    // The world point under the starting midpoint follows the current one.
    view.tx = (a.x + b.x) / 2 - r.left - (pinch.mx - pinch.tx) * (k / pinch.k);
    view.ty = (a.y + b.y) / 2 - r.top - (pinch.my - pinch.ty) * (k / pinch.k);
    view.k = k;
    applyTransform();
    return;
  }
  if (!drag) return;
  const dx = ev.clientX - drag.x;
  const dy = ev.clientY - drag.y;
  if (!drag.moved) {
    if (Math.hypot(dx, dy) < DRAG_PX) return;
    drag.moved = true;
    flowBodyEl.classList.add("dragging");
    clearHover();
  }
  view.tx = drag.tx + dx;
  view.ty = drag.ty + dy;
  applyTransform();
}

function onPointerEnd(ev) {
  if (!pointers.delete(ev.pointerId)) return;
  if (pinch && pointers.size < 2) {
    pinch = null;
    if (pointers.size === 1) {
      const [p] = pointers.values();
      drag = { x: p.x, y: p.y, tx: view.tx, ty: view.ty, moved: true, downNode: null };
    }
  }
  if (pointers.size > 0) return;
  const d = drag;
  drag = null;
  flowBodyEl.classList.remove("dragging");
  if (d && !d.moved && d.downNode && ev.type === "pointerup") onNodeClick(d.downNode, ev);
}

function onWheel(ev) {
  ev.preventDefault();
  const r = svg.getBoundingClientRect();
  const dy = ev.deltaMode === 1 ? ev.deltaY * 16 : ev.deltaY; // Firefox reports mouse wheels in lines
  zoomAt(ev.clientX - r.left, ev.clientY - r.top, Math.exp(-dy * 0.0015));
}

function onKey(ev) {
  if (!visible || ev.ctrlKey || ev.metaKey || ev.altKey) return;
  const tag = document.activeElement ? document.activeElement.tagName : "";
  if (tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA") return;
  const r = svg.getBoundingClientRect();
  if (ev.key === "+" || ev.key === "=") zoomAt(r.width / 2, r.height / 2, KEY_ZOOM);
  else if (ev.key === "-") zoomAt(r.width / 2, r.height / 2, 1 / KEY_ZOOM);
  else if (ev.key === "0") fit();
  else return;
  ev.preventDefault();
}

// ---------- node interaction (D7) ----------

function onNodeClick(g, ev) {
  const key = keyOfEl.get(g);
  const [col, name] = splitKey(key);
  if (col === "group") {
    userExpanded.set(name, !isExpanded(name));
    orderDirty = true;
    layout();
    paint();
    return;
  }
  toggleFilter(col, name, ev.shiftKey || ev.ctrlKey || ev.metaKey);
}

function onPointerOver(ev) {
  if (drag) return;
  const g = ev.target.closest(".node");
  const key = g ? keyOfEl.get(g) : undefined;
  if (key === hoveredKey) return;
  clearHover();
  if (key !== undefined && nodeEls.has(key)) highlight(key, ev);
}

// highlight keeps every drawn branch through the node (upstream prefix and
// downstream suffix) at full opacity and dims the rest.
function highlight(key, ev) {
  hoveredKey = key;
  const node = nodeEls.get(key);
  const targets = new Set(node.memberKeys || [key]);
  const onNodes = new Set([key]);
  const onEdges = new Set();
  for (const entry of paths.values()) {
    if (pathTotal(entry) === 0) continue;
    for (const keys of displayBranches(entry.path)) {
      if (!keys.some((k) => targets.has(k))) continue;
      for (const k of keys) onNodes.add(k);
      for (let i = 1; i < keys.length; i++) onEdges.add(edgeKey(keys[i - 1], keys[i]));
    }
  }
  for (const k of onNodes) {
    const n = nodeEls.get(k);
    if (n) n.g.classList.add("on");
  }
  for (const k of onEdges) {
    const e = edgeEls.get(k);
    if (e) e.el.classList.add("on");
  }
  svg.classList.add("hl");
  showTip(node, ev);
}

function clearHover() {
  if (hoveredKey === undefined) return;
  hoveredKey = undefined;
  for (const el of svg.querySelectorAll(".on")) el.classList.remove("on");
  svg.classList.remove("hl");
  tipEl.hidden = true;
}

function memberCounts(members) {
  const counts = new Map(members.map((id) => [id, 0]));
  for (const entry of paths.values()) {
    const n = pathTotal(entry);
    if (n === 0) continue;
    for (const b of pathBranches(entry.path)) {
      const h = b.nodes[2];
      if (h[0] === "handler" && counts.has(h[1])) counts.set(h[1], counts.get(h[1]) + n);
    }
  }
  return [...counts].sort((a, b) => b[1] - a[1]);
}

function showTip(node, ev) {
  const key = hoveredKey;
  const lines = [[node.tipName + " — " + COL_TITLES[node.col], "flow-tip-title"]];
  lines.push([node.n + " " + unitOf(node.col) + (node.ghost ? " · not in the installed table" : ""), ""]);
  const outs = [];
  const ins = [];
  for (const e of edgeEls.values()) {
    if (e.n === 0) continue;
    if (e.from === key) outs.push(e);
    else if (e.to === key) ins.push(e);
  }
  const top = (list) => list.sort((a, b) => b.n - a.n).slice(0, 4);
  for (const e of top(outs)) lines.push(["→ " + nodeEls.get(e.to).tipName + "  " + e.n, ""]);
  for (const e of top(ins)) lines.push(["← " + nodeEls.get(e.from).tipName + "  " + e.n, ""]);
  if (node.members) {
    for (const [id, n] of memberCounts(node.members)) lines.push(["  " + id + "  " + n, ""]);
  }
  lines.push([node.col === "group" ? "click: expand/collapse" : "click: filter · shift-click: add", "dim"]);

  tipEl.textContent = "";
  for (const [text, cls] of lines) {
    const div = document.createElement("div");
    if (cls) div.className = cls;
    div.textContent = text;
    tipEl.appendChild(div);
  }
  tipEl.hidden = false;
  moveTip(ev);
}

function moveTip(ev) {
  const r = flowBodyEl.getBoundingClientRect();
  const x = ev.clientX - r.left;
  const y = ev.clientY - r.top;
  const w = tipEl.offsetWidth;
  const h = tipEl.offsetHeight;
  tipEl.style.left = (x + 14 + w > r.width ? Math.max(0, x - 14 - w) : x + 14) + "px";
  tipEl.style.top = (y + 14 + h > r.height ? Math.max(0, y - 14 - h) : y + 14) + "px";
}

// ---------- animation (live only, view visible, fixed dot pool) ----------

function canAnimate() {
  return visible && !document.hidden;
}

function branchKey(b) {
  return b.nodes.map(([c, n]) => c + "\x00" + n).join("\x01");
}

function queueAnimation(branches) {
  if (!canAnimate()) return; // nothing is scheduled while hidden — the queue just isn't grown
  for (const b of branches) {
    const key = branchKey(b);
    let item = queue.get(key);
    if (!item) {
      item = { branch: b, n: 0 };
      queue.set(key, item);
    }
    item.n++;
  }
  requestTick();
}

function claimDot() {
  for (const d of dots) if (!d.busy) return d;
  return null;
}

function freeDot(dot) {
  dot.busy = false;
  dot.el.setAttribute("r", "0");
}

function flushQueue(now) {
  const items = Array.from(queue.values());
  queue.clear();
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    const dot = claimDot();
    if (!dot) {
      for (let j = i; j < items.length; j++) dropped += items[j].n;
      break;
    }
    const nodes = item.branch.nodes.map(([col, name]) => displayKey(col, name));
    const edges = [];
    for (const [a, b] of consecutivePairs(nodes)) {
      const e = edgeEls.get(edgeKey(a, b));
      if (e) edges.push(e);
    }
    if (edges.length === 0) {
      freeDot(dot); // topology hasn't caught up to this brand-new branch yet — skip this frame
      continue;
    }
    dot.busy = true;
    const radius = 3 + Math.min(4, Math.log2(Math.max(1, item.n)));
    dot.el.setAttribute("r", String(radius));
    activeDots.push({ dot, edges, start: now, perEdgeMs: DOT_MS / edges.length });
  }
}

function tick(now) {
  rafId = null;
  flushQueue(now);
  for (let i = activeDots.length - 1; i >= 0; i--) {
    const st = activeDots[i];
    const elapsed = now - st.start;
    if (elapsed >= DOT_MS) {
      freeDot(st.dot);
      activeDots.splice(i, 1);
      continue;
    }
    const edgeIdx = Math.min(st.edges.length - 1, Math.floor(elapsed / st.perEdgeMs));
    const edgeElapsed = elapsed - edgeIdx * st.perEdgeMs;
    const t = Math.min(1, edgeElapsed / st.perEdgeMs);
    const edge = st.edges[edgeIdx];
    const pt = edge.el.getPointAtLength(t * edge.length);
    st.dot.el.setAttribute("cx", String(pt.x));
    st.dot.el.setAttribute("cy", String(pt.y));
  }
  if (canAnimate() && (activeDots.length > 0 || queue.size > 0)) rafId = requestAnimationFrame(tick);
}

function requestTick() {
  if (rafId !== null || !canAnimate()) return;
  rafId = requestAnimationFrame(tick);
}

function cancelAllDots() {
  if (rafId !== null) {
    cancelAnimationFrame(rafId);
    rafId = null;
  }
  for (const st of activeDots) freeDot(st.dot);
  activeDots.length = 0;
  queue.clear();
}

// ---------- wiring ----------

for (const btn of viewButtons) btn.addEventListener("click", () => setView(btn.dataset.view));

flowIdleEl.addEventListener("change", () => {
  showIdle = flowIdleEl.checked;
  orderDirty = true;
  layout();
  paint();
});
flowFitEl.addEventListener("click", () => fit());

svg.addEventListener("wheel", onWheel, { passive: false });
svg.addEventListener("pointerdown", onPointerDown);
svg.addEventListener("pointermove", onPointerMove);
svg.addEventListener("pointerup", onPointerEnd);
svg.addEventListener("pointercancel", onPointerEnd);
svg.addEventListener("pointerover", onPointerOver);
svg.addEventListener("pointerleave", clearHover);
document.addEventListener("keydown", onKey);

document.addEventListener("hookyard:sync", (ev) => sync(ev.detail));
document.addEventListener("hookyard:call", (ev) => onCall(ev.detail));
document.addEventListener("visibilitychange", () => {
  if (document.hidden) cancelAllDots();
});

setInterval(() => {
  if (ring && advanceRing(Date.now())) render();
}, 1000);

// The first hookyard:sync loads the topology; until then layout() and
// paint() are no-ops (#95).
setView(new URLSearchParams(location.search).get("view") === "flow" ? "flow" : "feed");
