import assert from "node:assert/strict";
import { test } from "node:test";
import { clampView, fitView, MAX_K, panBy, wheelFactor, zoomAt } from "./viewport.ts";

const scene = { w: 1200, h: 600 };
const panel = { w: 400, h: 500 };

test("fit scales a wide scene down to the panel, never a narrow one up", () => {
  assert.deepEqual(fitView(scene, panel), { k: 400 / 1200, tx: 0, ty: 0 });
  assert.deepEqual(fitView({ w: 300, h: 600 }, panel), { k: 1, tx: 0, ty: 0 });
});

test("zoomAt keeps the point under the cursor fixed", () => {
  const fit = { k: 1, tx: -300, ty: -100 };
  const v = zoomAt(fit, 2, 200, 100, scene, panel, 1 / 3);
  const before = [(200 - fit.tx) / fit.k, (100 - fit.ty) / fit.k];
  const after = [(200 - v.tx) / v.k, (100 - v.ty) / v.k];
  assert.ok(Math.abs(before[0] - after[0]) < 1e-9 && Math.abs(before[1] - after[1]) < 1e-9);
  assert.equal(v.k, 2);
});

test("k is clamped to [fit/2, MAX_K]", () => {
  const fit = fitView(scene, panel);
  assert.equal(zoomAt(fit, 1000, 0, 0, scene, panel, fit.k).k, MAX_K);
  assert.ok(Math.abs(zoomAt(fit, 0.001, 0, 0, scene, panel, fit.k).k - fit.k / 2) < 1e-9);
});

test("pan stops when the scene's far edge meets the panel's", () => {
  const v = { k: 1, tx: 0, ty: 0 };
  const far = panBy(v, -99999, -99999, scene, panel, 1 / 3);
  assert.equal(far.tx, panel.w - scene.w - 24);
  assert.equal(far.ty, panel.h - scene.h - 24);
  const back = panBy(v, 99999, 99999, scene, panel, 1 / 3);
  assert.deepEqual([back.tx, back.ty], [24, 24]);
});

test("a scene smaller than the panel cannot leave it", () => {
  const small = { w: 200, h: 100 };
  const v = panBy({ k: 1, tx: 0, ty: 0 }, 9999, 9999, small, panel, 1);
  assert.deepEqual([v.tx, v.ty], [panel.w - small.w - 24, panel.h - small.h - 24]);
});

test("clampView leaves an in-range view alone", () => {
  const v = { k: 1, tx: -100, ty: -20 };
  assert.deepEqual(clampView(v, scene, panel, 1 / 3), v);
});

test("wheelFactor normalises line and page deltas", () => {
  assert.equal(wheelFactor(3, 1, false, 500), wheelFactor(48, 0, false, 500));
  assert.equal(wheelFactor(1, 2, false, 500), wheelFactor(500, 0, false, 500));
  assert.ok(wheelFactor(-100, 0, false, 500) > 1);
  assert.ok(wheelFactor(-5, 0, true, 500) > wheelFactor(-5, 0, false, 500));
});

test("wheel zoom-out floors at fitK so a fitted graph is left unchanged", () => {
  const fit = fitView(scene, panel);
  const out = zoomAt(fit, 0.5, 100, 100, scene, panel, fit.k, fit.k);
  assert.deepEqual(out, fit);
  const dflt = zoomAt(fit, 0.5, 100, 100, scene, panel, fit.k);
  assert.ok(dflt.k < fit.k);
});
