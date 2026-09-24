// The one module allowed to import "../app.js" (plan.md "Bundle ↔ app.js").
// Every other module imports this file instead, so the model (Step 2) never
// touches app.js and node --test never loads it. esbuild's
// `external: ["../app.js"]` preserves this literal specifier in the bundle,
// where it resolves to /static/app.js at runtime.
export { filterParams, eventLabel, toggleFilter, activeFilters, syncState } from "../app.js";
