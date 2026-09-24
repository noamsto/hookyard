// The live window: n one-bucket-per-minute counts ending at the current
// minute. null ring = a static whole-day view (one bucket per path).
export interface Ring { start: number; bucketMs: number; n: number; }

// advanceRing shifts every count array left by the whole buckets elapsed
// since the ring's last bucket, zero-filling at the end. Returns whether it
// moved.
export function advanceRing(ring: Ring, counts: Iterable<number[]>, nowMs: number): boolean {
  const curBucketStart = Math.floor(nowMs / ring.bucketMs) * ring.bucketMs;
  const lastBucketStart = ring.start + (ring.n - 1) * ring.bucketMs;
  const shift = Math.round((curBucketStart - lastBucketStart) / ring.bucketMs);
  if (shift <= 0) return false;
  for (const c of counts) {
    c.splice(0, Math.min(shift, ring.n));
    while (c.length < ring.n) c.push(0);
  }
  // The full shift, not the clamped one: after a gap longer than the window
  // (a suspended laptop) the ring must land on the current minute at once.
  ring.start += shift * ring.bucketMs;
  return true;
}

// bucketIndex places a live record's ts in the ring: past the end clamps to
// the last bucket (clock skew), before the start or unparsable is null.
export function bucketIndex(ring: Ring, ts: string): number | null {
  const t = Date.parse(ts);
  if (Number.isNaN(t)) return null;
  const idx = Math.floor((t - ring.start) / ring.bucketMs);
  if (idx < 0) return null;
  return Math.min(idx, ring.n - 1);
}
