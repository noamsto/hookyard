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
      return qa("#flow-body [data-key]").map((p) => [p.dataset.key, Number(p.dataset.n)]);
    },
    // hit: the element's centre, and whether a click there lands on it.
    hit(el) {
      const r = el.getBoundingClientRect();
      const x = r.left + r.width / 2;
      const y = r.top + r.height / 2;
      const at = document.elementFromPoint(x, y);
      return { x, y, ok: !!at && (at === el || el.contains(at)) };
    },
    node(col, name) {
      return q(`#flow-body [data-col="${CSS.escape(col)}"][data-name="${CSS.escape(name)}"]`);
    },
    nodePoint(col, name) {
      const el = this.node(col, name);
      return el ? this.hit(el) : null;
    },
    // headerPoint: the i-th column header (engine, event, handler, outcome).
    // Headers aren't graph nodes, so unlike nodePoint this can't key off
    // data-col/data-name.
    headerPoint(i) {
      const el = qa("#flow-body .flow-colhead")[i];
      return el ? this.hit(el) : null;
    },
    // groupHeaderPoint: what toggles the member's group — a collapsed
    // group's plate, or an open group's fold header.
    groupHeaderPoint(member) {
      const g = groupEls().find((el) => JSON.parse(el.dataset.members).includes(member));
      const h = g?.querySelector(".flow-group-header");
      return h ? this.hit(h) : null;
    },
    selected(col, name) {
      return !!this.node(col, name)?.classList.contains("selected");
    },
    // boxes: every drawn node's hit area in page coordinates, by column.
    boxes() {
      return qa("#flow-body [data-col]").map((el) => {
        const r = el.querySelector(".hit").getBoundingClientRect();
        return { col: el.dataset.col, name: el.dataset.name, expanded: el.dataset.expanded, left: r.left + scrollX, right: r.right + scrollX, top: r.top + scrollY, bottom: r.bottom + scrollY };
      });
    },
    // overlaps: pairs of handler-column hit areas (plates, open-group fold
    // headers) that overlap, and any that reach into the legend row.
    overlaps() {
      const rs = this.boxes().filter((b) => b.col === "handler" || b.col === "group").sort((a, b) => a.top - b.top);
      const bad = [];
      for (let i = 1; i < rs.length; i++) {
        if (rs[i].top < rs[i - 1].bottom - 0.5 && rs[i].left < rs[i - 1].right && rs[i - 1].left < rs[i].right) bad.push(rs[i - 1].name + " / " + rs[i].name);
      }
      const legend = q("#flow-body .l-legend")?.getBoundingClientRect();
      const floor = legend && legend.height > 0 ? legend.top + scrollY : Infinity;
      for (const b of this.boxes()) if (b.bottom > floor + 0.5) bad.push(b.name + " reaches the legend");
      return bad;
    },
    // outside: nodes not fully inside the flow body.
    outside() {
      const r = body().getBoundingClientRect();
      const [left, top] = [r.left + scrollX, r.top + scrollY];
      return this.boxes().filter((b) => b.left < left - 1 || b.top < top - 1 || b.right > left + r.width + 1 || b.bottom > top + r.height + 1)
        .map((b) => b.col + ":" + b.name);
    },
    horizontalScroll() {
      return document.documentElement.scrollWidth - document.documentElement.clientWidth;
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

// openFlow loads the flow view and waits for the first committed snapshot
// to be drawn.
export async function openFlow(page, base, query = "") {
  await page.navigate(base + "/?view=flow" + (query ? "&" + query : ""));
  await page.waitFor("window.__e2e.settled()", 15000, "first layout committed");
  await page.waitFor(`!!document.querySelector("#flow-body [data-col]") || !document.querySelector("#flow-body .flow-empty").hidden`,
    5000, "flow drawn");
}
