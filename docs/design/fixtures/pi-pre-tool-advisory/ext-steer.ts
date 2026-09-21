import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Allows the call, but queues a custom message from inside tool_call — the one
// side channel an extension has into model context while a tool is pending.
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async () => {
    pi.sendMessage({ customType: "probe", content: "PROBE_STEER", display: false }, { deliverAs: "steer" });
  });
}
