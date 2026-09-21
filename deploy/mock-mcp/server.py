"""Deterministic remote MCP server for the standalone stack.

WHY THIS EXISTS. `services/elitea-worker-rust/src/toolkits/mcp.rs` is the one
toolkit family in the native runtime that reaches a THIRD-PARTY process over
the network before an agent can answer: `AdkHttpMcpConnector::connect`
(mcp.rs:215-248) opens a Streamable HTTP session, and ADK then asks the server
for its catalogue. Every other family is materialized from stored settings
alone. Without a server on the other end there is no way to tell a runtime
that speaks MCP from one that refuses it — both produce a failed turn — so an
end-to-end MCP journey needs a real, if tiny, MCP server inside the stack.

WHAT IT SERVES. The MCP Streamable HTTP transport, which is JSON-RPC 2.0 over
plain HTTP POST, at ONE path:

  POST /mcp    initialize | notifications/initialized | tools/list | tools/call
  GET  /mcp    405, which rmcp reads as "server does not support SSE" and skips
               (rmcp-3.1.2 `streamable_http_client.rs:735`) — this mock has no
               server-initiated messages to push
  DELETE /mcp  405, likewise tolerated (`common/reqwest/streamable_http_client.rs:163`)
  GET  /healthz  for the compose healthcheck

Every served JSON-RPC method is logged to stderr, so `podman logs
elitea-standalone-mcp-mock-1` tells "the worker connected and discovered the
catalogue" from "the worker never dialled" — two states that look identical
from an agent transcript when the model does not call the tool. A TLS
handshake this server rejects is logged too; see `LoggingTLSServer`.

Responses are `application/json`, never SSE. That is a shape the client
explicitly accepts (`StreamableHttpPostResponse::Json`, same file :302-310) and
it keeps this mock a request/response server with no streaming state.

WHY HTTPS, AND WHY A BAKED CERTIFICATE. The worker's MCP client is built with
`.https_only(true)` (mcp.rs:221) and `parse_endpoint` rejects any scheme but
`https` (mcp.rs:599). There is no verification switch: `ssl_verify: false` in a
toolkit's settings is refused outright (mcp.rs:146-153), and the client sets no
`danger_accept_invalid_certs`. So the endpoint must be real TLS with a
certificate the worker's trust store accepts. The certificate and its key are
generated at IMAGE BUILD time (see Containerfile) rather than at container
start, so the same bytes are served across restarts and the trust bundle handed
to the worker never goes stale mid-run.

The tool catalogue is deliberately tiny and deterministic: one `echo` tool that
returns its own argument. A test asserts on a string it supplied.
"""

from __future__ import annotations

import json
import os
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("MOCK_MCP_PORT", "8443"))
CERT_FILE = os.environ.get("MOCK_MCP_CERT", "/opt/mock-mcp/tls/server-chain.crt")
KEY_FILE = os.environ.get("MOCK_MCP_KEY", "/opt/mock-mcp/tls/server.key")
SERVER_NAME = "elitea-mock-mcp"
SERVER_VERSION = "1.0.0"

# The newest revision `services/elitea-main/internal/mcpregistry/discover.go:31`
# announces, and one rmcp-3.1.2 knows by name (`model.rs:214`). A version rmcp
# does not recognise still deserializes — it keeps the string — but answering
# with one both halves of this repository already agree on keeps the mock from
# being the thing under test.
DEFAULT_PROTOCOL_VERSION = "2025-06-18"
KNOWN_PROTOCOL_VERSIONS = frozenset(
    {"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"}
)

# One fixed session id. The spec allows the server to omit `Mcp-Session-Id`
# entirely, but returning one exercises the header round trip the client
# performs on every subsequent POST.
SESSION_ID = "elitea-mock-mcp-session"

# The authorization-demanding twin of `/mcp`. A toolkit pointed here gets a 401
# with a `WWW-Authenticate` challenge on every request; a toolkit pointed at
# `/mcp` is unaffected. See `_send_auth_challenge`.
AUTH_PATH = "/mcp-auth"

# The `resource_metadata` the challenge advertises. The worker parses it out of
# the header and carries it in the delegated authorization requirement, so it
# has to be a well-formed absolute URL on this server's own origin — a relative
# or malformed value is dropped and the requirement loses the field.
RESOURCE_METADATA_URL = os.environ.get(
    "MOCK_MCP_RESOURCE_METADATA_URL",
    "https://mcp-mock:8443/.well-known/oauth-protected-resource",
)

ECHO_TOOL = {
    "name": "echo",
    "description": (
        "Return the text passed in. Deterministic, so a test can assert on a "
        "value it supplied rather than on a fixed string."
    ),
    "inputSchema": {
        "type": "object",
        "properties": {
            "text": {"type": "string", "description": "The text to echo back."}
        },
        "required": ["text"],
        "additionalProperties": False,
    },
}

TOOLS = [ECHO_TOOL]


def _result(request_id, result: dict) -> dict:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def _error(request_id, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}}


def _initialize(params: dict) -> dict:
    requested = params.get("protocolVersion")
    version = (
        requested
        if isinstance(requested, str) and requested in KNOWN_PROTOCOL_VERSIONS
        else DEFAULT_PROTOCOL_VERSION
    )
    return {
        "protocolVersion": version,
        # `listChanged: false` is the honest answer: this catalogue is a
        # constant, so there is nothing for the client to subscribe to. It is
        # also why refusing the GET stream below costs nothing.
        "capabilities": {"tools": {"listChanged": False}},
        "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
    }


def _tools_call(params: dict, request_id):
    name = params.get("name")
    if name != ECHO_TOOL["name"]:
        return _error(request_id, -32602, "unknown tool")
    arguments = params.get("arguments")
    arguments = arguments if isinstance(arguments, dict) else {}
    text = arguments.get("text")
    text = text if isinstance(text, str) else json.dumps(arguments, sort_keys=True)
    return _result(
        request_id,
        {"content": [{"type": "text", "text": text}], "isError": False},
    )


def _dispatch(message: dict):
    """Answer one JSON-RPC message, or return None for a notification.

    A notification (no `id`) must NOT be answered with a JSON-RPC body — the
    client turns a 202/204, or an empty 200, into `Accepted`
    (rmcp-3.1.2 `common/reqwest/streamable_http_client.rs:243-266`).
    """
    method = message.get("method")
    request_id = message.get("id")
    params = message.get("params")
    params = params if isinstance(params, dict) else {}
    sys.stderr.write("mock-mcp jsonrpc %s\n" % method)

    if request_id is None:
        return None

    if method == "initialize":
        return _result(request_id, _initialize(params))
    if method == "tools/list":
        # No `nextCursor`: the whole catalogue fits in one page, and a cursor
        # the client then had to follow would make the mock the thing under
        # test rather than the runtime.
        return _result(request_id, {"tools": TOOLS})
    if method == "tools/call":
        return _tools_call(params, request_id)
    if method == "ping":
        return _result(request_id, {})
    return _error(request_id, -32601, "method not found")


class Handler(BaseHTTPRequestHandler):
    # HTTP/1.1: `reqwest-mcp` is built with `default-features = false` and does
    # not enable `http2`, so the client never offers h2. Declaring 1.1 also
    # turns on keep-alive, which matters because the client reuses one
    # connection for initialize + tools/list.
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003 - stdlib name
        sys.stderr.write("mock-mcp %s\n" % (fmt % args))

    def _send(self, code: int, body: bytes, content_type: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Mcp-Session-Id", SESSION_ID)
        self.end_headers()
        if body:
            self.wfile.write(body)

    def _send_json(self, code: int, payload) -> None:
        self._send(code, json.dumps(payload).encode("utf-8"), "application/json")

    def _send_empty(self, code: int) -> None:
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.send_header("Mcp-Session-Id", SESSION_ID)
        self.end_headers()

    def _send_auth_challenge(self) -> None:
        """Refuse with the 401 an OAuth-protected MCP server sends.

        THE POINT OF THE PATH. `AUTH_PATH` is a SECOND endpoint on the same
        server rather than a mode switch on `/mcp`, so a toolkit chooses the
        behaviour by the URL it is configured with and no global state can
        leak from one journey into another running beside it. `/mcp` keeps
        answering exactly as it did.

        THE HEADER IS THE CONTRACT, not the status. The worker's client reports
        `is_authorization_required()` for a 401 carrying a `WWW-Authenticate`
        challenge, and `toolkits/mcp.rs::authorization_required` then parses
        `resource_metadata` out of that challenge to build the delegated
        authorization requirement the agent surfaces — carrying the TOOLKIT
        NAME. A bare 401 with no header is just a failed dial.
        """
        body = json.dumps(
            {"error": "unauthorized", "error_description": "this MCP server requires authorization"}
        ).encode("utf-8")
        self.send_response(401)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header(
            "WWW-Authenticate",
            'Bearer realm="mock-mcp", '
            f'resource_metadata="{RESOURCE_METADATA_URL}"',
        )
        self.end_headers()
        self.wfile.write(body)
        self.log_message("%s", "401 authorization required on " + AUTH_PATH)

    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        if self.path.split("?", 1)[0] == AUTH_PATH:
            self._send_auth_challenge()
            return
        if self.path.split("?", 1)[0] == "/healthz":
            self._send_json(200, {"ok": True})
            return
        if self.path.split("?", 1)[0] == "/mcp":
            # 405 is the documented way to say "no server-initiated stream";
            # rmcp logs it and carries on with POST-only traffic.
            self._send_empty(405)
            return
        self._send_empty(404)

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib name
        # Session teardown. Answering 405 is explicitly tolerated by the client.
        self._send_empty(405)

    def do_POST(self) -> None:  # noqa: N802 - stdlib name
        if self.path.split("?", 1)[0] == AUTH_PATH:
            # Refused BEFORE the body is read: an OAuth-protected server does
            # not need to see the request to know the caller has no token, and
            # answering at the same point for `initialize` and for a later call
            # keeps the challenge independent of how far the session got.
            self._send_auth_challenge()
            return
        if self.path.split("?", 1)[0] != "/mcp":
            self._send_empty(404)
            return
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b""
        try:
            message = json.loads(raw.decode("utf-8")) if raw else None
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._send_json(400, _error(None, -32700, "parse error"))
            return

        if isinstance(message, list):
            replies = [r for r in (_dispatch(m) for m in message if isinstance(m, dict)) if r]
            if not replies:
                self._send_empty(202)
                return
            self._send_json(200, replies)
            return

        if not isinstance(message, dict):
            self._send_json(400, _error(None, -32600, "invalid request"))
            return

        reply = _dispatch(message)
        if reply is None:
            self._send_empty(202)
            return
        self._send_json(200, reply)


class LoggingTLSServer(ThreadingHTTPServer):
    """A server that says so when a TLS handshake fails.

    `socketserver` performs the handshake inside `get_request()` (the listening
    socket is TLS-wrapped, so `accept()` completes it), and `ssl.SSLError` is a
    subclass of `OSError`, which `_handle_request_noblock` swallows without a
    word. A client whose trust store rejects this certificate would therefore
    leave NO trace here and NO usable error at the caller — the worker maps
    every connect failure to one data-free `dependency_unavailable`
    (`toolkits/mcp.rs:247`). Two silences on the same event is how a trust-store
    mistake gets misread as "the mock is down".
    """

    def get_request(self):
        try:
            return super().get_request()
        except ssl.SSLError as error:
            sys.stderr.write("mock-mcp: TLS handshake failed: %s\n" % error)
            raise


def main() -> int:
    for path in (CERT_FILE, KEY_FILE):
        if not os.path.exists(path):
            sys.stderr.write("mock-mcp: missing TLS material at %s\n" % path)
            return 1
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    # PROTOCOL_TLS_SERVER negotiates DOWN to TLSv1/TLSv1.1 on builds whose
    # OpenSSL still enables them, which CodeQL flags as py/insecure-protocol
    # (high). This is a mock the e2e stack talks to over the compose network,
    # so nothing here is exposed — but the client under test is the real
    # gateway, and pinning the floor keeps the mock from being the one place
    # that would still accept a protocol production refuses.
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(certfile=CERT_FILE, keyfile=KEY_FILE)
    server = LoggingTLSServer(("0.0.0.0", PORT), Handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    sys.stderr.write("mock-mcp: serving https on :%d\n" % PORT)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
