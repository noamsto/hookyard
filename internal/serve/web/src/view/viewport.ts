// Pure pan/zoom math for the flow scene. A View maps scene coordinates to
// panel coordinates: panel = scene * k + (tx, ty).

export interface View { k: number; tx: number; ty: number; }
export interface Size { w: number; h: number; }

export const MAX_K = 8;
const PAD = 24; // how far the scene's edge may pull away from the panel's

export const FIT_TO_MIN = 0.5; // the smallest k is this fraction of the fit scale

// fitView: the whole scene, scaled down (never up) to the panel's width.
export function fitView(scene: Size, panel: Size): View {
  const k = scene.w > 0 && panel.w > 0 ? Math.min(1, panel.w / scene.w) : 1;
  return { k, tx: 0, ty: 0 };
}

// range: where the scene's origin may sit along one axis. A scene bigger than
// the panel pans until its far edge meets the panel's; a smaller one stays inside.
const range = (panel: number, scene: number): [number, number] => {
  const [a, b] = [panel - scene - PAD, PAD];
  return a < b ? [a, b] : [b, a];
};

export function clampView(v: View, scene: Size, panel: Size, fitK: number): View {
  const k = Math.min(MAX_K, Math.max(fitK * FIT_TO_MIN, v.k));
  const [x0, x1] = range(panel.w, scene.w * k);
  const [y0, y1] = range(panel.h, scene.h * k);
  return { k, tx: Math.min(x1, Math.max(x0, v.tx)), ty: Math.min(y1, Math.max(y0, v.ty)) };
}

// zoomAt scales by factor about the panel point (px, py), which stays put.
export function zoomAt(v: View, factor: number, px: number, py: number, scene: Size, panel: Size, fitK: number): View {
  const k = Math.min(MAX_K, Math.max(fitK * FIT_TO_MIN, v.k * factor));
  const f = k / v.k;
  return clampView({ k, tx: px - (px - v.tx) * f, ty: py - (py - v.ty) * f }, scene, panel, fitK);
}

export function panBy(v: View, dx: number, dy: number, scene: Size, panel: Size, fitK: number): View {
  return clampView({ ...v, tx: v.tx + dx, ty: v.ty + dy }, scene, panel, fitK);
}

export const sameView = (a: View, b: View): boolean => a.k === b.k && a.tx === b.tx && a.ty === b.ty;

// wheelFactor: the zoom factor for one wheel event. deltaMode 1 (Firefox
// lines) and 2 (pages) are normalised to pixels; a trackpad pinch arrives as
// ctrl+wheel with much smaller deltas.
export function wheelFactor(deltaY: number, deltaMode: number, ctrl: boolean, pageH: number): number {
  const px = deltaMode === 1 ? deltaY * 16 : deltaMode === 2 ? deltaY * pageH : deltaY;
  return Math.exp(-px * (ctrl ? 0.01 : 0.0015));
}
