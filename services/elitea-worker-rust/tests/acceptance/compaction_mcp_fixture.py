"""Serve fictional records for isolated, live compaction acceptance.

This fixture performs no external reads or writes. It is not a product server.
Start it only on the rehearsal network with an existing trusted TLS certificate.
"""
import argparse
import json
import os
import ssl
from http.server import BaseHTTPRequestHandler, HTTPServer

TOOL = {
    "name": "read_compaction_record",
    "description": "Read one fictional Cedar archive record. Read indexes 1 through 12 in order. Each record supplies the next index.",
    "inputSchema": {"type": "object", "properties": {"index": {"type": "integer", "minimum": 1, "maximum": 12}},
                    "required": ["index"], "additionalProperties": False},
    "annotations": {"readOnlyHint": True, "destructiveHint": False, "idempotentHint": True, "openWorldHint": False},
}


def record(index, size):
    line = "Archive inspection confirms a matching checksum. This fictional evidence requires no external action.\n"
    return {
        "index": index,
        "next_index": index + 1 if index < 12 else None,
        "delivery_code": "CEDAR-731",
        "color": "blue" if index < 3 else "teal",
        "status": "archive verified",
        "next_step": "prepare the handoff note",
        "evidence": (line * (size // len(line) + 1))[:size],
    }


def respond(body, size):
    if not isinstance(body, dict) or body.get("jsonrpc") != "2.0":
        return {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "Invalid request"}}
    if "id" not in body:
        return None
    method = body.get("method")
    if method == "initialize":
        result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                  "serverInfo": {"name": "elitea-compaction-fixture", "version": "1.0.0"}}
    elif method == "tools/list":
        result = {"tools": [TOOL]}
    elif method == "ping":
        result = {}
    elif method == "tools/call":
        params = body.get("params", {})
        args = params.get("arguments", {}) if isinstance(params, dict) else None
        index = args.get("index") if isinstance(args, dict) else None
        if not isinstance(params, dict) or params.get("name") != TOOL["name"] or type(index) is not int or not 1 <= index <= 12 or set(args) != {"index"}:
            return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32602, "message": "Invalid record index"}}
        result = {"content": [{"type": "text", "text": json.dumps(record(index, size))}], "isError": False}
    else:
        return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "Method not found"}}
    return {"jsonrpc": "2.0", "id": body["id"], "result": result}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        if self.path != "/mcp":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 4096:
                raise ValueError()
            body = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            self.send_error(400)
            return
        result = respond(body, self.server.record_bytes)
        data = json.dumps(result).encode() if result is not None else b""
        self.send_response(200 if result is not None else 202)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        if isinstance(body, dict) and body.get("method") == "tools/call":
            params = body.get("params")
            args = params.get("arguments") if isinstance(params, dict) else None
            index = args.get("index") if isinstance(args, dict) else None
            print(json.dumps({"method": "tools/call", "index": index, "response_bytes": len(data)}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--port", type=int, default=8444)
    parser.add_argument("--record-bytes", type=int, default=48000)
    args = parser.parse_args()
    if os.environ.get("ELITEA_COMPACTION_FIXTURE") != "test-only" or not 1024 <= args.record_bytes <= 256000:
        parser.error("Require explicit test-only enablement and bounded records")
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.cert, args.key)
    with HTTPServer(("0.0.0.0", args.port), Handler) as server:
        server.record_bytes = args.record_bytes
        server.socket = context.wrap_socket(server.socket, server_side=True)
        print("Compaction fixture ready", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    main()
