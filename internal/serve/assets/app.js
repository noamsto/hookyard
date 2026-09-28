// hookyard serve frontend. No framework, no build step, no network fetch
// beyond this origin (SPEC 4.8) — this file is exactly what the browser
// loads. All record-derived text goes through textContent/createElement,
// never innerHTML: cwd, tool names and handler messages are operator input
// hookyard does not sanitize, and this page is the last stop before a DOM.

// DOM row cap (PLAN step 8): a JS constant because no Go code can see it.
const MAX_ROWS = 2000;

const OUTCOME_OPTIONS = [
  "allow", "deny", "ask", "advise", "abstain",
  "dispatched", "suppressed", "timeout", "error", "router-error",
];

const VERDICT_OPTIONS = [
  "allow", "deny", "ask", "abstain", "suppressed", "ok", "error", "timeout",
];

// FIELDS is the branch-then-call filter order shared by filterParams, the
// chip row and the add-selects: engine, event, handler, outcome (branch),
// verdict (call), then session.
const FIELDS = ["engine", "event", "handler", "outcome", "verdict"];

const FILTER_SELECTS = { engine: "f-engine", event: "f-event", handler: "f-handler", outcome: "f-outcome", verdict: "f-verdict" };
const FIELD_LABELS = { engine: "engine", event: "event", handler: "handler", outcome: "handler outcome", verdict: "call verdict" };

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
const filterChipsEl = el("filter-chips");

const rowTemplate = el("row-template");
const chipTemplate = el("handler-chip-template");
const handlerDetailTemplate = el("handler-detail-template");
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

// emit is how app.js hands flow.js what it needs (sync points, live calls)
// without either module reaching into the other's state.
function emit(name, detail) {
  document.dispatchEvent(new CustomEvent(name, { detail }));
}

// lastSyncDetail is the most recent hookyard:sync payload, so a late listener
// (the flow bundle can still be evaluating when a past day's sync fires) can
// ask for the state it missed instead of waiting for the next event.
let lastSyncDetail = null;

function emitSync(detail) {
  lastSyncDetail = detail;
  emit("hookyard:sync", detail);
}

export function syncState() {
  return lastSyncDetail;
}

function debounce(fn, ms) {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}

// ---------- filters ----------

// filterState is the one source of truth for active filters: one array per
// field plus session, mirrored to the URL and read by filterParams. The
// add-selects never carry selection themselves — they only append to this.
const filterState = { engine: [], event: [], handler: [], outcome: [], verdict: [], session: "" };

// trail is the same values as filterState's arrays, in the order they were
// added: the drill path. Each step narrows the one before it, so a crumb is a
// way back to its level. It is kept in lockstep with filterState by
// setTrail, and the URL carries it in this order, so a reload or a shared
// link keeps the path.
let trail = [];

function setTrail(next) {
  trail = next;
  for (const field of FIELDS) filterState[field] = [];
  for (const [field, value] of trail) filterState[field].push(value);
}

// activeFilters returns a deep copy for callers outside this module (the
// flow view reads it to know which nodes are already filtered-in).
export function activeFilters() {
  const f = { session: filterState.session };
  for (const field of FIELDS) f[field] = filterState[field].slice();
  return f;
}

// filterTrail is the drill path, oldest step first, as [field, value] pairs.
export function filterTrail() {
  return trail.map(([field, value]) => [field, value]);
}

// filterParams is the one place filterState becomes a query string, shared
// by the SSE URL, the events fetch and the URL bar mirror. Values go out in
// trail order; the server reads them as sets, so the order is only for the
// URL's sake.
export function filterParams() {
  const params = new URLSearchParams();
  for (const [field, value] of trail) params.append(field, value);
  if (filterState.session) params.set("session", filterState.session);
  return params;
}

// applyFiltersFromParams loads filterState from the URL, in the URL's order.
// A value not among a field's current select options still lands in the
// state and still shows as a chip — the state is the truth, not the options.
function applyFiltersFromParams(params) {
  const next = [];
  for (const [field, value] of params) {
    if (!FIELDS.includes(field) || !value) continue;
    if (!next.some(([f, v]) => f === field && v === value)) next.push([field, value]);
  }
  setTrail(next);
  filterState.session = params.get("session") || "";
  sessionInput.value = filterState.session;
}

// updateURL mirrors the state to the address bar. push is set for a change
// the operator made to the drill path, so the browser's back button steps
// back up it; day switches and bookkeeping replace in place.
function updateURL(push) {
  const params = filterParams();
  if (state.day) params.set("day", state.day);
  const view = new URLSearchParams(location.search).get("view");
  if (view) params.set("view", view);
  const qs = params.toString();
  const url = qs ? "?" + qs : location.pathname;
  if (push && url !== location.search && !(url === location.pathname && !location.search)) history.pushState(null, "", url);
  else history.replaceState(null, "", url);
}

function clearFilters() {
  setTrail([]);
  filterState.session = "";
  sessionInput.value = "";
  onFiltersChanged(true);
}

// toggleFilter is the one mutator shared by the filter-chip × buttons and
// flow.js's node clicks: active → remove; else additive → append; else
// replace the field's values with just this one, at the field's first step.
export function toggleFilter(field, value, additive) {
  const idx = trail.findIndex(([f, v]) => f === field && v === value);
  let next;
  if (idx !== -1) next = trail.filter((_, i) => i !== idx);
  else if (additive) next = trail.concat([[field, value]]);
  else {
    const at = trail.findIndex(([f]) => f === field);
    next = trail.filter(([f]) => f !== field);
    next.splice(at === -1 ? next.length : at, 0, [field, value]);
  }
  setTrail(next);
  onFiltersChanged(true);
}

// backTo keeps the first n steps of the drill path; popFilter drops the last.
export function backTo(n) {
  if (n >= trail.length) return;
  setTrail(trail.slice(0, Math.max(0, n)));
  onFiltersChanged(true);
}

export function popFilter() {
  if (trail.length) backTo(trail.length - 1);
}

// renderChips draws the drill path: one crumb per step, in trail order. The
// crumb's label goes back to that level; its × removes just that value.
function renderChips() {
  filterChipsEl.textContent = "";
  trail.forEach(([field, value], i) => {
    if (i > 0) {
      const sep = document.createElement("span");
      sep.className = "crumb-sep";
      sep.textContent = "›";
      sep.setAttribute("aria-hidden", "true");
      filterChipsEl.appendChild(sep);
    }
    const last = i === trail.length - 1;
    const chip = document.createElement("span");
    chip.className = "fchip crumb" + (last ? " current" : "");
    chip.dataset.field = field;
    if (field === "outcome" || field === "verdict") chip.dataset.o = value;
    const label = document.createElement("button");
    label.type = "button";
    label.className = "crumb-go";
    const fullLabel = FIELD_LABELS[field] + ": " + value;
    label.textContent = fullLabel;
    if (last) label.setAttribute("aria-current", "step");
    label.title = last ? fullLabel + " — the current level" : fullLabel + " — back to this level";
    label.addEventListener("click", () => backTo(i + 1));
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "fchip-remove";
    remove.textContent = "×";
    remove.setAttribute("aria-label", "remove " + FIELD_LABELS[field] + ": " + value);
    remove.addEventListener("click", () => toggleFilter(field, value));
    chip.appendChild(label);
    chip.appendChild(remove);
    filterChipsEl.appendChild(chip);
  });
  const any = trail.length > 0;
  el("trail-back").hidden = !any;
  el("trail-root").classList.toggle("current", !any);
  el("clear-filters").hidden = !any && !filterState.session;
  document.body.classList.toggle("filtered", any);
  markTrailOverflow();
}

// markTrailOverflow flags a path too long for the bar: its oldest crumbs
// slide out on the left, and the class fades that edge so the cut shows.
function markTrailOverflow() {
  let need = 0;
  for (const c of filterChipsEl.children) need += c.offsetWidth + 4;
  filterChipsEl.classList.toggle("overflowing", need > filterChipsEl.clientWidth + 1);
}
window.addEventListener("resize", debounce(markTrailOverflow, 100));

function onFiltersChanged(push) {
  renderChips();
  updateURL(push);
  if (state.day === state.today) {
    // Reopening the stream runs the one connect sequence (reset -> backfill),
    // which is also how the new filters reach /api/events — there is no
    // separate refetch-then-reopen implementation (PLAN step 8).
    connectStream();
  } else {
    loadEvents(state.day);
  }
}

// The browser's back and forward buttons walk the drill path.
window.addEventListener("popstate", () => {
  const params = new URLSearchParams(location.search);
  applyFiltersFromParams(params);
  const day = params.get("day");
  renderChips();
  if (day && day !== state.day) {
    fillDaySelect([...daySelect.options].map((o) => o.value), day);
    selectDay(day);
  } else {
    onFiltersChanged(false);
  }
});

// ---------- focus mode ----------

// Focus mode hides the chrome so the flow graph gets the whole window. The
// graph sizes itself from the window, so a resize event re-fits it.
function setFocus(on) {
  document.body.classList.toggle("focus", on);
  el("focus-toggle").setAttribute("aria-pressed", String(on));
  window.dispatchEvent(new Event("resize"));
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

// Outcomes that decided something, most consequential first. They are drawn
// in their outcome colour; everything else stays quiet. Class names and
// data-o values come only from this list and OUTCOME_OPTIONS, never the wire.
const DECISIONS = ["router-error", "error", "timeout", "deny", "ask", "advise", "allow"];

function outcomeKey(o) {
  return OUTCOME_OPTIONS.includes(o) || VERDICT_OPTIONS.includes(o) ? o : "other";
}

function buildChip(h) {
  const node = chipTemplate.content.cloneNode(true);
  const chip = node.querySelector(".chip");
  chip.dataset.o = outcomeKey(h.outcome);
  if (DECISIONS.includes(h.outcome)) chip.classList.add("chip--loud");
  chip.title = h.name + " · " + h.outcome + " · " + fmtMS(h.ms);
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

// eventLabel renders a record's event cell. A router-error row carries the
// manifest's canonical event name in native_event and no canonical event, so
// it reads by native_event as-is. A routed record with no canonical event
// (pi's engine-scoped per-turn turn_end) reads as engine:native_event — the
// same string its filter value uses. A canonical row reads by canonical_event.
export function eventLabel(rec) {
  if (rec.router === "error") return rec.native_event || "—";
  if (rec.canonical_event) return rec.canonical_event;
  if (rec.native_event) return rec.engine + ":" + rec.native_event;
  return "—";
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
  li.dataset.o = outcomeKey(rec.verdict);
  if (DECISIONS.includes(rec.verdict)) li.classList.add("row--loud");

  node.querySelector(".c-time").textContent = fmtTime(rec.ts);
  node.querySelector(".c-engine").textContent = rec.engine;
  node.querySelector(".c-key").textContent = rec.key;
  node.querySelector(".c-key").title = rec.key;
  node.querySelector(".c-event").textContent = eventLabel(rec);
  node.querySelector(".c-tool").textContent = rec.tool_name || "—";

  const verdictEl = node.querySelector(".c-verdict");
  verdictEl.textContent = rec.verdict;
  verdictEl.className = "c-verdict badge badge--" + rec.verdict;

  const enforced = node.querySelector(".c-enforced");
  enforced.textContent = rec.enforced ? "E" : "";
  if (rec.enforced) enforced.title = "enforced: the engine acted on this verdict";

  const handlers = rec.handlers || [];
  const chips = node.querySelector(".c-handlers");
  const hits = entry.hits;
  handlers.forEach((h, i) => {
    const chipNode = buildChip(h);
    if (hits) chipNode.querySelector(".chip").classList.add(hits.includes(i) ? "chip--hit" : "chip--dim");
    chips.appendChild(chipNode);
  });

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
  feedCountEl.textContent = "· " + rowCount.toLocaleString("en-US") + (rowCount === 1 ? " row" : " rows");
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
  emitSync({ day, live: day === state.today });
}

// renderStats draws the day's counts: the call total, the verdict mix as one
// bar and a table (split by enforced), the slowest handlers with a bar each,
// and router status and truncation on one line. Unfiltered by design: it
// describes the whole day, whatever the drill path.
const SLOW_ROWS = 10;
let slowExpanded = false;

function renderStats(snap) {
  lastStats = snap;
  statsDayEl.textContent = snap.day;
  statsBodyEl.textContent = "";

  const total = document.createElement("div");
  total.className = "sb-total";
  const big = document.createElement("span");
  big.className = "sb-big";
  big.textContent = snap.calls.toLocaleString("en-US");
  const unit = document.createElement("span");
  unit.className = "dim";
  unit.textContent = snap.calls === 1 ? "call" : "calls";
  total.append(big, unit);
  statsBodyEl.appendChild(total);

  const verdictRows = Object.entries(snap.verdicts || {}).sort((a, b) => {
    const la = DECISIONS.indexOf(a[0]), lb = DECISIONS.indexOf(b[0]);
    if ((la === -1) !== (lb === -1)) return la === -1 ? 1 : -1;
    return la !== lb && la !== -1 ? la - lb : b[1].total - a[1].total;
  });

  // The mix bar: decisions get a floor width so a single deny among
  // thousands of abstains is still a visible sliver.
  if (snap.calls > 0) {
    const bar = document.createElement("div");
    bar.className = "sb-mix";
    bar.setAttribute("role", "img");
    bar.setAttribute("aria-label", "verdict mix: " + verdictRows.map(([v, c]) => c.total + " " + v).join(", "));
    for (const [v, c] of verdictRows) {
      const seg = document.createElement("span");
      seg.dataset.o = outcomeKey(v);
      if (DECISIONS.includes(v)) seg.classList.add("loud");
      seg.style.flexGrow = String(c.total);
      seg.style.minWidth = DECISIONS.includes(v) ? "3px" : "0";
      seg.title = c.total + " " + v;
      bar.appendChild(seg);
    }
    statsBodyEl.appendChild(bar);
  }

  statsBodyEl.appendChild(statGrid(["verdict", "calls", "enforced"], verdictRows.map(([v, c]) => ({
    cells: [v, c.total.toLocaleString("en-US"), c.enforced ? c.enforced.toLocaleString("en-US") : "—"],
    cls: DECISIONS.includes(v) ? "v-" + v : "",
    title: c.enforced + " enforced / " + c.unenforced + " unenforced",
  }))));

  const handlerRows = (snap.handlers || []).map((h) => ({ ...h, mean: h.total_ms / Math.max(h.calls, 1) }))
    .sort((a, b) => b.mean - a.mean);
  const maxMean = Math.max(1, ...handlerRows.map((h) => h.mean));
  const slow = document.createElement("div");
  slow.className = "sb-slow";
  const head = document.createElement("div");
  head.className = "sb-slow-head";
  for (const [t, cls] of [["slowest handlers", ""], ["mean", "n"], ["max ms", "n"]]) {
    const h = document.createElement("span");
    h.className = "h" + (cls ? " " + cls : "");
    h.textContent = t;
    head.appendChild(h);
  }
  slow.appendChild(head);
  if (handlerRows.length === 0) {
    const empty = document.createElement("span");
    empty.className = "dim sb-empty";
    empty.textContent = "none yet";
    slow.appendChild(empty);
  }
  for (const h of slowExpanded ? handlerRows : handlerRows.slice(0, SLOW_ROWS)) {
    const row = document.createElement("div");
    row.className = "sb-slow-row";
    row.title = h.name + " · " + h.calls + " calls";
    const name = document.createElement("span");
    name.className = "sb-slow-name";
    const label = document.createElement("span");
    label.textContent = h.name;
    const track = document.createElement("span");
    track.className = "sb-track";
    const fill = document.createElement("span");
    fill.className = "sb-fill" + (h.max_ms >= 1000 ? " slow" : "");
    fill.style.width = Math.max(2, Math.round(Math.sqrt(h.mean / maxMean) * 100)) + "%";
    track.appendChild(fill);
    name.append(label, track);
    const mean = document.createElement("span");
    mean.className = "n";
    mean.textContent = String(Math.round(h.mean));
    const max = document.createElement("span");
    max.className = "n dim";
    max.textContent = String(h.max_ms);
    row.append(name, mean, max);
    slow.appendChild(row);
  }
  if (handlerRows.length > SLOW_ROWS) {
    const more = document.createElement("button");
    more.type = "button";
    more.className = "sb-more";
    more.textContent = slowExpanded ? "show the top " + SLOW_ROWS : "show all " + handlerRows.length;
    more.addEventListener("click", () => {
      slowExpanded = !slowExpanded;
      renderStats(lastStats);
    });
    slow.appendChild(more);
  }
  statsBodyEl.appendChild(slow);

  const foot = document.createElement("div");
  foot.className = "sb-foot dim";
  const routerRows = Object.entries(snap.router || {}).sort((a, b) => b[1] - a[1]);
  for (const [status, n] of routerRows) {
    const s = document.createElement("span");
    s.textContent = "router " + status + " " + n.toLocaleString("en-US");
    if (status === "ok") s.className = "sb-ok";
    else s.className = "v-error";
    foot.appendChild(s);
  }
  const tr = document.createElement("span");
  tr.textContent = "truncated " + snap.truncated;
  if (snap.truncated > 0) tr.className = "v-timeout";
  foot.appendChild(tr);
  statsBodyEl.appendChild(foot);

  if (tableCache) renderTable(tableCache, snap);
}

// statGrid lays a block out as one grid: a header row, then one line per
// row, the first column ellipsized and every number right-aligned.
function statGrid(head, rows) {
  const grid = document.createElement("div");
  grid.className = "sb-grid c" + head.length;
  head.forEach((h, i) => {
    const cell = document.createElement("span");
    cell.className = "h" + (i > 0 ? " n" : "");
    cell.textContent = h;
    grid.appendChild(cell);
  });
  if (rows.length === 0) {
    const empty = document.createElement("span");
    empty.className = "dim sb-empty";
    empty.textContent = "none yet";
    grid.appendChild(empty);
  }
  for (const row of rows) {
    row.cells.forEach((v, i) => {
      const cell = document.createElement("span");
      cell.className = i > 0 ? "n" : row.cls || "";
      cell.textContent = v;
      if (row.title) cell.title = row.title;
      grid.appendChild(cell);
    });
  }
  return grid;
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
  const grid = document.createElement("div");
  grid.className = "sb-grid table-grid";
  for (const h of ["", "handler", "lane"]) {
    const cell = document.createElement("span");
    cell.className = "h";
    cell.textContent = h;
    grid.appendChild(cell);
  }
  for (const h of table.handlers) {
    const dot = document.createElement("span");
    dot.className = "tr-fired" + (fired.has(h.id) ? " fired" : "");
    dot.title = fired.has(h.id) ? "fired today" : "idle today";
    const id = document.createElement("span");
    id.className = "tr-id";
    id.textContent = h.id;
    id.title = h.id + "\n" + (h.events || []).join(", ");
    const lane = document.createElement("span");
    lane.className = "tr-lane";
    lane.textContent = h.lane === "fire_and_forget" ? "async" : h.lane || "";
    grid.append(dot, id, lane);
  }
  tableBodyEl.appendChild(grid);
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
  fillSelect(el("f-engine"), "engine", table.engines || []);
  fillSelect(el("f-event"), "event", eventOptions(table));
  fillSelect(el("f-handler"), "handler", (table.handlers || []).map((h) => h.id));
}

// eventOptions is table.events plus every engine-scoped handler event, in
// table order, deduped.
function eventOptions(table) {
  const seen = new Set();
  const out = [];
  for (const v of table.events || []) {
    if (seen.has(v)) continue;
    seen.add(v);
    out.push(v);
  }
  for (const h of table.handlers || []) {
    for (const v of h.events || []) {
      if (!v.includes(":") || seen.has(v)) continue;
      seen.add(v);
      out.push(v);
    }
  }
  return out;
}

// fillSelect rebuilds an add-select: a "+ <label>" placeholder (value "")
// followed by the options. These selects never hold selection themselves —
// filterState does — so there is nothing to preserve across a rebuild.
function fillSelect(select, label, values) {
  select.textContent = "";
  const placeholder = document.createElement("option");
  placeholder.value = "";
  placeholder.textContent = "+ " + label;
  select.appendChild(placeholder);
  for (const v of values) {
    const opt = document.createElement("option");
    opt.value = v;
    opt.textContent = v;
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
  emit("hookyard:call", entry);
}

function handleStatsFrame(snap) {
  if (snap.day === state.day) renderStats(snap);
}

function handleDayFrame(data) {
  insertDivider(data.day);
  state.day = data.day;
  emitSync({ day: data.day, live: true });
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
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
  if (ev.key === "/" && !typing) {
    ev.preventDefault();
    sessionInput.focus();
  } else if (ev.key === "Escape") {
    if (document.body.classList.contains("focus")) setFocus(false);
    else if (!el("filters").hidden) closeFilterPop();
    else clearFilters();
  } else if (ev.key === "Backspace" && (!typing || (ev.target === sessionInput && !sessionInput.value))) {
    if (!trail.length) return;
    ev.preventDefault();
    popFilter();
  } else if (ev.key === "f" && !typing) {
    setFocus(!document.body.classList.contains("focus"));
  }
});

// ---------- wiring ----------

// Each add-select is wired individually, not through a form-level "change"
// listener: picking a value appends it to filterState and resets the select
// to its placeholder, which a generic listener can't express.
for (const field of FIELDS) {
  const select = el(FILTER_SELECTS[field]);
  select.addEventListener("change", () => {
    const value = select.value;
    select.value = "";
    if (!value) return;
    if (!filterState[field].includes(value)) setTrail(trail.concat([[field, value]]));
    closeFilterPop();
    onFiltersChanged(true);
  });
}
sessionInput.addEventListener("input", debounce(() => {
  filterState.session = sessionInput.value.trim();
  onFiltersChanged(false);
}, 150));
el("clear-filters").addEventListener("click", clearFilters);
el("trail-root").addEventListener("click", () => backTo(0));
el("trail-back").addEventListener("click", popFilter);
el("focus-toggle").addEventListener("click", () => setFocus(!document.body.classList.contains("focus")));
el("focus-exit").addEventListener("click", () => setFocus(false));

// The add-selects live in a small popover under "+ filter": the drill path
// is how filters are usually made, so the selects stay out of the way.
const filterPop = el("filters");
const filterAddToggle = el("filter-add-toggle");
function closeFilterPop() {
  filterPop.hidden = true;
  filterAddToggle.setAttribute("aria-expanded", "false");
}
filterAddToggle.addEventListener("click", () => {
  const open = filterPop.hidden;
  filterPop.hidden = !open;
  filterAddToggle.setAttribute("aria-expanded", String(open));
  if (open) el("f-engine").focus();
});
document.addEventListener("click", (ev) => {
  if (!filterPop.hidden && !ev.target.closest(".filter-add")) closeFilterPop();
});
daySelect.addEventListener("change", () => selectDay(daySelect.value));
loadOlderBtn.addEventListener("click", loadOlder);

async function init() {
  const params = new URLSearchParams(location.search);
  applyFiltersFromParams(params);
  state.day = params.get("day") || null;

  fillSelect(el("f-outcome"), FIELD_LABELS.outcome, OUTCOME_OPTIONS);
  fillSelect(el("f-verdict"), FIELD_LABELS.verdict, VERDICT_OPTIONS);
  renderChips();

  await loadTable(); // populates engine/event/handler options
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
