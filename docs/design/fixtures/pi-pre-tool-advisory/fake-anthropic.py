#!/usr/bin/env python3
"""Scripted Anthropic Messages endpoint for the Claude Code parity probe.

The first tool-bearing request answers with a single Bash tool_use; every
later one answers with plain text. Requests without tools (Claude Code's
side calls: titles, quota checks) get plain text and are not logged. Each
logged line is the conversation Claude Code sent, elided to its PROBE
markers, so what reached the model is read off the wire.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

REQUEST_LOG = sys.argv[2]
count = 0


STRUCTURAL_KEYS = {"type", "id", "name", "input", "tool_use_id", "is_error", "cache_control", "role"}


def marked_lines(text):
    lines = [ln for ln in text.splitlines() if "PROBE" in ln or "Run the probe." in ln]
    return "\n".join(lines) if lines else "[elided]"


def elide(content):
    """Fails closed: keeps only the allow-listed structural keys verbatim and
    PROBE-marked lines of text/content strings; every other key and every
    other block type (thinking, redacted_thinking, a list-valued tool_result
    content, ...) is recursed into or replaced with "[elided]", so a shape
    this fixture doesn't special-case can't leak this machine's paths,
    skills, or plugins into a public repo."""
    if isinstance(content, str):
        content = [{"type": "text", "text": content}]
    if not isinstance(content, list):
        return "[elided]"
    kept = []
    for block in content:
        if not isinstance(block, dict):
            kept.append("[elided]")
            continue
        out = {}
        for key, value in block.items():
            if key in STRUCTURAL_KEYS:
                out[key] = value
            elif key in ("text", "content") and isinstance(value, list):
                out[key] = elide(value)
            elif key in ("text", "content") and isinstance(value, str):
                out[key] = marked_lines(value)
            else:
                out[key] = "[elided]"
        kept.append(out)
    return kept


def events(blocks, stop_reason):
    yield "message_start", {"type": "message_start", "message": {
        "id": "msg_PROBE", "type": "message", "role": "assistant", "model": "claude-probe",
        "content": [], "stop_reason": None, "stop_sequence": None,
        "usage": {"input_tokens": 1, "output_tokens": 1}}}
    for i, block in enumerate(blocks):
        if block["type"] == "tool_use":
            yield "content_block_start", {"type": "content_block_start", "index": i,
                "content_block": {"type": "tool_use", "id": block["id"], "name": block["name"], "input": {}}}
            yield "content_block_delta", {"type": "content_block_delta", "index": i,
                "delta": {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}}
        else:
            yield "content_block_start", {"type": "content_block_start", "index": i,
                "content_block": {"type": "text", "text": ""}}
            yield "content_block_delta", {"type": "content_block_delta", "index": i,
                "delta": {"type": "text_delta", "text": block["text"]}}
        yield "content_block_stop", {"type": "content_block_stop", "index": i}
    yield "message_delta", {"type": "message_delta", "delta": {"stop_reason": stop_reason, "stop_sequence": None},
        "usage": {"output_tokens": 1}}
    yield "message_stop", {"type": "message_stop"}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        global count
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "count_tokens" in self.path:
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"input_tokens":1}')
            return
        blocks, stop = [{"type": "text", "text": "done"}], "end_turn"
        if body.get("tools"):
            count += 1
            with open(REQUEST_LOG, "a") as f:
                messages = [{"role": m["role"], "content": elide(m["content"])} for m in body.get("messages", [])]
                f.write(json.dumps({"request": count, "messages": messages}) + "\n")
            if count == 1:
                blocks = [{"type": "tool_use", "id": "toolu_PROBE01", "name": "Bash",
                           "input": {"command": "echo PROBE_EXECUTED", "description": "probe"}}]
                stop = "tool_use"
        if not body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"id": "msg_PROBE", "type": "message", "role": "assistant",
                "model": "claude-probe", "content": blocks, "stop_reason": stop, "stop_sequence": None,
                "usage": {"input_tokens": 1, "output_tokens": 1}}).encode())
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for name, data in events(blocks, stop):
            self.wfile.write(f"event: {name}\ndata: {json.dumps(data)}\n\n".encode())


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
