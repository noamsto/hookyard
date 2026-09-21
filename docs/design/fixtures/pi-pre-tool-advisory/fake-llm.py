#!/usr/bin/env python3
"""Scripted OpenAI-compatible chat endpoint for pi probes.

Request 1 answers with a single bash tool call; every later request answers
with plain text. Every conversation pi sends is appended to REQUEST_LOG, so
what reached the model is read off the wire rather than off a model's reply.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

REQUEST_LOG = sys.argv[2]
COMMAND = os.environ.get("PROBE_COMMAND", "echo PROBE_EXECUTED")
count = 0


def chunk(delta, finish=None):
    return {
        "id": "chatcmpl-PROBE",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": "probe",
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"object":"list","data":[{"id":"probe","object":"model"}]}')

    def do_POST(self):
        global count
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        count += 1
        with open(REQUEST_LOG, "a") as f:
            # The system prompt is identical across requests and names this
            # machine's working directory, so only the conversation is kept.
            messages = [m for m in body.get("messages", []) if m.get("role") != "system"]
            f.write(json.dumps({"request": count, "messages": messages}) + "\n")
        if count == 1:
            chunks = [
                chunk({"role": "assistant", "tool_calls": [{
                    "index": 0, "id": "PROBE-TOOLCALL-01", "type": "function",
                    "function": {"name": "bash", "arguments": json.dumps({"command": COMMAND})},
                }]}),
                chunk({}, "tool_calls"),
            ]
        else:
            chunks = [chunk({"role": "assistant", "content": "done"}), chunk({}, "stop")]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for c in chunks:
            self.wfile.write(f"data: {json.dumps(c)}\n\n".encode())
        self.wfile.write(b"data: [DONE]\n\n")


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
