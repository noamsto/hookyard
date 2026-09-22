// hookyard serve frontend. No framework, no build step, no network fetch
// beyond this origin (SPEC 4.8) — this file is exactly what the browser
// loads. All record-derived text goes through textContent/createElement,
// never innerHTML: cwd, tool names and handler messages are operator input
// hookyard does not sanitize, and this page is the last stop before a DOM.

// DOM row cap (PLAN step 8): a JS constant because no Go code can see it.
const MAX_ROWS = 2000;

const VERDICT_OPTIONS = [
  "allow", "deny", "ask", "advise", "abstain",
  "error", "timeout", "suppressed", "dispatched", "ok",
];

const FILTER_SELECTS = { engine: "f-engine", event: "f-event", handler: "f-handler", verdict: "f-verdict" };

const el = (id) => document.getElementById(id);
const feedEl = el("feed");
const daySelect = el("day-select");
const connEl = el("conn");
const stateDirEl = el("state-dir");
const feedCountEl = el("feed-count");
const feedWindowedEl = el("feed-windowed");
const loadOlderBtn = el("load-older");
const statsDayEl = el("stats-day");
const statsBodyEl = el("stats-body");
const tableBodyEl = el("table-body");
const sessionInput = el("f-session");
const filtersForm = el("filters");

const rowTemplate = el("row-template");
const chipTemplate = el("handler-chip-template");
const handlerDetailTemplate = el("handler-detail-template");
const tableRowTemplate = el("table-row-template");
const dividerTemplate = el("divider-template");

// state.day is the day currently displayed; state.today is what the server
// most recently reported as "today" (SPEC 4.4a — the client always sends an
// explicit day, never "today" implicitly, after its first load).
const state = { day: null, today: null };

let rowCount = 0;
let es = null;
let tableCache = null;
let lastStats = null;

// oldestOffset is the "load older" cursor (SPEC 4.4: `before=<offset>` pages
// older, oldest-shown-record first) for the day currently rendered.
// windowedFlag mirrors the most recent page's EventsResponse.Windowed, so the
// indicator stays accurate as older pages are appended below the first.
let oldestOffset = null;
let windowedFlag = false;

// pagingOlder flips true on the first "load older" click for the current
// view. See trimFeed: it is what keeps the MAX_ROWS cap from deleting the
// rows the button just appended.
let pagingOlder = false;

// backfillDay/nextOffset are the connect sequence's dedupe cursor (SPEC 4.4):
// a `call` frame for backfillDay at or below nextOffset was already rendered
// by the backfill fetch and must be dropped, not re-shown.
let backfillDay = null;
let nextOffset = 0;

// A `reset`'s backfill fetch is async; `call` frames that arrive on the same
// stream while it is in flight are buffered and replayed once the backfill's
// cursor is known, rather than raced against it.
let backfilling = false;
let pendingCalls = [];

async function fetchJSON(url) {
  const resp = await fetch(url, { credentials: "same-origin" });
  if (!resp.ok) throw new Error(url + ": " + resp.status);
  return resp.json();
}

function debounce(fn, ms) {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}

// ---------- filters ----------

function currentFilters() {
  const f = { engine: [], event: [], handler: [], verdict: [], session: sessionInput.value.trim() };
  for (const [key, id] of Object.entries(FILTER_SELECTS)) {
    f[key] = Array.from(el(id).selectedOptions).map((o) => o.value);
  }
  return f;
}

// filterParams is the one place a Filter becomes a query string, shared by
// the SSE URL, the events fetch and the URL bar mirror.
function filterParams() {
  const f = currentFilters();
  const params = new URLSearchParams();
  for (const key of ["engine", "event", "handler", "verdict"]) {
    for (const v of f[key]) params.append(key, v);
  }
  if (f.session) params.set("session", f.session);
  return params;
}

function applyFiltersFromParams(params) {
  for (const [key, id] of Object.entries(FILTER_SELECTS)) {
    const wanted = new Set(params.getAll(key));
    for (const opt of el(id).options) opt.selected = wanted.has(opt.value);
  }
  sessionInput.value = params.get("session") || "";
}

function updateURL() {
  const params = filterParams();
  if (state.day) params.set("day", state.day);
  const qs = params.toString();
  history.replaceState(null, "", qs ? "?" + qs : location.pathname);
}

function clearFilters() {
  for (const id of Object.values(FILTER_SELECTS)) {
    for (const opt of el(id).options) opt.selected = false;
  }
  sessionInput.value = "";
  onFiltersChanged();
}

function onFiltersChanged() {
  updateURL();
  if (state.day === state.today) {
    // Reopening the stream runs the one connect sequence (reset -> backfill),
    // which is also how the new filters reach /api/events — there is no
    // separate refetch-then-reopen implementation (PLAN step 8).
    connectStream();
  } else {
    loadEvents(state.day);
  }
}

// ---------- connection indicator ----------

function setConnState(s, label) {
  connEl.dataset.state = s;
  connEl.querySelector(".conn-label").textContent = label || s;
}

// ---------- feed rendering ----------

function fmtTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return ts;
  return d.toLocaleTimeString(undefined, { hour12: false });
}

function fmtMS(ms) {
  return ms + "ms";
}

// eventLabel: a record with no canonical_event (pi's per-turn native events,
// per D1 §11.2) would otherwise show its bare native_event, identical to a
// canonical row of the same name (e.g. pi's per-turn "turn_end" next to
// canonical "turn_end"). Falling back to the manifest's own engine-scoped
// spelling — "pi:turn_end" — keeps the two distinguishable in the table.
function eventLabel(rec) {
  if (rec.canonical_event) return rec.canonical_event;
  if (rec.native_event) return rec.engine + ":" + rec.native_event;
  return "—";
}

function buildChip(h) {
  const node = chipTemplate.content.cloneNode(true);
  const chip = node.querySelector(".chip");
  chip.classList.add("badge--" + h.outcome);
  node.querySelector(".chip-name").textContent = h.name;
  node.querySelector(".chip-ms").textContent = fmtMS(h.ms);
  return node;
}

function buildHandlerDetail(h) {
  const node = handlerDetailTemplate.content.cloneNode(true);
  node.querySelector(".hd-name").textContent = h.name;
  const outcome = node.querySelector(".hd-outcome");
  outcome.textContent = h.outcome;
  outcome.className = "hd-outcome badge badge--" + h.outcome;
  node.querySelector(".hd-ms").textContent = fmtMS(h.ms);
  const advice = node.querySelector(".hd-advice");
  if (h.advice) advice.textContent = "advice: " + h.advice;
  const message = node.querySelector(".hd-message");
  if (h.message) message.textContent = "message: " + h.message;
  return node;
}

function toggleRow(li) {
  const main = li.querySelector(".row-main");
  const detail = li.querySelector(".row-detail");
  const expanded = !li.classList.contains("expanded");
  li.classList.toggle("expanded", expanded);
  detail.hidden = !expanded;
  main.setAttribute("aria-expanded", String(expanded));
}

// buildRow renders one Entry. Every piece of record-derived text is set via
// textContent — the record carries operator-controlled strings, and this is
// the one place they meet the DOM.
function buildRow(entry) {
  const rec = entry.rec;
  const node = rowTemplate.content.cloneNode(true);
  const li = node.querySelector(".row");
  const main = node.querySelector(".row-main");

  if (rec.truncated) li.classList.add("truncated");

  node.querySelector(".c-time").textContent = fmtTime(rec.ts);
  node.querySelector(".c-engine").textContent = rec.engine;
  node.querySelector(".c-key").textContent = rec.key;
  node.querySelector(".c-event").textContent = eventLabel(rec);
  node.querySelector(".c-tool").textContent = rec.tool_name || "—";

  const verdictEl = node.querySelector(".c-verdict");
  verdictEl.textContent = rec.verdict;
  verdictEl.className = "c-verdict badge badge--" + rec.verdict;

  node.querySelector(".c-enforced").textContent = rec.enforced ? "E" : "";

  const handlers = rec.handlers || [];
  const chips = node.querySelector(".c-handlers");
  for (const h of handlers) chips.appendChild(buildChip(h));

  node.querySelector(".d-session").textContent = rec.session_id || "—";
  node.querySelector(".d-cwd").textContent = rec.cwd || "—";
  node.querySelector(".d-reason").textContent = rec.reason || "—";

  const detailHandlers = node.querySelector(".d-handlers");
  if (rec.truncated) {
    const note = document.createElement("p");
    note.className = "truncated-note";
    note.textContent = "truncated: this record was shortened to fit the record size cap — the handler list above may be incomplete.";
    detailHandlers.appendChild(note);
  }
  for (const h of handlers) detailHandlers.appendChild(buildHandlerDetail(h));

  main.addEventListener("click", () => toggleRow(li));
  main.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" || ev.key === " ") {
      ev.preventDefault();
      toggleRow(li);
    }
  });

  return node;
}

function clearFeed() {
  feedEl.textContent = "";
  rowCount = 0;
  pagingOlder = false;
}

// Once the operator has clicked "load older" for this view, appended history
// is deliberate, so the cap is paused rather than deleting exactly the rows
// the button just added — a naive bottom-trim and a bottom-append are the
// same operation from the DOM's point of view. A fresh render (backfill,
// reset, day switch, filter change) always goes through clearFeed first,
// which resets pagingOlder, so the cap is back in force for the ordinary
// live-tail case.
function trimFeed() {
  if (pagingOlder) return;
  while (rowCount > MAX_ROWS && feedEl.lastElementChild) {
    if (feedEl.lastElementChild.classList.contains("row")) rowCount--;
    feedEl.removeChild(feedEl.lastElementChild);
  }
}

function updateFeedMeta() {
  feedCountEl.textContent = rowCount + (rowCount === 1 ? " row" : " rows");
  feedWindowedEl.hidden = !windowedFlag;
}

// renderFeed replaces the feed with records, which the server already
// returns newest-first (EventsResponse.Records) — appending in that order
// keeps the newest at the top with no extra sort.
function renderFeed(records) {
  clearFeed();
  for (const entry of records) {
    feedEl.appendChild(buildRow(entry));
    rowCount++;
  }
}

function prependRow(entry) {
  feedEl.insertBefore(buildRow(entry), feedEl.firstChild);
  rowCount++;
  trimFeed();
  updateFeedMeta();
}

function setLoadOlder(loadState) {
  // loadState: "ready" | "loading" | "bottom"
  loadOlderBtn.disabled = loadState !== "ready";
  loadOlderBtn.textContent =
    loadState === "bottom" ? "bottom of day" : loadState === "loading" ? "loading…" : "load older";
}

// loadOlder appends one older page below the feed (SPEC 4.4: "'Load older'
// is a button, not infinite scroll"). It is the one implementation used
// whether the day is live or static — it only ever talks to /api/events.
async function loadOlder() {
  if (oldestOffset === null) return;
  setLoadOlder("loading");
  try {
    const params = filterParams();
    params.set("day", state.day);
    params.set("limit", "500");
    params.set("before", String(oldestOffset));
    const resp = await fetchJSON("/api/events?" + params.toString());
    if (resp.records.length === 0) {
      setLoadOlder("bottom"); // the scan reached byte 0 — nothing older exists
      return;
    }
    pagingOlder = true;
    for (const entry of resp.records) {
      feedEl.appendChild(buildRow(entry));
      rowCount++;
    }
    oldestOffset = resp.records[resp.records.length - 1].offset;
    windowedFlag = resp.windowed;
    updateFeedMeta();
    setLoadOlder("ready");
  } catch {
    setLoadOlder("ready"); // transient fetch error — let the operator retry rather than getting stuck disabled
  }
}

function insertDivider(day) {
  const node = dividerTemplate.content.cloneNode(true);
  node.querySelector("span").textContent = day;
  feedEl.insertBefore(node, feedEl.firstChild);
}

// ---------- events / stats / table ----------

async function loadEvents(day) {
  const params = filterParams();
  params.set("day", day);
  params.set("limit", "500");
  const resp = await fetchJSON("/api/events?" + params.toString());
  renderFeed(resp.records);
  backfillDay = resp.day;
  nextOffset = resp.next_offset;
  oldestOffset = resp.records.length ? resp.records[resp.records.length - 1].offset : null;
  windowedFlag = resp.windowed;
  updateFeedMeta();
  setLoadOlder(oldestOffset === null ? "bottom" : "ready");
}

function renderStats(snap) {
  lastStats = snap;
  statsDayEl.textContent = snap.day + " · " + snap.calls + " calls";

  const blocks = [];

  const verdictRows = Object.entries(snap.verdicts || {}).sort((a, b) => b[1].total - a[1].total);
  blocks.push(statBlock("verdict mix", verdictRows.map(([v, c]) =>
    [v, c.total + " (" + c.enforced + " enforced / " + c.unenforced + " un)"])));

  const handlerRows = (snap.handlers || []).slice().sort((a, b) => (b.total_ms / Math.max(b.calls, 1)) - (a.total_ms / Math.max(a.calls, 1)));
  blocks.push(statBlock("slowest handlers", handlerRows.map((h) =>
    [h.name, Math.round(h.total_ms / Math.max(h.calls, 1)) + "ms mean / " + h.max_ms + "ms max / " + h.calls + " calls"])));

  const routerRows = Object.entries(snap.router || {}).sort((a, b) => b[1] - a[1]);
  blocks.push(statBlock("router status", routerRows.map(([status, n]) => [status, String(n)])));

  blocks.push(statBlock("truncated", [["records", String(snap.truncated)]]));

  statsBodyEl.textContent = "";
  for (const b of blocks) statsBodyEl.appendChild(b);

  if (tableCache) renderTable(tableCache, snap);
}

function statBlock(title, rows) {
  const block = document.createElement("div");
  block.className = "stat-block";
  const h3 = document.createElement("h3");
  h3.textContent = title;
  block.appendChild(h3);
  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "dim stat-row";
    empty.textContent = "none yet";
    block.appendChild(empty);
  }
  for (const [label, value] of rows) {
    const row = document.createElement("div");
    row.className = "stat-row";
    const l = document.createElement("span");
    l.textContent = label;
    const v = document.createElement("span");
    v.className = "n";
    v.textContent = value;
    row.appendChild(l);
    row.appendChild(v);
    block.appendChild(row);
  }
  return block;
}

function renderTable(table, snap) {
  tableBodyEl.textContent = "";
  if (table.error) {
    const err = document.createElement("div");
    err.className = "table-error";
    err.textContent = table.error;
    tableBodyEl.appendChild(err);
    return;
  }
  if (!table.handlers || table.handlers.length === 0) {
    const empty = document.createElement("div");
    empty.className = "table-empty";
    empty.textContent = "nothing installed";
    tableBodyEl.appendChild(empty);
    return;
  }
  // A handler stat only exists once a handler has fired at least once
  // (Accumulator.Add only creates the entry on first sight), so membership
  // in snap.handlers is exactly "has fired today".
  const fired = new Set((snap && snap.handlers || []).map((h) => h.name));
  for (const h of table.handlers) {
    const node = tableRowTemplate.content.cloneNode(true);
    const row = node.querySelector(".table-row");
    if (fired.has(h.id)) row.classList.add("fired");
    node.querySelector(".tr-id").textContent = h.id;
    node.querySelector(".tr-lane").textContent = h.lane || "";
    node.querySelector(".tr-events").textContent = (h.events || []).join(", ");
    tableBodyEl.appendChild(node);
  }
}

async function loadTable() {
  tableCache = await fetchJSON("/api/table");
  populateFilterOptions(tableCache);
  renderTable(tableCache, lastStats);
  // TableResponse.Path is the only place the wire contract carries the
  // resolved state dir (it is table.json's path, one level down from it).
  if (tableCache.path) stateDirEl.textContent = tableCache.path.replace(/\/?table\.json$/, "");
}

function populateFilterOptions(table) {
  fillSelect(el("f-engine"), table.engines || []);
  fillSelect(el("f-event"), table.events || []);
  fillSelect(el("f-handler"), (table.handlers || []).map((h) => h.id));
}

function fillSelect(select, values) {
  const selected = new Set(Array.from(select.selectedOptions).map((o) => o.value));
  select.textContent = "";
  for (const v of values) {
    const opt = document.createElement("option");
    opt.value = v;
    opt.textContent = v;
    opt.selected = selected.has(v);
    select.appendChild(opt);
  }
}

async function loadDays() {
  const resp = await fetchJSON("/api/days");
  state.today = resp.today;
  fillDaySelect(resp.days, state.day);
  return resp;
}

function fillDaySelect(days, selected) {
  daySelect.textContent = "";
  const list = selected && !days.includes(selected) ? [selected, ...days] : days;
  for (const d of list) {
    const opt = document.createElement("option");
    opt.value = d;
    opt.textContent = d === state.today ? d + " (today)" : d;
    opt.selected = d === selected;
    daySelect.appendChild(opt);
  }
}

// ---------- day selection ----------

async function selectDay(day) {
  if (es) {
    es.close();
    es = null;
  }
  state.day = day;
  updateURL();
  await loadEvents(day);
  const snap = await fetchJSON("/api/stats?day=" + encodeURIComponent(day));
  renderStats(snap);
  if (day === state.today) {
    connectStream();
  } else {
    setConnState("static", "static: " + day);
  }
}

// ---------- SSE connect sequence (SPEC 4.4) ----------
//
// One path for the first load, every automatic EventSource reconnect, and a
// server-side restart: open /api/stream, wait for its first frame (always
// `reset`), then backfill via /api/events and remember next_offset. Nothing
// here special-cases which of the three triggered it.

function connectStream() {
  if (es) es.close();
  setConnState("reconnecting", "connecting…");
  es = new EventSource("/api/stream?" + filterParams().toString());
  es.addEventListener("reset", () => { runBackfill(); });
  es.addEventListener("call", (ev) => handleCallFrame(JSON.parse(ev.data)));
  es.addEventListener("stats", (ev) => handleStatsFrame(JSON.parse(ev.data)));
  es.addEventListener("day", (ev) => handleDayFrame(JSON.parse(ev.data)));
  es.onerror = () => setConnState("reconnecting", "reconnecting…");
}

async function runBackfill() {
  backfilling = true;
  pendingCalls = [];
  try {
    await loadDays();
    if (!state.day) state.day = state.today;
    await loadEvents(state.day);
  } finally {
    backfilling = false;
  }
  for (const entry of pendingCalls) applyCallFrame(entry);
  pendingCalls = [];
  const snap = await fetchJSON("/api/stats?day=" + encodeURIComponent(state.day));
  renderStats(snap);
  await loadTable();
  updateURL();
  setConnState("live", "live");
}

function handleCallFrame(entry) {
  if (backfilling) {
    pendingCalls.push(entry);
    return;
  }
  applyCallFrame(entry);
}

function applyCallFrame(entry) {
  if (entry.day !== state.day) return; // stale — a day frame has since moved us on
  if (entry.day === backfillDay && entry.offset <= nextOffset) return; // already rendered by backfill
  prependRow(entry);
}

function handleStatsFrame(snap) {
  if (snap.day === state.day) renderStats(snap);
}

function handleDayFrame(data) {
  insertDivider(data.day);
  state.day = data.day;
  backfillDay = data.day;
  nextOffset = 0;
  pendingCalls = [];
  loadDays();
  loadTable();
  updateURL();
}

// ---------- keyboard ----------

document.addEventListener("keydown", (ev) => {
  const tag = document.activeElement && document.activeElement.tagName;
  const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
  if (ev.key === "/" && !typing) {
    ev.preventDefault();
    sessionInput.focus();
  } else if (ev.key === "Escape") {
    clearFilters();
  }
});

// ---------- wiring ----------

filtersForm.addEventListener("change", onFiltersChanged);
sessionInput.addEventListener("input", debounce(onFiltersChanged, 150));
el("clear-filters").addEventListener("click", clearFilters);
daySelect.addEventListener("change", () => selectDay(daySelect.value));
loadOlderBtn.addEventListener("click", loadOlder);

async function init() {
  const params = new URLSearchParams(location.search);
  applyFiltersFromParams(params);
  state.day = params.get("day") || null;

  fillSelect(el("f-verdict"), VERDICT_OPTIONS);
  applyFiltersFromParams(params); // re-apply now that verdict options exist

  await loadTable();
  applyFiltersFromParams(params); // re-apply again: engine/event/handler options exist only now
  const days = await loadDays();
  if (!state.day) state.day = days.today;

  if (state.day === state.today) {
    // Live day: connectStream's `reset` handshake does the backfill fetch
    // (SPEC 4.4) — first load runs the exact same path as a reconnect.
    fillDaySelect(days.days, state.day);
    updateURL();
    connectStream();
  } else {
    await selectDay(state.day);
  }
}

init();
