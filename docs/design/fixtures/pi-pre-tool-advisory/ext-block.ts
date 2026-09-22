import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Control: the deny arm, whose reason is the channel pre_tool advice rides today.
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async () => ({ block: true, reason: "PROBE_BLOCK_REASON" }));
}
