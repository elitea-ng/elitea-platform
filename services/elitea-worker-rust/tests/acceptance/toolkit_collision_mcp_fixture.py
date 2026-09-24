"""TLS-only, read-only MCP fixture for toolkit identity acceptance."""
import argparse
import json
import os
import ssl
from http.server import BaseHTTPRequestHandler, HTTPServer

SOURCES = {"/release": "RELEASE-731", "/audit": "AUDIT-942"}
TOOL = {
    "name": "lookup_record",
    "description": "Return the exact source marker from this toolkit.",
    "inputSchema": {"type": "object", "properties": {"probe": {"type": "string", "enum": ["identity"]}}, "required": ["probe"], "additionalProperties": False},
    "annotations": {"readOnlyHint": True, "destructiveHint": False,
                    "idempotentHint": True, "openWorldHint": False},
}


def respond(path, body):
    if path not in SOURCES or not isinstance(body, dict) or body.get("jsonrpc") != "2.0":
        return {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "Invalid request"}}
    if "id" not in body:
        return None
    method = body.get("method")
    if method == "initialize":
        result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                  "serverInfo": {"name": "toolkit-collision-fixture", "version": "1.0.0"}}
    elif method == "tools/list":
        result = {"tools": [TOOL]}
    elif method == "ping":
        result = {}
    elif method == "tools/call":
        params = body.get("params", {})
        if not isinstance(params, dict) or params.get("name") != TOOL["name"] or params.get("arguments") != {"probe": "identity"}:
            return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32602, "message": "Invalid tool request"}}
        result = {"content": [{"type": "text", "text": json.dumps({"source": path[1:], "marker": SOURCES[path]})}], "isError": False}
    else:
        return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "Method not found"}}
    return {"jsonrpc": "2.0", "id": body["id"], "result": result}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 4096:
                raise ValueError()
            body = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            self.send_error(400)
            return
        result = respond(self.path, body)
        data = json.dumps(result).encode() if result is not None else b""
        self.send_response(200 if result is not None else 202)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        if isinstance(body, dict) and body.get("method") == "tools/call":
            params = body.get("params", {})
            arguments = params.get("arguments") if isinstance(params, dict) else None
            print(json.dumps({"source": self.path, "success": bool(result and "result" in result),
                              "tool": params.get("name") if isinstance(params, dict) else None,
                              "argument_type": type(arguments).__name__,
                              "argument_keys": sorted(arguments) if isinstance(arguments, dict) else []}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--port", type=int, default=8445)
    args = parser.parse_args()
    if os.environ.get("ELITEA_COLLISION_FIXTURE") != "test-only":
        parser.error("Require explicit test-only enablement")
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.cert, args.key)
    with HTTPServer(("0.0.0.0", args.port), Handler) as server:
        server.socket = context.wrap_socket(server.socket, server_side=True)
        print("Collision fixture ready", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    main()
