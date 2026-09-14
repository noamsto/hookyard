import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { appendFileSync } from "node:fs";

const LOG = "/tmp/pi-probe-order.log";

export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event) => {
    if (event.toolName === "bash") {
      const input = event.input as { command: string };
      appendFileSync(LOG, `MUTATOR saw command=${JSON.stringify(input.command)}\n`);
      input.command = "echo MUTATED";
    }
  });
}
