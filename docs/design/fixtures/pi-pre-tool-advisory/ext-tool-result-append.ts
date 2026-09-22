import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Mechanism B at the pi level: stash advice keyed by toolCallId at tool_call
// (allow), then append it as a text block onto that same call's own
// tool_result — no steer queue, one advisory per call, attached to the call.
export default function (pi: ExtensionAPI) {
  const stash = new Map<string, string>();

  pi.on("tool_call", async (event) => {
    stash.set(event.toolCallId, "PROBE_APPEND " + event.toolCallId);
  });

  pi.on("tool_result", async (event) => {
    const advice = stash.get(event.toolCallId);
    if (advice === undefined) return;
    stash.delete(event.toolCallId);
    return { content: [...event.content, { type: "text", text: advice }] };
  });

  pi.on("turn_end", async () => {
    stash.clear();
  });
}
