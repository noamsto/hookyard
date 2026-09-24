import assert from "node:assert/strict";
import { test } from "node:test";
import { advanceRing, bucketIndex } from "./ring.ts";

const MIN = 60_000;

test("advanceRing shifts by whole elapsed buckets and zero-fills", () => {
  const ring = { start: 0, bucketMs: MIN, n: 3 };
  const counts = [[1, 2, 3], [4, 5, 6]];
  assert.equal(advanceRing(ring, counts, 2 * MIN + 59_000), false); // still in the last bucket
  assert.equal(advanceRing(ring, counts, 4 * MIN + 1), true);
  assert.deepEqual(counts, [[3, 0, 0], [6, 0, 0]]);
  assert.equal(ring.start, 2 * MIN);
});

test("advanceRing past the whole window clears everything", () => {
  const ring = { start: 0, bucketMs: MIN, n: 3 };
  const counts = [[1, 2, 3]];
  assert.equal(advanceRing(ring, counts, 100 * MIN), true);
  assert.deepEqual(counts, [[0, 0, 0]]);
  assert.equal(ring.start, 98 * MIN);
});

test("bucketIndex: clamps past the end, rejects before the start and bad ts", () => {
  const start = Date.parse("2026-09-24T12:00:00Z");
  const ring = { start, bucketMs: MIN, n: 10 };
  assert.equal(bucketIndex(ring, "2026-09-24T12:03:59Z"), 3);
  assert.equal(bucketIndex(ring, "2026-09-24T13:00:00Z"), 9);
  assert.equal(bucketIndex(ring, "2026-09-24T11:59:59Z"), null);
  assert.equal(bucketIndex(ring, "nope"), null);
});
