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
    // label: a node's label group (labels sit in their own layer, above the
    // bands, so they are not children of the node).
    label(col, name) {
      return q(`#flow-body .label[data-of="${CSS.escape(JSON.stringify([col, name]))}"]`);
    },
    // bandPoint: a point that hit-tests to one of the edge's bands.
    bandPoint(key) {
      const g = qa("#flow-body .flow-edge").find((el) => el.dataset.key === key);
      for (const p of g ? g.querySelectorAll(".band") : []) {
        const r = p.getBoundingClientRect();
        const x = r.left + r.width / 2;
        for (let y = r.top; y <= r.bottom; y++) if (document.elementFromPoint(x, y) === p) return { x, y, ok: true };
      }
      return null;
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
    // headerBoxes: every column header's rendered extent (rect + text, so
    // header text overflowing its background box is caught), in page
    // coordinates, left to right.
    headerBoxes() {
      return qa("#flow-body .flow-colhead").map((el) => {
        const r = el.getBoundingClientRect();
        return { left: r.left + scrollX, right: r.right + scrollX, top: r.top + scrollY, bottom: r.bottom + scrollY };
      }).sort((a, b) => a.left - b.left);
    },
    // tipFit: whether the open tooltip sits fully inside the flow body's
    // visible area, or — when taller than that — scrolls inside it instead
    // of being clipped.
    tipFit() {
      const t = q("#flow-body .flow-tip");
      const b = body();
      if (!t || !b) return null;
      const tr = t.getBoundingClientRect();
      const br = b.getBoundingClientRect();
      const visibleBottom = br.top + b.clientHeight;
      const overflowY = getComputedStyle(t).overflowY;
      return {
        tipH: tr.height, bodyH: b.clientHeight,
        fits: tr.top >= br.top - 0.5 && tr.bottom <= visibleBottom + 0.5,
        scrolls: t.scrollHeight > t.clientHeight && overflowY === "auto",
        scrollHeight: t.scrollHeight, clientHeight: t.clientHeight, overflowY,
      };
    },
    // groupHeaderPoint: what toggles the member's group — a collapsed
    // group's plate, or an open group's fold header.
    groupHeaderPoint(member) {
      const g = groupEls().find((el) => JSON.parse(el.dataset.members).includes(member));
      const h = g?.querySelector(".flow-group-header");
      return h ? this.hit(h) : null;
    },
    // scrollGroupIntoView: bring a member's group element into view — the
    // flow body scrolls horizontally at narrow widths, not just the page.
    scrollGroupIntoView(member) {
      const g = groupEls().find((el) => JSON.parse(el.dataset.members).includes(member));
      g?.scrollIntoView({ block: "center", inline: "center" });
      return !!g;
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
    // view: the flow's pan/zoom state, as the view publishes it.
    view() {
      const d = body().dataset;
      return { k: Number(d.viewK), tx: Number(d.viewTx), ty: Number(d.viewTy), fitted: d.fitted === "1" };
    },
    // bgPoint: a point in the flow body that hit-tests to the bare svg (no
    // node or band), i.e. empty background a drag can start on.
    bgPoint() {
      const b = body().getBoundingClientRect();
      const svg = q("#flow-body svg.flow-svg");
      for (let y = b.bottom - 4; y > b.top + 30; y -= 6) {
        for (let x = b.right - 4; x > b.left + 4; x -= 6) if (document.elementFromPoint(x, y) === svg) return { x, y, ok: true };
      }
      return { ok: false };
    },
    // strayDots: drawn pulse dots whose centre is outside every band of
    // their outcome (client coordinates, so the scene transform is included).
    strayDots() {
      const bands = qa("#flow-body .l-bands .band").map((p) => ({ o: p.dataset.o, r: p.getBoundingClientRect() }));
      const dots = qa("#flow-body .flow-dot").filter((c) => Number(c.getAttribute("r")) > 0);
      const stray = dots.filter((c) => {
        const r = c.getBoundingClientRect();
        const [x, y] = [r.left + r.width / 2, r.top + r.height / 2];
        return !bands.some((b) => b.o === c.dataset.o && x >= b.r.left - 1 && x <= b.r.right + 1 && y >= b.r.top - 1 && y <= b.r.bottom + 1);
      });
      return { dots: dots.length, stray: stray.length };
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
