// hookyard serve flow view. A separate ES module from app.js — it imports
// only filterParams/eventLabel (the one filter serialization and the one
// event-label rule) and listens for app.js's hookyard:sync/hookyard:call
// CustomEvents. Same safety rule as app.js: every piece of record-derived
// text (engine, event, handler, outcome names are operator-controlled) goes
// through textContent, never innerHTML. Class names derived from an outcome
// are restricted to the known outcome list (verdictClass) rather than
// sanitized by regex, so an unrecognised value never reaches a class
// attribute at all.

import { filterParams, eventLabel } from "./app.js";

const SVG_NS = "http://www.w3.org/2000/svg";

const WINDOW_MIN = 10;
const MAX_DOTS = 48;
const RENDER_MS = 500;
const DOT_MS = 900;
const PENDING_CAP = 5000;

const NODE_W = 180;
const NODE_H = 20;
const GAP = 6;
const PADDING = 10;
const TRUNCATE_AT = 24;

const COLUMNS = ["engine", "event", "handler", "verdict"];
const COL_X = { engine: 0.01, event: 0.26, handler: 0.51, verdict: 0.79 };

const OUTCOMES = [
  "allow", "deny", "ask", "advise", "abstain",
  "dispatched", "suppressed", "timeout", "error",
];
const ROUTER_ERROR = "router error"; // the ⟂ node

// ---------- DOM ----------

const feedPanel = document.querySelector(".feed-panel");
const flowPanel = document.getElementById("flow-panel");
const flowBodyEl = document.getElementById("flow-body");
const flowMetaEl = document.getElementById("flow-meta");
const flowErrorEl = document.getElementById("flow-error");
const viewButtons = document.querySelectorAll(".view-toggle [role=tab]");

// ---------- state ----------

let tableCache = null;
let tableError = "";
let fetchError = "";
let topo = null; // built by buildTopology()
let observedEdges = new Set();

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

let layoutDirty = true;
let nodeEls = new Map(); // nodeKey -> { g, x, y, w, h }
let edgeEls = new Map(); // edgeKey -> { el, titleEl, length, from, to }
let dots = []; // fixed pool: { el, busy }
const activeDots = []; // { dot, edges, start, perEdgeMs }
const queue = new Map(); // branchKey -> { branch, n } — coalesced per animation frame

let rafId = null;
let renderTimer = null;
let lastRenderAt = 0;

// ---------- key helpers ----------

function nodeKey(col, name) {
  return col + "\x00" + name;
}

function keyName(key) {
  return key.slice(key.indexOf("\x00") + 1);
}

function edgeKey(colA, nameA, colB, nameB) {
  return nodeKey(colA, nameA) + "\x01" + nodeKey(colB, nameB);
}

function edgeKeyFromPair(a, b) {
  return nodeKey(a[0], a[1]) + "\x01" + nodeKey(b[0], b[1]);
}

function consecutivePairs(nodes) {
  const pairs = [];
  for (let i = 0; i < nodes.length - 1; i++) pairs.push([nodes[i], nodes[i + 1]]);
  return pairs;
}

// pathKey groups a path/record the same way the server does: engine,
// canonical_event, native_event, router, verdict, then each handler's
// name+outcome in record order. Used only to merge the server's aggregate
// with live calls client-side — it need not match the server's own key
// format, only be internally consistent.
function pathKey(p) {
  const hh = (p.handlers || []).map((h) => h.name + "\x02" + h.outcome).join("\x03");
  return [p.engine, p.canonical_event, p.native_event, p.router, p.verdict, hh].join("\x01");
}

// ---------- the one edge rule ----------

// pathBranches is the one place the engine -> event -> handler -> verdict
// edges are derived, shared by the aggregate FlowPaths from /api/flow and
// live records from hookyard:call. p is path-shaped: {engine, canonical_event,
// native_event, router, verdict, handlers}, true for both a FlowPath (JSON)
// and a live rec — the wire uses the same field names as the record.
function pathBranches(p) {
  const engine = p.engine;
  const ev = eventLabel(p);
  if (p.router === "error") {
    return [{ nodes: [["engine", engine], ["event", ev], ["verdict", ROUTER_ERROR]] }];
  }
  const handlers = p.handlers || [];
  if (handlers.length === 0) {
    return [{ nodes: [["engine", engine], ["event", ev], ["verdict", p.verdict]] }];
  }
  return handlers.map((h) => ({
    nodes: [["engine", engine], ["event", ev], ["handler", h.name], ["verdict", h.outcome]],
  }));
}

// ---------- topology ----------

async function fetchTable() {
  try {
    return await fetchJSON("/api/table");
  } catch (err) {
    return { handlers: [], engines: [], events: [], error: String(err && err.message || err) };
  }
}

// refreshTable is init()'s one-shot table load, before any load() has run.
// Guarded by gen so a late response can't clobber a load() that started (and
// fetched its own, newer table) while this was in flight.
async function refreshTable() {
  const startGen = gen;
  const table = await fetchTable();
  if (gen !== startGen) return;
  tableCache = table;
  tableError = table.error || "";
  buildTopology();
  updateErrorBanner();
}

// buildTopology rebuilds the fixed skeleton from the handler table and
// resets the "observed beyond skeleton" extras — ghost handlers, engine-
// scoped events not named by any handler, unusual verdicts — so a day/filter
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

  const verdictOrder = OUTCOMES.concat([ROUTER_ERROR]);
  const verdictSet = new Set(verdictOrder);

  const skeletonEdges = new Set();
  for (const h of handlers) {
    for (const e of h.events || []) {
      skeletonEdges.add(edgeKey("event", e, "handler", h.id));
      const colon = e.indexOf(":");
      for (const g of h.engines || []) {
        if (colon === -1 || e.slice(0, colon) === g) {
          skeletonEdges.add(edgeKey("engine", g, "event", e));
        }
      }
    }
  }

  topo = {
    engineOrder, eventOrder, handlerOrder, handlerSet, verdictOrder, verdictSet,
    skeletonEdges,
    extra: { engine: [], event: [], handler: [], verdict: [] },
    extraSeen: {
      engine: new Set(engineOrder),
      event: new Set(eventOrder),
      handler: new Set(handlerOrder),
      verdict: new Set(verdictOrder),
    },
  };
  observedEdges = new Set();
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
    for (const pair of consecutivePairs(b.nodes)) {
      const k = edgeKeyFromPair(pair[0], pair[1]);
      if (!observedEdges.has(k)) {
        observedEdges.add(k);
        changed = true;
      }
    }
  }
  if (changed) layoutDirty = true;
  return branches;
}

function columnNames(col) {
  const base = col === "engine" ? topo.engineOrder
    : col === "event" ? topo.eventOrder
    : col === "handler" ? topo.handlerOrder
    : topo.verdictOrder;
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

function addCall(entry) {
  if (ring) advanceRing(Date.now());
  if (entry.offset <= cursor) return;
  cursor = entry.offset;

  const rec = entry.rec;
  const key = pathKey(rec);
  let node = paths.get(key);
  if (!node) {
    node = {
      path: {
        engine: rec.engine, canonical_event: rec.canonical_event, native_event: rec.native_event,
        router: rec.router, verdict: rec.verdict, handlers: rec.handlers || [],
      },
      counts: new Array(ring ? ring.n : 1).fill(0),
    };
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

  const branches = observePath(node.path);
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
  params.set("day", day);
  if (live) params.set("window", String(WINDOW_MIN));

  // /api/table and /api/flow are independent — fetch them concurrently.
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

  paths = new Map();
  for (const p of resp.paths) {
    paths.set(pathKey(p), { path: p, counts: (p.counts || []).slice() });
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
  layout();
  render();
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
    return;
  }
  if (stale) {
    stale = false;
    load();
  } else {
    render();
  }
}

// ---------- layout (rebuilds the SVG; only when the node/edge set changes) ----------

function truncateLabel(name) {
  return name.length > TRUNCATE_AT ? name.slice(0, TRUNCATE_AT - 1) + "…" : name;
}

function verdictClass(name) {
  if (name === ROUTER_ERROR) return "v-router-error";
  return OUTCOMES.includes(name) ? "v-" + name : "";
}

function layout() {
  // In-flight dots hold references to the old edge paths.
  cancelAllDots();

  const cols = {};
  for (const col of COLUMNS) cols[col] = columnNames(col);
  const rows = Math.max(1, ...COLUMNS.map((c) => cols[c].length));
  const width = 1000;
  const height = PADDING * 2 + rows * (NODE_H + GAP) - GAP;

  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 " + width + " " + height);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "flow graph");

  const edgesGroup = document.createElementNS(SVG_NS, "g");
  edgesGroup.setAttribute("class", "edges");
  const nodesGroup = document.createElementNS(SVG_NS, "g");
  nodesGroup.setAttribute("class", "nodes");
  const dotsGroup = document.createElementNS(SVG_NS, "g");
  dotsGroup.setAttribute("class", "dots");

  nodeEls = new Map();
  for (const col of COLUMNS) {
    const names = cols[col];
    const x = COL_X[col] * width;
    names.forEach((name, i) => {
      const y = PADDING + i * (NODE_H + GAP);
      const ghost = col === "handler" && !topo.handlerSet.has(name);

      const g = document.createElementNS(SVG_NS, "g");
      let cls = "node node-" + col;
      if (ghost) cls += " ghost";
      if (col === "verdict") {
        const vc = verdictClass(name);
        if (vc) cls += " " + vc;
      }
      g.setAttribute("class", cls);

      const rect = document.createElementNS(SVG_NS, "rect");
      rect.setAttribute("x", String(x));
      rect.setAttribute("y", String(y));
      rect.setAttribute("width", String(NODE_W));
      rect.setAttribute("height", String(NODE_H));
      rect.setAttribute("rx", "4");
      g.appendChild(rect);

      const text = document.createElementNS(SVG_NS, "text");
      text.setAttribute("x", String(x + 6));
      text.setAttribute("y", String(y + NODE_H / 2 + 4));
      text.textContent = truncateLabel(name);
      g.appendChild(text);

      let titleText = "";
      if (ghost) titleText = name + " — not in the installed table";
      else if (name.length > TRUNCATE_AT) titleText = name;
      if (titleText) {
        const title = document.createElementNS(SVG_NS, "title");
        title.textContent = titleText;
        g.appendChild(title);
      }

      nodesGroup.appendChild(g);
      nodeEls.set(nodeKey(col, name), { g, x, y, w: NODE_W, h: NODE_H });
    });
  }

  edgeEls = new Map();
  const allEdgeKeys = new Set([...topo.skeletonEdges, ...observedEdges]);
  for (const key of allEdgeKeys) {
    const sep = key.indexOf("\x01");
    const aKey = key.slice(0, sep);
    const bKey = key.slice(sep + 1);
    const a = nodeEls.get(aKey);
    const b = nodeEls.get(bKey);
    if (!a || !b) continue;

    const aCol = aKey.slice(0, aKey.indexOf("\x00"));
    const bCol = bKey.slice(0, bKey.indexOf("\x00"));
    const bName = keyName(bKey);

    const x1 = a.x + a.w, y1 = a.y + a.h / 2;
    const x2 = b.x, y2 = b.y + b.h / 2;
    const dx = Math.max(30, (x2 - x1) / 2);
    const d = "M " + x1 + " " + y1 + " C " + (x1 + dx) + " " + y1 + ", " + (x2 - dx) + " " + y2 + ", " + x2 + " " + y2;

    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    let cls = "edge";
    if (aCol === "event" && bCol === "verdict" && bName !== ROUTER_ERROR) cls += " direct";
    path.setAttribute("class", cls);

    const title = document.createElementNS(SVG_NS, "title");
    path.appendChild(title);
    edgesGroup.appendChild(path);
    edgeEls.set(key, { el: path, titleEl: title, length: path.getTotalLength(), from: aKey, to: bKey });
  }

  dots = [];
  for (let i = 0; i < MAX_DOTS; i++) {
    const c = document.createElementNS(SVG_NS, "circle");
    c.setAttribute("class", "dot");
    c.setAttribute("r", "0");
    dotsGroup.appendChild(c);
    dots.push({ el: c, busy: false });
  }

  svg.appendChild(edgesGroup);
  svg.appendChild(nodesGroup);
  svg.appendChild(dotsGroup);

  flowBodyEl.textContent = "";
  flowBodyEl.appendChild(svg);
  layoutDirty = false;
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

function paint() {
  const edgeTotals = new Map();
  let calls = 0;
  for (const entry of paths.values()) {
    const n = entry.counts.reduce((a, b) => a + b, 0);
    calls += n;
    if (n === 0) continue;
    // The engine->event edge is shared by every handler branch of the same
    // call and must be counted once per call, not once per branch.
    const branches = pathBranches(entry.path);
    const keys = new Set();
    for (const b of branches) {
      for (const pair of consecutivePairs(b.nodes)) keys.add(edgeKeyFromPair(pair[0], pair[1]));
    }
    for (const k of keys) edgeTotals.set(k, (edgeTotals.get(k) || 0) + n);
  }

  let maxN = 1;
  for (const n of edgeTotals.values()) if (n > maxN) maxN = n;

  const liveNodes = new Set();
  for (const [key, edge] of edgeEls) {
    const n = edgeTotals.get(key) || 0;
    edge.el.classList.toggle("faint", n === 0);
    edge.el.style.strokeWidth = n > 0 ? (1 + 7 * Math.sqrt(n / maxN)).toFixed(2) : "";
    edge.titleEl.textContent = keyName(edge.from) + " → " + keyName(edge.to) + ": " + n + " calls";
    if (n > 0) {
      liveNodes.add(edge.from);
      liveNodes.add(edge.to);
    }
  }
  for (const [key, node] of nodeEls) node.g.classList.toggle("idle", !liveNodes.has(key));

  flowMetaEl.textContent = live
    ? "live · last " + WINDOW_MIN + " min · " + calls + " calls"
    : (day || "") + " · whole day · " + calls + " calls";

  flowBodyEl.dataset.activeDots = String(activeDots.length);
  flowBodyEl.dataset.dropped = String(dropped);
  flowBodyEl.dataset.calls = String(calls);

  lastRenderAt = performance.now();
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
    const edges = [];
    for (const pair of consecutivePairs(item.branch.nodes)) {
      const e = edgeEls.get(edgeKeyFromPair(pair[0], pair[1]));
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

document.addEventListener("hookyard:sync", (ev) => sync(ev.detail));
document.addEventListener("hookyard:call", (ev) => onCall(ev.detail));
document.addEventListener("visibilitychange", () => {
  if (document.hidden) cancelAllDots();
});

setInterval(() => {
  if (ring && advanceRing(Date.now())) render();
}, 1000);

(async function init() {
  await refreshTable();
  const initialView = new URLSearchParams(location.search).get("view") === "flow" ? "flow" : "feed";
  setView(initialView);
})();
