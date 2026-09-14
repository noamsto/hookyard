import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { appendFileSync } from "node:fs";

const LOG = "/tmp/pi-probe-order.log";

export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event) => {
    appendFileSync(LOG, `BLOCKER saw ${event.toolName}\n`);
    if (event.toolName === "bash") {
      return { block: true, reason: "BLOCKER_DENY" };
    }
  });
}
