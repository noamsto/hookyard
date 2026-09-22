import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Same shape as ext-steer.ts, but the steer content is tagged with the
// call's own toolCallId, so a parallel batch's requests show which call each
// steer belongs to (or that they all land together, unordered by call).
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event) => {
    pi.sendMessage(
      { customType: "probe", content: "PROBE_STEER " + event.toolCallId, display: false },
      { deliverAs: "steer" },
    );
  });
}
