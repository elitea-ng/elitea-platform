"""Test-only OAuth issuer, stored-client OpenAPI, and DCR MCP resources."""

from __future__ import annotations

import base64
from collections import Counter
import hashlib
import html
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import re
import secrets
import signal
import ssl
import threading
import time
from urllib.parse import parse_qs, urlencode, urlsplit

MODES = ("stored", "dcr-public", "dcr-secret")
MAX_BODY = 16 * 1024
MAX_ENTRIES = 256
SCOPES = {"records.read", "offline_access"}


class ProtocolError(Exception):
    def __init__(self, code, status=400):
        self.code, self.status = code, status


def scalar_form(raw):
    values = parse_qs(raw, keep_blank_values=True, max_num_fields=32)
    if any(len(items) != 1 for items in values.values()):
        raise ProtocolError("invalid_request")
    return {key: items[0] for key, items in values.items()}


class Issuer:
    def __init__(self, internal_origin, browser_origin, callback, client_secret, token_ttl=60, clock=time.monotonic):
        self.internal = internal_origin.rstrip("/")
        self.browser = browser_origin.rstrip("/")
        self.callback, self.clock, self.token_ttl = callback, clock, token_ttl
        self.lock = threading.Lock()
        self.clients = {
            ("stored", "emulator-stored-client"): client_secret,
        }
        self.codes, self.pending, self.access, self.refresh = {}, {}, {}, {}
        self.counts = {mode: Counter() for mode in MODES}

    def bounded_put(self, target, key, value, ttl):
        now = self.clock()
        for expired in [item for item, record in target.items() if record[0] <= now]:
            del target[expired]
        if len(target) >= MAX_ENTRIES:
            raise ProtocolError("temporarily_unavailable", 503)
        target[key] = (now + ttl, value)

    def get(self, target, key):
        record = target.get(key)
        if record is None or record[0] <= self.clock():
            raise ProtocolError("invalid_grant")
        return record[1]

    def metadata(self, mode):
        issuer = f"{self.internal}/{mode}"
        result = {
            "issuer": issuer,
            "authorization_endpoint": f"{self.browser}/{mode}/authorize",
            "token_endpoint": f"{issuer}/token",
            "revocation_endpoint": f"{issuer}/revoke",
            "response_types_supported": ["code"],
            "grant_types_supported": ["authorization_code", "refresh_token"] + (["client_credentials"] if mode == "stored" else []),
            "code_challenge_methods_supported": ["S256"],
            "token_endpoint_auth_methods_supported": ["none"] if mode == "dcr-public" else ["client_secret_post", "client_secret_basic"],
            "scopes_supported": sorted(SCOPES),
        }
        if mode.startswith("dcr-"):
            result["registration_endpoint"] = f"{issuer}/register"
        return result

    def spec(self, mode):
        if mode != "stored":
            raise ProtocolError("not_found", 404)
        return {
            "openapi": "3.0.3",
            "info": {"title": f"OAuth Emulator {mode}", "version": "1.0.0"},
            "servers": [{"url": f"{self.internal}/{mode}/api"}],
            "paths": {"/echo": {"get": {
                "operationId": "echo_marker",
                "description": "Read and echo the supplied marker after toolkit authorization.",
                "parameters": [{"name": "marker", "in": "query", "required": True,
                                "schema": {"type": "string", "minLength": 1, "maxLength": 128}}],
                "security": [{"delegated": ["records.read"]}],
                "responses": {"200": {"description": "Authorized marker",
                    "content": {"application/json": {"schema": {"type": "object", "properties": {
                        "marker": {"type": "string"}, "mode": {"type": "string"}, "generation": {"type": "integer"}
                    }}}}}, "401": {"description": "Authorization required"}},
            }}},
            "components": {"securitySchemes": {"delegated": {"type": "oauth2", "flows": {"clientCredentials": {
                "tokenUrl": self.metadata(mode)["token_endpoint"],
                "scopes": {"records.read": "Read fixture records"},
            }, "authorizationCode": {
                "authorizationUrl": self.metadata(mode)["authorization_endpoint"],
                "tokenUrl": self.metadata(mode)["token_endpoint"],
                "scopes": {"records.read": "Read fixture records"},
            }}}}},
        }

    def register(self, mode, body):
        if not mode.startswith("dcr-") or not isinstance(body, dict):
            raise ProtocolError("invalid_client_metadata")
        if body.get("redirect_uris") != [self.callback]:
            raise ProtocolError("invalid_redirect_uri")
        if body.get("token_endpoint_auth_method", "none") not in ("none", "client_secret_post"):
            raise ProtocolError("invalid_client_metadata")
        if len(self.clients) >= MAX_ENTRIES:
            raise ProtocolError("temporarily_unavailable", 503)
        client = "emulator-" + secrets.token_urlsafe(18)
        secret = secrets.token_urlsafe(32) if mode == "dcr-secret" else ""
        self.clients[(mode, client)] = secret
        self.counts[mode]["registrations"] += 1
        result = {**body, "client_id": client,
                  "token_endpoint_auth_method": "client_secret_post" if secret else "none"}
        if secret:
            result.update(client_secret=secret, client_secret_expires_at=0)
        return result

    def audience(self, mode):
        return f"{self.internal}/{mode}/mcp" if mode.startswith("dcr-") else None

    def check_resource(self, mode, form):
        # MCP requires a resource indicator on both authorization and token requests.
        if mode.startswith("dcr-") and form.get("resource") != self.audience(mode):
            raise ProtocolError("invalid_target")

    def resource_metadata(self, mode):
        if not mode.startswith("dcr-"):
            raise ProtocolError("not_found", 404)
        return {"resource": self.audience(mode), "authorization_servers": [f"{self.internal}/{mode}"],
                "scopes_supported": ["records.read"], "bearer_methods_supported": ["header"]}

    def begin_authorization(self, mode, form):
        self.check_resource(mode, form)
        if (mode, form.get("client_id")) not in self.clients:
            raise ProtocolError("invalid_client")
        if form.get("redirect_uri") != self.callback:
            raise ProtocolError("invalid_redirect_uri")
        if form.get("response_type") != "code" or not form.get("state"):
            raise ProtocolError("invalid_request")
        if form.get("code_challenge_method") != "S256" or not re.fullmatch(r"[A-Za-z0-9_-]{43}", form.get("code_challenge", "")):
            raise ProtocolError("invalid_request")
        if not set(form.get("scope", "").split()) <= SCOPES:
            raise ProtocolError("invalid_scope")
        ticket = secrets.token_urlsafe(32)
        self.bounded_put(self.pending, ticket, (mode, form), 120)
        return ticket

    def decide(self, mode, form):
        ticket = form.get("ticket", "")
        bound_mode, request = self.get(self.pending, ticket)
        if mode != bound_mode or form.get("decision") not in ("allow", "deny"):
            raise ProtocolError("invalid_request")
        del self.pending[ticket]
        result = {"state": request["state"]}
        if form["decision"] == "allow":
            code = secrets.token_urlsafe(32)
            self.bounded_put(self.codes, code, (mode, request), 120)
            self.counts[mode]["consents"] += 1
            result["code"] = code
        else:
            self.counts[mode]["denials"] += 1
            result["error"] = "access_denied"
        return self.callback + "?" + urlencode(result)

    def authenticate(self, mode, form):
        client = form.get("client_id", "")
        expected = self.clients.get((mode, client))
        if expected is None or not secrets.compare_digest(expected, form.get("client_secret", "")):
            raise ProtocolError("invalid_client", 401)
        return client

    def token(self, mode, form):
        self.check_resource(mode, form)
        client = self.authenticate(mode, form)
        grant = form.get("grant_type")
        generation = 1
        if grant == "authorization_code":
            key = form.get("code", "")
            bound_mode, request = self.get(self.codes, key)
            verifier = form.get("code_verifier", "")
            challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
            if (bound_mode != mode or client != request["client_id"] or form.get("redirect_uri") != request["redirect_uri"]
                    or not re.fullmatch(r"[A-Za-z0-9._~-]{43,128}", verifier)
                    or not secrets.compare_digest(challenge, request["code_challenge"])):
                raise ProtocolError("invalid_grant")
            scope = request.get("scope", "")
            consumed, consumed_key = self.codes, key
        elif grant == "refresh_token":
            key = form.get("refresh_token", "")
            bound_mode, bound_client, scope, generation = self.get(self.refresh, key)
            if bound_mode != mode or bound_client != client:
                raise ProtocolError("invalid_grant")
            if "scope" in form and not set(form["scope"].split()) <= set(scope.split()):
                raise ProtocolError("invalid_scope")
            scope = form.get("scope", scope)
            generation += 1
            consumed, consumed_key = self.refresh, key
        elif grant == "client_credentials" and mode == "stored":
            scope = form.get("scope", "records.read")
            if not set(scope.split()) <= SCOPES:
                raise ProtocolError("invalid_scope")
            consumed, consumed_key = None, None
        else:
            raise ProtocolError("unsupported_grant_type")
        access, refresh = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
        value = (mode, client, scope, generation)
        self.bounded_put(self.access, access, value, self.token_ttl)
        if grant != "client_credentials":
            try:
                self.bounded_put(self.refresh, refresh, value, 3600)
            except ProtocolError:
                del self.access[access]
                raise
        if consumed is not None:
            del consumed[consumed_key]
        self.counts[mode][grant] += 1
        result = {"access_token": access, "token_type": "Bearer", "expires_in": self.token_ttl, "scope": scope}
        if grant != "client_credentials":
            result["refresh_token"] = refresh
        return result

    def authorize_resource(self, mode, authorization):
        try:
            token = authorization.removeprefix("Bearer ") if authorization.startswith("Bearer ") else ""
            bound_mode, _, scope, generation = self.get(self.access, token)
            if bound_mode != mode:
                raise ProtocolError("invalid_token", 401)
        except ProtocolError:
            self.counts[mode]["unauthorized_resource"] += 1
            raise ProtocolError("invalid_token", 401) from None
        if "records.read" not in scope.split():
            raise ProtocolError("insufficient_scope", 403)
        return generation

    def resource(self, mode, authorization, marker):
        generation = self.authorize_resource(mode, authorization)
        if not isinstance(marker, str) or not 1 <= len(marker) <= 128:
            raise ProtocolError("invalid_request")
        self.counts[mode]["resource_calls"] += 1
        return {"marker": marker, "mode": mode, "generation": generation}

    def mcp(self, mode, authorization, body):
        self.authorize_resource(mode, authorization)
        if not isinstance(body, dict) or body.get("jsonrpc") != "2.0" or not isinstance(body.get("params", {}), dict):
            raise ProtocolError("invalid_request")
        method, params = body.get("method"), body.get("params", {})
        if method == "notifications/initialized" and "id" not in body:
            return None
        if "id" not in body or not isinstance(body["id"], (str, int)) or isinstance(body["id"], bool):
            raise ProtocolError("invalid_request")
        if method == "initialize":
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {"listChanged": False}},
                      "serverInfo": {"name": f"elitea-aha-like-{mode}", "version": "1.0.0"}}
        elif method == "tools/list":
            result = {"tools": [{"name": "echo_marker", "description": "Read and echo one authorized fixture marker.",
                                "inputSchema": {"type": "object", "properties": {"marker": {"type": "string", "minLength": 1, "maxLength": 128}},
                                                "required": ["marker"], "additionalProperties": False},
                                "annotations": {"readOnlyHint": True, "destructiveHint": False, "idempotentHint": True, "openWorldHint": False}}]}
        elif method == "tools/call":
            args = params.get("arguments", {})
            if params.get("name") != "echo_marker" or not isinstance(args, dict) or set(args) != {"marker"}:
                return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32602, "message": "Invalid tool parameters"}}
            value = self.resource(mode, authorization, args["marker"])
            result = {"content": [{"type": "text", "text": json.dumps(value)}], "isError": False}
        elif method == "ping":
            result = {}
        else:
            return {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "Method not found"}}
        self.counts[mode]["mcp_" + method.replace("/", "_")] += 1
        return {"jsonrpc": "2.0", "id": body["id"], "result": result}


class Server(ThreadingHTTPServer):
    daemon_threads = False
    allow_reuse_address = True

    def __init__(self, address, issuer):
        self.issuer = issuer
        self.slots = threading.BoundedSemaphore(32)
        super().__init__(address, Handler)

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *_):
        pass  # Request paths can contain authorization codes and state.

    def send(self, status, body, content_type="application/json", location=None, challenge=None):
        raw = body.encode() if isinstance(body, str) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Pragma", "no-cache")
        self.send_header("Referrer-Policy", "no-referrer")
        # Chrome checks the post-consent redirect against form-action too.
        callback = urlsplit(self.server.issuer.callback)
        callback_origin = f"{callback.scheme}://{callback.netloc}"
        self.send_header("Content-Security-Policy", f"default-src 'none'; form-action 'self' {callback_origin}; frame-ancestors 'none'")
        if location:
            self.send_header("Location", location)
        if challenge:
            self.send_header("WWW-Authenticate", challenge)
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.dispatch("GET")

    def do_POST(self):
        self.dispatch("POST")

    def do_DELETE(self):
        self.dispatch("DELETE")

    def dispatch(self, method):
        issuer = self.server.issuer
        mode, route = None, None
        try:
            parts = urlsplit(self.path)
            if len(self.path) > MAX_BODY:
                raise ProtocolError("invalid_request", 413)
            if method == "GET" and parts.path == "/healthz":
                self.send(200, {"ok": True, "test_only": True})
                return
            if method == "GET" and parts.path == "/stats":
                with issuer.lock:
                    self.send(200, issuer.counts)
                return
            mode, _, route = parts.path.lstrip("/").partition("/")
            prefix = "/.well-known/oauth-authorization-server/"
            if parts.path.startswith(prefix):
                mode, route = parts.path[len(prefix):], ".well-known/oauth-authorization-server"
            if mode not in MODES:
                raise ProtocolError("not_found", 404)
            body = {}
            if method == "POST":
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= MAX_BODY or self.headers.get("Transfer-Encoding"):
                    raise ProtocolError("invalid_request", 413)
                raw = self.rfile.read(length).decode("utf-8")
                expected_type = "application/json" if route in ("register", "mcp") else "application/x-www-form-urlencoded"
                if self.headers.get_content_type() != expected_type:
                    raise ProtocolError("invalid_request", 415)
                body = json.loads(raw) if route in ("register", "mcp") else scalar_form(raw)
                if self.headers.get("Authorization", "").startswith("Basic "):
                    if not isinstance(body, dict) or "client_id" in body or "client_secret" in body:
                        raise ProtocolError("invalid_request")
                    decoded = base64.b64decode(self.headers["Authorization"][6:], validate=True).decode()
                    from urllib.parse import unquote_plus
                    client, separator, secret = decoded.partition(":")
                    if not separator:
                        raise ProtocolError("invalid_client", 401)
                    body.update(client_id=unquote_plus(client), client_secret=unquote_plus(secret))
            with issuer.lock:
                if method == "GET" and route in (".well-known/openid-configuration", ".well-known/oauth-authorization-server"):
                    self.send(200, issuer.metadata(mode))
                elif method == "GET" and route == "openapi.json":
                    self.send(200, issuer.spec(mode))
                elif method == "GET" and route == "resource-metadata":
                    self.send(200, issuer.resource_metadata(mode))
                elif route == "mcp" and mode.startswith("dcr-"):
                    if method != "POST":
                        raise ProtocolError("method_not_allowed", 405)
                    result = issuer.mcp(mode, self.headers.get("Authorization", ""), body)
                    self.send(202 if result is None else 200, "" if result is None else result)
                elif method == "POST" and route == "register":
                    self.send(201, issuer.register(mode, body))
                elif method == "GET" and route == "authorize":
                    ticket = issuer.begin_authorization(mode, scalar_form(parts.query))
                    self.send(200, '<!doctype html><html><title>Elitea OAuth test consent</title>'
                        f'<h1>Test authorization: {html.escape(mode)}</h1><p>No real account or provider is used.</p>'
                        f'<form method="post" action="/{mode}/authorize"><input type="hidden" name="ticket" value="{ticket}">'
                        '<button name="decision" value="allow">Authorize test toolkit</button>'
                        '<button name="decision" value="deny">Deny test authorization</button></form></html>', "text/html; charset=utf-8")
                elif method == "POST" and route == "authorize":
                    self.send(303, "", location=issuer.decide(mode, body))
                elif method == "POST" and route == "token":
                    self.send(200, issuer.token(mode, body))
                elif method == "GET" and route == "api/echo" and mode == "stored":
                    self.send(200, issuer.resource(mode, self.headers.get("Authorization", ""), scalar_form(parts.query).get("marker")))
                elif method == "POST" and route == "revoke":
                    client = issuer.authenticate(mode, body)
                    token = body.get("token", "")
                    for store in (issuer.refresh, issuer.access):
                        entry = store.get(token)
                        if entry and entry[1][:2] == (mode, client):
                            del store[token]
                    self.send(200, {})
                else:
                    raise ProtocolError("not_found", 404)
        except ProtocolError as error:
            challenge = None
            if error.status == 401:
                if route == "mcp":
                    challenge = f'Bearer resource_metadata="{issuer.internal}/{mode}/resource-metadata", scope="records.read"'
                elif error.code == "invalid_client":
                    challenge = 'Basic realm="test-issuer"'
                else:
                    challenge = 'Bearer error="invalid_token"'
            elif error.status == 403:
                challenge = 'Bearer error="insufficient_scope", scope="records.read"'
            self.send(error.status, {"error": error.code}, challenge=challenge)
        except (ValueError, UnicodeError, TypeError):
            self.send(400, {"error": "invalid_request"})


def main():
    if os.environ.get("OAUTH_EMULATOR_ENABLED") != "test-only":
        raise SystemExit("Set OAUTH_EMULATOR_ENABLED=test-only for this isolated fixture.")
    secret = os.environ["OAUTH_EMULATOR_CLIENT_SECRET"]
    if len(secret) < 24:
        raise SystemExit("The fixture needs a separate client secret of at least 24 characters.")
    issuer = Issuer(os.environ["OAUTH_EMULATOR_INTERNAL_ORIGIN"], os.environ["OAUTH_EMULATOR_BROWSER_ORIGIN"],
                    os.environ["OAUTH_EMULATOR_CALLBACK"], secret, int(os.environ.get("OAUTH_EMULATOR_TOKEN_TTL", "60")))
    server = Server(("0.0.0.0", 8443), issuer)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain("/opt/oauth-emulator/tls/server-chain.crt", "/opt/oauth-emulator/tls/server.key")
    server.socket = context.wrap_socket(server.socket, server_side=True)
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    runner = threading.Thread(target=server.serve_forever)
    runner.start()
    try:
        stop.wait()
    finally:
        server.shutdown()
        server.server_close()
        runner.join()


if __name__ == "__main__":
    main()
