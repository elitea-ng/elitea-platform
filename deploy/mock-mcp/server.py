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

A TEST-ONLY OAUTH AUTHORIZATION SERVER FOR `/mcp-auth` (FU-250). The
authorization-demanding twin can also be signed into, so a browser run proves
Main's real delegated sign-in: discovery (RFC 9728 and RFC 8414 documents),
dynamic client registration (RFC 7591, public clients), the authorization code
with S256 PKCE (RFC 7636), and the code exchange. The mock holds Main to the
real checks: `/authorize` refuses an unknown client, a `redirect_uri` that was
not registered, a missing `state` or a missing S256 challenge; `/token` refuses
a reused or expired code, another client's code, another `redirect_uri` and a
verifier that does not match the challenge. A `resource` indicator, when sent,
must name `/mcp-auth`. Each exchange issues its own random bearer with a
`test-only-` prefix, minted at run time and never written anywhere; `/mcp-auth`
answers like `/mcp` only for a bearer this process issued.

The browser opens `/authorize` itself, so that one endpoint may be served on a
second, browser-facing listener (MOCK_MCP_PUBLIC_PORT, with its own certificate
and key) that answers nothing else. Without that listener `/authorize` is
served on the main port. Discovery, registration and the exchange always stay
on the main port, which only Main and the worker reach.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import ssl
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlencode, urlsplit

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

# The test-only authorization server (see the module docstring). The issuer is
# this server's own origin; the protected resource is `/mcp-auth` on it.
ISSUER = os.environ.get("MOCK_MCP_ISSUER", "https://mcp-mock:8443")
PROTECTED_RESOURCE = ISSUER + AUTH_PATH
PUBLIC_PORT = int(os.environ.get("MOCK_MCP_PUBLIC_PORT") or 0)
PUBLIC_CERT_FILE = os.environ.get("MOCK_MCP_PUBLIC_CERT", "")
PUBLIC_KEY_FILE = os.environ.get("MOCK_MCP_PUBLIC_KEY", "")
AUTHORIZE_URL = os.environ.get("MOCK_MCP_PUBLIC_AUTHORIZE_URL") or ISSUER + "/authorize"

# Every table is bounded: a fixture must not grow without limit.
MAX_ENTRIES = 64
CODE_SECONDS = 120
TOKEN_SECONDS = 3600
MAX_FORM_BYTES = 16 * 1024


class OAuthState:
    """Registered clients, outstanding codes and issued bearers, in memory."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._clients: dict[str, list[str]] = {}
        self._codes: dict[str, dict] = {}
        self._tokens: dict[str, float] = {}

    @staticmethod
    def _bounded(table: dict) -> None:
        while len(table) >= MAX_ENTRIES:
            table.pop(next(iter(table)))

    def register(self, redirect_uris: list[str]) -> str:
        client_id = "test-only-client-" + secrets.token_urlsafe(12)
        with self._lock:
            self._bounded(self._clients)
            self._clients[client_id] = list(redirect_uris)
        return client_id

    def redirect_uris(self, client_id: str) -> list[str] | None:
        with self._lock:
            return self._clients.get(client_id)

    def issue_code(self, grant: dict) -> str:
        code = secrets.token_urlsafe(32)
        with self._lock:
            self._bounded(self._codes)
            self._codes[code] = dict(grant, expires=time.monotonic() + CODE_SECONDS)
        return code

    def take_code(self, code: str) -> dict | None:
        """A code is single-use: it is removed whether or not the exchange succeeds."""
        with self._lock:
            grant = self._codes.pop(code, None)
        if grant is None or grant["expires"] < time.monotonic():
            return None
        return grant

    def issue_token(self) -> str:
        token = "test-only-" + secrets.token_urlsafe(24)
        with self._lock:
            self._bounded(self._tokens)
            self._tokens[token] = time.monotonic() + TOKEN_SECONDS
        return token

    def token_valid(self, token: str) -> bool:
        with self._lock:
            expires = self._tokens.get(token)
        return expires is not None and expires >= time.monotonic()


OAUTH = OAuthState()


def _pkce_matches(verifier: str, challenge: str) -> bool:
    if not 43 <= len(verifier) <= 128:
        return False
    digest = hashlib.sha256(verifier.encode("ascii", "replace")).digest()
    expected = base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")
    return secrets.compare_digest(expected, challenge)


def _protected_resource_metadata() -> dict:
    return {
        "resource": PROTECTED_RESOURCE,
        "authorization_servers": [ISSUER],
        "bearer_methods_supported": ["header"],
    }


def _authorization_server_metadata() -> dict:
    return {
        "issuer": ISSUER,
        "authorization_endpoint": AUTHORIZE_URL,
        "token_endpoint": ISSUER + "/token",
        "registration_endpoint": ISSUER + "/register",
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none"],
    }

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

# A SECOND tool name, so two connections to this server can be attached to one
# agent without colliding.
#
# An agent exposes its tools to the model as one flat list of function names,
# so two MCP connections that both publish `echo` give it two functions with
# one name — which ends the turn before any model round trip. That collision is
# a real defect in its own right, and it is NOT what the authorization journey
# is about: `chat.mcp-auth-prompt.spec.ts` needs a SECOND connection only as
# the control that the message names the one that actually challenged. Giving
# this server a second tool lets that control exist without the case measuring
# the collision by accident.
REVERSE_TOOL = {
    "name": "reverse",
    "description": (
        "Return the text passed in, reversed. Exists so a second connection to "
        "this server contributes a DIFFERENT function name than `echo`."
    ),
    "inputSchema": {
        "type": "object",
        "properties": {
            "text": {"type": "string", "description": "The text to reverse."}
        },
        "required": ["text"],
        "additionalProperties": False,
    },
}

TOOLS = [ECHO_TOOL, REVERSE_TOOL]


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
    if name not in (ECHO_TOOL["name"], REVERSE_TOOL["name"]):
        return _error(request_id, -32602, "unknown tool")
    arguments = params.get("arguments")
    arguments = arguments if isinstance(arguments, dict) else {}
    text = arguments.get("text")
    text = text if isinstance(text, str) else json.dumps(arguments, sort_keys=True)
    if name == REVERSE_TOOL["name"]:
        text = text[::-1]
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

    def _authorized(self) -> bool:
        """Does the request carry a bearer this process issued?"""
        header = self.headers.get("Authorization") or ""
        scheme, _, token = header.partition(" ")
        return scheme.lower() == "bearer" and OAUTH.token_valid(token.strip())

    def _send_oauth_error(self, code: int, error: str) -> None:
        body = json.dumps({"error": error}).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)
        self.log_message("%s", "oauth refused: " + error)

    def _read_body(self) -> bytes | None:
        length = int(self.headers.get("Content-Length") or 0)
        if length < 0 or length > MAX_FORM_BYTES:
            return None
        return self.rfile.read(length) if length > 0 else b""

    def _authorize(self) -> None:
        """Issue a code for a registered client and redirect URI, with state and S256 PKCE.

        An unknown client or redirect URI is answered here, never redirected
        (RFC 6749 §4.1.2.1). Consent is implicit: this is a fixture.
        """
        query = {key: values[0] for key, values in parse_qs(urlsplit(self.path).query).items()}
        client_id = query.get("client_id", "")
        redirect_uri = query.get("redirect_uri", "")
        registered = OAUTH.redirect_uris(client_id)
        if registered is None:
            self._send_oauth_error(400, "invalid_client")
            return
        if redirect_uri not in registered:
            self._send_oauth_error(400, "invalid_redirect_uri")
            return
        state = query.get("state", "")
        challenge = query.get("code_challenge", "")
        resource = query.get("resource")
        if (
            query.get("response_type") != "code"
            or not state
            or query.get("code_challenge_method") != "S256"
            or not 43 <= len(challenge) <= 128
            or (resource is not None and resource != PROTECTED_RESOURCE)
        ):
            self._send_oauth_error(400, "invalid_request")
            return
        code = OAUTH.issue_code(
            {"client_id": client_id, "redirect_uri": redirect_uri, "challenge": challenge, "resource": resource}
        )
        separator = "&" if "?" in redirect_uri else "?"
        self.send_response(302)
        self.send_header("Location", redirect_uri + separator + urlencode({"code": code, "state": state}))
        self.send_header("Content-Length", "0")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.log_message("%s", "authorize: code issued")

    def _token(self) -> None:
        raw = self._read_body()
        if raw is None:
            self._send_oauth_error(400, "invalid_request")
            return
        form = {key: values[0] for key, values in parse_qs(raw.decode("utf-8", "replace")).items()}
        if form.get("grant_type") != "authorization_code":
            self._send_oauth_error(400, "unsupported_grant_type")
            return
        grant = OAUTH.take_code(form.get("code", ""))
        resource = form.get("resource")
        if (
            grant is None
            or grant["client_id"] != form.get("client_id")
            or grant["redirect_uri"] != form.get("redirect_uri")
            or not _pkce_matches(form.get("code_verifier", ""), grant["challenge"])
            or (resource is not None and resource != PROTECTED_RESOURCE)
        ):
            self._send_oauth_error(400, "invalid_grant")
            return
        body = json.dumps(
            {"access_token": OAUTH.issue_token(), "token_type": "Bearer", "expires_in": TOKEN_SECONDS}
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)
        self.log_message("%s", "token: bearer issued")

    def _register(self) -> None:
        raw = self._read_body()
        try:
            metadata = json.loads(raw.decode("utf-8")) if raw else None
        except (UnicodeDecodeError, json.JSONDecodeError):
            metadata = None
        uris = metadata.get("redirect_uris") if isinstance(metadata, dict) else None
        if (
            not isinstance(uris, list)
            or not 1 <= len(uris) <= 4
            or not all(isinstance(uri, str) and urlsplit(uri).scheme in ("http", "https") for uri in uris)
        ):
            self._send_oauth_error(400, "invalid_redirect_uri")
            return
        client_id = OAUTH.register(uris)
        self._send_json(
            201,
            {
                "client_id": client_id,
                "client_id_issued_at": int(time.time()),
                "redirect_uris": uris,
                "token_endpoint_auth_method": "none",
                "grant_types": ["authorization_code"],
                "response_types": ["code"],
            },
        )

    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        path = self.path.split("?", 1)[0]
        if path == AUTH_PATH:
            if self._authorized():
                self._send_empty(405)
                return
            self._send_auth_challenge()
            return
        if path in ("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource" + AUTH_PATH):
            self._send_json(200, _protected_resource_metadata())
            return
        if path == "/.well-known/oauth-authorization-server":
            self._send_json(200, _authorization_server_metadata())
            return
        if path == "/authorize" and not PUBLIC_PORT:
            self._authorize()
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
        path = self.path.split("?", 1)[0]
        if path == "/token":
            self._token()
            return
        if path == "/register":
            self._register()
            return
        if path == AUTH_PATH and not self._authorized():
            # Refused BEFORE the body is read: an OAuth-protected server does
            # not need to see the request to know the caller has no token, and
            # answering at the same point for `initialize` and for a later call
            # keeps the challenge independent of how far the session got.
            self._send_auth_challenge()
            return
        if path not in ("/mcp", AUTH_PATH):
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


class PublicHandler(Handler):
    """The browser-facing listener: `/authorize` and nothing else."""

    # One request per connection: a refused request's unread body can never be
    # parsed as the next request.
    protocol_version = "HTTP/1.0"

    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        if self.path.split("?", 1)[0] == "/authorize":
            self._authorize()
            return
        self._send_empty(404)

    def do_POST(self) -> None:  # noqa: N802 - stdlib name
        self._send_empty(404)

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib name
        self._send_empty(404)


def _tls_server(port: int, handler, cert_file: str, key_file: str) -> LoggingTLSServer:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    # PROTOCOL_TLS_SERVER negotiates DOWN to TLSv1/TLSv1.1 on builds whose
    # OpenSSL still enables them, which CodeQL flags as py/insecure-protocol
    # (high). This is a mock the e2e stack talks to over the compose network,
    # so nothing here is exposed — but the client under test is the real
    # gateway, and pinning the floor keeps the mock from being the one place
    # that would still accept a protocol production refuses.
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(certfile=cert_file, keyfile=key_file)
    server = LoggingTLSServer(("0.0.0.0", port), handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    return server


def main() -> int:
    material = [CERT_FILE, KEY_FILE] + ([PUBLIC_CERT_FILE, PUBLIC_KEY_FILE] if PUBLIC_PORT else [])
    for path in material:
        if not path or not os.path.exists(path):
            sys.stderr.write("mock-mcp: missing TLS material at %s\n" % path)
            return 1
    server = _tls_server(PORT, Handler, CERT_FILE, KEY_FILE)
    if PUBLIC_PORT:
        public = _tls_server(PUBLIC_PORT, PublicHandler, PUBLIC_CERT_FILE, PUBLIC_KEY_FILE)
        threading.Thread(target=public.serve_forever, name="mock-mcp-public", daemon=True).start()
        sys.stderr.write("mock-mcp: serving /authorize on https :%d\n" % PUBLIC_PORT)
    sys.stderr.write("mock-mcp: serving https on :%d\n" % PORT)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
