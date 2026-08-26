#!/usr/bin/env python3
"""Minimal MCP server for testing the client end to end.

Speaks JSON-RPC 2.0 over newline-delimited stdio, the same transport real
servers use. Declares one tool whose schema covers every type the parameter
coercion has to handle, and echoes back the exact JSON types it received —
which is what lets the test assert that an integer arrived as an integer and
not as the string kinclaw flattened it into.

Also writes to stderr on startup: an unread stderr pipe fills and blocks the
server mid-response, so this doubles as a regression test for draining it.
"""
import json
import sys

TOOL = {
    "name": "echo_types",
    "description": "Echo back the JSON type of each argument received.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "text": {"type": "string"},
            "count": {"type": "integer"},
            "ratio": {"type": "number"},
            "flag": {"type": "boolean"},
            "items": {"type": "array", "items": {"type": "string"}},
            "opts": {"type": "object"},
            "untyped": {},
        },
        "required": ["text"],
    },
}


def reply(msg_id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg_id, "result": result}) + "\n")
    sys.stdout.flush()


def main():
    print("mock server starting", file=sys.stderr, flush=True)
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue

        method = req.get("method")
        msg_id = req.get("id")

        if method == "initialize":
            reply(msg_id, {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "mock", "version": "1.0.0"},
            })
        elif method == "notifications/initialized":
            pass  # notification, no reply
        elif method == "tools/list":
            reply(msg_id, {"tools": [TOOL]})
        elif method == "tools/call":
            args = req.get("params", {}).get("arguments", {})
            # Report the Python type name for each argument, which maps 1:1
            # onto the JSON type the client actually sent.
            types = {k: type(v).__name__ for k, v in sorted(args.items())}
            reply(msg_id, {
                "content": [{"type": "text", "text": json.dumps(types, sort_keys=True)}],
                "isError": False,
            })
        elif msg_id is not None:
            sys.stdout.write(json.dumps({
                "jsonrpc": "2.0", "id": msg_id,
                "error": {"code": -32601, "message": f"unknown method {method}"},
            }) + "\n")
            sys.stdout.flush()


if __name__ == "__main__":
    main()
