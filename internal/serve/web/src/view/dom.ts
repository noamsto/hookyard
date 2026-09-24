// DOM/SVG builders. Operator strings (handler, event, engine names) only
// ever reach the page through textContent or data-* attributes.

export const SVGNS = "http://www.w3.org/2000/svg";

export type Attrs = Record<string, string | number>;

export function svgEl<K extends keyof SVGElementTagNameMap>(tag: K, attrs: Attrs = {}, parent?: Element): SVGElementTagNameMap[K] {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (parent) parent.appendChild(e);
  return e;
}

export function htmlEl<K extends keyof HTMLElementTagNameMap>(tag: K, cls = "", text = "", parent?: Element): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text) e.textContent = text;
  if (parent) parent.appendChild(e);
  return e;
}

// A run of styled text: [text, class, outcome for data-o].
export type Part = [string, string?, string?];

export function svgText(parent: Element, x: number, y: number, parts: readonly Part[], attrs: Attrs = {}): SVGTextElement {
  const t = svgEl("text", { x, y, ...attrs }, parent);
  for (const [s, cls, o] of parts) {
    const sp = svgEl("tspan", {}, t);
    if (cls) sp.setAttribute("class", cls);
    if (o) sp.setAttribute("data-o", o);
    sp.textContent = s;
  }
  return t;
}

export const fmt = (n: number): string => n.toLocaleString("en-US");

// fit shortens s to at most max characters, ending in an ellipsis.
export function fit(s: string, max: number): string {
  if (s.length <= max) return s;
  return max <= 1 ? "…" : s.slice(0, max - 1) + "…";
}

export function partsLen(parts: readonly Part[]): number {
  return parts.reduce((a, [s]) => a + s.length, 0);
}
