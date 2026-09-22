import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Allows the call while returning every content-bearing key a handler might
// plausibly reach for, each with its own marker, so the request log shows which
// (if any) pi forwards to the model.
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async () => ({
    block: false,
    reason: "PROBE_ALLOW_REASON",
    content: [{ type: "text", text: "PROBE_ALLOW_CONTENT" }],
    message: { customType: "probe", content: "PROBE_ALLOW_MESSAGE", display: false },
    additionalContext: "PROBE_ALLOW_ADDITIONALCONTEXT",
    systemMessage: "PROBE_ALLOW_SYSTEMMESSAGE",
  }));
}
