"""Protocol tests for the isolated OAuth fixture. No provider credentials."""

import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
from http.client import HTTPConnection
import io
import json
import threading
import unittest
from contextlib import redirect_stderr
from urllib.parse import parse_qs, urlencode, urlsplit

from server import Issuer, MAX_BODY, MAX_ENTRIES, MODES, ProtocolError, Server, scalar_form


class GrantFixture:
    def setUp(self):
        self.now = 1000
        self.secret = "fixture-client-secret-not-a-provider-credential"
        self.issuer = Issuer("https://oauth-emulator:8443", "https://localhost:18443",
                             "http://localhost:18084/app/mcp-auth-callback", self.secret, 60, lambda: self.now)
        self.verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
        self.challenge = base64.urlsafe_b64encode(hashlib.sha256(self.verifier.encode()).digest()).rstrip(b"=").decode()

    def client(self, mode):
        if mode == "stored":
            return {"client_id": "emulator-stored-client", "client_secret": self.secret}
        result = self.issuer.register(mode, {"redirect_uris": [self.issuer.callback], "token_endpoint_auth_method": "none"})
        return {"client_id": result["client_id"], "client_secret": result.get("client_secret", "")}

    def authorization(self, mode, client):
        form = {"client_id": client["client_id"], "redirect_uri": self.issuer.callback, "response_type": "code",
                "state": "test-state", "code_challenge": self.challenge, "code_challenge_method": "S256",
                "scope": "records.read offline_access"}
        if mode.startswith("dcr-"):
            form["resource"] = self.issuer.audience(mode)
        return form

    def code_form(self, mode, client):
        form = self.authorization(mode, client)
        ticket = self.issuer.begin_authorization(mode, form)
        location = self.issuer.decide(mode, {"ticket": ticket, "decision": "allow"})
        query = parse_qs(urlsplit(location).query)
        self.assertEqual(query["state"], ["test-state"])
        result = {**client, "grant_type": "authorization_code", "code": query["code"][0],
                  "code_verifier": self.verifier, "redirect_uri": self.issuer.callback}
        if "resource" in form:
            result["resource"] = form["resource"]
        return result

    def assert_error(self, code, function, *args):
        with self.assertRaises(ProtocolError) as error:
            function(*args)
        self.assertEqual(error.exception.code, code)
        return error.exception


class IssuerTests(GrantFixture, unittest.TestCase):
    def test_openapi_only_exposes_stored_client_grants(self):
        spec = self.issuer.spec("stored")
        self.assertEqual(set(spec["components"]["securitySchemes"]["delegated"]["flows"]), {"authorizationCode", "clientCredentials"})
        self.assertNotIn("registration_endpoint", self.issuer.metadata("stored"))
        for mode in MODES[1:]:
            self.assert_error("not_found", self.issuer.spec, mode)
            self.assertIn("registration_endpoint", self.issuer.metadata(mode))

    def test_aha_like_registration_can_issue_secret_for_none_request(self):
        self.assertTrue(self.client("dcr-secret")["client_secret"])
        self.assertEqual(self.client("dcr-public")["client_secret"], "")

    def test_registration_rejects_unknown_redirects_and_modes(self):
        for redirects in ([], ["https://other.example/callback"], [self.issuer.callback, "https://other.example"]):
            self.assert_error("invalid_redirect_uri", self.issuer.register, "dcr-secret", {"redirect_uris": redirects})
        self.assert_error("invalid_client_metadata", self.issuer.register, "stored", {})

    def test_all_code_grants_refresh_and_protected_markers(self):
        for mode in MODES:
            with self.subTest(mode=mode):
                client = self.client(mode)
                form = self.code_form(mode, client)
                token = self.issuer.token(mode, form)
                self.assertEqual(self.issuer.resource(mode, "Bearer " + token["access_token"], "first")["generation"], 1)
                refresh = {**client, "grant_type": "refresh_token", "refresh_token": token["refresh_token"]}
                if mode.startswith("dcr-"):
                    refresh["resource"] = self.issuer.audience(mode)
                renewed = self.issuer.token(mode, refresh)
                self.assertNotEqual(token["refresh_token"], renewed["refresh_token"])
                self.assertEqual(self.issuer.resource(mode, "Bearer " + renewed["access_token"], "second")["generation"], 2)
                self.assert_error("invalid_grant", self.issuer.token, mode, form)
                self.assert_error("invalid_grant", self.issuer.token, mode, refresh)

    def test_invalid_grant_does_not_consume_valid_code(self):
        client = self.client("stored")
        form = self.code_form("stored", client)
        for key, value, error in (("client_secret", "wrong", "invalid_client"), ("code_verifier", "x" * 43, "invalid_grant"),
                                  ("redirect_uri", "https://other.example", "invalid_grant")):
            self.assert_error(error, self.issuer.token, "stored", {**form, key: value})
        self.assertIn("access_token", self.issuer.token("stored", form))

    def test_dcr_secret_cannot_be_replaced_by_stored_secret(self):
        client = self.client("dcr-secret")
        form = self.code_form("dcr-secret", client)
        for secret in ("", self.secret):
            self.assert_error("invalid_client", self.issuer.token, "dcr-secret", {**form, "client_secret": secret})
        self.assertIn("access_token", self.issuer.token("dcr-secret", form))

    def test_mcp_requires_resource_indicator_at_both_endpoints(self):
        client = self.client("dcr-public")
        auth = self.authorization("dcr-public", client)
        form = self.code_form("dcr-public", client)
        for resource in (None, "https://other.example/mcp", self.issuer.audience("dcr-secret")):
            self.assert_error("invalid_target", self.issuer.begin_authorization, "dcr-public", {**auth, "resource": resource})
            self.assert_error("invalid_target", self.issuer.token, "dcr-public", {**form, "resource": resource})

    def test_client_credentials_does_not_return_refresh_token(self):
        client = self.client("stored")
        token = self.issuer.token("stored", {**client, "grant_type": "client_credentials", "scope": "records.read"})
        self.assertNotIn("refresh_token", token)
        self.assertEqual(self.issuer.resource("stored", "Bearer " + token["access_token"], "machine")["marker"], "machine")

    def test_dcr_surface_rejects_client_credentials(self):
        for mode in MODES[1:]:
            self.assert_error("unsupported_grant_type", self.issuer.token, mode,
                              {**self.client(mode), "grant_type": "client_credentials", "resource": self.issuer.audience(mode)})

    def test_expiry_scope_and_audience_are_enforced(self):
        form = self.code_form("stored", self.client("stored"))
        token = self.issuer.token("stored", form)
        bearer = "Bearer " + token["access_token"]
        self.assert_error("invalid_token", self.issuer.resource, "dcr-public", bearer, "wrong-audience")
        self.now += 61
        self.assert_error("invalid_token", self.issuer.resource, "stored", bearer, "expired")
        token = self.issuer.token("stored", {**self.client("stored"), "grant_type": "client_credentials", "scope": "offline_access"})
        error = self.assert_error("insufficient_scope", self.issuer.resource, "stored", "Bearer " + token["access_token"], "no-scope")
        self.assertEqual(error.status, 403)

    def test_refresh_cannot_escalate_scope_or_change_client(self):
        client = self.client("dcr-public")
        token = self.issuer.token("dcr-public", self.code_form("dcr-public", client))
        form = {**client, "grant_type": "refresh_token", "refresh_token": token["refresh_token"], "resource": self.issuer.audience("dcr-public")}
        self.assert_error("invalid_scope", self.issuer.token, "dcr-public", {**form, "scope": "admin"})
        self.assert_error("invalid_grant", self.issuer.token, "dcr-public", {**form, **self.client("dcr-public")})

    def test_denial_returns_state_and_never_issues_code(self):
        ticket = self.issuer.begin_authorization("stored", self.authorization("stored", self.client("stored")))
        query = parse_qs(urlsplit(self.issuer.decide("stored", {"ticket": ticket, "decision": "deny"})).query)
        self.assertEqual(query, {"state": ["test-state"], "error": ["access_denied"]})
        self.assertEqual(self.issuer.codes, {})
        self.assert_error("invalid_grant", self.issuer.decide, "stored", {"ticket": ticket, "decision": "allow"})

    def test_expired_pending_and_code_rejected(self):
        client = self.client("stored")
        form = self.code_form("stored", client)
        ticket = self.issuer.begin_authorization("stored", self.authorization("stored", client))
        self.now += 121
        self.assert_error("invalid_grant", self.issuer.token, "stored", form)
        self.assert_error("invalid_grant", self.issuer.decide, "stored", {"ticket": ticket, "decision": "allow"})

    def test_state_is_bounded_and_expired_entries_reclaimed(self):
        for i in range(MAX_ENTRIES):
            self.issuer.bounded_put(self.issuer.pending, str(i), {}, 1)
        self.assert_error("temporarily_unavailable", self.issuer.bounded_put, self.issuer.pending, "overflow", {}, 1)
        self.now += 2
        self.issuer.bounded_put(self.issuer.pending, "new", {}, 1)
        self.assertEqual(len(self.issuer.pending), 1)

    def test_form_rejects_duplicate_parameters(self):
        self.assert_error("invalid_request", scalar_form, "code=one&code=two")


class HTTPTests(GrantFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.server = Server(("127.0.0.1", 0), self.issuer)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01})
        self.thread.start()
        self.addCleanup(self.stop_server)

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.assertFalse(self.thread.is_alive())

    def request(self, method, path, body=None, headers=None):
        conn = HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        try:
            conn.request(method, path, body, headers or {})
            response = conn.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            conn.close()

    def test_http_mcp_discovery_dcr_and_tool_call(self):
        mode = "dcr-secret"
        rpc = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}}
        headers = {"Content-Type": "application/json"}
        status, response_headers, _ = self.request("POST", f"/{mode}/mcp", json.dumps(rpc), headers)
        self.assertEqual(status, 401)
        self.assertIn(f'/{mode}/resource-metadata"', response_headers["WWW-Authenticate"])
        status, _, body = self.request("GET", f"/{mode}/resource-metadata")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["resource"], self.issuer.audience(mode))
        status, _, body = self.request("GET", f"/.well-known/oauth-authorization-server/{mode}")
        self.assertEqual(status, 200)
        self.assertIn("registration_endpoint", json.loads(body))
        status, _, body = self.request("POST", f"/{mode}/register", json.dumps({"redirect_uris": [self.issuer.callback], "token_endpoint_auth_method": "none"}), headers)
        self.assertEqual(status, 201)
        client = json.loads(body)
        token = self.issuer.token(mode, self.code_form(mode, client))
        headers["Authorization"] = "Bearer " + token["access_token"]
        for method in ("initialize", "tools/list", "ping"):
            status, _, body = self.request("POST", f"/{mode}/mcp", json.dumps({**rpc, "method": method}), headers)
            self.assertEqual(status, 200)
            self.assertIn("result", json.loads(body))
        status, _, body = self.request("POST", f"/{mode}/mcp", json.dumps({**rpc, "method": "tools/call", "params": {"name": "echo_marker", "arguments": {"marker": "DCR_PROTOCOL_PROOF"}}}), headers)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(json.loads(body)["result"]["content"][0]["text"])["marker"], "DCR_PROTOCOL_PROOF")
        self.assertEqual(self.issuer.counts[mode]["resource_calls"], 1)
        status, _, body = self.request("POST", f"/{mode}/mcp", json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}), headers)
        self.assertEqual((status, body), (202, b""))
        self.assertEqual(self.request("GET", f"/{mode}/mcp")[0], 405)
        self.assertEqual(self.request("DELETE", f"/{mode}/mcp")[0], 405)

    def test_http_basic_credentials_revocation_and_no_secret_logs(self):
        auth = base64.b64encode(("emulator-stored-client:" + self.secret).encode()).decode()
        headers = {"Authorization": "Basic " + auth, "Content-Type": "application/x-www-form-urlencoded"}
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status, response_headers, raw = self.request("POST", "/stored/token", "grant_type=client_credentials", headers)
            self.assertEqual(status, 200)
            self.assertEqual(response_headers["Cache-Control"], "no-store")
            token = json.loads(raw)["access_token"]
            self.assertEqual(self.request("GET", "/stored/api/echo?marker=ok", headers={"Authorization": "Bearer " + token})[0], 200)
            self.assertEqual(self.request("POST", "/stored/revoke", urlencode({"token": token}), headers)[0], 200)
            self.assertEqual(self.request("GET", "/stored/api/echo?marker=revoked", headers={"Authorization": "Bearer " + token})[0], 401)
        self.assertEqual(stderr.getvalue(), "")

    def test_http_rejects_body_bounds_content_types_and_malformed_json(self):
        for body, content_type, status in (("x" * (MAX_BODY + 1), "application/json", 413),
                                         ("{", "application/json", 400), ("{}", "text/plain", 415)):
            self.assertEqual(self.request("POST", "/dcr-public/register", body, {"Content-Type": content_type})[0], status)

    def test_consent_policy_allows_only_self_and_configured_callback_origin(self):
        auth = self.authorization("stored", self.client("stored"))
        status, headers, _ = self.request("GET", "/stored/authorize?" + urlencode(auth))
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Security-Policy"],
                         "default-src 'none'; form-action 'self' http://localhost:18084; frame-ancestors 'none'")
        ticket = next(iter(self.issuer.pending))
        status, headers, _ = self.request("POST", "/stored/authorize", urlencode({"ticket": ticket, "decision": "allow"}),
                                          {"Content-Type": "application/x-www-form-urlencoded"})
        self.assertEqual(status, 303)
        self.assertTrue(headers["Location"].startswith(self.issuer.callback + "?"))

    def test_parallel_code_exchange_has_one_winner(self):
        form = self.code_form("stored", self.client("stored"))
        def exchange(_):
            return self.request("POST", "/stored/token", urlencode(form), {"Content-Type": "application/x-www-form-urlencoded"})[0]
        with ThreadPoolExecutor(max_workers=2) as executor:
            self.assertEqual(sorted(executor.map(exchange, range(2))), [200, 400])


if __name__ == "__main__":
    unittest.main(verbosity=2)
