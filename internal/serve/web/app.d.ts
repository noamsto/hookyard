// Declaration file for the hand-written internal/serve/assets/app.js (SPEC
// 4.8's non-bundled shell): TS resolves the relative specifier "../app.js"
// from src/* to this file. A relative ambient `declare module "../app.js"`
// is TS2436 (relative paths aren't allowed in ambient module declarations),
// so this is a real declaration file instead [plan-critic r1 HIGH].
import type { Filters, PathLike } from "./src/types.ts";

export declare function filterParams(): URLSearchParams;
export declare function eventLabel(rec: PathLike): string;
export declare function toggleFilter(field: string, value: string, additive?: boolean): void;
export declare function activeFilters(): Filters;
export declare function syncState(): { day: string; live: boolean } | null;
