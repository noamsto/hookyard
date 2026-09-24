// Runs ELK layout in a Web Worker (elkjs/lib/elk-api + workerUrl), so the
// layered algorithm never blocks the main thread.
import ELK from "elkjs/lib/elk-api.js";
import type { ElkNode } from "elkjs/lib/elk-api.js";
import type { LayoutInput, LayoutResult } from "../types.ts";
import { fromElk, toElkGraph } from "./elkGraph.ts";

export function createLayout(workerUrl = "/static/flow/elk-worker.js"): (input: LayoutInput) => Promise<LayoutResult> {
  const elk = new ELK({ workerUrl });
  return async (input: LayoutInput): Promise<LayoutResult> => {
    const out = await elk.layout(toElkGraph(input));
    return fromElk(out as ElkNode, input.gen);
  };
}
