// Placeholder entry point — Step 4 replaces this with the real React tree.
// Referencing bridge.ts keeps esbuild from dropping the "../app.js" import,
// so the bundle carries the literal specifier `external` needs to preserve.
import "@xyflow/react/dist/style.css";
import { activeFilters } from "./bridge.ts";

export function _unused(): void {
  activeFilters();
}
