// Ported from flow.js's fmtCount.
export function fmtCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1e5) return (n / 1e3).toFixed(1) + "k";
  if (n < 1e6) return Math.round(n / 1e3) + "k";
  return (n / 1e6).toFixed(1) + "M";
}
