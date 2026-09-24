// Page-side helpers shared by run.mjs and screenshots.mjs: window.__e2e is
// installed into every document the page loads, and reads the flow view's
// test hooks (data-* attributes) and geometry.

function pageHelpers() {
  const q = (s) => document.querySelector(s);
  const qa = (s) => [...document.querySelectorAll(s)];
  const body = () => document.getElementById("flow-body");
  const groupEls = () => qa('#flow-body [data-col="group"]');
  window.__e2e = {
    state() {
      const b = body();
      if (!b) return null;
      const d = b.dataset;
      return {
        gen: Number(d.layoutGen ?? 0), pending: d.pendingLayout === "1",
        calls: Number(d.calls), branches: Number(d.branches),
        dots: Number(d.activeDots ?? 0), dropped: Number(d.dropped ?? 0),
      };
    },
    settled() {
      const s = this.state();
      return !!s && s.gen >= 1 && !s.pending;
    },
    groups() {
      return groupEls().map((el) => ({
        name: el.dataset.name, members: JSON.parse(el.dataset.members),
        expanded: el.dataset.expanded === "true", forced: el.dataset.forced === "true", n: Number(el.dataset.n),
      }));
    },
    groupOf(member) {
      return this.groups().find((g) => g.members.includes(member)) ?? null;
    },
    edges() {
      return qa("#flow-body path[data-key]").map((p) => [p.dataset.key, Number(p.dataset.n)]);
    },
    // hit: the element's centre, and whether a click there lands on it.
    hit(el) {
      const r = el.getBoundingClientRect();
      const x = r.left + r.width / 2;
      const y = r.top + r.height / 2;
      const at = document.elementFromPoint(x, y);
      return { x, y, ok: !!at && (at === el || el.contains(at)) };
    },
    nodePoint(col, name) {
      const el = q(`#flow-body [data-col="${CSS.escape(col)}"][data-name="${CSS.escape(name)}"]`);
      return el ? this.hit(el) : null;
    },
    // headerPoint: the i-th column header (engine, event, handler, outcome —
    // DOM order matches buildNodes' COLUMNS loop). Headers aren't graph
    // nodes, so unlike nodePoint this can't key off data-col/data-name.
    headerPoint(i) {
      const el = qa(".react-flow__node-colHeader")[i];
      return el ? this.hit(el) : null;
    },
    groupHeaderPoint(member) {
      const g = groupEls().find((el) => JSON.parse(el.dataset.members).includes(member));
      const h = g?.querySelector(".flow-group-header");
      return h ? this.hit(h) : null;
    },
    selected(col, name) {
      return !!q(`#flow-body [data-col="${CSS.escape(col)}"][data-name="${CSS.escape(name)}"].selected`);
    },
    transform() {
      const t = q("#flow-body .react-flow__viewport")?.style.transform ?? "";
      const m = t.match(/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([-\d.e]+)\)/);
      return m ? { x: Number(m[1]), y: Number(m[2]), k: Number(m[3]) } : null;
    },
    paneCenter() {
      const r = q("#flow-body .react-flow").getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
    },
    // panePoint: a point where a press lands on the bare pane.
    panePoint() {
      const r = q("#flow-body .react-flow").getBoundingClientRect();
      for (let fy = 0.1; fy < 0.95; fy += 0.05) {
        for (let fx = 0.1; fx < 0.9; fx += 0.05) {
          const x = r.left + r.width * fx;
          const y = r.top + r.height * fy;
          if (document.elementFromPoint(x, y)?.classList.contains("react-flow__pane")) return { x, y };
        }
      }
      return null;
    },
    // outside: ids of nodes not fully inside the flow canvas.
    outside() {
      const r = q("#flow-body .react-flow").getBoundingClientRect();
      return qa("#flow-body .react-flow__node").filter((n) => {
        const b = n.getBoundingClientRect();
        return b.left < r.left - 1 || b.top < r.top - 1 || b.right > r.right + 1 || b.bottom > r.bottom + 1;
      }).map((n) => n.dataset.id.replace("\x00", ":"));
    },
    nodeCount() {
      return qa("#flow-body .react-flow__node").length;
    },
    // topLevel: drawn cards and groups that are not members inside an
    // expanded group.
    topLevel() {
      const inside = new Set();
      for (const g of this.groups()) if (g.expanded) for (const m of g.members) inside.add(m);
      return qa("#flow-body [data-col]").filter((el) => !(el.dataset.col === "handler" && inside.has(el.dataset.name))).length;
    },
    minimapNodes() {
      return qa("#flow-body .react-flow__minimap .react-flow__minimap-node:not(.flow-minimap-hidden)").length;
    },
    chips() {
      return qa("#filter-chips .fchip").map((c) => c.firstChild.textContent);
    },
    params(field) {
      return new URLSearchParams(location.search).getAll(field);
    },
    meta() {
      return q("#flow-panel .panel-header .dim")?.textContent ?? "";
    },
    elementCount() {
      return qa("#flow-body *").length;
    },
    circles() {
      return qa("#flow-body circle").length;
    },
    // activeCircles: pulse dots actually drawn (r > 0), vs. the fixed,
    // mostly-idle MAX_DOTS pool circles() counts.
    activeCircles() {
      return qa("#flow-body circle").filter((c) => Number(c.getAttribute("r")) > 0).length;
    },
    appJsLoads() {
      return performance.getEntriesByType("resource").filter((e) => new URL(e.name).pathname === "/static/app.js").length;
    },
    // appFilter reads the engine filter from THE app.js module instance the
    // page loaded (the module map returns it, never a second copy).
    async appFilter() {
      return (await import("/static/app.js")).activeFilters().engine;
    },
  };
}

export function installHelpers(page) {
  return page.addInitScript(`(${pageHelpers.toString()})();`);
}

// openFlow loads the flow view and waits for the first committed layout and
// the initial fitView animation to land inside the canvas.
export async function openFlow(page, base, query = "") {
  await page.navigate(base + "/?view=flow" + (query ? "&" + query : ""));
  await page.waitFor("window.__e2e.settled()", 15000, "first layout committed");
  await page.waitFor(`new Promise((r) => { const a = JSON.stringify(__e2e.transform());
    setTimeout(() => r(a === JSON.stringify(__e2e.transform()) && __e2e.outside().length === 0), 150); })`, 5000, "initial fit");
}
